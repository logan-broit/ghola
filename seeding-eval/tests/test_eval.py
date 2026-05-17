"""Unit tests for the eval orchestrator.

Per the task hard constraints, the orchestration loop itself is observed
via D6 (integration) — these tests exercise the *pure* aggregation
function `_aggregate` with hand-built per-case results, the bucket
lookup helpers, and the EvalCase JSONL round-trip. No mocks; canned
data structures only.
"""
from __future__ import annotations

import json
import math
from pathlib import Path

import pytest

from seeding_eval.cases import EvalCase
from seeding_eval.eval import (
    _aggregate,
    _bucket_for_hit,
    _CaseResult,
    _UNKNOWN_BUCKET,
    _VariantResult,
    _print_summary,
    _write_outputs,
    case_from_dict,
    case_to_jsonl_line,
    load_cases_jsonl,
    render_markdown,
)
from seeding_eval.report import (
    CaseFailure,
    H1Result,
    H2Result,
    H3PerEra,
    H3Result,
    RunReport,
)


def _case(case_id: str, era: str, gt_bucket: str = "packages/next/src/server") -> EvalCase:
    """Minimal EvalCase factory for aggregation tests."""
    return EvalCase(
        case_id=case_id,
        issue_id=case_id.split("#")[-1],
        thread_session_id=f"sess-{case_id}",
        query_text=f"q for {case_id}",
        era=era,
        ground_truth_event_ids=("evt-1",),
        module_path_buckets=(gt_bucket,),
        held_out=True,
    )


def _all_success(hits: dict[str, float]) -> dict[str, _VariantResult]:
    return {v: _VariantResult(failed=False, hit=h) for v, h in hits.items()}


# ---------- _aggregate (pure) ----------


def test_aggregate_basic_arithmetic():
    """4 held-out cases, all variants succeed.
    P@5 hand-arithmetic: none=2/4, correct=3/4, wrong=1/4.
    H1 entropies = [1.0, 1.0, 0.0, 2.0] → avg = 1.0.
    """
    crs = [
        _CaseResult(
            case=_case("case-r#1", "v13"),
            variants=_all_success({"none": 1.0, "correct_era": 1.0, "wrong_era": 0.0}),
            h1_entropy=1.0,
        ),
        _CaseResult(
            case=_case("case-r#2", "v13"),
            variants=_all_success({"none": 1.0, "correct_era": 1.0, "wrong_era": 1.0}),
            h1_entropy=1.0,
        ),
        _CaseResult(
            case=_case("case-r#3", "v13"),
            variants=_all_success({"none": 0.0, "correct_era": 1.0, "wrong_era": 0.0}),
            h1_entropy=0.0,
        ),
        _CaseResult(
            case=_case("case-r#4", "v13"),
            variants=_all_success({"none": 0.0, "correct_era": 0.0, "wrong_era": 0.0}),
            h1_entropy=2.0,
        ),
    ]
    report = _aggregate(
        crs, [], n_total=10, n_held_out=4, run_id="rid", config_hash="cfg"
    )

    assert report.h2.p_at_5_none == pytest.approx(0.5)
    assert report.h2.p_at_5_correct_era == pytest.approx(0.75)
    assert report.h2.p_at_5_wrong_era == pytest.approx(0.25)
    assert report.h2.n_cases == 4

    # H3 lifts derived from h2
    assert report.h3.l_correct == pytest.approx(0.75 - 0.5)
    assert report.h3.l_decay == pytest.approx(0.75 - 0.25)

    # H1: all 4 cases succeeded on "none" + all have a gt bucket
    assert report.h1.n_cases == 4
    assert report.h1.avg_entropy == pytest.approx(1.0)

    # All 4 share the same era + gt_bucket
    assert "v13" in report.h3.per_era
    assert report.h3.per_era["v13"].n_cases == 4
    assert report.h3.per_era["v13"].l_correct == pytest.approx(0.25)

    assert report.run_id == "rid"
    assert report.config_hash == "cfg"
    assert report.n_cases == 10
    assert report.n_held_out == 4
    assert report.failures == ()


def test_aggregate_excludes_failed_variants_from_denominator():
    """A case that errored on `correct_era` must not count against
    correct_era's P@5 denominator. The other variants score normally."""
    crs = [
        _CaseResult(
            case=_case("case-x#1", "v13"),
            variants={
                "none": _VariantResult(failed=False, hit=1.0),
                "correct_era": _VariantResult(failed=True),
                "wrong_era": _VariantResult(failed=False, hit=0.0),
            },
            h1_entropy=0.5,
        ),
        _CaseResult(
            case=_case("case-x#2", "v13"),
            variants=_all_success({"none": 0.0, "correct_era": 1.0, "wrong_era": 0.0}),
            h1_entropy=0.5,
        ),
    ]
    failures = [
        CaseFailure(case_id="case-x#1", variant="correct_era", error="HTTP 500")
    ]
    report = _aggregate(
        crs, failures, n_total=2, n_held_out=2, run_id="r", config_hash="c"
    )

    # none: both succeeded, mean = 0.5
    assert report.h2.p_at_5_none == pytest.approx(0.5)
    # correct_era: only case 2 succeeded, mean = 1.0
    assert report.h2.p_at_5_correct_era == pytest.approx(1.0)
    # wrong_era: both succeeded, mean = 0.0
    assert report.h2.p_at_5_wrong_era == pytest.approx(0.0)
    # n_cases is min across variants (correct_era has 1)
    assert report.h2.n_cases == 1

    # H3 per-era requires all three variants to have ≥1 success — true here
    assert "v13" in report.h3.per_era
    assert report.h3.per_era["v13"].n_cases == 1

    assert len(report.failures) == 1
    assert report.failures[0].error == "HTTP 500"


def test_aggregate_h1_entropy_skips_failed_none_variant():
    """If `none` variant failed, that case contributes no H1 entropy."""
    crs = [
        _CaseResult(
            case=_case("case-h#1", "v13"),
            variants={
                "none": _VariantResult(failed=True),
                "correct_era": _VariantResult(failed=False, hit=1.0),
                "wrong_era": _VariantResult(failed=False, hit=0.0),
            },
            h1_entropy=99.0,  # poison value — must NOT be counted
            h1_failed=True,
        ),
        _CaseResult(
            case=_case("case-h#2", "v13"),
            variants=_all_success({"none": 1.0, "correct_era": 1.0, "wrong_era": 0.0}),
            h1_entropy=1.5,
        ),
    ]
    report = _aggregate(
        crs, [], n_total=2, n_held_out=2, run_id="r", config_hash="c"
    )

    assert report.h1.n_cases == 1
    assert report.h1.avg_entropy == pytest.approx(1.5)


def test_aggregate_h1_skips_cases_without_gt_bucket():
    """A case with no module_path_buckets has no ground-truth bucket
    to credit — exclude it from H1 to avoid mis-attribution."""
    case_no_buckets = EvalCase(
        case_id="case-n#1",
        issue_id="1",
        thread_session_id="s",
        query_text="q",
        era="v13",
        ground_truth_event_ids=("evt-1",),
        module_path_buckets=(),  # empty — no gt bucket
        held_out=True,
    )
    crs = [
        _CaseResult(
            case=case_no_buckets,
            variants=_all_success({"none": 1.0, "correct_era": 1.0, "wrong_era": 0.0}),
            h1_entropy=99.0,
        ),
        _CaseResult(
            case=_case("case-n#2", "v13"),
            variants=_all_success({"none": 0.0, "correct_era": 0.0, "wrong_era": 0.0}),
            h1_entropy=2.0,
        ),
    ]
    report = _aggregate(
        crs, [], n_total=2, n_held_out=2, run_id="r", config_hash="c"
    )
    assert report.h1.n_cases == 1
    assert report.h1.avg_entropy == pytest.approx(2.0)


def test_aggregate_per_era_emits_only_when_all_variants_have_data():
    """Per-era H3 entry is suppressed if any variant has 0 successes
    in that era — the per-variant mean would be 0 by convention but
    the lift would be misleading."""
    crs = [
        _CaseResult(
            case=_case("case-a#1", "v13"),
            variants={
                "none": _VariantResult(failed=False, hit=1.0),
                "correct_era": _VariantResult(failed=True),  # missing for v13
                "wrong_era": _VariantResult(failed=False, hit=0.0),
            },
            h1_entropy=0.0,
        ),
        # Different era, all three present
        _CaseResult(
            case=_case("case-a#2", "v14"),
            variants=_all_success({"none": 0.0, "correct_era": 1.0, "wrong_era": 0.0}),
            h1_entropy=0.0,
        ),
    ]
    report = _aggregate(
        crs, [], n_total=2, n_held_out=2, run_id="r", config_hash="c"
    )
    # v13 has no correct_era successes → suppressed
    assert "v13" not in report.h3.per_era
    assert "v14" in report.h3.per_era
    assert report.h3.per_era["v14"].l_correct == pytest.approx(1.0)


def test_aggregate_empty_inputs_zero_filled():
    """No held-out cases → all means are 0, all dicts empty.
    The report serializes cleanly even in this degenerate state."""
    report = _aggregate(
        [], [], n_total=0, n_held_out=0, run_id="r", config_hash="c"
    )
    assert report.h2.p_at_5_none == 0.0
    assert report.h2.n_cases == 0
    assert report.h1.avg_entropy == 0.0
    assert report.h1.n_cases == 0
    assert report.h1.per_bucket == {}
    assert report.h3.per_era == {}
    assert report.failures == ()


def test_aggregate_h1_per_bucket_groups_by_ground_truth_bucket():
    """Per-bucket entropy means are grouped by the case's first
    module_path_bucket (its ground-truth bucket)."""
    crs = [
        _CaseResult(
            case=_case("case-b#1", "v13", gt_bucket="packages/next/src/server"),
            variants=_all_success({"none": 1.0, "correct_era": 1.0, "wrong_era": 0.0}),
            h1_entropy=1.0,
        ),
        _CaseResult(
            case=_case("case-b#2", "v13", gt_bucket="packages/next/src/server"),
            variants=_all_success({"none": 1.0, "correct_era": 1.0, "wrong_era": 0.0}),
            h1_entropy=2.0,
        ),
        _CaseResult(
            case=_case("case-b#3", "v13", gt_bucket="packages/next/src/client"),
            variants=_all_success({"none": 1.0, "correct_era": 1.0, "wrong_era": 0.0}),
            h1_entropy=3.0,
        ),
    ]
    report = _aggregate(
        crs, [], n_total=3, n_held_out=3, run_id="r", config_hash="c"
    )
    assert report.h1.per_bucket["packages/next/src/server"] == pytest.approx(1.5)
    assert report.h1.per_bucket["packages/next/src/client"] == pytest.approx(3.0)


# ---------- _bucket_for_hit ----------


def test_bucket_for_hit_none_returns_unknown():
    assert _bucket_for_hit({"id": "x"}, None) == _UNKNOWN_BUCKET


def test_bucket_for_hit_lookup():
    table = {"evt-1": "packages/next/src/server"}
    assert _bucket_for_hit({"id": "evt-1"}, table) == "packages/next/src/server"


def test_bucket_for_hit_missing_id_returns_unknown():
    table = {"evt-1": "packages/next/src/server"}
    assert _bucket_for_hit({"id": "evt-99"}, table) == _UNKNOWN_BUCKET
    assert _bucket_for_hit({}, table) == _UNKNOWN_BUCKET


# ---------- EvalCase JSONL round-trip ----------


def test_evalcase_round_trip_preserves_tuples():
    """Serializing then deserializing must return a frozen, hashable
    EvalCase with tuples (not lists) on collection fields."""
    original = _case("case-rt#1", "v13")
    line = case_to_jsonl_line(original)
    rebuilt = case_from_dict(json.loads(line))

    assert rebuilt == original
    assert isinstance(rebuilt.ground_truth_event_ids, tuple)
    assert isinstance(rebuilt.module_path_buckets, tuple)
    # frozen dataclass → hashable
    {rebuilt}


def test_load_cases_jsonl(tmp_path: Path):
    cases = [_case("case-l#1", "v13"), _case("case-l#2", "v14")]
    p = tmp_path / "cases.jsonl"
    p.write_text("\n".join(case_to_jsonl_line(c) for c in cases) + "\n")
    loaded = load_cases_jsonl(p)
    assert loaded == cases


def test_load_cases_jsonl_skips_blank_lines(tmp_path: Path):
    cases = [_case("case-l#1", "v13")]
    p = tmp_path / "cases.jsonl"
    p.write_text("\n" + case_to_jsonl_line(cases[0]) + "\n\n")
    loaded = load_cases_jsonl(p)
    assert loaded == cases


# ---------- spot-check: shannon_entropy values match what we expect ----------


def test_aggregate_h1_uses_provided_entropies_as_is():
    """The aggregator does not recompute entropy — it averages the
    values the orchestrator handed it. Sanity-check with a known mix."""
    # log2(2) = 1.0; log2(4) = 2.0
    crs = [
        _CaseResult(
            case=_case("case-e#1", "v13"),
            variants=_all_success({"none": 1.0, "correct_era": 1.0, "wrong_era": 0.0}),
            h1_entropy=math.log2(2),
        ),
        _CaseResult(
            case=_case("case-e#2", "v13"),
            variants=_all_success({"none": 1.0, "correct_era": 1.0, "wrong_era": 0.0}),
            h1_entropy=math.log2(4),
        ),
    ]
    report = _aggregate(
        crs, [], n_total=2, n_held_out=2, run_id="r", config_hash="c"
    )
    assert report.h1.avg_entropy == pytest.approx(1.5)


# ---------- D4: report writers ----------


def _sample_report(*, with_failures: bool = False) -> RunReport:
    """Hand-built RunReport for renderer tests. Mirrors the fixture from
    test_report.py but kept local so eval-tests don't reach across files."""
    failures: tuple[CaseFailure, ...] = ()
    if with_failures:
        failures = (
            CaseFailure(
                case_id="case-vercel/next.js#999",
                variant="correct_era",
                error="recall returned 502",
            ),
            CaseFailure(
                case_id="case-vercel/next.js#1000",
                variant="wrong_era",
                error="timeout after 30s",
            ),
        )
    return RunReport(
        run_id="2026-05-02T20:55:00Z+abc1234",
        config_hash="cfg-1f2e3d",
        n_cases=42,
        n_held_out=8,
        h1=H1Result(
            avg_entropy=2.31,
            n_cases=8,
            per_bucket={
                "packages/next/src": 1.85,
                "test/integration/prefetch": 3.10,
            },
        ),
        h2=H2Result(
            p_at_5_none=0.375,
            p_at_5_correct_era=0.500,
            p_at_5_wrong_era=0.250,
            n_cases=8,
        ),
        h3=H3Result(
            l_correct=0.125,
            l_decay=0.250,
            per_era={
                "v15": H3PerEra(n_cases=5, l_correct=0.000, l_decay=0.200),
                "v14": H3PerEra(n_cases=3, l_correct=0.000, l_decay=0.000),
            },
        ),
        failures=failures,
    )


def test_render_markdown_contains_top_level_numbers():
    """The markdown renderer surfaces the H1/H2/H3 numbers + counts and
    metadata (run_id, config_hash). Lifts are signed."""
    md = render_markdown(_sample_report())

    # Run metadata
    assert "2026-05-02T20:55:00Z+abc1234" in md
    assert "cfg-1f2e3d" in md
    assert "42" in md  # n_cases
    assert "8" in md   # n_held_out

    # H2 P@5 numbers (3dp)
    assert "P@5" in md
    assert "0.375" in md
    assert "0.500" in md
    assert "0.250" in md
    assert "n_cases" in md

    # H3 lifts (signed, 3dp)
    assert "+0.125" in md
    assert "+0.250" in md

    # H1 entropy + bucket
    assert "2.31 bits" in md or "2.31" in md
    assert "packages/next/src" in md
    assert "1.85" in md


def test_render_markdown_failures_section_with_failures():
    """When failures exist, list each with case_id + variant + error."""
    md = render_markdown(_sample_report(with_failures=True))
    assert "case-vercel/next.js#999" in md
    assert "correct_era" in md
    assert "recall returned 502" in md
    assert "case-vercel/next.js#1000" in md
    assert "timeout after 30s" in md


def test_render_markdown_no_failures_section_still_present():
    """When failures is empty, the section reads '(0 cases failed)' or
    similar — never silently disappears."""
    md = render_markdown(_sample_report(with_failures=False))
    assert "Failures" in md or "failures" in md
    # Some indication of zero — either "0 cases failed" or "no failures"
    assert "0 cases failed" in md or "no failures" in md.lower()


def test_render_markdown_per_era_table_has_each_era():
    """H3 per-era table includes a row per era with n + signed lifts."""
    md = render_markdown(_sample_report())
    assert "v15" in md
    assert "v14" in md
    # Per-era counts present
    assert "5" in md
    assert "3" in md
    # Per-era lifts (signed, 3dp). v15 has l_decay=0.200 → "+0.200"
    assert "+0.200" in md


def test_render_markdown_lift_negative_is_signed():
    """Negative lifts render with a leading '-' sign."""
    rep = _sample_report()
    bad = RunReport(
        run_id=rep.run_id, config_hash=rep.config_hash,
        n_cases=rep.n_cases, n_held_out=rep.n_held_out,
        h1=rep.h1, h2=rep.h2,
        h3=H3Result(l_correct=-0.125, l_decay=-0.250, per_era={}),
        failures=rep.failures,
    )
    md = render_markdown(bad)
    assert "-0.125" in md
    assert "-0.250" in md


def test_write_outputs_writes_three_files(tmp_path: Path):
    """_write_outputs produces report.md, report.json, per-case-traces.jsonl."""
    rep = _sample_report()
    traces = [
        {
            "case_id": "case-1", "variant": "none", "query": "q1",
            "top_k": [{"event_id": "e1", "tier": "ep", "score": 0.9, "bucket": "b"}],
            "hit_p_at_5": 1.0, "ground_truth_event_ids": ["e1"],
        },
    ]
    _write_outputs(tmp_path, rep, traces)
    assert (tmp_path / "report.md").exists()
    assert (tmp_path / "report.json").exists()
    assert (tmp_path / "per-case-traces.jsonl").exists()
    # report.json is a real JSON document
    parsed = json.loads((tmp_path / "report.json").read_text())
    assert parsed["run_id"] == rep.run_id


def test_traces_jsonl_one_per_line(tmp_path: Path):
    """Each line of per-case-traces.jsonl is valid JSON and the count
    matches the input. ensure_ascii=False but unicode survives round-trip."""
    rep = _sample_report()
    traces = [
        {"case_id": "case-1", "variant": "none", "query": "α-test"},
        {"case_id": "case-2", "variant": "correct_era", "query": "β"},
        {"case_id": "case-3", "variant": "wrong_era", "query": "γ"},
    ]
    _write_outputs(tmp_path, rep, traces)
    lines = (tmp_path / "per-case-traces.jsonl").read_text(encoding="utf-8").splitlines()
    assert len(lines) == 3
    parsed = [json.loads(l) for l in lines]
    queries = {p["query"] for p in parsed}
    assert queries == {"α-test", "β", "γ"}


def test_traces_jsonl_sorted_by_case_variant(tmp_path: Path):
    """Traces sorted by (case_id, variant) for grep-friendliness — input
    order shuffled, output stable."""
    rep = _sample_report()
    traces = [
        {"case_id": "case-b", "variant": "wrong_era"},
        {"case_id": "case-a", "variant": "correct_era"},
        {"case_id": "case-b", "variant": "none"},
        {"case_id": "case-a", "variant": "none"},
    ]
    _write_outputs(tmp_path, rep, traces)
    lines = (tmp_path / "per-case-traces.jsonl").read_text(encoding="utf-8").splitlines()
    parsed = [json.loads(l) for l in lines]
    keys = [(p["case_id"], p["variant"]) for p in parsed]
    assert keys == [
        ("case-a", "correct_era"),
        ("case-a", "none"),
        ("case-b", "none"),
        ("case-b", "wrong_era"),
    ]


def test_print_summary_includes_key_metrics(capsys, tmp_path: Path):
    """Terminal summary mentions all three H-numbers, run_id, n counts,
    and the outputs path."""
    rep = _sample_report()
    _print_summary(rep, tmp_path)
    out = capsys.readouterr().out
    # Run metadata
    assert rep.run_id in out
    # H2 P@5 trio
    assert "0.375" in out
    assert "0.500" in out
    assert "0.250" in out
    # H3 signed lifts
    assert "+0.125" in out or "+0.125" in out
    # H1 entropy
    assert "2.31" in out
    # Outputs path mentioned
    assert str(tmp_path) in out
    # Counts
    assert "8" in out  # n_held_out
    assert "42" in out  # n_total
