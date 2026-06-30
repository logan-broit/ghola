"""lme-qa-sweep: drive the reader+judge over a (compressor, budget, sample) grid.

The rate-distortion frontier is traced by running the existing reader+judge once
per setting. Each ``(compressor, budget, sample)`` is a LEAF: its own directory
holding an ``answers.jsonl`` + ``judgments.jsonl``, written by a normal
``lme-qa-run`` / ``lme-qa-judge`` invocation pointed at the leaf's paths. That
reuse is the whole point — the existing PER-QUESTION resume (append-only,
last-wins, usage-window-aware) operates unchanged within each leaf. The sweep
adds only LEAF-LEVEL resume on top: a leaf whose judgments already cover every
question is skipped wholesale on a re-run, so a multi-window measurement run
picks up at the first unfinished leaf.

Leaf layout::

    <outdir>/<compressor>__b<budget>/s<i>/answers.jsonl
    <outdir>/<compressor>__b<budget>/s<i>/judgments.jsonl
    <outdir>/<compressor>__b<budget>/s<i>/report.md

``budget`` is ``None`` -> ``bNone`` in the path (the ``full`` compressor ignores
the budget). ``s<i>`` is the 0-based sample index.

Window-aware stop: the underlying reader/judge stop submitting and save progress
when the subscription usage window is exhausted (or after a run of consecutive
failures), but they exit 0 either way (partial progress is on disk). So the
sweep does not read a return code for exhaustion; instead, AFTER running a
stage it checks whether the leaf reached completion (every expected question
succeeded). If a stage ran but did not complete the leaf, the window is
exhausted (or the failure is systemic) — the sweep stops and reports which
leaves remain, so a re-run after the window resets resumes from there.
"""

from __future__ import annotations

import argparse
import dataclasses
import json
import re
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from . import cli, compress
from .scorer import _SCORERS

# Allowed values for the enum-typed compressor hyperparameters. Validated in
# ``load_settings`` so a typo in a settings file fails before any claude call.
_QUERY_MODES = ("agnostic", "aware")
_OUTPUT_FORMS = ("prose", "structured")
_EDGE_METRICS = ("jaccard", "tfidf", "bm25")
_PRUNE_LEVELS = ("turn", "sentence", "token")


@dataclass(frozen=True)
class Setting:
    """One rate-distortion setting: a compressor, a token budget, and optional
    compressor-specific parameters.

    ``expansion_reserve`` is specific to ``extractive_relevance_expanded``: it
    controls the fraction of the budget reserved for the neighbor-expansion
    pass (0.0 = no reserve = plain extractive; 1.0 = all budget goes to
    expansion). ``None`` means "use the compressor's default" (0.3 for the
    expanded compressor). The reserve is an un-swept hyperparameter — sweeping
    2–3 values at a fixed budget is the minimum to interpret an expanded-vs-
    extractive comparison without the reserve-mistuning confound.

    The Stage B compressors add five more optional, compressor-specific
    hyperparameters (each ``None`` when the setting does not carry it):

    * ``query_mode`` ("agnostic" | "aware") — statistical_prune, graph_community,
      perplexity_prune, llm_distill;
    * ``output_form`` ("prose" | "structured") — llm_distill;
    * ``oracle_model`` (vLLM model name) — perplexity_prune;
    * ``edge_metric`` ("jaccard" | …) — graph_community;
    * ``prune_level`` ("turn" | "sentence" | "token") — reserved for a future
      variant; accepted + validated here but no compressor consumes it yet.

    Every field participates in the leaf-dir name (see ``leaf_dir`` / ``_hp_tag``)
    so two settings that differ in ANY field get distinct leaves — results never
    collide.
    """

    compressor: str
    budget: int | None
    expansion_reserve: float | None = None
    query_mode: str | None = None
    output_form: str | None = None
    prune_level: str | None = None
    oracle_model: str | None = None
    edge_metric: str | None = None


def _budget_tag(budget: int | None) -> str:
    """Filesystem-safe budget tag for a leaf path. ``None`` -> ``None`` (the
    full compressor's no-budget case), else the integer."""
    return "None" if budget is None else str(budget)


def _reserve_tag(reserve: float | None) -> str:
    """Filesystem-safe expansion-reserve tag for a leaf path.

    ``None`` -> empty (the compressor's default). Non-``None`` -> ``r0.3``
    style tag so multiple reserve values at the same budget get distinct leaf
    directories. The separator is ``__r`` (matching the ``__b`` budget
    separator) so the aggregator can parse it back out.
    """
    if reserve is None:
        return ""
    # Format: strip trailing zeros (0.30 -> 0.3, 0.0 -> 0), keep at least
    # one decimal digit so the tag is never empty-string (which would
    # collide with the None/default case in the directory name).
    s = f"{reserve:.1f}"
    return f"__r{s}"


def _sanitize_tag(value: str) -> str:
    """Make a hyperparameter value safe to embed as one path component.

    Replaces ``/`` (and any other char outside ``[A-Za-z0-9._-]``) with ``_`` so
    a value like a HF repo id (``org/model``) cannot introduce a directory
    separator into the leaf name. The substitution is deterministic and
    collision-prone only across values that already differ solely by an unsafe
    char (acceptable: such pairs do not occur in practice).
    """
    return re.sub(r"[^A-Za-z0-9._-]", "_", value)


def _hp_tag(
    *,
    expansion_reserve: float | None = None,
    query_mode: str | None = None,
    output_form: str | None = None,
    prune_level: str | None = None,
    oracle_model: str | None = None,
    edge_metric: str | None = None,
) -> str:
    """Fixed-order short tag for the compressor hyperparameters in a leaf name.

    Each field contributes a tag ONLY when set (``None`` -> empty), mirroring
    ``_reserve_tag``'s "empty when None" rule so a setting carrying no new field
    produces a byte-identical leaf name to the pre-Stage-B format (back-compat:
    existing done leaves are still skipped on resume). Because EVERY field emits
    a distinct, ordered tag, two settings that differ in any one field get
    different leaf names — the collision that would silently overwrite results is
    structurally impossible.

    Fixed order: budget(reserve), q, of, pl, om, e — the budget + reserve tags
    are emitted by ``leaf_dir`` directly (reserve via ``_reserve_tag`` for
    byte-compat with existing leaves); this helper covers the five new fields in
    order ``q, of, pl, om, e``.
    """
    parts: list[str] = []
    if expansion_reserve is not None:
        parts.append(_reserve_tag(expansion_reserve))
    if query_mode is not None:
        parts.append(f"__q{query_mode}")
    if output_form is not None:
        parts.append(f"__of{output_form}")
    if prune_level is not None:
        parts.append(f"__pl{prune_level}")
    if oracle_model is not None:
        parts.append(f"__om{_sanitize_tag(oracle_model)}")
    if edge_metric is not None:
        parts.append(f"__e{edge_metric}")
    return "".join(parts)


def leaf_dir(
    outdir: Path,
    compressor: str,
    budget: int | None,
    sample: int,
    expansion_reserve: float | None = None,
    query_mode: str | None = None,
    output_form: str | None = None,
    prune_level: str | None = None,
    oracle_model: str | None = None,
    edge_metric: str | None = None,
) -> Path:
    """The directory for one ``(compressor, budget, ...hyperparameters, sample)``
    leaf.

    ``<outdir>/<compressor>__b<budget>[__r<reserve>][__q…][__of…][__pl…][__om…][__e…]/s<sample>``.
    Stable + UNIQUE per distinct setting: every hyperparameter that is set emits
    a fixed-order tag (see ``_hp_tag``), so two settings differing in ANY field
    map to different directories — their results can never overwrite each other.
    A setting with all new fields ``None`` produces a byte-identical leaf name to
    the pre-Stage-B format, so existing done leaves are unaffected on resume.
    """
    tag = _hp_tag(
        expansion_reserve=expansion_reserve,
        query_mode=query_mode,
        output_form=output_form,
        prune_level=prune_level,
        oracle_model=oracle_model,
        edge_metric=edge_metric,
    )
    name = f"{compressor}__b{_budget_tag(budget)}{tag}"
    return outdir / name / f"s{sample}"


def _leaf_dir_for(outdir: Path, setting: Setting, sample: int) -> Path:
    """``leaf_dir`` keyed off a ``Setting`` — the single place that maps a
    setting's full field set to its leaf path, so the sweep, the label strings,
    and the remaining-leaves report all agree on the name."""
    return leaf_dir(
        outdir,
        setting.compressor,
        setting.budget,
        sample,
        expansion_reserve=setting.expansion_reserve,
        query_mode=setting.query_mode,
        output_form=setting.output_form,
        prune_level=setting.prune_level,
        oracle_model=setting.oracle_model,
        edge_metric=setting.edge_metric,
    )


def _setting_label(setting: Setting, sample: int) -> str:
    """The human label for a leaf in the sweep log + remaining-leaves report:
    the leaf's setting-dir name plus ``/s<sample>`` (mirrors the leaf path so the
    report is unambiguous across hyperparameter variants)."""
    name = _leaf_dir_for(Path(""), setting, sample).parent.name
    return f"{name}/s{sample}"


def _write_setting_sidecar(setting_dir: Path, setting: Setting) -> None:
    """Write ``<setting_dir>/setting.json`` = the full ``Setting`` as JSON.

    One sidecar per setting (the parent of the sample leaves), so the aggregator
    can recover every field — including the ones not encoded losslessly in the
    dir name (e.g. an oracle model whose name was sanitized) — without parsing.
    Idempotent: samples of the same setting rewrite the same bytes.
    """
    setting_dir.mkdir(parents=True, exist_ok=True)
    (setting_dir / "setting.json").write_text(
        json.dumps(dataclasses.asdict(setting))
    )


def load_settings(path: Path) -> list[Setting]:
    """Load the settings spec: a JSON list of ``{compressor, budget}`` objects.

    Each compressor name is validated against the registry so a typo fails here
    (with the leaf tree untouched) rather than after burning subscription tokens
    on a partial sweep. ``budget`` defaults to ``None`` when omitted.
    """
    data = json.loads(path.read_text())
    if not isinstance(data, list):
        raise ValueError(f"{path}: settings must be a JSON list of objects")
    settings: list[Setting] = []
    for i, item in enumerate(data):
        if not isinstance(item, dict) or "compressor" not in item:
            raise ValueError(
                f"{path}: settings entry at index {i} must be an object with "
                f"a 'compressor' key"
            )
        name = item["compressor"]
        if name not in compress.REGISTRY:
            raise ValueError(
                f"{path}: settings entry at index {i} names unknown compressor "
                f"{name!r}; known: {sorted(compress.REGISTRY)}"
            )
        budget = item.get("budget")
        if budget is not None and not isinstance(budget, int):
            raise ValueError(
                f"{path}: settings entry at index {i} 'budget' must be an "
                f"integer or null, got {budget!r}"
            )
        expansion_reserve = item.get("expansion_reserve")
        if expansion_reserve is not None:
            if not isinstance(expansion_reserve, (int, float)):
                raise ValueError(
                    f"{path}: settings entry at index {i} 'expansion_reserve' "
                    f"must be a number or null, got {expansion_reserve!r}"
                )
            if not (0.0 <= float(expansion_reserve) <= 1.0):
                raise ValueError(
                    f"{path}: settings entry at index {i} 'expansion_reserve' "
                    f"must be in [0.0, 1.0], got {expansion_reserve!r}"
                )
            expansion_reserve = float(expansion_reserve)

        # Stage B compressor hyperparameters: each optional (None when absent),
        # enums validated against their allowed sets, oracle_model any non-empty
        # string. Errors name the offending entry index + bad value (mirroring
        # the expansion_reserve style) so a settings typo fails before any
        # claude call.
        query_mode = _enum_field(item, "query_mode", _QUERY_MODES, path, i)
        output_form = _enum_field(item, "output_form", _OUTPUT_FORMS, path, i)
        prune_level = _enum_field(item, "prune_level", _PRUNE_LEVELS, path, i)
        edge_metric = _enum_field(item, "edge_metric", _EDGE_METRICS, path, i)
        oracle_model = item.get("oracle_model")
        if oracle_model is not None:
            if not isinstance(oracle_model, str) or not oracle_model.strip():
                raise ValueError(
                    f"{path}: settings entry at index {i} 'oracle_model' must "
                    f"be a non-empty string or null, got {oracle_model!r}"
                )

        settings.append(
            Setting(
                compressor=name,
                budget=budget,
                expansion_reserve=expansion_reserve,
                query_mode=query_mode,
                output_form=output_form,
                prune_level=prune_level,
                oracle_model=oracle_model,
                edge_metric=edge_metric,
            )
        )
    return settings


def _enum_field(
    item: dict[str, Any],
    key: str,
    allowed: tuple[str, ...],
    path: Path,
    index: int,
) -> str | None:
    """Read + validate an optional enum-typed settings field.

    ``None`` (absent) is allowed; any present value must be a string in
    ``allowed`` or a ValueError naming the path, index, key, bad value, and the
    allowed set is raised — diagnosable from the message alone.
    """
    value = item.get(key)
    if value is None:
        return None
    if not isinstance(value, str) or value not in allowed:
        raise ValueError(
            f"{path}: settings entry at index {index} {key!r} must be one of "
            f"{list(allowed)} or null, got {value!r}"
        )
    return value


def _expected_qids(dataset: Path, run: Path) -> set[str]:
    """Question ids a leaf must cover: present in the run file AND the dataset.

    The reader skips run lines whose qid is absent from the dataset, so leaf
    completeness is judged against the intersection — otherwise a run file with
    an orphan qid would make every leaf look perpetually incomplete.
    """
    by_qid = cli._load_dataset(dataset)
    out: set[str] = set()
    for line in cli._load_jsonl(run):
        qid = line.get("question_id")
        if qid in by_qid:
            out.add(qid)
    return out


def _leaf_complete(judgments: Path, expected: set[str]) -> bool:
    """True if ``judgments`` records a succeeded row for every expected qid.

    Reuses the reader's ``_completed_ids`` (latest-row-per-qid status==succeeded)
    so leaf-level resume keys on the SAME completion notion the per-question
    resume uses — a leaf is done iff judging finished for all its questions.
    """
    return expected.issubset(cli._completed_ids(judgments))


def _stage_complete(out_path: Path, expected: set[str]) -> bool:
    """True if a stage's append-only output covers every expected qid (succeeded).

    Used both for the reader's answers (before judging) and the judge's
    judgments (leaf completion) — both files share the same row schema
    (``question_id`` + ``status``), so ``_completed_ids`` applies uniformly.
    """
    return expected.issubset(cli._completed_ids(out_path))


def sweep_main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(
        prog="lme-qa-sweep",
        description="Run the reader+judge over a (compressor, budget, sample) "
        "grid, one leaf directory each, with leaf-level resume.",
    )
    ap.add_argument("--dataset", required=True, type=Path, help="LongMemEval dataset JSON")
    ap.add_argument("--run", required=True, type=Path, help="retrieve results JSONL")
    ap.add_argument("--outdir", required=True, type=Path, help="sweep output root")
    ap.add_argument(
        "--settings",
        required=True,
        type=Path,
        help="JSON list of {compressor, budget} settings to sweep",
    )
    ap.add_argument(
        "--samples",
        type=int,
        default=1,
        help="independent samples per setting (default 1). Each sample is its "
        "own leaf dir; only meaningful if the reader is stochastic — see the "
        "self-consistency pilot note in the bench README.",
    )
    ap.add_argument("--k", type=int, default=10, help="top-K sessions for context (default 10)")
    ap.add_argument(
        "--backend",
        choices=cli.BACKENDS,
        default="claude-code",
        help="execution backend (default claude-code: the subscription path)",
    )
    ap.add_argument(
        "--scorer",
        default="truthsayer",
        choices=sorted(_SCORERS),
        help="relevance scorer for the extractive_relevance compressor "
        "(default truthsayer)",
    )
    ap.add_argument("--parallel", type=int, default=2, help="concurrent claude -p calls per stage (default 2)")
    ap.add_argument("--timeout-s", type=int, default=300, help="per-call timeout seconds (default 300)")
    ap.add_argument("--poll-interval", type=int, default=60, help="batches backend poll seconds")
    args = ap.parse_args(argv)

    settings = load_settings(args.settings)
    if args.samples < 1:
        sys.exit("error: --samples must be >= 1")
    expected = _expected_qids(args.dataset, args.run)
    if not expected:
        sys.exit("error: no questions in common between --dataset and --run")

    # Materialize the full grid of leaves up front so the order is deterministic
    # and the "remaining" report is meaningful.
    leaves: list[tuple[Setting, int]] = [
        (s, i) for s in settings for i in range(args.samples)
    ]

    print(
        f"sweep: {len(settings)} settings x {args.samples} samples = "
        f"{len(leaves)} leaves over {len(expected)} questions",
        file=sys.stderr,
    )

    ran = 0
    skipped = 0
    completed = 0
    for idx, (setting, sample) in enumerate(leaves):
        leaf = _leaf_dir_for(args.outdir, setting, sample)
        answers = leaf / "answers.jsonl"
        judgments = leaf / "judgments.jsonl"
        report = leaf / "report.md"

        # Leaf-level resume: a leaf already covering every expected question is
        # skipped wholesale — no reader, no judge, zero claude calls.
        if _leaf_complete(judgments, expected):
            skipped += 1
            completed += 1
            continue

        leaf.mkdir(parents=True, exist_ok=True)
        # The per-setting sidecar (the aggregator's source of truth) — written
        # next to the sample leaves, idempotently re-written per sample.
        _write_setting_sidecar(leaf.parent, setting)
        label = _setting_label(setting, sample)
        print(f"sweep: leaf {idx + 1}/{len(leaves)} {label}", file=sys.stderr)
        ran += 1

        # --- reader: a normal lme-qa-run pointed at the leaf's answers/state.
        reader_argv = [
            "--dataset", str(args.dataset),
            "--run", str(args.run),
            "--out", str(answers),
            "--state", str(answers.with_suffix(answers.suffix + ".state.json")),
            "--k", str(args.k),
            "--compressor", setting.compressor,
            "--scorer", args.scorer,
            "--backend", args.backend,
            "--parallel", str(args.parallel),
            "--timeout-s", str(args.timeout_s),
            "--poll-interval", str(args.poll_interval),
        ]
        if setting.budget is not None:
            reader_argv += ["--budget", str(setting.budget)]
        if setting.expansion_reserve is not None:
            reader_argv += ["--expansion-reserve", str(setting.expansion_reserve)]
        # Stage B hyperparameters: forward each only when the setting carries it,
        # so the reader's argparse defaults stand for legacy settings (and the
        # cc backend's flag surface is identical to a hand-run lme-qa-run).
        if setting.query_mode is not None:
            reader_argv += ["--query-mode", setting.query_mode]
        if setting.output_form is not None:
            reader_argv += ["--output-form", setting.output_form]
        if setting.prune_level is not None:
            reader_argv += ["--prune-level", setting.prune_level]
        if setting.oracle_model is not None:
            reader_argv += ["--oracle-model", setting.oracle_model]
        if setting.edge_metric is not None:
            reader_argv += ["--edge-metric", setting.edge_metric]
        cli.run_main(reader_argv)

        # The reader exits 0 even on a usage-window stop (partial progress saved).
        # If it did not complete the leaf's answers, the window is exhausted (or
        # the failures are systemic) — stop the sweep and report the remainder.
        if not _stage_complete(answers, expected):
            return _stop_incomplete(leaves, idx, "reader", label, args.outdir)

        # --- judge: a normal lme-qa-judge over the leaf's answers.
        judge_argv = [
            "--dataset", str(args.dataset),
            "--answers", str(answers),
            "--out", str(judgments),
            "--report", str(report),
            "--state", str(judgments.with_suffix(judgments.suffix + ".state.json")),
            "--backend", args.backend,
            "--parallel", str(args.parallel),
            "--timeout-s", str(args.timeout_s),
            "--poll-interval", str(args.poll_interval),
        ]
        cli.judge_main(judge_argv)

        if not _leaf_complete(judgments, expected):
            return _stop_incomplete(leaves, idx, "judge", label, args.outdir)

        completed += 1

    print(
        f"sweep: done — {completed}/{len(leaves)} leaves complete "
        f"({ran} ran, {skipped} skipped as already complete)",
        file=sys.stderr,
    )
    return 0


def _stop_incomplete(
    leaves: list[tuple[Setting, int]],
    idx: int,
    stage: str,
    label: str,
    outdir: Path,
) -> int:
    """Report the unfinished + remaining leaves and stop the sweep cleanly.

    Called when a stage ran but did not complete its leaf — the usage window is
    exhausted (the reader/judge already saved partial per-question progress). A
    re-run after the window resets resumes: the per-question resume finishes
    this leaf, then leaf-level resume skips the already-complete earlier leaves.
    Returns 0 (a clean, expected stop — not an error), matching the
    reader/judge's window-exhaustion contract.
    """
    remaining = [_setting_label(s, i) for s, i in leaves[idx:]]
    print(
        f"sweep: stopped in {stage} on leaf {label} (usage window exhausted "
        f"or systemic failure; partial progress saved). "
        f"{len(remaining)} leaf(s) remain: {', '.join(remaining)}. "
        f"Re-run the same command after the window resets to continue.",
        file=sys.stderr,
    )
    return 0


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(sweep_main())
