"""Local LM token-logprob client for the perplexity_prune compressor.

The "small local oracle" tier: a small instruction LM (e.g. Qwen2.5-1.5B) served
by vLLM's OpenAI-compatible ``/v1/completions`` endpoint, queried on the echo
path (``echo=True, max_tokens=0, logprobs=0``) to get the per-token logprobs of
the ACTUAL prompt tokens. The compressor turns those logprobs into surprisal and
keeps the highest-surprisal (most informative) turns -- the LM-based twin of
``statistical_prune``'s free IDF self-information.

Why the echo path and not ``prompt_logprobs``: vLLM's ``prompt_logprobs`` returns
the top candidate(s) per position, and the ``rank == 1`` entry is the model's
ARGMAX token -- not necessarily the actual prompt token. Reading rank==1 measures
the model's confidence in *its own* prediction, not the surprisal of the actual
text (on a real prompt these diverged at ~6/14 positions: argmax ' following'
-0.34 vs actual ' capital' -11.13). The standard OpenAI echo logprobs
(``choices[0].logprobs.token_logprobs``) are the actual prompt-token logprobs by
construction, so surprisal of the real text falls out directly with no per-entry
rank heuristic. This matches the directional-cross-entropy pilot, which verified
the echo logprobs against the same oracle.

Two pieces, mirroring scorer.py:

  - ``parse_echo_logprobs`` -- a PURE function over vLLM's response dict, unit
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


def parse_echo_logprobs(resp: dict) -> list[tuple[str, float]]:
    """Extract ``[(token, logprob), ...]`` of the ACTUAL prompt tokens from a
    vLLM completions response requested on the echo path (``echo=True,
    max_tokens=0, logprobs=0``).

    ``resp["choices"][0]["logprobs"]`` is the standard OpenAI logprobs object:

      - ``tokens``          -- the prompt's decoded tokens, in order.
      - ``token_logprobs``  -- each token's logprob under the model, aligned to
        ``tokens``. Element 0 is ``null`` (no logprob for the first token --
        nothing precedes it).

    These are the actual prompt tokens' logprobs, so the surprisal of the real
    text (``-logprob``) falls out directly -- no argmax/rank disambiguation.
    Emit ``(token, logprob)`` in prompt order, skipping null positions.
    """
    choices = resp.get("choices") or []
    if not choices:
        return []
    logprobs = choices[0].get("logprobs") or {}
    tokens = logprobs.get("tokens") or []
    token_logprobs = logprobs.get("token_logprobs") or []

    out: list[tuple[str, float]] = []
    for token, logprob in zip(tokens, token_logprobs):
        if logprob is None:
            # Position 0 (and any null position): no logprob to emit.
            continue
        out.append((token, float(logprob)))
    return out


class LocalLMClient:
    """Thin client for vLLM's ``/v1/completions`` prompt-logprob path.

    ``token_logprobs(text)`` POSTs ``text`` as the prompt on the echo path
    (``echo=True, max_tokens=0, logprobs=0``) and returns
    ``parse_echo_logprobs`` of the response: ``[(token, logprob), ...]`` over the
    ACTUAL prompt tokens.
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
        if not text:
            # vLLM rejects an empty prompt (HTTP 400 "Either prompt or
            # prompt_embeds must be provided and non-empty"), and LongMemEval-S
            # has exactly-empty turn contents. No tokens -> no logprobs: [] is
            # the same value a whitespace-only prompt yields via parse (single
            # token with a null logprob), so the scorer's `if not lp` guard
            # handles both identically (surprisal 0.0).
            return []
        body = {
            "model": self.model,
            "prompt": text,
            "max_tokens": 0,
            "echo": True,
            # Echo path: choices[0].logprobs.token_logprobs = ACTUAL prompt-token
            # logprobs (not prompt_logprobs' rank==1 argmax). See module docstring.
            "logprobs": 0,
        }
        resp = _post_json(self.base_url + "/v1/completions", body, self.timeout)
        return parse_echo_logprobs(resp)
