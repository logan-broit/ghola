"""CCRunner tests over a real subprocess boundary (no mocks).

Every test points LME_QA_CLAUDE_BIN at a fake `claude` executable (a small
Python script written into tmp_path by tests/fake_claude_bin.py) and drives the
runner through the real subprocess path. The fake records its argv + stdin and
emits a canned --output-format json array shaped like the empirically-verified
claude 2.1.170 output (init event + result element).
"""

from __future__ import annotations

import pytest

from lme_qa.cc import CCRequest, CCRunner, UsageLimitExhausted
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
