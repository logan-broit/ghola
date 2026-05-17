from __future__ import annotations

import pytest

from seeding_eval.cases import EvalCase
from seeding_eval.contexts import render_query


def _case(case_id: str = "case-vercel/next.js#100", era: str = "v15") -> EvalCase:
    return EvalCase(
        case_id=case_id,
        issue_id="100",
        thread_session_id="00000000-0000-0000-0000-000000000001",
        query_text="App Router prefetch race",
        era=era,
        ground_truth_event_ids=("e1",),
        module_path_buckets=("packages/next/src",),
        held_out=True,
    )


# ---------- variant: none ----------

def test_render_none_passthrough():
    case = _case()
    query, tags_any = render_query(case, "none")
    assert query == case.query_text
    assert tags_any is None, "none variant must not emit a tag filter"


# ---------- variant: correct_era ----------

def test_render_correct_era_no_prefix_with_filter():
    """H3.c: query_text is the bare issue body — era differentiation
    moves entirely into tags_any."""
    case = _case(era="v15")
    query, tags_any = render_query(case, "correct_era")
    assert query == case.query_text, \
        "correct_era must NOT prepend a string prefix to the query text"
    assert "Context: working on Next.js" not in query, \
        "the H3.a string prefix must be gone from the query"
    assert tags_any == ["era:v15"]


def test_render_correct_era_unknown_era_raises():
    case = _case(era="v999")
    with pytest.raises(ValueError):
        render_query(case, "correct_era")


# ---------- variant: wrong_era ----------

def test_render_wrong_era_no_prefix_with_different_filter():
    case = _case(case_id="case-X", era="v15")
    query, tags_any = render_query(case, "wrong_era")
    assert query == case.query_text, \
        "wrong_era must NOT prepend a string prefix to the query text"
    assert tags_any is not None and len(tags_any) == 1
    assert tags_any[0] != "era:v15"
    assert tags_any[0].startswith("era:v")


def test_render_wrong_era_deterministic_per_case_id():
    case = _case(case_id="case-X", era="v15")
    q1, t1 = render_query(case, "wrong_era")
    q2, t2 = render_query(case, "wrong_era")
    assert q1 == q2
    assert t1 == t2


def test_render_wrong_era_varies_across_case_ids():
    """Different case_ids generally pick different wrong eras."""
    cases = [_case(case_id=f"case-{i}", era="v15") for i in range(50)]
    wrong_tags = {render_query(c, "wrong_era")[1][0] for c in cases}
    assert len(wrong_tags) >= 2, \
        "wrong_era must produce at least 2 distinct era tags across 50 cases"


# ---------- error paths ----------

def test_render_unknown_variant_raises():
    case = _case()
    with pytest.raises(ValueError):
        render_query(case, "no_such_variant")


# ---------- pre-v12 ----------

def test_render_pre_v12_correct_era_emits_filter():
    """`pre-v12` is a real era_for() output. correct_era should emit
    the era:pre-v12 tag (no validation against the wrong-era pool —
    the filter exists solely to scope retrieval)."""
    case = _case(era="pre-v12")
    query, tags_any = render_query(case, "correct_era")
    assert query == case.query_text
    assert tags_any == ["era:pre-v12"]


def test_render_pre_v12_wrong_era_picks_concrete():
    """wrong_era for pre-v12 must pick from the concrete-era pool —
    the filter has to point at *something* the corpus has tagged."""
    case = _case(era="pre-v12")
    query, tags_any = render_query(case, "wrong_era")
    assert query == case.query_text
    assert tags_any is not None and len(tags_any) == 1
    assert tags_any[0].startswith("era:v")  # concrete era, not pre-v12
    assert tags_any[0] != "era:pre-v12"
