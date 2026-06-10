"""Build a fake `claude` executable for the cc-backend tests.

The cc backend shells out to the claude binary (selected via LME_QA_CLAUDE_BIN),
so the honest test seam is a REAL executable invoked over a real subprocess
boundary — not a mock. ``write_fake_claude`` writes a small Python script into
tmp_path, chmods it +x, and returns its path; the script records its argv and
stdin to files next to itself and emits a canned ``--output-format json`` array
mirroring the shape verified empirically against claude 2.1.170 (an init event
with ``tools``/``mcp_servers`` and a final ``result`` element with
``result``/``usage``).

Each fake is parameterized by a tiny JSON "scenario" file the script reads at
run time, so one fake binary can model: a clean answer, a per-call sequence of
answers, an isolation leak (a connected MCP server in the init event), a
nonzero exit, garbage output, a sleep (for the timeout path), and a usage-limit
exhaustion that flips to a nonzero limit error after N successful calls.
"""

from __future__ import annotations

import json
import stat
from pathlib import Path

# The fake script body. Dependency-free (stdlib only); it reads a sibling
# scenario.json to decide what to emit and writes argv/stdin/counter logs next
# to itself so the test can assert the exact flag set, prompt routing, and call
# count across the separate subprocess invocations the runner makes. No
# str.format() is used (the body contains literal JSON braces), so the shebang
# is prepended at write time instead.
_FAKE_BODY = r'''
import json, os, sys, time

HERE = os.path.dirname(os.path.abspath(__file__))
SCENARIO = os.path.join(HERE, "scenario.json")
ARGV_LOG = os.path.join(HERE, "argv.log")
STDIN_LOG = os.path.join(HERE, "stdin.log")
COUNTER = os.path.join(HERE, "counter")

scenario = json.load(open(SCENARIO))

# Record argv (one JSON array per invocation) and stdin (one record per
# invocation) so the test can assert the exact flag set and prompt routing.
argv = sys.argv[1:]
stdin_data = sys.stdin.read()
with open(ARGV_LOG, "a") as fh:
    fh.write(json.dumps(argv) + "\n")
with open(STDIN_LOG, "a") as fh:
    fh.write(json.dumps(stdin_data) + "\n")

# Advance the per-binary call counter (0-based index of THIS call).
n = 0
if os.path.exists(COUNTER):
    n = int(open(COUNTER).read() or "0")
with open(COUNTER, "w") as fh:
    fh.write(str(n + 1))

mode = scenario.get("mode", "ok")

if mode == "sleep":
    # Outlive a short --timeout-s so the runner kills us (timeout path).
    time.sleep(scenario.get("sleep_s", 30))
    sys.exit(0)

if mode == "garbage":
    sys.stdout.write("this is not json{{{")
    sys.exit(0)

if mode == "nonzero":
    sys.stderr.write(scenario.get("stderr", "boom"))
    sys.exit(scenario.get("exit_code", 1))

if mode == "usage_limit":
    # Succeed for the first `ok_calls`, then every further call exits nonzero
    # with a usage-limit message (models an exhausted subscription window).
    if n >= scenario.get("ok_calls", 1):
        sys.stderr.write(scenario.get("limit_msg", "Claude usage limit reached"))
        sys.exit(1)

# --- success path: emit a JSON array (init event + assistant + result) ---
tools = scenario.get("tools", ["LSP"])
mcp_servers = scenario.get("mcp_servers", [])
model = scenario.get("model", "claude-opus-4-8")

# answer: a fixed string, or the n-th element of a sequence (for resume tests).
if "answers" in scenario:
    seq = scenario["answers"]
    answer = seq[n] if n < len(seq) else seq[-1]
else:
    answer = scenario.get("answer", "fake answer")

in_tok = scenario.get("input_tokens", 100)
out_tok = scenario.get("output_tokens", 7)

events = [
    {
        "type": "system",
        "subtype": "init",
        "model": model,
        "tools": tools,
        "mcp_servers": mcp_servers,
        "session_id": "fake-session",
    },
    {
        "type": "assistant",
        "message": {
            "model": model,
            "role": "assistant",
            "content": [{"type": "text", "text": answer}],
        },
    },
    {
        "type": "result",
        "subtype": "success",
        "is_error": False,
        "result": answer,
        "model": model,
        "usage": {"input_tokens": in_tok, "output_tokens": out_tok},
    },
]
sys.stdout.write(json.dumps(events))
sys.exit(0)
'''


def write_fake_claude(dir_path: Path, scenario: dict) -> Path:
    """Write a fake claude executable + its scenario into ``dir_path``.

    Returns the path to the executable (point LME_QA_CLAUDE_BIN at it). The
    scenario dict controls behavior; see the module docstring for modes.
    """
    dir_path.mkdir(parents=True, exist_ok=True)
    (dir_path / "scenario.json").write_text(json.dumps(scenario))
    # Reset any logs/counter from a prior fake in the same dir.
    for name in ("argv.log", "stdin.log", "counter"):
        p = dir_path / name
        if p.exists():
            p.unlink()
    binpath = dir_path / "claude"
    binpath.write_text("#!/usr/bin/env python3\n" + _FAKE_BODY)
    binpath.chmod(binpath.stat().st_mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)
    return binpath


def read_argv_log(dir_path: Path) -> list[list[str]]:
    """The recorded argv arrays, one per fake invocation (in call order)."""
    p = dir_path / "argv.log"
    if not p.exists():
        return []
    return [json.loads(line) for line in p.read_text().splitlines() if line.strip()]


def read_stdin_log(dir_path: Path) -> list[str]:
    """The recorded stdin payloads, one per fake invocation (in call order)."""
    p = dir_path / "stdin.log"
    if not p.exists():
        return []
    return [json.loads(line) for line in p.read_text().splitlines() if line.strip()]


def call_count(dir_path: Path) -> int:
    p = dir_path / "counter"
    return int(p.read_text() or "0") if p.exists() else 0
