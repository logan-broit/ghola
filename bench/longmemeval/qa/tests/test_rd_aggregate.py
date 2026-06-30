"""Rate-distortion aggregation (Task 8): join + average math on KNOWN numbers.

Builds leaf trees by hand (no live model, no sweep run) and asserts the joined
+ aggregated numbers exactly. The join is answers.context_tokens (rate, the
emitted-context token count) -> judgments.label (distortion) per question_id per
leaf; samples are averaged per (setting, question) then questions are averaged
per setting; distortion is 1 - accuracy by construction. A row missing
context_tokens (a pre-fix run) falls back to usage.input_tokens with a warning.
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

    The two files share question_ids; answers carry context_tokens (the rate,
    the emitted-context token count) AND usage.input_tokens (a fixed harness
    proxy, deliberately DIFFERENT from the rate so a test relying on the wrong
    field would fail), judgments carry the label. Both succeeded.
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
                        "context_tokens": rate,
                        "rate_tokenizer": "cl100k",
                        "hypothesis": "x",
                        "status": "succeeded",
                        "error": "",
                        # Fixed harness-overhead proxy, NOT the rate. Distinct
                        # from `rate` so any code reading this field instead of
                        # context_tokens would produce the wrong mean_rate.
                        "usage": {"input_tokens": 3279, "output_tokens": 1},
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
        json.dumps({"question_id": "q1", "status": "errored", "context_tokens": 1})
        + "\n"
        + json.dumps({"question_id": "q1", "status": "succeeded", "context_tokens": 999})
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


def test_rate_uses_context_tokens_not_usage(tmp_path):
    # The rate axis is context_tokens (1100 mean), NOT usage.input_tokens (3279,
    # the harness-overhead proxy _write_leaf stamps). This is the fix's whole
    # point: a constant usage.input_tokens cannot be the rate.
    _write_leaf(tmp_path, "full", None, 0, [("q1", 1000, True), ("q2", 1200, False)])
    rows = rd_aggregate.aggregate(tmp_path)
    full = next(r for r in rows if r["compressor"] == "full")
    assert full["mean_rate"] == 1100  # from context_tokens, not 3279


def test_rate_tokenizer_carried_to_curve_row(tmp_path):
    _write_leaf(tmp_path, "full", None, 0, [("q1", 1000, True), ("q2", 1200, True)])
    rows = rd_aggregate.aggregate(tmp_path)
    full = next(r for r in rows if r["compressor"] == "full")
    # The unit lives on the row (it was stamped "cl100k" by _write_leaf).
    assert full["rate_tokenizer"] == "cl100k"


def test_missing_context_tokens_falls_back_to_usage_and_warns(tmp_path, capsys):
    # A pre-fix run has no context_tokens; the aggregator falls back to
    # usage.input_tokens for the rate and warns on stderr naming the setting so
    # a stale-schema leaf is obvious in the run log.
    leaf = tmp_path / "full__bNone" / "s0"
    leaf.mkdir(parents=True)
    (leaf / "answers.jsonl").write_text(
        json.dumps(
            {
                "question_id": "q1",
                "compressor": "full",
                "budget": None,
                "status": "succeeded",
                "usage": {"input_tokens": 2500, "output_tokens": 1},
            }
        )
        + "\n"
    )
    (leaf / "judgments.jsonl").write_text(
        json.dumps({"question_id": "q1", "label": True, "status": "succeeded"}) + "\n"
    )
    rows = rd_aggregate.aggregate(tmp_path)
    full = next(r for r in rows if r["compressor"] == "full")
    assert full["mean_rate"] == 2500  # fell back to usage.input_tokens
    err = capsys.readouterr().err
    assert "context_tokens" in err  # warning fired
    assert "full" in err            # names the offending setting


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


# --- expansion_reserve parsing -----------------------------------------------


def test_parse_setting_dir_with_expansion_reserve():
    """The ``__r<reserve>`` suffix is parsed back as expansion_reserve."""
    s = rd_aggregate._parse_setting_dir(
        "extractive_relevance_expanded__b1000__r0.3"
    )
    assert s is not None
    assert s.compressor == "extractive_relevance_expanded"
    assert s.budget == 1000
    assert s.expansion_reserve == 0.3


def test_parse_setting_dir_without_expansion_reserve():
    """A leaf without the ``__r`` suffix has expansion_reserve=None."""
    s = rd_aggregate._parse_setting_dir("extractive_relevance__b500")
    assert s is not None
    assert s.compressor == "extractive_relevance"
    assert s.budget == 500
    assert s.expansion_reserve is None


def test_parse_setting_dir_reserve_zero():
    """r0.0 parses to 0.0, not None."""
    s = rd_aggregate._parse_setting_dir(
        "extractive_relevance_expanded__b1000__r0.0"
    )
    assert s is not None
    assert s.expansion_reserve == 0.0


def test_aggregate_carries_expansion_reserve_to_row(tmp_path):
    """The expansion_reserve travels from the leaf directory name into the
    curve row so the aggregator can distinguish reserve variants."""
    # Two leaves of the same compressor+budget but different reserves.
    _write_leaf(
        tmp_path, "extractive_relevance_expanded", 500, 0,
        [("q1", 400, True), ("q2", 450, True)],
    )
    # _write_leaf doesn't add the __r suffix, so write one by hand.
    leaf2 = tmp_path / "extractive_relevance_expanded__b500__r0.5" / "s0"
    leaf2.mkdir(parents=True, exist_ok=True)
    (leaf2 / "answers.jsonl").write_text(
        json.dumps({
            "question_id": "q1", "context_tokens": 420, "rate_tokenizer": "cl100k",
            "status": "succeeded", "usage": {"input_tokens": 3279, "output_tokens": 1},
        }) + "\n" +
        json.dumps({
            "question_id": "q2", "context_tokens": 430, "rate_tokenizer": "cl100k",
            "status": "succeeded", "usage": {"input_tokens": 3279, "output_tokens": 1},
        }) + "\n"
    )
    (leaf2 / "judgments.jsonl").write_text(
        json.dumps({"question_id": "q1", "label": True, "status": "succeeded"}) + "\n" +
        json.dumps({"question_id": "q2", "label": False, "status": "succeeded"}) + "\n"
    )
    rows = rd_aggregate.aggregate(tmp_path)
    assert len(rows) == 2
    # The default-reserve leaf (no __r suffix) has None.
    default_row = next(r for r in rows if r.get("expansion_reserve") is None)
    assert default_row["compressor"] == "extractive_relevance_expanded"
    # The __r0.5 leaf carries the reserve.
    r05_row = next(r for r in rows if r.get("expansion_reserve") == 0.5)
    assert r05_row["budget"] == 500


# --- Stage B: setting.json sidecar is the aggregation source of truth --------


def _write_sidecar_leaf(
    outdir: Path,
    dir_name: str,
    setting: dict,
    sample: int,
    rows: list[tuple[str, int, bool]],
) -> None:
    """Write a setting dir with a setting.json sidecar + one sample leaf
    (answers + judgments). ``dir_name`` is the leaf SETTING dir component;
    ``setting`` is the full Setting dict the sidecar holds."""
    setting_dir = outdir / dir_name
    setting_dir.mkdir(parents=True, exist_ok=True)
    (setting_dir / "setting.json").write_text(json.dumps(setting))
    leaf = setting_dir / f"s{sample}"
    leaf.mkdir(parents=True, exist_ok=True)
    with (leaf / "answers.jsonl").open("w") as fh:
        for qid, rate, _label in rows:
            fh.write(json.dumps({
                "question_id": qid, "context_tokens": rate,
                "rate_tokenizer": "cl100k", "status": "succeeded",
                "usage": {"input_tokens": 3279, "output_tokens": 1},
            }) + "\n")
    with (leaf / "judgments.jsonl").open("w") as fh:
        for qid, _rate, label in rows:
            fh.write(json.dumps({
                "question_id": qid, "label": label, "status": "succeeded",
            }) + "\n")


def test_aggregate_reads_sidecar_and_surfaces_query_mode_output_form(tmp_path):
    """When a setting.json sidecar exists, aggregate reads ALL fields from it
    (including query_mode/output_form) and surfaces them on the curve row."""
    _write_sidecar_leaf(
        tmp_path,
        "llm_distill__b1000__qagnostic__ofstructured",
        {
            "compressor": "llm_distill", "budget": 1000,
            "expansion_reserve": None, "query_mode": "agnostic",
            "output_form": "structured", "prune_level": None,
            "oracle_model": None, "edge_metric": None,
        },
        0,
        [("q1", 800, True), ("q2", 820, False)],
    )
    rows = rd_aggregate.aggregate(tmp_path)
    assert len(rows) == 1
    r = rows[0]
    assert r["compressor"] == "llm_distill"
    assert r["budget"] == 1000
    assert r["query_mode"] == "agnostic"
    assert r["output_form"] == "structured"
    assert r["mean_rate"] == 810
    assert r["accuracy"] == 0.5


def test_aggregate_groups_distinct_settings_from_sidecars(tmp_path):
    """Two settings differing only in output_form (distinct sidecars + dirs)
    aggregate as TWO rows — they never collapse together."""
    _write_sidecar_leaf(
        tmp_path,
        "llm_distill__b1000__qagnostic__ofprose",
        {
            "compressor": "llm_distill", "budget": 1000,
            "expansion_reserve": None, "query_mode": "agnostic",
            "output_form": "prose", "prune_level": None,
            "oracle_model": None, "edge_metric": None,
        },
        0,
        [("q1", 500, True), ("q2", 520, True)],
    )
    _write_sidecar_leaf(
        tmp_path,
        "llm_distill__b1000__qagnostic__ofstructured",
        {
            "compressor": "llm_distill", "budget": 1000,
            "expansion_reserve": None, "query_mode": "agnostic",
            "output_form": "structured", "prune_level": None,
            "oracle_model": None, "edge_metric": None,
        },
        0,
        [("q1", 700, True), ("q2", 720, False)],
    )
    rows = rd_aggregate.aggregate(tmp_path)
    assert len(rows) == 2
    forms = {r["output_form"] for r in rows}
    assert forms == {"prose", "structured"}


def test_aggregate_sidecar_oracle_model_survives_sanitized_dir(tmp_path):
    """The sidecar carries the UNsanitized oracle_model even though the dir name
    sanitized the ``/`` — proving the sidecar (not the dir name) is the truth."""
    _write_sidecar_leaf(
        tmp_path,
        "perplexity_prune__b500__qagnostic__omQwen_Qwen2.5-1.5B-Instruct",
        {
            "compressor": "perplexity_prune", "budget": 500,
            "expansion_reserve": None, "query_mode": "agnostic",
            "output_form": None, "prune_level": None,
            "oracle_model": "Qwen/Qwen2.5-1.5B-Instruct", "edge_metric": None,
        },
        0,
        [("q1", 400, True)],
    )
    rows = rd_aggregate.aggregate(tmp_path)
    assert len(rows) == 1
    assert rows[0]["oracle_model"] == "Qwen/Qwen2.5-1.5B-Instruct"


def test_aggregate_legacy_leaf_without_sidecar_still_parses(tmp_path):
    """Back-compat: a legacy leaf (no setting.json) still aggregates via the
    dir-name parse, with the new fields left None."""
    # _write_leaf does NOT write a sidecar — the legacy path.
    _write_leaf(tmp_path, "extractive_relevance", 500, 0,
                [("q1", 480, True), ("q2", 500, True)])
    rows = rd_aggregate.aggregate(tmp_path)
    assert len(rows) == 1
    r = rows[0]
    assert r["compressor"] == "extractive_relevance"
    assert r["budget"] == 500
    assert r["query_mode"] is None
    assert r["output_form"] is None


def test_sidecarless_stage_b_leaf_warns_and_is_dropped(tmp_path, capsys):
    """A Stage-B leaf name (carries a __q tag) with NO setting.json sidecar that
    also fails the legacy dir-name parse must WARN (naming the dir) and be
    dropped — not vanish silently. A legacy leaf that parses fine must NOT warn.
    """
    # Stage-B leaf: __qaware tag, no sidecar. The dir name does not match the
    # legacy <compressor>__b<budget> pattern (no __b), so _parse_setting_dir
    # returns None — exactly the silent-drop path the warning guards.
    leaf = tmp_path / "statistical_prune__b1000__qaware" / "s0"
    leaf.mkdir(parents=True)
    (leaf / "answers.jsonl").write_text(
        json.dumps({
            "question_id": "q1", "context_tokens": 800, "rate_tokenizer": "cl100k",
            "status": "succeeded", "usage": {"input_tokens": 3279, "output_tokens": 1},
        }) + "\n"
    )
    (leaf / "judgments.jsonl").write_text(
        json.dumps({"question_id": "q1", "label": True, "status": "succeeded"}) + "\n"
    )
    # A legacy leaf alongside it that parses cleanly (no Stage-B tag) — must not
    # warn and must still be counted.
    _write_leaf(tmp_path, "extractive_relevance", 1000,
                0, [("q1", 900, True), ("q2", 920, True)])

    rows = rd_aggregate.aggregate(tmp_path)
    err = capsys.readouterr().err

    # The Stage-B leaf warned, naming the directory, and is not counted.
    assert "statistical_prune__b1000__qaware" in err
    assert all(r["compressor"] != "statistical_prune" for r in rows)
    # The legacy leaf parsed fine, did NOT warn, and IS counted.
    assert "extractive_relevance__b1000" not in err
    assert any(
        r["compressor"] == "extractive_relevance" and r["budget"] == 1000
        for r in rows
    )


def test_render_markdown_has_query_mode_and_output_form_columns(tmp_path):
    _write_sidecar_leaf(
        tmp_path,
        "llm_distill__b1000__qaware__ofprose",
        {
            "compressor": "llm_distill", "budget": 1000,
            "expansion_reserve": None, "query_mode": "aware",
            "output_form": "prose", "prune_level": None,
            "oracle_model": None, "edge_metric": None,
        },
        0,
        [("q1", 500, True), ("q2", 520, True)],
    )
    rows = rd_aggregate.aggregate(tmp_path)
    md = rd_aggregate.render_markdown(rows)
    assert "query_mode" in md
    assert "output_form" in md
    assert "aware" in md
    assert "prose" in md


def test_render_markdown_shows_dash_for_none_query_mode(tmp_path):
    # A legacy leaf has query_mode/output_form None — the table shows "—".
    _write_leaf(tmp_path, "full", None, 0, [("q1", 1000, True)])
    rows = rd_aggregate.aggregate(tmp_path)
    md = rd_aggregate.render_markdown(rows)
    # The full row's query/output cells render the em dash placeholder.
    assert "—" in md
