from __future__ import annotations

import json

import pytest

from seeding_eval.report import (
    H1Result,
    H2Result,
    H3PerEra,
    H3Result,
    RunReport,
    CaseFailure,
    report_to_json,
    report_from_json,
)


def _sample_report() -> RunReport:
    return RunReport(
        run_id="2026-05-02T20:30:00Z+abc1234",
        config_hash="cfg-abc1234",
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
            p_at_5_correct_era=0.375,
            p_at_5_wrong_era=0.250,
            n_cases=8,
        ),
        h3=H3Result(
            l_correct=0.0,
            l_decay=0.125,
            per_era={
                "v15": H3PerEra(n_cases=5, l_correct=0.0, l_decay=0.20),
                "v14": H3PerEra(n_cases=3, l_correct=0.0, l_decay=0.0),
            },
        ),
        failures=(
            CaseFailure(case_id="case-vercel/next.js#999", variant="correct_era",
                        error="recall returned 502"),
        ),
    )


def test_report_constructible():
    rep = _sample_report()
    assert rep.h2.p_at_5_correct_era == 0.375
    assert rep.h3.per_era["v15"].l_decay == 0.20
    assert len(rep.failures) == 1


def test_run_report_frozen():
    rep = _sample_report()
    with pytest.raises(Exception):
        rep.run_id = "x"  # type: ignore[misc]


def test_report_to_json_returns_str():
    s = report_to_json(_sample_report())
    assert isinstance(s, str)
    parsed = json.loads(s)
    assert parsed["run_id"] == "2026-05-02T20:30:00Z+abc1234"
    assert parsed["h2"]["p_at_5_correct_era"] == 0.375
    assert parsed["h3"]["per_era"]["v15"]["l_decay"] == 0.20
    assert parsed["failures"][0]["error"] == "recall returned 502"


def test_report_round_trip():
    """Serialize -> parse -> reconstruct -> equal."""
    original = _sample_report()
    s = report_to_json(original)
    rebuilt = report_from_json(s)
    assert rebuilt == original


def test_report_from_json_validates_shape():
    """Missing required field -> raise during reconstruction."""
    with pytest.raises((TypeError, KeyError, ValueError)):
        report_from_json('{"run_id": "x"}')


def test_failures_is_tuple_not_list():
    """The frozen dataclass requires hashable fields; failures must be a tuple."""
    rep = _sample_report()
    assert isinstance(rep.failures, tuple)


def test_h1_per_bucket_dict_round_trips():
    rep = _sample_report()
    s = report_to_json(rep)
    rebuilt = report_from_json(s)
    assert rebuilt.h1.per_bucket == rep.h1.per_bucket


def test_empty_failures():
    """A clean run: no failures."""
    rep = _sample_report()
    rep_clean = RunReport(
        run_id=rep.run_id, config_hash=rep.config_hash,
        n_cases=rep.n_cases, n_held_out=rep.n_held_out,
        h1=rep.h1, h2=rep.h2, h3=rep.h3,
        failures=(),
    )
    s = report_to_json(rep_clean)
    rebuilt = report_from_json(s)
    assert rebuilt.failures == ()


def test_report_settle_fields_default_off():
    """A report built without settle kwargs records the off defaults —
    pre-P4 constructor calls stay valid."""
    rep = _sample_report()
    assert rep.settle == "off"
    assert rep.activation_weight is None


def test_report_settle_fields_round_trip():
    """settle + activation_weight survive the JSON round-trip so runs
    are self-describing about their P4 run-matrix cell."""
    rep = _sample_report()
    rep_channel = RunReport(
        run_id=rep.run_id, config_hash=rep.config_hash,
        n_cases=rep.n_cases, n_held_out=rep.n_held_out,
        h1=rep.h1, h2=rep.h2, h3=rep.h3,
        failures=rep.failures,
        settle="channel",
        activation_weight=0.2,
    )
    s = report_to_json(rep_channel)
    parsed = json.loads(s)
    assert parsed["settle"] == "channel"
    assert parsed["activation_weight"] == 0.2
    rebuilt = report_from_json(s)
    assert rebuilt == rep_channel


def test_report_from_json_defaults_missing_settle_fields():
    """Legacy report.json files written before the P4 settle fields
    existed reconstruct with the off defaults instead of failing."""
    data = json.loads(report_to_json(_sample_report()))
    del data["settle"]
    del data["activation_weight"]
    rebuilt = report_from_json(json.dumps(data))
    assert rebuilt.settle == "off"
    assert rebuilt.activation_weight is None
