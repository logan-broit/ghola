"""Rate-distortion aggregation (Task 8): join + average math on KNOWN numbers.

Builds leaf trees by hand (no live model, no sweep run) and asserts the joined
+ aggregated numbers exactly. The join is answers.usage.input_tokens (rate) ->
judgments.label (distortion) per question_id per leaf; samples are averaged per
(setting, question) then questions are averaged per setting; distortion is
1 - accuracy by construction.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from lme_qa import rd_aggregate


def _write_leaf(
    outdir: Path,
    compressor: str,
    budget,
    sample: int,
    rows: list[tuple[str, int, bool]],
) -> None:
    """Write a leaf's answers.jsonl + judgments.jsonl from (qid, rate, label).

    The two files share question_ids; answers carry usage.input_tokens (the
    rate), judgments carry the label (correct/incorrect). Both succeeded.
    """
    btag = "None" if budget is None else str(budget)
    leaf = outdir / f"{compressor}__b{btag}" / f"s{sample}"
    leaf.mkdir(parents=True, exist_ok=True)
    with (leaf / "answers.jsonl").open("w") as fh:
        for qid, rate, _label in rows:
            fh.write(
                json.dumps(
                    {
                        "question_id": qid,
                        "question_type": "single-session-user",
                        "k": 10,
                        "compressor": compressor,
                        "budget": budget,
                        "hypothesis": "x",
                        "status": "succeeded",
                        "error": "",
                        "usage": {"input_tokens": rate, "output_tokens": 1},
                    }
                )
                + "\n"
            )
    with (leaf / "judgments.jsonl").open("w") as fh:
        for qid, _rate, label in rows:
            fh.write(
                json.dumps(
                    {
                        "question_id": qid,
                        "question_type": "single-session-user",
                        "is_abstention": False,
                        "judge_text": "yes" if label else "no",
                        "label": label,
                        "status": "succeeded",
                        "error": "",
                        "usage": {"input_tokens": 1, "output_tokens": 1},
                    }
                )
                + "\n"
            )


def test_join_and_aggregate_known_numbers(tmp_path):
    # One setting, one sample, two questions. Plan's worked example.
    _write_leaf(
        tmp_path,
        "full",
        None,
        0,
        [("q1", 1000, True), ("q2", 1200, False)],
    )
    rows = rd_aggregate.aggregate(tmp_path)
    full = next(r for r in rows if r["compressor"] == "full")
    assert full["budget"] is None
    assert full["n"] == 2
    assert full["mean_rate"] == 1100
    assert full["accuracy"] == 0.5
    assert full["distortion"] == 0.5
    # CIs present and non-negative.
    assert full["rate_ci"] >= 0
    assert full["acc_ci"] >= 0


def test_samples_averaged_per_question_then_questions(tmp_path):
    # Two samples of the SAME setting. q1: rates 1000 & 1400 (avg 1200), labels
    # True & True (acc 1.0). q2: rates 800 & 1000 (avg 900), labels False &
    # True (acc 0.5). Per setting: mean_rate = mean(1200, 900) = 1050;
    # accuracy = mean(1.0, 0.5) = 0.75.
    _write_leaf(tmp_path, "full", None, 0, [("q1", 1000, True), ("q2", 800, False)])
    _write_leaf(tmp_path, "full", None, 1, [("q1", 1400, True), ("q2", 1000, True)])
    rows = rd_aggregate.aggregate(tmp_path)
    full = next(r for r in rows if r["compressor"] == "full")
    assert full["n"] == 2  # two distinct questions
    assert full["mean_rate"] == 1050
    assert full["accuracy"] == 0.75
    assert full["distortion"] == 0.25


def test_dedup_last_wins_within_a_leaf(tmp_path):
    # A leaf's answers.jsonl is append-only: a failed-then-succeeded qid appears
    # twice. The aggregator must take the LAST row per qid (the succeeded one).
    leaf = tmp_path / "full__bNone" / "s0"
    leaf.mkdir(parents=True)
    (leaf / "answers.jsonl").write_text(
        json.dumps({"question_id": "q1", "status": "errored", "usage": {"input_tokens": 1}})
        + "\n"
        + json.dumps({"question_id": "q1", "status": "succeeded", "usage": {"input_tokens": 999}})
        + "\n"
    )
    (leaf / "judgments.jsonl").write_text(
        json.dumps({"question_id": "q1", "label": False, "status": "errored"})
        + "\n"
        + json.dumps({"question_id": "q1", "label": True, "status": "succeeded"})
        + "\n"
    )
    rows = rd_aggregate.aggregate(tmp_path)
    full = next(r for r in rows if r["compressor"] == "full")
    assert full["n"] == 1
    assert full["mean_rate"] == 999  # the succeeded row's rate, not the errored 1
    assert full["accuracy"] == 1.0  # the succeeded label


def test_two_settings_distinct_rows(tmp_path):
    _write_leaf(tmp_path, "full", None, 0, [("q1", 2000, True), ("q2", 2000, True)])
    _write_leaf(tmp_path, "truncate_tokens", 500, 0, [("q1", 500, True), ("q2", 500, False)])
    rows = rd_aggregate.aggregate(tmp_path)
    settings = {(r["compressor"], r["budget"]) for r in rows}
    assert settings == {("full", None), ("truncate_tokens", 500)}
    trunc = next(r for r in rows if r["compressor"] == "truncate_tokens")
    assert trunc["budget"] == 500
    assert trunc["mean_rate"] == 500
    assert trunc["accuracy"] == 0.5


# --- artifacts ---------------------------------------------------------------


def test_write_artifacts_emits_jsonl_and_md(tmp_path):
    _write_leaf(tmp_path, "full", None, 0, [("q1", 2000, True), ("q2", 1800, True)])
    _write_leaf(tmp_path, "truncate_tokens", 500, 0, [("q1", 500, True), ("q2", 520, False)])
    _write_leaf(tmp_path, "extractive_relevance", 500, 0, [("q1", 510, True), ("q2", 540, True)])

    rc = rd_aggregate.rd_main(["--outdir", str(tmp_path)])
    assert rc == 0

    jsonl = tmp_path / "rd-curve.jsonl"
    md = tmp_path / "rd-curve.md"
    assert jsonl.exists() and md.exists()

    settings = [json.loads(l) for l in jsonl.read_text().splitlines() if l.strip()]
    # One row per setting, sorted by mean_rate ascending.
    rates = [s["mean_rate"] for s in settings]
    assert rates == sorted(rates)

    text = md.read_text()
    assert "| compressor | budget |" in text or "compressor" in text
    # truncate-vs-extractive gap at the shared budget (500) is called out.
    assert "truncate" in text and "extractive" in text


def test_aggregate_empty_outdir_returns_empty(tmp_path):
    assert rd_aggregate.aggregate(tmp_path) == []


def test_rd_main_no_settings_errors(tmp_path):
    with pytest.raises(SystemExit):
        rd_aggregate.rd_main(["--outdir", str(tmp_path)])
