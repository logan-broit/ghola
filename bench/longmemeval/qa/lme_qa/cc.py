"""Claude Code execution backend for lme-qa (subscription auth, no API key).

The operator has no ANTHROPIC_API_KEY, so the reader/judge can't hit the
Batches API. This backend instead drives headless ``claude -p`` once per
request through a bounded worker pool, parsing the ``--output-format json``
array the CLI emits. It deliberately mirrors batch.py's ItemResult surface
(``CCResult`` carries the same custom_id/status/text/usage fields) so the CLI
treats both backends uniformly and the downstream row-building logic is not
forked.

Isolation is load-bearing for benchmark purity. A bare ``claude -p`` session
loads the operator's MCP servers (ghola) and plugins — which would let the
reader reach tools/memory the benchmark must not give it. Every call passes
``--tools ""``, ``--strict-mcp-config``, ``--no-session-persistence``, and a
full ``--system-prompt`` replacement. We then assert isolation from the init
event the CLI emits and warn loudly on any leak.

Empirically verified (claude 2.1.170 on this machine): with the full flag set
the init event reports ``mcp_servers: []`` (isolation works) but ``tools:
["LSP"]`` — the rust-analyzer-lsp plugin injects an LSP tool that ``--tools ""``
does not strip. LSP cannot fire on a non-interactive ``-p`` benchmark prompt
(no editor session), so it is treated as a benign builtin; the isolation check
warns only on a CONNECTED MCP server or on a non-builtin (fireable) tool.

The result element (``type:"result"``) carries the answer in ``result`` and
token counts under ``usage`` (``input_tokens``/``output_tokens``); ``model``
appears on the init + assistant events. A nonzero exit, timeout, or unparseable
output yields ``CCResult(status="errored")`` rather than aborting the whole run
— one bad question never loses the rest.

Usage-limit handling: the subscription has a rolling usage window. When the CLI
signals the window is exhausted (nonzero exit + a limit/usage message), the
runner STOPS submitting new work, lets in-flight calls finish, and raises
``UsageLimitExhausted`` carrying the partial results so the caller can persist
progress and re-run after the window resets — never busy-retrying an exhausted
window.
"""

from __future__ import annotations

import json
import subprocess
import sys
from collections.abc import Callable
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
from typing import Any

# Fixed model surface, matching the Batches backend (batch.MODEL) so both paths
# answer with the same model — comparability of the published number.
MODEL = "claude-opus-4-8"

# Per-call wall-clock timeout (seconds). A reader call over a top-10-session
# context can take a while with adaptive thinking; 300s is generous headroom.
DEFAULT_TIMEOUT_S = 300

# Default worker pool size. Kept low: the subscription window is the bottleneck,
# and a flood of parallel calls just exhausts it faster with no throughput win.
DEFAULT_PARALLEL = 2

# Tools the CLI injects that cannot fire on a headless -p benchmark prompt, so
# their presence in the init event is NOT an isolation leak. "LSP" is the
# rust-analyzer-lsp plugin tool observed on this machine even with --tools "".
_BENIGN_TOOLS = frozenset({"LSP"})


@dataclass(frozen=True)
class CCRequest:
    """One reader/judge request to run through ``claude -p``.

    ``system`` is the full system-prompt replacement; ``prompt`` is the user
    turn (sent on stdin). Mirrors the (system, user) split the Batches backend
    builds so the SAME prompt content drives both paths.
    """

    custom_id: str
    system: str
    prompt: str


@dataclass(frozen=True)
class CCResult:
    """One collected per-request result. Field-compatible with batch.ItemResult
    (custom_id/status/text/input_tokens/output_tokens/error) so the CLI's
    row-building logic treats both backends identically; adds ``model``, which
    the CLI ignores but is useful for provenance/debugging.
    """

    custom_id: str
    status: str  # "succeeded" | "errored"
    text: str
    input_tokens: int
    output_tokens: int
    error: str
    model: str = ""


class UsageLimitExhausted(Exception):
    """Raised when the subscription usage window is exhausted mid-run.

    Carries the results collected before the limit hit so the caller can
    persist progress and exit cleanly (re-run after the window resets).
    """

    def __init__(self, message: str, results: list[CCResult]) -> None:
        super().__init__(message)
        self.results = results


# Substrings that, on a nonzero exit, indicate the subscription usage window is
# exhausted. Anchored on "usage"/"limit" phrasing so transient crashes don't
# masquerade as a usage-window stop — notably the bare "out of" marker was
# removed because it matched "ran out of memory" (an OOM crash should be
# recorded errored + retried, not treated as a recoverable-window stop). The
# matcher is consulted ONLY on a nonzero exit against stderr/stdout (never the
# model's answer text): a false positive only costs a recoverable early stop
# (the next re-run picks up where it left off) — the deliberate tradeoff over a
# missed match that would busy-retry an exhausted window.
_USAGE_LIMIT_MARKERS = (
    "usage limit",
    "rate limit",
    "limit reached",  # "5-hour limit reached", "weekly limit reached"
    "limit exceeded",
    "hour limit",  # "5-hour limit", "you've hit your 5-hour limit"
    "upgrade to",
    "resets at",
    "try again later",
)


def _looks_like_usage_limit(text: str) -> bool:
    low = text.lower()
    return any(m in low for m in _USAGE_LIMIT_MARKERS)


def _find_event(events: list[Any], event_type: str, subtype: str | None = None) -> dict | None:
    """First array element matching ``type`` (and optional ``subtype``)."""
    for e in events:
        if not isinstance(e, dict):
            continue
        if e.get("type") != event_type:
            continue
        if subtype is not None and e.get("subtype") != subtype:
            continue
        return e
    return None


@dataclass
class CCRunner:
    """Drives one stage's requests through ``claude -p``, one subprocess each.

    ``claude_bin`` is the binary path/name (env LME_QA_CLAUDE_BIN at the CLI
    layer — also the test seam). ``run()`` returns a CCResult per request; it
    raises ``UsageLimitExhausted`` (with partial results) if the subscription
    window runs out mid-run.
    """

    claude_bin: str = "claude"
    timeout_s: int = DEFAULT_TIMEOUT_S
    parallel: int = DEFAULT_PARALLEL
    progress_every: int = 10
    # Phrasing-independent stop: the 2026-06-10 run proved the marker list
    # can miss the real limit message (334 calls churned uselessly). N
    # consecutive errored results — whatever the message — means the window
    # is exhausted or the failure is systemic; stop submitting either way.
    max_consecutive_failures: int = 10
    # Set once a usage-limit signal is seen, so queued work short-circuits to a
    # no-op instead of hammering the exhausted window.
    _stopped: bool = field(default=False, init=False)

    def _argv(self, system: str) -> list[str]:
        """The isolation flag set for one call. ``--tools ""`` passes an
        explicit empty string (strip the default tool set); the prompt is NOT
        here — it goes on stdin.
        """
        return [
            self.claude_bin,
            "-p",
            "--model",
            MODEL,
            "--system-prompt",
            system,
            "--tools",
            "",
            "--strict-mcp-config",
            "--no-session-persistence",
            "--output-format",
            "json",
        ]

    def _check_isolation(self, init: dict, custom_id: str) -> None:
        """Warn loudly if the init event shows the call was NOT isolated.

        A connected MCP server or a non-builtin (fireable) tool means the
        benchmark prompt could reach memory/tools it must not — print one loud
        stderr warning naming the leak. Benign builtins (LSP) are allowed.
        """
        leaks: list[str] = []
        servers = init.get("mcp_servers") or []
        connected = [
            s.get("name", "?")
            for s in servers
            if not isinstance(s, dict) or s.get("status") != "disconnected"
        ]
        if connected:
            leaks.append(f"connected MCP servers {connected}")
        tools = init.get("tools") or []
        fireable = [t for t in tools if t not in _BENIGN_TOOLS]
        if fireable:
            leaks.append(f"non-builtin tools {fireable}")
        if leaks:
            print(
                f"warning: {custom_id}: claude session was NOT isolated "
                f"({'; '.join(leaks)}) — benchmark purity is compromised; "
                f"check --tools/--strict-mcp-config support in this CLI version.",
                file=sys.stderr,
            )

    def _run_one(self, req: CCRequest) -> CCResult:
        """Invoke ``claude -p`` for one request; never raises for a per-item
        failure (returns an errored CCResult instead). A detected usage-limit
        exit is signaled by raising ``UsageLimitExhausted`` so the pool stops.
        """
        try:
            proc = subprocess.run(
                self._argv(req.system),
                input=req.prompt,
                capture_output=True,
                text=True,
                timeout=self.timeout_s,
            )
        except subprocess.TimeoutExpired:
            return CCResult(
                custom_id=req.custom_id,
                status="errored",
                text="",
                input_tokens=0,
                output_tokens=0,
                error=f"timeout after {self.timeout_s}s",
            )
        except OSError as exc:
            return CCResult(
                custom_id=req.custom_id,
                status="errored",
                text="",
                input_tokens=0,
                output_tokens=0,
                error=f"failed to launch claude: {exc}",
            )

        if proc.returncode != 0:
            detail = (proc.stderr or proc.stdout or "").strip()
            if _looks_like_usage_limit(detail):
                # Stop the whole run: the subscription window is exhausted.
                raise UsageLimitExhausted(detail or "usage limit reached", [])
            return CCResult(
                custom_id=req.custom_id,
                status="errored",
                text="",
                input_tokens=0,
                output_tokens=0,
                # Keep the TAIL: claude's JSON event stream puts the failure
                # detail in the final result element; the head is just the
                # init event. (The 2026-06-10 run stored heads and the actual
                # limit message was unrecoverable from the answer rows.)
                error=f"exit {proc.returncode}: ...{detail[-700:]}" if len(detail) > 700
                else f"exit {proc.returncode}: {detail}",
            )

        return self._parse(req.custom_id, proc.stdout)

    def _parse(self, custom_id: str, stdout: str) -> CCResult:
        """Parse the --output-format json array into a CCResult.

        Unparseable / unexpected output -> errored (the run continues). The
        isolation check runs off the init event when present.
        """
        try:
            events = json.loads(stdout)
        except json.JSONDecodeError as exc:
            return CCResult(
                custom_id=custom_id,
                status="errored",
                text="",
                input_tokens=0,
                output_tokens=0,
                error=f"unparseable claude output: {exc}",
            )
        if not isinstance(events, list):
            return CCResult(
                custom_id=custom_id,
                status="errored",
                text="",
                input_tokens=0,
                output_tokens=0,
                error="claude output was not a JSON array of events",
            )

        init = _find_event(events, "system", subtype="init")
        if init is not None:
            self._check_isolation(init, custom_id)

        result = _find_event(events, "result")
        if result is None:
            return CCResult(
                custom_id=custom_id,
                status="errored",
                text="",
                input_tokens=0,
                output_tokens=0,
                error="no result element in claude output",
            )
        if result.get("is_error"):
            return CCResult(
                custom_id=custom_id,
                status="errored",
                text="",
                input_tokens=0,
                output_tokens=0,
                error=str(result.get("result") or result.get("subtype") or "result is_error"),
            )

        usage = result.get("usage") or {}
        model = (
            result.get("model")
            or (init.get("model") if init else None)
            or ""
        )
        return CCResult(
            custom_id=custom_id,
            status="succeeded",
            text=str(result.get("result", "")).strip(),
            input_tokens=int(usage.get("input_tokens", 0) or 0),
            output_tokens=int(usage.get("output_tokens", 0) or 0),
            error="",
            model=model,
        )

    def run(
        self,
        requests: list[CCRequest],
        label: str = "stage",
        on_result: Callable[[CCResult], None] | None = None,
    ) -> list[CCResult]:
        """Run all requests through a bounded pool; return one CCResult each.

        ``on_result`` (if given) is invoked once per landed result so the caller
        can persist each row AS IT LANDS — per-question durability, not
        end-of-stage. It is called from THIS method's ``as_completed`` loop,
        which runs on the calling (main) thread; the worker threads only run
        ``_run_one`` and never touch ``on_result``. So the callback is
        single-threaded by construction and needs no lock — it may freely
        append+fsync to a file. The callback fires for every collected result,
        including on the usage-limit path: the partial results that landed
        before the limit are persisted as they land, and the limit only stops
        NEW submissions (the in-flight items still drain through the callback
        before ``UsageLimitExhausted`` is raised). A callback that raises
        propagates out of ``run`` (it is the caller's failure to handle).

        Emits a stderr progress line every ``progress_every`` completions.
        Raises ``UsageLimitExhausted`` (carrying whatever results landed) on a
        usage-window exhaustion, so the caller persists partial progress and
        re-runs after the window resets.
        """
        results: list[CCResult] = []
        done = 0
        errored = 0
        consecutive = 0
        n = len(requests)
        limit_hit: UsageLimitExhausted | None = None

        with ThreadPoolExecutor(max_workers=max(1, self.parallel)) as pool:
            futures = {pool.submit(self._guarded, req): req for req in requests}
            for fut in as_completed(futures):
                req = futures[fut]
                try:
                    res = fut.result()
                except UsageLimitExhausted as exc:
                    # First limit signal: flip the stop flag so still-queued
                    # work short-circuits, and remember the exception to raise
                    # once in-flight items drain.
                    self._stopped = True
                    if limit_hit is None:
                        limit_hit = exc
                    continue
                if res is None:
                    # Short-circuited after the stop flag was set; not counted.
                    continue
                results.append(res)
                # Persist-as-it-lands hook (main thread; see method docstring).
                # Done BEFORE counting/progress so a row is durable the moment
                # the future is collected.
                if on_result is not None:
                    on_result(res)
                done += 1
                if res.status != "succeeded":
                    errored += 1
                    consecutive += 1
                    if (
                        limit_hit is None
                        and self.max_consecutive_failures
                        and consecutive >= self.max_consecutive_failures
                    ):
                        # Circuit breaker: stop submitting regardless of the
                        # failure phrasing; in-flight items still drain (and
                        # persist via on_result) before the raise below.
                        self._stopped = True
                        limit_hit = UsageLimitExhausted(
                            f"{consecutive} consecutive failures; stopping new "
                            f"submissions (last: {res.error[-200:]})",
                            results,
                        )
                else:
                    consecutive = 0
                if self.progress_every and done % self.progress_every == 0:
                    print(
                        f"{label}: {done}/{n} done, {errored} errored",
                        file=sys.stderr,
                    )

        if limit_hit is not None:
            raise UsageLimitExhausted(str(limit_hit), results)
        return results

    def _guarded(self, req: CCRequest) -> CCResult | None:
        """Pool task wrapper: short-circuits to None once the stop flag is set
        (don't keep hitting an exhausted usage window for queued work).
        """
        if self._stopped:
            return None
        return self._run_one(req)
