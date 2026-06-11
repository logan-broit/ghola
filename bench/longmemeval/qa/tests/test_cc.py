"""CCRunner tests over a real subprocess boundary (no mocks).

Every test points LME_QA_CLAUDE_BIN at a fake `claude` executable (a small
Python script written into tmp_path by tests/fake_claude_bin.py) and drives the
runner through the real subprocess path. The fake records its argv + stdin and
emits a canned --output-format json array shaped like the empirically-verified
claude 2.1.170 output (init event + result element).
"""

from __future__ import annotations

import json
import os

import pytest

from lme_qa.cc import CCRequest, CCRunner, UsageLimitExhausted, _looks_like_usage_limit
from tests.fake_claude_bin import (
    call_count,
    read_argv_log,
    read_stdin_log,
    write_fake_claude,
)


def _runner(binpath, **kw) -> CCRunner:
    # parallel=1 by default in tests for deterministic ordering of fake calls.
    kw.setdefault("parallel", 1)
    return CCRunner(claude_bin=str(binpath), **kw)


def test_flag_set_and_prompt_routing(tmp_path):
    binpath = write_fake_claude(tmp_path / "bin", {"answer": "hi"})
    runner = _runner(binpath)
    results = runner.run([CCRequest("q1", "MY SYSTEM PROMPT", "the user prompt")])
    assert len(results) == 1

    argv = read_argv_log(tmp_path / "bin")[0]
    # The exact isolation flag set the plan mandates.
    assert "-p" in argv
    assert argv[argv.index("--model") + 1] == "claude-opus-4-8"
    assert argv[argv.index("--system-prompt") + 1] == "MY SYSTEM PROMPT"
    # --tools "" present as an explicit empty-string value.
    ti = argv.index("--tools")
    assert argv[ti + 1] == ""
    assert "--strict-mcp-config" in argv
    assert "--no-session-persistence" in argv
    assert argv[argv.index("--output-format") + 1] == "json"

    # The user prompt goes on stdin, NOT argv (avoids arg-length limits and
    # keeps the prompt out of the process table).
    assert read_stdin_log(tmp_path / "bin")[0] == "the user prompt"
    assert "the user prompt" not in argv


def test_parses_text_usage_model(tmp_path):
    binpath = write_fake_claude(
        tmp_path / "bin",
        {"answer": "Business Administration", "input_tokens": 4242, "output_tokens": 9},
    )
    runner = _runner(binpath)
    (r,) = runner.run([CCRequest("q1", "SYS", "u")])
    assert r.custom_id == "q1"
    assert r.status == "succeeded"
    assert r.text == "Business Administration"
    assert r.input_tokens == 4242
    assert r.output_tokens == 9
    assert r.model == "claude-opus-4-8"
    assert r.error == ""


def test_nonzero_exit_is_errored_not_fatal(tmp_path):
    binpath = write_fake_claude(tmp_path / "bin", {"mode": "nonzero", "stderr": "kaboom"})
    runner = _runner(binpath)
    (r,) = runner.run([CCRequest("q1", "SYS", "u")])
    assert r.status == "errored"
    assert r.text == ""
    assert "kaboom" in r.error or r.error  # carries some detail


def test_garbage_output_is_errored(tmp_path):
    binpath = write_fake_claude(tmp_path / "bin", {"mode": "garbage"})
    runner = _runner(binpath)
    (r,) = runner.run([CCRequest("q1", "SYS", "u")])
    assert r.status == "errored"
    assert r.error  # parse failure detail


def test_timeout_is_errored(tmp_path):
    # Fake sleeps 30s; a 1s per-call timeout must kill it and record errored.
    binpath = write_fake_claude(tmp_path / "bin", {"mode": "sleep", "sleep_s": 30})
    runner = _runner(binpath, timeout_s=1)
    (r,) = runner.run([CCRequest("q1", "SYS", "u")])
    assert r.status == "errored"
    assert "timeout" in r.error.lower() or "timed out" in r.error.lower()


def test_isolation_warning_fires_on_connected_mcp_server(tmp_path, capsys):
    # The fake's init event lists a connected MCP server -> loud stderr warning
    # naming the leak (benchmark purity is compromised).
    binpath = write_fake_claude(
        tmp_path / "bin",
        {"answer": "leaky", "mcp_servers": [{"name": "ghola", "status": "connected"}]},
    )
    runner = _runner(binpath)
    runner.run([CCRequest("q1", "SYS", "u")])
    err = capsys.readouterr().err.lower()
    assert "warning" in err
    assert "ghola" in err or "mcp" in err


def test_no_isolation_warning_with_clean_init(tmp_path, capsys):
    # tools == ["LSP"] (the real CLI's benign builtin) and no MCP servers must
    # NOT warn — LSP cannot fire on a headless -p prompt.
    binpath = write_fake_claude(tmp_path / "bin", {"answer": "ok", "tools": ["LSP"]})
    runner = _runner(binpath)
    runner.run([CCRequest("q1", "SYS", "u")])
    err = capsys.readouterr().err.lower()
    assert "warning" not in err


def test_isolation_warning_on_non_builtin_tool(tmp_path, capsys):
    # A real, fireable tool (e.g. Bash) in the init event is a leak -> warn.
    binpath = write_fake_claude(
        tmp_path / "bin", {"answer": "ok", "tools": ["LSP", "Bash"]}
    )
    runner = _runner(binpath)
    runner.run([CCRequest("q1", "SYS", "u")])
    err = capsys.readouterr().err.lower()
    assert "warning" in err
    assert "bash" in err


def test_usage_limit_stops_submitting(tmp_path):
    # Fake succeeds for the first 2 calls, then every call exits nonzero with a
    # usage-limit message. The runner must STOP submitting new work and raise
    # UsageLimitExhausted carrying the partial (successful) results so progress
    # is preserved — not busy-retry the exhausted window.
    binpath = write_fake_claude(
        tmp_path / "bin",
        {"mode": "usage_limit", "ok_calls": 2, "limit_msg": "Claude usage limit reached"},
    )
    runner = _runner(binpath, parallel=1)
    reqs = [CCRequest(f"q{i}", "SYS", f"u{i}") for i in range(6)]
    with pytest.raises(UsageLimitExhausted) as ei:
        runner.run(reqs)
    partial = ei.value.results
    # The two pre-limit calls produced succeeded results; preserved on the exc.
    succeeded = [r for r in partial if r.status == "succeeded"]
    assert len(succeeded) == 2
    # The runner stopped early: it did not invoke the fake once per remaining
    # request after the limit hit (6 requests would be 6+ calls without a stop).
    assert call_count(tmp_path / "bin") < 6


def test_progress_heartbeat_every_n(tmp_path, capsys):
    # Many quick requests -> a periodic "reader: X/Y done" stderr line.
    binpath = write_fake_claude(tmp_path / "bin", {"answer": "ok"})
    runner = _runner(binpath, parallel=2, progress_every=2)
    reqs = [CCRequest(f"q{i}", "SYS", f"u{i}") for i in range(4)]
    runner.run(reqs, label="reader")
    err = capsys.readouterr().err.lower()
    assert "reader" in err
    assert "done" in err


def test_results_cover_all_custom_ids(tmp_path):
    binpath = write_fake_claude(tmp_path / "bin", {"answer": "ok"})
    runner = _runner(binpath, parallel=3)
    reqs = [CCRequest(f"q{i}", "SYS", f"u{i}") for i in range(5)]
    results = runner.run(reqs)
    assert {r.custom_id for r in results} == {f"q{i}" for i in range(5)}


def test_on_result_callback_invoked_per_result(tmp_path):
    # The on_result callback fires once per landed future, on the main thread,
    # so the CLI can persist each row as it lands rather than after the whole
    # stage. Collect (custom_id, status) per call and assert coverage.
    binpath = write_fake_claude(tmp_path / "bin", {"answer": "ok"})
    runner = _runner(binpath, parallel=1)
    seen: list[tuple[str, str]] = []
    reqs = [CCRequest(f"q{i}", "SYS", f"u{i}") for i in range(3)]
    results = runner.run(reqs, on_result=lambda r: seen.append((r.custom_id, r.status)))
    assert {cid for cid, _ in seen} == {"q0", "q1", "q2"}
    assert {r.custom_id for r in results} == {"q0", "q1", "q2"}


def test_on_result_persists_before_stage_completes(tmp_path):
    # Durability proof: the callback runs as each future lands, BEFORE the stage
    # finishes. Persist inside on_result; raise after the first row so the stage
    # is aborted mid-flight — then assert the first row is already on disk.
    binpath = write_fake_claude(tmp_path / "bin", {"answer": "ok"})
    runner = _runner(binpath, parallel=1)
    out = tmp_path / "landed.jsonl"
    count = 0

    def persist(r):
        nonlocal count
        with out.open("a") as fh:
            fh.write(json.dumps({"question_id": r.custom_id, "status": r.status}) + "\n")
            fh.flush()
            os.fsync(fh.fileno())
        count += 1
        if count == 1:
            raise RuntimeError("abort mid-stage after first row")

    reqs = [CCRequest(f"q{i}", "SYS", f"u{i}") for i in range(3)]
    with pytest.raises(RuntimeError, match="abort mid-stage"):
        runner.run(reqs, on_result=persist)

    # The first row is durably on disk even though the stage was aborted before
    # completing — proves persistence is per-question, not end-of-stage.
    rows = [json.loads(l) for l in out.read_text().splitlines() if l.strip()]
    assert len(rows) == 1
    assert rows[0]["status"] == "succeeded"


def test_usage_limit_matcher_no_false_positive_on_oom():
    # "ran out of memory" must NOT trip the matcher — the bare "out of"
    # substring was removed precisely because it matched OOM crashes (a
    # transient failure that should be recorded errored + retried, not a
    # recoverable-window stop).
    assert _looks_like_usage_limit("ran out of memory") is False
    assert _looks_like_usage_limit("Killed: out of memory (oom)") is False
    # A bare "out of" with no usage/limit phrasing stays a normal error.
    assert _looks_like_usage_limit("the model is out of scope here") is False


def test_usage_limit_matcher_still_trips_on_real_limits():
    # The genuine usage-window messages must still match (a missed match would
    # busy-retry an exhausted window).
    assert _looks_like_usage_limit("Claude usage limit reached") is True
    assert _looks_like_usage_limit("5-hour limit reached; try again later") is True
    assert _looks_like_usage_limit("You've hit your usage limit") is True
    assert _looks_like_usage_limit("rate limit exceeded") is True


def test_consecutive_failure_breaker_stops_submitting(tmp_path):
    # Failure phrasing the marker list does NOT know -> the breaker must
    # still stop the run after max_consecutive_failures (2026-06-10 lesson:
    # the real limit message slipped past the markers and 334 calls churned).
    binpath = write_fake_claude(
        tmp_path / "bin",
        {"mode": "nonzero", "stderr": "request failed for mysterious reasons", "exit_code": 1},
    )
    runner = _runner(binpath, max_consecutive_failures=3)
    reqs = [CCRequest(f"q{i}", "SYS", f"u{i}") for i in range(8)]
    with pytest.raises(UsageLimitExhausted) as ei:
        runner.run(reqs)
    assert "consecutive failures" in str(ei.value)
    # The stop flag flips on the main thread while a worker may already be
    # mid-call, so up to `parallel` extra invocations can slip through —
    # bounded at threshold + in-flight, far below the 8 queued requests.
    assert call_count(tmp_path / "bin") <= 4
    # The errored rows still landed (and would persist via on_result).
    assert len(ei.value.results) >= 3


def test_error_capture_keeps_tail(tmp_path):
    # The failure detail lives at the END of claude's output; the stored
    # error must keep the tail, not the head (2026-06-10: stored heads made
    # the real limit message unrecoverable from the answer rows).
    long_msg = "HEADMARK" + ("x" * 800) + "ACTUAL DETAIL AT TAIL"
    binpath = write_fake_claude(
        tmp_path / "bin", {"mode": "nonzero", "stderr": long_msg, "exit_code": 1}
    )
    runner = _runner(binpath, max_consecutive_failures=0)  # breaker off here
    res = runner.run([CCRequest("q1", "SYS", "u")])[0]
    assert res.status == "errored"
    assert "ACTUAL DETAIL AT TAIL" in res.error
    assert "HEADMARK" not in res.error
