"""Eval orchestrator + ``seeding-eval-run`` CLI.

The loop is the load-bearing artifact of the harness:

    for case in held_out_cases:
        for variant in ("none", "correct_era", "wrong_era"):
            query = render_query(case, variant)
            hits = ghola.recall(query=query, ...)
            score H2 (P@5) for this variant
            if variant == "none": score H1.c (entropy over top-k buckets)

H3 lifts are derived at the end from the per-variant H2 aggregates.

Aggregation lives in :func:`_aggregate`, a pure function that takes the
per-case scoring results + run metadata and returns a ``RunReport``. The
orchestration function only handles I/O — recall HTTP calls, query
rendering, scoring per case, and recording failures. This split keeps
the math unit-testable without standing up a real ghola.

Bucket-lookup gap: ``RecallHit`` (internal/core/types.go) has no tags or
raw_event field, so we can't derive a hit's module bucket from the
response alone. The orchestrator accepts an optional
``event_buckets: dict[event_id -> bucket]`` mapping (Option B from the
design doc); if absent, every hit is bucketed as ``"unknown"`` and H1
entropy collapses to 0. The case builder produces this mapping
naturally since it already knows every event's files.
"""
from __future__ import annotations

import argparse
import dataclasses
import datetime as _dt
import hashlib
import json
import logging
import subprocess
import sys
from collections import defaultdict
from dataclasses import dataclass, field
from pathlib import Path
from typing import Iterable

from .cases import EvalCase, HELD_OUT_FRACTION_PERCENT, is_held_out
from .contexts import render_query
from .ghola_client import GholaClient
from .metrics import p_at_5, shannon_entropy
from .report import (
    CaseFailure,
    H1Result,
    H2Result,
    H3PerEra,
    H3Result,
    RunReport,
    report_to_json,
)

logger = logging.getLogger(__name__)


_VARIANTS: tuple[str, ...] = ("none", "correct_era", "wrong_era")
_UNKNOWN_BUCKET = "unknown"


# ---------- Internal scoring records (orchestrator → aggregator) ----------


@dataclass(frozen=True)
class _VariantResult:
    """Per-(case, variant) scoring outcome.

    ``failed`` segregates exception cases so per-variant denominators
    can exclude them. ``hit`` is the P@5 score (0.0 or 1.0) when
    ``failed`` is False; otherwise it is unspecified.
    """
    failed: bool
    hit: float = 0.0


@dataclass(frozen=True)
class _CaseResult:
    """Per-case bundle of variant outcomes + the H1 entropy.

    H1 entropy is scored only on the ``"none"`` variant per the design
    (we want the recall's intrinsic locality, free of context-priming).
    Carrying it on the case keeps the aggregator pure.
    """
    case: EvalCase
    variants: dict[str, _VariantResult] = field(default_factory=dict)
    h1_entropy: float = 0.0
    h1_failed: bool = False


# ---------- Pure aggregation ----------


def _mean_or_zero(xs: list[float]) -> float:
    """Mean of `xs`, or 0.0 if empty. Matches the design doc's policy
    of reporting 0 for empty slices rather than NaN — downstream
    consumers (report writers, dashboards) handle 0 cleanly."""
    return sum(xs) / len(xs) if xs else 0.0


def _aggregate(
    case_results: list[_CaseResult],
    failures: list[CaseFailure],
    *,
    n_total: int,
    n_held_out: int,
    run_id: str,
    config_hash: str,
) -> RunReport:
    """Build a RunReport from per-case scoring records.

    Per-variant denominators exclude failed cases (a case that errored
    on ``correct_era`` does not count against ``correct_era``'s P@5).
    Per-era H3 entries are emitted only when all three variants have at
    least one successful case for that era, matching the per-bucket
    rule for H1.

    Pure function: no I/O, no side effects on inputs, deterministic.
    """
    # --- H2: P@5 per variant ---
    p_at_5_per_variant: dict[str, list[float]] = {v: [] for v in _VARIANTS}
    # --- H1: entropy across "none" variant only ---
    h1_entropies: list[float] = []
    per_bucket_entropies: dict[str, list[float]] = defaultdict(list)
    # --- per-era H2 for H3 slicing ---
    per_era: dict[str, dict[str, list[float]]] = defaultdict(
        lambda: {v: [] for v in _VARIANTS}
    )

    for cr in case_results:
        for variant in _VARIANTS:
            vr = cr.variants.get(variant)
            if vr is None or vr.failed:
                continue
            p_at_5_per_variant[variant].append(vr.hit)
            per_era[cr.case.era][variant].append(vr.hit)
        # H1 only on "none" — and only when both the recall succeeded
        # and we had a ground-truth bucket to credit.
        if not cr.h1_failed and cr.case.module_path_buckets:
            none_vr = cr.variants.get("none")
            if none_vr is not None and not none_vr.failed:
                h1_entropies.append(cr.h1_entropy)
                gt_bucket = cr.case.module_path_buckets[0]
                per_bucket_entropies[gt_bucket].append(cr.h1_entropy)

    h2 = H2Result(
        p_at_5_none=_mean_or_zero(p_at_5_per_variant["none"]),
        p_at_5_correct_era=_mean_or_zero(p_at_5_per_variant["correct_era"]),
        p_at_5_wrong_era=_mean_or_zero(p_at_5_per_variant["wrong_era"]),
        # n_cases = the smallest variant denominator (the "all three
        # variants succeeded" intersection lower bound). Using min keeps
        # the report honest when some variants had failures.
        n_cases=min(len(p_at_5_per_variant[v]) for v in _VARIANTS),
    )

    h1 = H1Result(
        avg_entropy=_mean_or_zero(h1_entropies),
        n_cases=len(h1_entropies),
        per_bucket={b: _mean_or_zero(es) for b, es in per_bucket_entropies.items()},
    )

    h3_per_era: dict[str, H3PerEra] = {}
    for era, d in per_era.items():
        # Emit per-era only when every variant has at least one success.
        if not all(d[v] for v in _VARIANTS):
            continue
        n_era = min(len(d[v]) for v in _VARIANTS)
        p_none = _mean_or_zero(d["none"])
        p_correct = _mean_or_zero(d["correct_era"])
        p_wrong = _mean_or_zero(d["wrong_era"])
        h3_per_era[era] = H3PerEra(
            n_cases=n_era,
            l_correct=p_correct - p_none,
            l_decay=p_correct - p_wrong,
        )

    h3 = H3Result(
        l_correct=h2.p_at_5_correct_era - h2.p_at_5_none,
        l_decay=h2.p_at_5_correct_era - h2.p_at_5_wrong_era,
        per_era=h3_per_era,
    )

    return RunReport(
        run_id=run_id,
        config_hash=config_hash,
        n_cases=n_total,
        n_held_out=n_held_out,
        h1=h1,
        h2=h2,
        h3=h3,
        failures=tuple(failures),
    )


# ---------- Orchestration ----------


def _bucket_for_hit(hit: dict, event_buckets: dict[str, str] | None) -> str:
    """Map a recall hit to its module bucket via the lookup table.

    RecallHit carries no tags (see internal/core/types.go), so the
    orchestrator can't derive a bucket from the wire shape alone — it
    has to lean on a pre-built id→bucket map from the case builder.
    Missing entries fall back to ``"unknown"`` so H1 still computes
    (entropy over a degenerate distribution) instead of crashing.
    """
    if event_buckets is None:
        return _UNKNOWN_BUCKET
    return event_buckets.get(hit.get("id", ""), _UNKNOWN_BUCKET)


def _trace_record(
    *,
    case: EvalCase,
    variant: str,
    query: str,
    hits: list[dict],
    hit_score: float,
    event_buckets: dict[str, str] | None,
) -> dict:
    """Build a per-(case, variant) trace dict ready for JSONL serialization.

    The bucket is precomputed at trace time so D4's report writer
    doesn't need to reach back into ``event_buckets``.
    """
    top_k = [
        {
            "event_id": h.get("id", ""),
            "tier": h.get("tier", ""),
            "score": h.get("score", 0.0),
            "bucket": _bucket_for_hit(h, event_buckets),
        }
        for h in hits
    ]
    return {
        "case_id": case.case_id,
        "variant": variant,
        "query": query,
        "top_k": top_k,
        "hit_p_at_5": hit_score,
        "ground_truth_event_ids": list(case.ground_truth_event_ids),
    }


def _trace_error(
    *, case: EvalCase, variant: str, query: str, error: str
) -> dict:
    return {
        "case_id": case.case_id,
        "variant": variant,
        "query": query,
        "error": error,
    }


def _default_run_id() -> str:
    """ISO-second timestamp + git short SHA when available.

    Two reasons to prefer composition over UUID: ordering by name in
    out-dirs, and a one-glance pointer to the code that produced the
    run. If git isn't reachable (worktree detached, no .git), we still
    emit a usable id with ``no-git`` in place of the SHA.
    """
    ts = _dt.datetime.now(_dt.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    try:
        sha = subprocess.check_output(
            ["git", "rev-parse", "--short", "HEAD"],
            stderr=subprocess.DEVNULL,
            text=True,
        ).strip()
    except (subprocess.CalledProcessError, FileNotFoundError, OSError):
        sha = "no-git"
    return f"{ts}-{sha}"


def _default_config_hash(*, k: int, primitives: bool = False) -> str:
    """Hash of the inputs that affect results, so longitudinal runs can
    be grouped by configuration. Keep the input set small and stable.

    ``primitives`` flips the chapterhouse 4th sub-list on/off — same
    case set, different ranking — so it MUST be in the fingerprint or
    Phase-2a A/B runs would alias to one config_hash and look like
    duplicates downstream.
    """
    parts = [
        f"k={k}",
        # H3.c: differentiation is a tags_any filter, not a string
        # template. Hashing the H3 mode keeps the run-config fingerprint
        # distinct from prefix-based runs.
        "h3_mode=tags_any_filter",
        f"held_out_pct={HELD_OUT_FRACTION_PERCENT}",
        f"primitives={'true' if primitives else 'false'}",
    ]
    return hashlib.sha256("|".join(parts).encode()).hexdigest()[:16]


def run_eval(
    cases: list[EvalCase],
    *,
    workspace_id: str,
    user_id: str,
    ghola: GholaClient,
    event_buckets: dict[str, str] | None = None,
    k: int = 20,
    primitives: bool = False,
    run_id: str | None = None,
    config_hash: str | None = None,
) -> tuple[RunReport, list[dict]]:
    """Run the eval loop. Returns ``(report, traces)``.

    Only held-out cases contribute to metrics. Every (case, variant)
    pair produces a trace (success or error). Failures land in
    ``report.failures`` with their error messages; per-variant
    denominators exclude them.

    ``event_buckets`` is a mapping ``event_id -> module_bucket`` used
    to assign hits to buckets for H1 entropy. When None, every hit
    becomes ``"unknown"`` and H1 entropy reads 0; the report still
    serializes cleanly (documented degenerate state).

    ``primitives`` is the Phase 2a opt-in — when True, every recall
    call sends ``primitives=true`` so ghola folds the chapterhouse
    Hebbian sub-list into RRF as a 6th tier. Default False keeps the
    legacy 5-tier ranking byte-identical.

    The orchestration is single-threaded by design — the eval set is
    small (held-out ≈ 20% of ~50 cases) and parallelism would obscure
    failure attribution. YAGNI on async until volume forces it.
    """
    if run_id is None:
        run_id = _default_run_id()
    if config_hash is None:
        config_hash = _default_config_hash(k=k, primitives=primitives)

    held_out = [c for c in cases if c.held_out]
    case_results: list[_CaseResult] = []
    failures: list[CaseFailure] = []
    traces: list[dict] = []

    for case in held_out:
        variants_out: dict[str, _VariantResult] = {}
        h1_entropy = 0.0
        h1_failed = False

        for variant in _VARIANTS:
            query, tags_any = render_query(case, variant)
            try:
                hits = ghola.recall(
                    query=query,
                    workspace_id=workspace_id,
                    user_id=user_id,
                    k=k,
                    tags_any=tags_any,
                    primitives=primitives,
                )
            except Exception as exc:  # noqa: BLE001 — record, don't drop
                err = f"{type(exc).__name__}: {exc}"
                failures.append(
                    CaseFailure(case_id=case.case_id, variant=variant, error=err)
                )
                variants_out[variant] = _VariantResult(failed=True)
                traces.append(
                    _trace_error(
                        case=case, variant=variant, query=query, error=err
                    )
                )
                if variant == "none":
                    h1_failed = True
                continue

            top_ids = [h.get("id", "") for h in hits]
            try:
                hit = p_at_5(list(case.ground_truth_event_ids), top_ids)
            except ValueError as exc:
                # Empty ground_truth shouldn't happen — case builder
                # guarantees at least one PR id. Treat as a hard case
                # failure if it does.
                err = f"p_at_5 failed: {exc}"
                failures.append(
                    CaseFailure(case_id=case.case_id, variant=variant, error=err)
                )
                variants_out[variant] = _VariantResult(failed=True)
                traces.append(
                    _trace_error(
                        case=case, variant=variant, query=query, error=err
                    )
                )
                if variant == "none":
                    h1_failed = True
                continue

            variants_out[variant] = _VariantResult(failed=False, hit=hit)
            traces.append(
                _trace_record(
                    case=case,
                    variant=variant,
                    query=query,
                    hits=hits,
                    hit_score=hit,
                    event_buckets=event_buckets,
                )
            )

            if variant == "none":
                # H1.c: entropy over module-buckets of top-K. Empty hits
                # → entropy 0 by convention (degenerate distribution).
                buckets = [_bucket_for_hit(h, event_buckets) for h in hits]
                if buckets:
                    h1_entropy = shannon_entropy(buckets)
                else:
                    h1_entropy = 0.0

        case_results.append(
            _CaseResult(
                case=case,
                variants=variants_out,
                h1_entropy=h1_entropy,
                h1_failed=h1_failed,
            )
        )

    report = _aggregate(
        case_results,
        failures,
        n_total=len(cases),
        n_held_out=len(held_out),
        run_id=run_id,
        config_hash=config_hash,
    )
    return report, traces


# ---------- EvalCase JSONL round-trip ----------
#
# Tuples become JSON arrays; deserialization re-tuples so the frozen
# dataclass stays hashable. Keep this beside the orchestrator since the
# CLI is the only consumer right now — promote to cases.py if other
# code grows a need.


def case_to_jsonl_line(case: EvalCase) -> str:
    """Serialize one EvalCase to a single JSONL line."""
    return json.dumps(dataclasses.asdict(case))


def case_from_dict(d: dict) -> EvalCase:
    """Reconstruct an EvalCase from a dict (e.g. ``json.loads`` of a JSONL line)."""
    return EvalCase(
        case_id=d["case_id"],
        issue_id=d["issue_id"],
        thread_session_id=d["thread_session_id"],
        query_text=d["query_text"],
        era=d["era"],
        ground_truth_event_ids=tuple(d["ground_truth_event_ids"]),
        module_path_buckets=tuple(d["module_path_buckets"]),
        held_out=d["held_out"],
    )


def load_cases_jsonl(path: Path) -> list[EvalCase]:
    """Load EvalCases from a JSONL file. One case per line."""
    cases: list[EvalCase] = []
    with path.open("r", encoding="utf-8") as fh:
        for line in fh:
            line = line.strip()
            if not line:
                continue
            cases.append(case_from_dict(json.loads(line)))
    return cases


# ---------- CLI output writers (D4 will polish) ----------


# Format helpers — keep numbers consistent across markdown + terminal.
# P@5/lifts: signed 3dp.  Entropy: 2dp + " bits".  Counts: int.
def _fmt_p(x: float) -> str:
    return f"{x:.3f}"


def _fmt_lift(x: float) -> str:
    return f"{x:+.3f}"


def _fmt_entropy(x: float) -> str:
    return f"{x:.2f} bits"


def _md_table(headers: list[str], rows: list[list[str]]) -> str:
    """Render a simple GFM table. No alignment heroics — just `|` + `---`.

    Markdown renderers do their own alignment; we keep the source
    diff-friendly. Empty rows produce a header-only table (so empty
    sections still render as a real table, not an aside).
    """
    sep = ["---"] * len(headers)
    lines = ["| " + " | ".join(headers) + " |", "| " + " | ".join(sep) + " |"]
    for row in rows:
        lines.append("| " + " | ".join(row) + " |")
    return "\n".join(lines)


def render_markdown(report: RunReport) -> str:
    """Render the human-readable markdown summary of a run.

    Sections, in order: top-level metadata, H2 (retrieval precision),
    H3 (lifts derived from H2), H1 (module-path entropy), H3 per-era
    breakdown, failures. The failures section always renders, even
    when empty, so an operator scanning the report never has to wonder
    whether failures are missing or zero.
    """
    lines: list[str] = []
    lines.append("# Eval Run Report")
    lines.append("")
    lines.append(f"**Run ID**: {report.run_id}")
    lines.append(f"**Config hash**: {report.config_hash}")
    lines.append(
        f"**Cases**: {report.n_cases} total, {report.n_held_out} held out"
    )
    lines.append("")

    # --- H2: P@5 per variant ---
    lines.append("## H2 — Retrieval precision (P@5)")
    lines.append("")
    h2 = report.h2
    lines.append(_md_table(
        ["Variant", "P@5", "n_cases"],
        [
            ["none",        _fmt_p(h2.p_at_5_none),        str(h2.n_cases)],
            ["correct_era", _fmt_p(h2.p_at_5_correct_era), str(h2.n_cases)],
            ["wrong_era",   _fmt_p(h2.p_at_5_wrong_era),   str(h2.n_cases)],
        ],
    ))
    lines.append("")
    lines.append(
        f"**L_correct**: {_fmt_lift(report.h3.l_correct)} (correct_era - none)"
    )
    lines.append(
        f"**L_decay**:   {_fmt_lift(report.h3.l_decay)} (correct_era - wrong_era)"
    )
    lines.append("")

    # --- H1.c: module-path entropy ---
    lines.append("## H1.c — Module-path entropy `H(context | top_k)`")
    lines.append("")
    h1 = report.h1
    lines.append(
        f"**Avg entropy**: {_fmt_entropy(h1.avg_entropy)} (n={h1.n_cases} cases)"
    )
    lines.append("")
    if h1.per_bucket:
        # Sort by avg entropy desc — biggest spread first is what the
        # eyeball wants. Ties broken by bucket name for determinism.
        items = sorted(h1.per_bucket.items(), key=lambda kv: (-kv[1], kv[0]))
        lines.append(_md_table(
            ["Ground-truth bucket", "avg H"],
            [[bucket, f"{ent:.2f}"] for bucket, ent in items],
        ))
    else:
        lines.append("(no per-bucket data)")
    lines.append("")

    # --- H3: per-era ---
    lines.append("## H3 — Per-era breakdown")
    lines.append("")
    if report.h3.per_era:
        # Sort by era name desc so newer eras (v15 > v14) come first.
        eras = sorted(report.h3.per_era.items(), key=lambda kv: kv[0], reverse=True)
        lines.append(_md_table(
            ["Era", "n", "L_correct", "L_decay"],
            [
                [era, str(pe.n_cases), _fmt_lift(pe.l_correct), _fmt_lift(pe.l_decay)]
                for era, pe in eras
            ],
        ))
    else:
        lines.append("(no per-era data)")
    lines.append("")

    # --- Failures: always rendered, even when empty ---
    lines.append("## Failures")
    lines.append("")
    if report.failures:
        lines.append(_md_table(
            ["case_id", "variant", "error"],
            [
                # Truncate runaway error messages so the table stays scannable.
                [f.case_id, f.variant, _truncate(f.error, 120)]
                for f in report.failures
            ],
        ))
    else:
        lines.append("(0 cases failed)")
    lines.append("")

    return "\n".join(lines)


def _truncate(s: str, n: int) -> str:
    """Truncate to ``n`` chars with an ellipsis suffix when cut. Markdown
    pipes inside the string are escaped so they don't break the table."""
    s = s.replace("|", "\\|").replace("\n", " ")
    return s if len(s) <= n else s[: n - 1] + "…"


def _trace_sort_key(trace: dict) -> tuple[str, str]:
    """Stable sort key for traces: (case_id, variant). Missing keys
    fall back to empty strings so a malformed trace still sorts."""
    return (trace.get("case_id", ""), trace.get("variant", ""))


def _write_outputs(
    out_dir: Path, report: RunReport, traces: Iterable[dict]
) -> None:
    """Write the three eval-run artifacts: report.md, report.json,
    per-case-traces.jsonl.

    Traces are sorted by (case_id, variant) before writing so downstream
    grep/diff is friendly. Markdown is rendered via :func:`render_markdown`.
    """
    out_dir.mkdir(parents=True, exist_ok=True)
    (out_dir / "report.json").write_text(report_to_json(report), encoding="utf-8")
    (out_dir / "report.md").write_text(render_markdown(report), encoding="utf-8")
    sorted_traces = sorted(traces, key=_trace_sort_key)
    with (out_dir / "per-case-traces.jsonl").open("w", encoding="utf-8") as fh:
        for trace in sorted_traces:
            fh.write(json.dumps(trace, ensure_ascii=False, sort_keys=False) + "\n")


def _print_summary(report: RunReport, out_dir: Path) -> None:
    """Compact 6-line terminal summary. Mentions every H-number, the
    run id, the failures count, and the output directory so the operator
    knows where to look next."""
    h2 = report.h2
    print(f"Eval run {report.run_id}")
    print(
        f"  cases: {report.n_held_out} held / {report.n_cases} total | "
        f"failures: {len(report.failures)}"
    )
    print(
        "  H2  P@5 none/correct/wrong:  "
        f"{_fmt_p(h2.p_at_5_none)} / "
        f"{_fmt_p(h2.p_at_5_correct_era)} / "
        f"{_fmt_p(h2.p_at_5_wrong_era)}"
    )
    print(
        "  H1  avg entropy: "
        f"{_fmt_entropy(report.h1.avg_entropy)} over "
        f"{report.h1.n_cases} cases ({len(report.h1.per_bucket)} buckets)"
    )
    print(
        "  H3  L_correct = "
        f"{_fmt_lift(report.h3.l_correct)}  "
        f"L_decay = {_fmt_lift(report.h3.l_decay)}"
    )
    print(
        f"  outputs: {out_dir}/report.md, report.json, per-case-traces.jsonl"
    )


# ---------- CLI entry ----------


def main() -> None:
    """``seeding-eval-run`` entry point.

    Loads cases from JSONL, drives :func:`run_eval`, writes outputs to
    ``--out-dir``, and prints a 5-line summary.
    """
    parser = argparse.ArgumentParser(prog="seeding-eval-run")
    parser.add_argument(
        "--cases",
        type=Path,
        required=True,
        help="JSONL file: one EvalCase per line (from D4's case-bundle writer)",
    )
    parser.add_argument(
        "--workspace", required=True, help="ghola workspace UUID"
    )
    parser.add_argument("--user", required=True, help="ghola user UUID")
    parser.add_argument("--out-dir", type=Path, required=True)
    parser.add_argument(
        "--ghola-base-url",
        default="http://localhost:7421",
        help="ghola base URL (default: http://localhost:7421)",
    )
    parser.add_argument("--k", type=int, default=20, help="recall top-K (default 20)")
    parser.add_argument(
        "--event-buckets",
        type=Path,
        default=None,
        help="optional JSON file: object mapping event_id (str) → module_bucket (str)",
    )
    parser.add_argument(
        "--primitives",
        action="store_true",
        help=(
            "enable Hebbian primitives ranking (Phase 2a A/B): sends "
            "primitives=true to ghola so chapterhouse's 4th sub-list is "
            "folded into RRF as a 6th tier. Default off — legacy 5-tier "
            "ranking, byte-identical wire shape."
        ),
    )
    args = parser.parse_args()

    logging.basicConfig(
        level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s"
    )

    cases = load_cases_jsonl(args.cases)
    event_buckets: dict[str, str] | None = None
    if args.event_buckets is not None:
        event_buckets = json.loads(args.event_buckets.read_text(encoding="utf-8"))

    if not cases:
        print("no cases loaded — refusing to run", file=sys.stderr)
        sys.exit(2)

    with GholaClient(base_url=args.ghola_base_url) as ghola:
        report, traces = run_eval(
            cases,
            workspace_id=args.workspace,
            user_id=args.user,
            ghola=ghola,
            event_buckets=event_buckets,
            k=args.k,
            primitives=args.primitives,
        )

    _write_outputs(args.out_dir, report, traces)
    _print_summary(report, args.out_dir)


__all__ = [
    "run_eval",
    "main",
    "render_markdown",
    "case_to_jsonl_line",
    "case_from_dict",
    "load_cases_jsonl",
]
