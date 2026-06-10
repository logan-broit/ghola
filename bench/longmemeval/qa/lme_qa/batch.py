"""Thin wrapper over the anthropic Messages Batches API.

Both the reader and the judge submit one batch keyed by custom_id=question_id,
poll until the batch ends, then collect per-item results tolerantly (an
errored/expired item is recorded, not allowed to abort the whole collection).

Model surface is fixed by the plan: claude-opus-4-8 with adaptive thinking and
NO sampling parameters (temperature/top_p/top_k 400 on Opus 4.8). Adaptive
thinking tokens count toward output, so the reader gets a generous max_tokens.

A small JSON state file records, per stage, the submitted batch_id plus a
fingerprint of the request set, so re-running an interrupted job resumes by
polling the in-flight batch instead of resubmitting (paying for it twice). The
fingerprint folds in the full request params, so resume means "identical work":
a changed K/prompt/context forces a fresh submission rather than reusing a stale
batch.

Submit uses an orphan-safe write protocol: a {pending: true} marker is written
BEFORE batches.create() and replaced with the real batch_id after, so a crash in
the create window leaves a trail the next run uses to adopt the orphaned paid
batch (or resubmit, loudly) instead of silently double-charging. poll() emits a
per-poll heartbeat and is bounded at 24h (when the API expires batches anyway);
a recorded batch_id that 404s self-heals by dropping the stale entry and
resubmitting.
"""

from __future__ import annotations

import hashlib
import json
import sys
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Iterable

MODEL = "claude-opus-4-8"
# Adaptive thinking only on Opus 4.8 (enabled/budget_tokens 400s). Thinking
# tokens count toward output, hence the headroom on the reader. Sampling
# parameters are omitted entirely — present any of them and the API 400s.
READER_MAX_TOKENS = 8000
JUDGE_MAX_TOKENS = 2048
_THINKING: dict[str, str] = {"type": "adaptive"}

# Poll cadence: the batch can take minutes to hours; 60s between checks is
# gentle on rate limits and fine for a benchmark run.
POLL_INTERVAL_S = 60

# Hard wall-clock bound on a single poll loop. The Batches API expires a batch
# at 24h anyway, so polling past that is pointless — raise instead of spinning
# forever (a hung run silently burns nothing but never returns a result).
POLL_MAX_WALL_CLOCK_S = 24 * 60 * 60


@dataclass(frozen=True)
class ItemResult:
    """One collected per-item result, keyed by custom_id (== question_id)."""

    custom_id: str
    status: str  # "succeeded" | "errored" | "canceled" | "expired"
    text: str  # final answer text (empty unless succeeded)
    input_tokens: int
    output_tokens: int
    error: str  # error detail (empty unless errored/canceled/expired)


def reader_request(custom_id: str, system: str, user_text: str) -> dict[str, Any]:
    """Build one Batches reader request.

    Cacheable frozen system prompt first; the per-question user text last.
    """
    return {
        "custom_id": custom_id,
        "params": {
            "model": MODEL,
            "max_tokens": READER_MAX_TOKENS,
            "thinking": _THINKING,
            "system": system,
            "messages": [{"role": "user", "content": user_text}],
        },
    }


def judge_request(custom_id: str, prompt: str) -> dict[str, Any]:
    """Build one Batches judge request. The judge prompt is a single user turn
    (upstream uses one user message); no system prompt, no prefill.
    """
    return {
        "custom_id": custom_id,
        "params": {
            "model": MODEL,
            "max_tokens": JUDGE_MAX_TOKENS,
            "thinking": _THINKING,
            "messages": [{"role": "user", "content": prompt}],
        },
    }


def fingerprint(requests: list[dict[str, Any]]) -> str:
    """Stable fingerprint of a request set: custom_ids AND request params.

    Resume means "identical work". We fold a canonical (sorted-key) JSON dump of
    each request's full params into the hash, not just the custom_id set, so a
    changed K, prompt, context, or model produces a different fingerprint and
    forces a resubmit instead of silently reusing a batch that answered a
    different question. Requests are sorted by custom_id first so request order
    doesn't perturb the hash.
    """
    canonical = sorted(
        (r["custom_id"], json.dumps(r.get("params", {}), sort_keys=True))
        for r in requests
    )
    blob = "\n".join(f"{cid}\t{params}" for cid, params in canonical)
    return hashlib.sha256(blob.encode()).hexdigest()[:16]


# --- state file -------------------------------------------------------------


def load_state(path: Path) -> dict[str, Any]:
    if path.exists():
        return json.loads(path.read_text())
    return {}


def save_state(path: Path, state: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(state, indent=2))


# --- the batch driver -------------------------------------------------------


def _parse_ts(value: Any) -> datetime | None:
    """Best-effort parse of an ISO 8601 timestamp (str or datetime) to an
    aware datetime, for cutoff comparison in adoption. Returns None if it can't
    be parsed — the caller then skips the timestamp filter rather than crash.
    """
    if value is None:
        return None
    if isinstance(value, datetime):
        return value if value.tzinfo else value.replace(tzinfo=timezone.utc)
    try:
        # fromisoformat handles "+00:00"; normalize a trailing 'Z' it predates.
        dt = datetime.fromisoformat(str(value).replace("Z", "+00:00"))
    except ValueError:
        return None
    return dt if dt.tzinfo else dt.replace(tzinfo=timezone.utc)


def _text_from_message(message: Any) -> str:
    """Concatenate the visible text blocks of a Message, skipping thinking.

    Adaptive-thinking responses interleave thinking blocks (empty text by
    default on Opus 4.8) ahead of the answer; we keep only ``type == "text"``.
    """
    parts: list[str] = []
    for block in getattr(message, "content", []) or []:
        if getattr(block, "type", None) == "text":
            parts.append(getattr(block, "text", ""))
    return "".join(parts).strip()


class BatchDriver:
    """Drives one stage's batch lifecycle against a live anthropic client."""

    def __init__(self, client: Any, state_path: Path) -> None:
        self._client = client
        self._state_path = state_path

    def _commit(self, stage: str, batch_id: str, fp: str) -> None:
        """Replace whatever is recorded for ``stage`` with the committed
        record: a real batch_id + fingerprint, no ``pending`` flag.
        """
        state = load_state(self._state_path)
        state[stage] = {"batch_id": batch_id, "fingerprint": fp}
        save_state(self._state_path, state)

    def _create(self, stage: str, requests: list[dict[str, Any]], fp: str) -> str:
        """Create a paid batch with an orphan-safe write protocol.

        A crash between ``batches.create()`` and recording its id would leave a
        paid in-flight batch with no state trail — the next run would resubmit
        and double-charge. To bound that window we write a ``pending`` marker
        (fingerprint + request count + a ``created_after`` timestamp) BEFORE the
        create call, then replace it with the real batch_id after. A crash in
        between leaves the marker, which ``_adopt_pending`` uses to find and
        adopt the orphaned batch on the next run instead of resubmitting.
        """
        state = load_state(self._state_path)
        created_after = datetime.now(timezone.utc).isoformat()
        state[stage] = {
            "pending": True,
            "fingerprint": fp,
            "n_requests": len(requests),
            "created_after": created_after,
        }
        save_state(self._state_path, state)

        batch = self._client.messages.batches.create(requests=requests)
        self._commit(stage, batch.id, fp)
        return batch.id

    def _adopt_pending(
        self, stage: str, marker: dict[str, Any], fp: str
    ) -> str | None:
        """Try to adopt an orphaned batch left by a crash mid-``_create``.

        ``batches.list()`` returns batch metadata only (no per-item custom_ids
        without an expensive results fetch), so we cannot fingerprint-match the
        listed batches directly. The pragmatic, robust heuristic: adopt the most
        recent batch created at/after the marker's ``created_after`` whose
        ``request_counts`` total equals the recorded ``n_requests``, and warn
        loudly. If none matches, return None so the caller resubmits (also with
        a warning). This guarantees an interrupt during submit never silently
        double-charges without at least an adoption attempt and a loud warning.
        """
        if marker.get("fingerprint") != fp:
            # The pending work doesn't match what we're submitting now; don't
            # adopt a batch for a different question set.
            return None
        created_after_raw = marker.get("created_after", "")
        cutoff = _parse_ts(created_after_raw)
        n_requests = marker.get("n_requests")

        best: Any = None
        for batch in self._client.messages.batches.list(limit=100):
            created = getattr(batch, "created_at", None)
            # Compare as datetimes (not lexically): the SDK may render created_at
            # with a 'Z' suffix or differing fractional precision than the
            # marker's isoformat(), which would defeat a string compare.
            if cutoff is not None and created is not None:
                created_dt = _parse_ts(created)
                if created_dt is not None and created_dt < cutoff:
                    continue
            counts = getattr(batch, "request_counts", None)
            total = 0
            if counts is not None:
                total = sum(
                    getattr(counts, f, 0) or 0
                    for f in ("processing", "succeeded", "errored", "canceled", "expired")
                )
            if n_requests is not None and total != n_requests:
                continue
            # list() returns newest-first; the first match is the most recent.
            best = batch
            break

        if best is None:
            return None
        print(
            f"warning: {stage}: a pending submit marker was found with no "
            f"committed batch_id; adopting the most recent matching batch "
            f"{best.id} ({n_requests} requests, created >= {created_after_raw}) "
            f"rather than resubmitting and paying twice. Verify with --adopt "
            f"if this is wrong.",
            file=sys.stderr,
        )
        self._commit(stage, best.id, fp)
        return best.id

    def submit(
        self,
        stage: str,
        requests: list[dict[str, Any]],
        fresh: bool = False,
        adopt: str | None = None,
    ) -> str:
        """Submit (or resume/adopt) a batch for ``stage``; returns the batch_id.

        Resumes a committed batch when the state file records a batch_id for
        this stage with a matching request fingerprint and ``fresh`` is False.
        A fingerprint mismatch (different K/prompt/context/question set) always
        resubmits.

        ``adopt`` is the manual override: take that batch_id verbatim, commit
        it, and never call create() — the escape hatch when automatic adoption
        of an orphaned paid batch fails.

        A ``pending`` marker (no committed batch_id) means a prior run crashed
        between create() and the state write; we try to adopt the orphaned
        batch before resubmitting (see ``_adopt_pending``).
        """
        state = load_state(self._state_path)
        fp = fingerprint(requests)

        if adopt is not None:
            self._commit(stage, adopt, fp)
            return adopt

        prior = state.get(stage)
        if not fresh and prior:
            if prior.get("pending"):
                adopted = self._adopt_pending(stage, prior, fp)
                if adopted is not None:
                    return adopted
                print(
                    f"warning: {stage}: pending submit marker with no adoptable "
                    f"batch found; resubmitting.",
                    file=sys.stderr,
                )
                # fall through to create
            elif prior.get("batch_id") and prior.get("fingerprint") == fp:
                return prior["batch_id"]

        return self._create(stage, requests, fp)

    def poll(
        self,
        batch_id: str,
        interval_s: int = POLL_INTERVAL_S,
        max_wall_clock_s: int = POLL_MAX_WALL_CLOCK_S,
    ) -> Any:
        """Block until the batch's processing_status is 'ended'; return it.

        Emits a per-poll stderr heartbeat from ``request_counts`` so a long run
        shows progress, and raises ``TimeoutError`` (naming the batch_id) once
        ``max_wall_clock_s`` elapses — the API expires a batch at 24h, so there
        is no point polling past that.
        """
        start = time.monotonic()
        while True:
            batch = self._client.messages.batches.retrieve(batch_id)
            counts = getattr(batch, "request_counts", None)
            if counts is not None:
                print(
                    f"{batch_id}: "
                    f"{getattr(counts, 'succeeded', 0)} succeeded / "
                    f"{getattr(counts, 'errored', 0)} errored / "
                    f"{getattr(counts, 'processing', 0)} processing",
                    file=sys.stderr,
                )
            if batch.processing_status == "ended":
                return batch
            if time.monotonic() - start >= max_wall_clock_s:
                raise TimeoutError(
                    f"batch {batch_id} did not end within {max_wall_clock_s}s "
                    f"(the Batches API expires batches at 24h)"
                )
            time.sleep(interval_s)

    def collect(self, batch_id: str) -> list[ItemResult]:
        """Iterate results, mapping custom_id -> ItemResult.

        Per-item errored/expired/canceled results are recorded with their
        status and error detail rather than raising, so one bad item never
        loses the rest of the batch.
        """
        out: list[ItemResult] = []
        for entry in self._client.messages.batches.results(batch_id):
            custom_id = entry.custom_id
            result = entry.result
            status = result.type
            if status == "succeeded":
                msg = result.message
                usage = getattr(msg, "usage", None)
                out.append(
                    ItemResult(
                        custom_id=custom_id,
                        status=status,
                        text=_text_from_message(msg),
                        input_tokens=getattr(usage, "input_tokens", 0) or 0,
                        output_tokens=getattr(usage, "output_tokens", 0) or 0,
                        error="",
                    )
                )
            else:
                # errored | canceled | expired — capture whatever detail the
                # SDK attaches without assuming a fixed shape.
                detail = ""
                err = getattr(result, "error", None)
                if err is not None:
                    detail = str(err)
                out.append(
                    ItemResult(
                        custom_id=custom_id,
                        status=status,
                        text="",
                        input_tokens=0,
                        output_tokens=0,
                        error=detail or status,
                    )
                )
        return out

    def _drop_stage(self, stage: str) -> None:
        """Remove a stage's (stale) state record so the next submit resubmits."""
        state = load_state(self._state_path)
        if stage in state:
            del state[stage]
            save_state(self._state_path, state)

    def run(
        self,
        stage: str,
        requests: list[dict[str, Any]],
        fresh: bool = False,
        interval_s: int = POLL_INTERVAL_S,
        adopt: str | None = None,
    ) -> list[ItemResult]:
        """Submit (or resume/adopt), poll to completion, collect — the whole stage.

        Self-heals a stale recorded batch_id: if the API 404s on retrieve
        (anthropic.NotFoundError), the batch no longer exists, so we drop the
        stale state entry, warn loudly, and resubmit a fresh batch once.
        """
        # Local import keeps the SDK out of the import path for pure-logic use.
        import anthropic

        batch_id = self.submit(stage, requests, fresh=fresh, adopt=adopt)
        try:
            self.poll(batch_id, interval_s=interval_s)
        except anthropic.NotFoundError:
            print(
                f"recorded batch {batch_id} no longer exists; resubmitting",
                file=sys.stderr,
            )
            self._drop_stage(stage)
            batch_id = self.submit(stage, requests, fresh=True)
            self.poll(batch_id, interval_s=interval_s)
        return self.collect(batch_id)


def iter_custom_ids(requests: Iterable[dict[str, Any]]) -> list[str]:
    """Convenience: the custom_ids in a request set (preserves order)."""
    return [r["custom_id"] for r in requests]
