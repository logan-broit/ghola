"""Thin wrapper over the anthropic Messages Batches API.

Both the reader and the judge submit one batch keyed by custom_id=question_id,
poll until the batch ends, then collect per-item results tolerantly (an
errored/expired item is recorded, not allowed to abort the whole collection).

Model surface is fixed by the plan: claude-opus-4-8 with adaptive thinking and
NO sampling parameters (temperature/top_p/top_k 400 on Opus 4.8). Adaptive
thinking tokens count toward output, so the reader gets a generous max_tokens.

A small JSON state file records, per stage, the submitted batch_id plus a
fingerprint of the request set, so re-running an interrupted job resumes by
polling the in-flight batch instead of resubmitting (paying for it twice).
"""

from __future__ import annotations

import hashlib
import json
import time
from dataclasses import dataclass
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
    """Stable fingerprint of a request set: the sorted custom_ids.

    Resume should reuse an in-flight batch only when it covers the same
    questions. We key on the custom_id set (not the full bodies) so a benign
    re-render of identical questions still resumes; a changed question set
    forces a fresh submission via the mismatch check in submit().
    """
    ids = sorted(r["custom_id"] for r in requests)
    return hashlib.sha256("\n".join(ids).encode()).hexdigest()[:16]


# --- state file -------------------------------------------------------------


def load_state(path: Path) -> dict[str, Any]:
    if path.exists():
        return json.loads(path.read_text())
    return {}


def save_state(path: Path, state: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(state, indent=2))


# --- the batch driver -------------------------------------------------------


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

    def submit(
        self,
        stage: str,
        requests: list[dict[str, Any]],
        fresh: bool = False,
    ) -> str:
        """Submit (or resume) a batch for ``stage``; returns the batch_id.

        Resumes an in-flight batch when the state file records a batch_id for
        this stage with a matching request fingerprint and ``fresh`` is False.
        A fingerprint mismatch (different question set) always resubmits.
        """
        state = load_state(self._state_path)
        fp = fingerprint(requests)
        prior = state.get(stage)
        if not fresh and prior and prior.get("fingerprint") == fp:
            return prior["batch_id"]

        batch = self._client.messages.batches.create(requests=requests)
        state[stage] = {"batch_id": batch.id, "fingerprint": fp}
        save_state(self._state_path, state)
        return batch.id

    def poll(self, batch_id: str, interval_s: int = POLL_INTERVAL_S) -> Any:
        """Block until the batch's processing_status is 'ended'; return it."""
        while True:
            batch = self._client.messages.batches.retrieve(batch_id)
            if batch.processing_status == "ended":
                return batch
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

    def run(
        self,
        stage: str,
        requests: list[dict[str, Any]],
        fresh: bool = False,
        interval_s: int = POLL_INTERVAL_S,
    ) -> list[ItemResult]:
        """Submit (or resume), poll to completion, collect. The whole stage."""
        batch_id = self.submit(stage, requests, fresh=fresh)
        self.poll(batch_id, interval_s=interval_s)
        return self.collect(batch_id)


def iter_custom_ids(requests: Iterable[dict[str, Any]]) -> list[str]:
    """Convenience: the custom_ids in a request set (preserves order)."""
    return [r["custom_id"] for r in requests]
