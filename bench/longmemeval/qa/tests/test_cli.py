"""CLI end-to-end: lme-qa-run then lme-qa-judge over 2 fixture questions,
driving a real anthropic client against the fake Batches server via the
ANTHROPIC_BASE_URL env var (no SDK patching).
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from lme_qa import cli
from tests.fake_batches_server import FakeBatchesServer


@pytest.fixture
def server_env(monkeypatch):
    with FakeBatchesServer() as s:
        # Real client construction reads these; base_url routes to the fake.
        monkeypatch.setenv("ANTHROPIC_API_KEY", "test-key")
        monkeypatch.setenv("ANTHROPIC_BASE_URL", s.base_url)
        yield s


@pytest.fixture
def dataset_file(tmp_path, answerable_entry, abstention_entry) -> Path:
    p = tmp_path / "dataset.json"
    p.write_text(json.dumps([answerable_entry, abstention_entry]))
    return p


@pytest.fixture
def run_file(tmp_path, answerable_result_line, abstention_result_line) -> Path:
    p = tmp_path / "run.jsonl"
    p.write_text(
        json.dumps(answerable_result_line) + "\n" + json.dumps(abstention_result_line) + "\n"
    )
    return p


def test_run_then_judge_end_to_end(server_env, dataset_file, run_file, tmp_path):
    # Reader stage. Fake server echoes "answer for <custom_id>" by default,
    # which the judge prompt will then receive as the hypothesis.
    answers = tmp_path / "answers.jsonl"
    rc = cli.run_main(
        [
            "--dataset", str(dataset_file),
            "--run", str(run_file),
            "--out", str(answers),
            "--k", "10",
            "--poll-interval", "0",
        ]
    )
    assert rc == 0
    rows = [json.loads(l) for l in answers.read_text().splitlines() if l.strip()]
    assert {r["question_id"] for r in rows} == {"e47becba", "0862e8bf_abs"}
    assert all(r["status"] == "succeeded" for r in rows)
    assert all(r["hypothesis"].startswith("answer for ") for r in rows)
    # answers carry question_type + usage for the report footer.
    assert all("question_type" in r and "usage" in r for r in rows)

    # Reader request shape check: model + adaptive thinking, no sampling params.
    sent = server_env.created_payloads[0]["requests"]
    for r in sent:
        p = r["params"]
        assert p["model"] == "claude-opus-4-8"
        assert p["thinking"] == {"type": "adaptive"}
        assert "temperature" not in p

    # Judge stage. Default fake row returns "answer for <id>" as judge text;
    # parse_judge_label keys on the substring 'yes', so default text -> False.
    # Override the fake to return explicit yes/no so we exercise label parsing.
    server_env.state.default_results = [
        server_env.succeeded_row("e47becba", "yes"),
        server_env.succeeded_row("0862e8bf_abs", "no"),
    ]
    judgments = tmp_path / "judgments.jsonl"
    report = tmp_path / "report.md"
    rc = cli.judge_main(
        [
            "--dataset", str(dataset_file),
            "--answers", str(answers),
            "--out", str(judgments),
            "--report", str(report),
            "--poll-interval", "0",
        ]
    )
    assert rc == 0
    jrows = {json.loads(l)["question_id"]: json.loads(l) for l in judgments.read_text().splitlines() if l.strip()}
    assert jrows["e47becba"]["label"] is True
    assert jrows["0862e8bf_abs"]["label"] is False
    assert jrows["0862e8bf_abs"]["is_abstention"] is True

    # Judge prompt for the abstention question used the abstention template.
    judge_sent = server_env.created_payloads[-1]["requests"]
    abs_prompt = next(
        r["params"]["messages"][0]["content"]
        for r in judge_sent
        if r["custom_id"] == "0862e8bf_abs"
    )
    assert "unanswerable question" in abs_prompt

    md = report.read_text()
    assert "## QA accuracy (LongMemEval-S)" in md
    assert "Overall accuracy" in md


def test_missing_api_key_errors(monkeypatch, dataset_file, run_file, tmp_path):
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    with pytest.raises(SystemExit):
        cli.run_main(
            [
                "--dataset", str(dataset_file),
                "--run", str(run_file),
                "--out", str(tmp_path / "answers.jsonl"),
            ]
        )


# --- Issue 4: a reader failure is counted incorrect AND visible -------------


def test_reader_failure_counted_incorrect_and_footnoted(
    server_env, dataset_file, run_file, tmp_path, capsys
):
    # Reader: make one of the two questions ERROR. Its hypothesis becomes "" and
    # the judge will score it wrong; the report must footnote the reader failure.
    server_env.state.default_results = [
        server_env.succeeded_row("e47becba", "Business Administration"),
        server_env.errored_row("0862e8bf_abs"),
    ]
    answers = tmp_path / "answers.jsonl"
    rc = cli.run_main(
        [
            "--dataset", str(dataset_file),
            "--run", str(run_file),
            "--out", str(answers),
            "--poll-interval", "0",
        ]
    )
    assert rc == 0
    rows = {json.loads(l)["question_id"]: json.loads(l) for l in answers.read_text().splitlines() if l.strip()}
    assert rows["0862e8bf_abs"]["status"] == "errored"
    assert rows["0862e8bf_abs"]["hypothesis"] == ""

    # Judge: yes for the succeeded one, no for the errored one.
    server_env.state.default_results = [
        server_env.succeeded_row("e47becba", "yes"),
        server_env.succeeded_row("0862e8bf_abs", "no"),
    ]
    judgments = tmp_path / "judgments.jsonl"
    report = tmp_path / "report.md"
    rc = cli.judge_main(
        [
            "--dataset", str(dataset_file),
            "--answers", str(answers),
            "--out", str(judgments),
            "--report", str(report),
            "--poll-interval", "0",
        ]
    )
    assert rc == 0
    md = report.read_text()
    # Footnote present, errored reader item counted as incorrect (1/2 overall).
    assert "1 reader failures (counted incorrect)" in md
    assert "**Overall accuracy: 50.0%**" in md
    assert "(1/2)" in md


# --- Issue 5: provenance footer rendered from the CLI -----------------------


def test_report_has_provenance_footer(server_env, dataset_file, run_file, tmp_path):
    answers = tmp_path / "answers.jsonl"
    cli.run_main(
        ["--dataset", str(dataset_file), "--run", str(run_file),
         "--out", str(answers), "--poll-interval", "0"]
    )
    judgments = tmp_path / "judgments.jsonl"
    report = tmp_path / "report.md"
    cli.judge_main(
        ["--dataset", str(dataset_file), "--answers", str(answers),
         "--out", str(judgments), "--report", str(report), "--poll-interval", "0"]
    )
    md = report.read_text()
    assert "claude-opus-4-8" in md
    assert "n=2" in md
    # A UTC date line (YYYY-MM-DD ... UTC) is present.
    assert "UTC" in md


# --- Issue 8: context diagnostics summary line ------------------------------


def test_reader_prints_context_diagnostics(server_env, dataset_file, run_file, tmp_path, capsys):
    # answerable_result_line includes a stale_id_not_in_haystack -> 1 unknown.
    answers = tmp_path / "answers.jsonl"
    cli.run_main(
        ["--dataset", str(dataset_file), "--run", str(run_file),
         "--out", str(answers), "--poll-interval", "0"]
    )
    err = capsys.readouterr().err
    # One stderr summary line tallying unknown / truncated session ids.
    assert "unknown" in err.lower()
    assert "1" in err  # the one stale id


# --- Issue 9: loader robustness ---------------------------------------------


def test_load_jsonl_reports_file_and_line(tmp_path):
    bad = tmp_path / "bad.jsonl"
    bad.write_text('{"ok": 1}\nnot json at all\n')
    with pytest.raises(ValueError) as ei:
        cli._load_jsonl(bad)
    msg = str(ei.value)
    assert "bad.jsonl" in msg
    assert "line 2" in msg or "line2" in msg.replace(" ", "")


def test_load_dataset_names_index_on_missing_qid(tmp_path):
    ds = tmp_path / "ds.json"
    ds.write_text(json.dumps([{"question_id": "ok"}, {"question": "no id here"}]))
    with pytest.raises(ValueError) as ei:
        cli._load_dataset(ds)
    msg = str(ei.value)
    assert "question_id" in msg
    assert "1" in msg  # the offending entry index
