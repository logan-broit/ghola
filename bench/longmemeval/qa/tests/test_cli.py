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
