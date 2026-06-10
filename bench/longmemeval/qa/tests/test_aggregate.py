"""Aggregation math: overall, per-type buckets, abstention split, edges."""

from __future__ import annotations

from lme_qa.aggregate import Judgment, aggregate, render_markdown


def _j(qid, qtype, abst, label):
    return Judgment(question_id=qid, question_type=qtype, is_abstention=abst, label=label)


def test_overall_and_per_type_accuracy():
    judgments = [
        _j("a", "single-session-user", False, True),
        _j("b", "single-session-user", False, False),
        _j("c", "multi-session", False, True),
        _j("d", "multi-session", False, True),
    ]
    r = aggregate(judgments)
    assert r.overall.n == 4
    assert r.overall.correct == 3
    assert r.overall.accuracy == 0.75
    assert r.by_type["single-session-user"].n == 2
    assert r.by_type["single-session-user"].accuracy == 0.5
    assert r.by_type["multi-session"].accuracy == 1.0


def test_abs_questions_bucket_into_base_type_upstream_behavior():
    # An _abs question is scored with the abstention prompt but its label lands
    # in its base question_type bucket (upstream qtype2acc[qid2qtype[qid]]).
    judgments = [
        _j("x", "single-session-user", False, True),
        _j("y_abs", "single-session-user", True, False),
    ]
    r = aggregate(judgments)
    # Both in the single-session-user bucket -> 1/2.
    assert r.by_type["single-session-user"].n == 2
    assert r.by_type["single-session-user"].correct == 1
    # Supplementary split keeps them visible separately.
    assert r.answerable.n == 1 and r.answerable.correct == 1
    assert r.abstention.n == 1 and r.abstention.correct == 0


def test_empty_judgments_zero_not_crash():
    r = aggregate([])
    assert r.overall.n == 0
    assert r.overall.accuracy == 0.0
    assert r.by_type == {}
    assert r.answerable.n == 0 and r.abstention.n == 0


def test_empty_abstention_bucket_when_no_abs_questions():
    r = aggregate([_j("a", "multi-session", False, True)])
    assert r.abstention.n == 0
    assert r.abstention.accuracy == 0.0  # n=0 reads as no-data
    assert r.answerable.n == 1


def test_token_usage_carried_through():
    r = aggregate([_j("a", "multi-session", False, True)], 1234, 56)
    assert r.total_input_tokens == 1234
    assert r.total_output_tokens == 56


def test_render_markdown_has_table_and_overall():
    r = aggregate(
        [
            _j("a", "single-session-user", False, True),
            _j("b", "multi-session", False, False),
            _j("c_abs", "multi-session", True, True),
        ],
        total_input_tokens=100,
        total_output_tokens=10,
    )
    md = render_markdown(r, title="QA accuracy (test)")
    assert "## QA accuracy (test)" in md
    assert "**Overall accuracy: 66.7%**" in md
    assert "| single-session-user | 1 | 100.0% |" in md
    # multi-session bucket holds both the answerable miss and the _abs hit -> 1/2
    assert "| multi-session | 2 | 50.0% |" in md
    assert "| abstention (`_abs`) | 1 | 100.0% |" in md
    assert "| answerable | 2 |" in md
    assert "100 input, 10 output" in md
    # Markdown table header present.
    assert "| Question type | N | Accuracy |" in md


# --- Issue 4: failure counts threaded into the report ----------------------


def test_failure_counts_carried_through():
    r = aggregate(
        [_j("a", "multi-session", False, True)],
        reader_failures=3,
        judge_failures=1,
    )
    assert r.reader_failures == 3
    assert r.judge_failures == 1


def test_render_markdown_renders_failure_footnote():
    r = aggregate(
        [_j("a", "multi-session", False, False)],
        reader_failures=3,
        judge_failures=1,
    )
    md = render_markdown(r)
    assert "3 reader failure" in md
    assert "judge failure" in md
    assert "counted incorrect" in md


def test_render_markdown_no_failure_footnote_when_clean():
    r = aggregate([_j("a", "multi-session", False, True)])
    md = render_markdown(r)
    assert "reader failure" not in md
    assert "judge failure" not in md


# --- Issue 5: provenance footer ---------------------------------------------


def test_render_markdown_provenance_footer():
    r = aggregate([_j("a", "multi-session", False, True)])
    md = render_markdown(
        r,
        title="QA accuracy",
        model="claude-opus-4-8",
        k=10,
        date="2026-06-10",
    )
    assert "claude-opus-4-8" in md
    assert "K=10" in md or "K = 10" in md
    assert "2026-06-10" in md
    # n is rendered too (the denominator).
    assert "n=1" in md or "n = 1" in md


def test_render_markdown_no_provenance_when_unspecified():
    # Backward-compatible: omitting provenance args renders no provenance line.
    r = aggregate([_j("a", "multi-session", False, True)])
    md = render_markdown(r)
    assert "claude-opus-4-8" not in md
