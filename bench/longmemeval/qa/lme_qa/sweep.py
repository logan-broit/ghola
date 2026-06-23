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
import json
import sys
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from . import cli, compress
from .scorer import _SCORERS


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
    """

    compressor: str
    budget: int | None
    expansion_reserve: float | None = None


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


def leaf_dir(
    outdir: Path,
    compressor: str,
    budget: int | None,
    sample: int,
    expansion_reserve: float | None = None,
) -> Path:
    """The directory for one ``(compressor, budget, expansion_reserve, sample)``
    leaf.

    ``<outdir>/<compressor>__b<budget>[__r<reserve>]/s<sample>``. Stable +
    parseable so the aggregator (lme-qa-rd) can walk the tree and recover the
    setting from the directory name without a side index. The optional
    ``__r<reserve>`` suffix only appears when the setting carries an explicit
    ``expansion_reserve`` so existing leaves without the suffix are unaffected.
    """
    name = f"{compressor}__b{_budget_tag(budget)}{_reserve_tag(expansion_reserve)}"
    return outdir / name / f"s{sample}"


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
        settings.append(
            Setting(
                compressor=name,
                budget=budget,
                expansion_reserve=expansion_reserve,
            )
        )
    return settings


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
        leaf = leaf_dir(
            args.outdir,
            setting.compressor,
            setting.budget,
            sample,
            expansion_reserve=setting.expansion_reserve,
        )
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
        label = f"{setting.compressor}__b{_budget_tag(setting.budget)}{_reserve_tag(setting.expansion_reserve)}/s{sample}"
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
    remaining = [
        f"{s.compressor}__b{_budget_tag(s.budget)}{_reserve_tag(s.expansion_reserve)}/s{i}"
        for s, i in leaves[idx:]
    ]
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
