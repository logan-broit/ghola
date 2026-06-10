"""CLI end-to-end through the claude-code backend (real subprocess, no mocks).

Drives lme-qa-run / lme-qa-judge with --backend claude-code against a fake
claude executable (LME_QA_CLAUDE_BIN), covering: a 2-question read+judge,
per-question resume (succeeded rows skipped, failed rows retried, append+flush),
the missing-key error pointing at the cc backend, and the `via claude-code`
report footer.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from lme_qa import cli
from tests.fake_claude_bin import call_count, read_stdin_log, write_fake_claude


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


def _use_fake(monkeypatch, tmp_path, scenario) -> Path:
    bindir = tmp_path / "bin"
    binpath = write_fake_claude(bindir, scenario)
    monkeypatch.setenv("LME_QA_CLAUDE_BIN", str(binpath))
    # No API key on the cc path — make sure its absence does NOT error.
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    return bindir


def test_run_then_judge_cc_backend(monkeypatch, dataset_file, run_file, tmp_path):
    bindir = _use_fake(monkeypatch, tmp_path, {"answer": "an answer"})
    answers = tmp_path / "answers.jsonl"
    rc = cli.run_main(
        [
            "--dataset", str(dataset_file),
            "--run", str(run_file),
            "--out", str(answers),
            "--k", "10",
            "--backend", "claude-code",
            "--parallel", "1",
        ]
    )
    assert rc == 0
    rows = [json.loads(l) for l in answers.read_text().splitlines() if l.strip()]
    assert {r["question_id"] for r in rows} == {"e47becba", "0862e8bf_abs"}
    assert all(r["status"] == "succeeded" for r in rows)
    assert all(r["hypothesis"] == "an answer" for r in rows)
    assert all("question_type" in r and "usage" in r and r["k"] == 10 for r in rows)

    # The SAME reader prompt content the batches path uses reaches the binary:
    # the reader system prompt + the rendered user prompt (on stdin). One stdin
    # per question; each carries the question text + context.
    stdins = read_stdin_log(bindir)
    assert len(stdins) == 2
    assert any("What degree did I graduate with?" in s for s in stdins)

    # Judge stage through the cc backend. Fake returns "yes"/"no" via answers
    # keyed off the call sequence so we can drive both labels.
    _use_fake(monkeypatch, tmp_path, {"answers": ["yes", "no"]})
    judgments = tmp_path / "judgments.jsonl"
    report = tmp_path / "report.md"
    rc = cli.judge_main(
        [
            "--dataset", str(dataset_file),
            "--answers", str(answers),
            "--out", str(judgments),
            "--report", str(report),
            "--backend", "claude-code",
            "--parallel", "1",
        ]
    )
    assert rc == 0
    jrows = {
        json.loads(l)["question_id"]: json.loads(l)
        for l in judgments.read_text().splitlines()
        if l.strip()
    }
    # One of the two got "yes" -> True; abstention flag preserved.
    labels = {jrows[q]["label"] for q in jrows}
    assert labels == {True, False}
    assert jrows["0862e8bf_abs"]["is_abstention"] is True
    md = report.read_text()
    assert "## QA accuracy (LongMemEval-S)" in md
    # Provenance footer records the access path.
    assert "via claude-code" in md


def test_cc_backend_per_question_resume(monkeypatch, dataset_file, run_file, tmp_path):
    # Pre-seed answers.jsonl: one succeeded row (must be skipped) and one
    # ERRORED row (must be retried). Only the un-done questions should hit the
    # fake binary, and the file must be appended (not rewritten from scratch).
    answers = tmp_path / "answers.jsonl"
    answers.write_text(
        json.dumps(
            {
                "question_id": "e47becba",
                "question_type": "single-session-user",
                "k": 10,
                "hypothesis": "already done",
                "status": "succeeded",
                "error": "",
                "usage": {"input_tokens": 1, "output_tokens": 1},
            }
        )
        + "\n"
        + json.dumps(
            {
                "question_id": "0862e8bf_abs",
                "question_type": "single-session-user",
                "k": 10,
                "hypothesis": "",
                "status": "errored",
                "error": "prior boom",
                "usage": {"input_tokens": 0, "output_tokens": 0},
            }
        )
        + "\n"
    )

    bindir = _use_fake(monkeypatch, tmp_path, {"answer": "fresh answer"})
    rc = cli.run_main(
        [
            "--dataset", str(dataset_file),
            "--run", str(run_file),
            "--out", str(answers),
            "--backend", "claude-code",
            "--parallel", "1",
        ]
    )
    assert rc == 0

    # The fake was invoked exactly ONCE — only the errored question was retried;
    # the succeeded one was skipped.
    assert call_count(bindir) == 1

    rows = {
        json.loads(l)["question_id"]: json.loads(l)
        for l in answers.read_text().splitlines()
        if l.strip()
    }
    # The succeeded row is preserved verbatim (not re-run).
    assert rows["e47becba"]["hypothesis"] == "already done"
    # The previously-errored row is now succeeded with the fresh answer.
    assert rows["0862e8bf_abs"]["status"] == "succeeded"
    assert rows["0862e8bf_abs"]["hypothesis"] == "fresh answer"
    # No duplicate rows for either id.
    all_ids = [json.loads(l)["question_id"] for l in answers.read_text().splitlines() if l.strip()]
    assert sorted(all_ids) == ["0862e8bf_abs", "e47becba"]


def test_batches_missing_key_error_suggests_cc(monkeypatch, dataset_file, run_file, tmp_path):
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    with pytest.raises(SystemExit) as ei:
        cli.run_main(
            [
                "--dataset", str(dataset_file),
                "--run", str(run_file),
                "--out", str(tmp_path / "answers.jsonl"),
                "--backend", "batches",
            ]
        )
    assert "claude-code" in str(ei.value)


def test_cc_backend_errors_when_binary_missing(monkeypatch, dataset_file, run_file, tmp_path):
    # claude-code backend but the binary doesn't exist -> clear error, no key
    # requirement.
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    monkeypatch.setenv("LME_QA_CLAUDE_BIN", str(tmp_path / "nonexistent-claude"))
    with pytest.raises(SystemExit) as ei:
        cli.run_main(
            [
                "--dataset", str(dataset_file),
                "--run", str(run_file),
                "--out", str(tmp_path / "answers.jsonl"),
                "--backend", "claude-code",
            ]
        )
    msg = str(ei.value).lower()
    assert "claude" in msg
