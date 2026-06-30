"""Local LM token-logprob client for the perplexity_prune compressor.

The "small local oracle" tier: a small instruction LM (e.g. Qwen2.5-1.5B) served
by vLLM's OpenAI-compatible ``/v1/completions`` endpoint, queried with
``echo=True, max_tokens=0, prompt_logprobs=1`` to get per-token logprobs over the
prompt itself. The compressor turns those logprobs into surprisal and keeps the
highest-surprisal (most informative) turns -- the LM-based twin of
``statistical_prune``'s free IDF self-information.

Two pieces, mirroring scorer.py:

  - ``parse_prompt_logprobs`` -- a PURE function over vLLM's response dict, unit
    tested without any network.
  - ``LocalLMClient`` -- a thin urllib client that reuses scorer.py's
    ``_post_json`` (loud RuntimeError on non-2xx/bad-JSON; no silent failure).

Defaults point at a local vLLM server; the sweep overrides via env or kwargs.
Importing this module never needs a live server -- the request only happens on a
``token_logprobs`` call.
"""

from __future__ import annotations

import os

from .scorer import _post_json

# Defaults match a local vLLM completions server; the sweep overrides via env.
DEFAULT_ORACLE_URL = os.environ.get("ORACLE_URL", "http://localhost:8000")
DEFAULT_ORACLE_MODEL = os.environ.get("ORACLE_MODEL", "Qwen2.5-1.5B-Instruct")


def parse_prompt_logprobs(resp: dict) -> list[tuple[str, float]]:
    """Extract ``[(decoded_token, logprob), ...]`` from a vLLM completions
    response requested with ``prompt_logprobs``.

    ``resp["choices"][0]["prompt_logprobs"]`` is a list aligned to the prompt
    tokens. Element 0 is ``null`` (no logprob for the first token -- nothing
    precedes it). Each non-null element is a dict mapping token-id-string ->
    ``{"logprob": float, "decoded_token": str, "rank": int}``, holding the
    candidate entries the server returned for that position.

    For each non-null position, pick the entry with ``rank == 1`` if present
    (the token the model actually scored at that position), else fall back to
    the max-logprob entry. Emit ``(decoded_token, logprob)`` in prompt order.
    Null positions are skipped.
    """
    choices = resp.get("choices") or []
    if not choices:
        return []
    prompt_logprobs = choices[0].get("prompt_logprobs") or []

    out: list[tuple[str, float]] = []
    for entry in prompt_logprobs:
        if entry is None:
            # Position 0 (and any null position): no logprob to emit.
            continue
        # entry: {token_id_str: {"logprob", "decoded_token", "rank"}, ...}
        candidates = list(entry.values())
        if not candidates:
            continue
        chosen = next(
            (c for c in candidates if c.get("rank") == 1),
            # Fall back to the highest-logprob candidate when no rank==1 entry
            # is present (rank 1 is the realized token, but be defensive).
            max(candidates, key=lambda c: c["logprob"]),
        )
        out.append((chosen["decoded_token"], float(chosen["logprob"])))
    return out


class LocalLMClient:
    """Thin client for vLLM's ``/v1/completions`` prompt-logprob path.

    ``token_logprobs(text)`` POSTs ``text`` as the prompt with ``echo=True,
    max_tokens=0, prompt_logprobs=1`` and returns ``parse_prompt_logprobs`` of
    the response: ``[(decoded_token, logprob), ...]`` over the prompt tokens.
    """

    def __init__(
        self,
        base_url: str = DEFAULT_ORACLE_URL,
        model: str = DEFAULT_ORACLE_MODEL,
        timeout: float = 60.0,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.model = model
        self.timeout = timeout

    def token_logprobs(self, text: str) -> list[tuple[str, float]]:
        body = {
            "model": self.model,
            "prompt": text,
            "max_tokens": 0,
            "echo": True,
            "prompt_logprobs": 1,
        }
        resp = _post_json(self.base_url + "/v1/completions", body, self.timeout)
        return parse_prompt_logprobs(resp)
