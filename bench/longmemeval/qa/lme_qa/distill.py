"""Claude distiller + disk cache: the most expensive rate-distortion tier.

The ``llm_distill`` compressor (see ``compress.py``) renders the retrieved
sessions, then asks ``claude -p`` to distill them down to a token budget and
returns the distilled text. Because that is ONE claude call per question — the
priciest compressor in the sweep — every distillation is disk-cached: a re-run
after a subscription usage-window stop reuses cached outputs for free and only
pays for the questions that did not land before the window closed.

Two orthogonal knobs, both swept later:

* ``output_form``: ``"prose"`` (a compressed prose summary) or ``"structured"``
  (a JSON object of atomic facts, rendered to a ``- fact`` bullet list).
* ``query_mode``: ``"agnostic"`` (the question is hidden — distill the context
  on its own merits) or ``"aware"`` (the question is shown so the distiller can
  bias toward answer-relevant content).

The default ``call`` REUSES ``cc.py``'s per-call primitive (``CCRunner._run_one``
over a one-shot ``CCRequest``) so the isolation flag set, JSON parsing, and the
usage-limit detection are NOT reinvented here. The cc.py import is deferred into
``_default_call`` so importing this module (and ``compress.py``) never needs the
claude runtime — pure-logic tests inject a fake ``call`` at the seam.
"""

from __future__ import annotations

import hashlib
import json
import os
from collections.abc import Callable
from pathlib import Path

# A system prompt for the distiller call. Empty-ish but deterministic: the real
# instruction lives in the user prompt (build_prompt). cc.py requires a system
# string; this keeps the distiller's behavior independent of the operator's
# default system prompt.
_DISTILL_SYSTEM = (
    "You are a context distiller. Follow the user's instruction exactly and "
    "output only the distilled result with no preamble."
)


class Cache:
    """One-file-per-key disk cache for distilled outputs.

    The key is a hex sha256 (filesystem-safe), so the cache file name is the key
    itself. The directory is created lazily on the first ``put``. ``get`` returns
    ``None`` on a miss (no file). Values are UTF-8 text.
    """

    def __init__(self, root: Path) -> None:
        self.root = Path(root).expanduser()

    @staticmethod
    def key_for(
        *,
        compressor: str,
        query_mode: str,
        output_form: str,
        budget: object,
        context_text: str,
    ) -> str:
        """Stable cache key over EVERY dimension that changes the distillation.

        Any difference in compressor name, query mode, output form, token budget,
        or the rendered context produces a different key — so a cache hit is only
        ever returned for an identical request.
        """
        parts = [
            compressor,
            query_mode,
            output_form,
            str(budget),
            context_text,
        ]
        return hashlib.sha256("|".join(parts).encode()).hexdigest()

    def get(self, key: str) -> str | None:
        path = self.root / key
        try:
            return path.read_text(encoding="utf-8")
        except (FileNotFoundError, NotADirectoryError):
            return None

    def put(self, key: str, value: str) -> None:
        self.root.mkdir(parents=True, exist_ok=True)
        (self.root / key).write_text(value, encoding="utf-8")


def _default_cache() -> Cache:
    """Cache rooted at ``$LME_DISTILL_CACHE`` (expanded) or the XDG-ish default
    ``~/.cache/lme-qa-distill``. Constructed lazily so the env var is read at use
    time, not import time."""
    root = os.environ.get("LME_DISTILL_CACHE", "~/.cache/lme-qa-distill")
    return Cache(Path(root).expanduser())


def build_prompt(
    context_text: str,
    *,
    query: str,
    query_mode: str,
    output_form: str,
    budget: int,
) -> str:
    """Build the deterministic distillation instruction for ``claude -p``.

    * ``agnostic`` does NOT include the question — the distiller compresses the
      context on its own merits (no answer leakage into the distilled text).
    * ``aware`` includes the question and biases toward answer-relevant content.
    * ``prose`` asks for a compressed prose summary preserving names, dates,
      numbers, and temporal order.
    * ``structured`` asks for a JSON object ``{"facts": ["...", ...]}`` of atomic
      facts, no prose (rendered to a bullet list by ``render_structured``).

    Kept short and deterministic so the cache key (which hashes the context, not
    the prompt) and the distilled output are reproducible.
    """
    if output_form == "structured":
        form_instruction = (
            'Output a single JSON object {"facts": ["...", ...]} of atomic '
            "facts and nothing else. No prose, no markdown, no preamble — just "
            "the JSON object."
        )
    else:
        form_instruction = (
            "Output a compressed prose summary preserving names, dates, numbers, "
            "and temporal order. No preamble — just the summary."
        )

    lines = [
        f"Distill the following context to at most {budget} tokens.",
    ]
    if query_mode == "aware":
        lines.append(
            "Bias toward content relevant to this question (do not answer it, "
            f"just keep what helps answer it): {query}"
        )
    lines.append(form_instruction)
    lines.append("")
    lines.append("CONTEXT:")
    lines.append(context_text)
    return "\n".join(lines)


def render_structured(raw: str) -> str:
    """Render a structured distillation response to a ``- fact`` bullet list.

    Parses ``raw`` as JSON and joins ``obj["facts"]`` (a list of strings) into
    ``"- f1\\n- f2\\n..."``. On ANY parse failure or wrong shape (not an object,
    no ``facts`` key, ``facts`` not a list of strings) it returns
    ``raw.strip()`` unchanged — treating the response as prose. It NEVER raises:
    the reader loop must survive a malformed distillation.
    """
    try:
        obj = json.loads(raw)
    except (json.JSONDecodeError, TypeError, ValueError):
        return raw.strip()
    if not isinstance(obj, dict):
        return raw.strip()
    facts = obj.get("facts")
    if not isinstance(facts, list) or not all(isinstance(f, str) for f in facts):
        return raw.strip()
    return "\n".join(f"- {f}" for f in facts)


def _default_call() -> Callable[[str], str]:
    """The production ``call``: wrap ``cc.py``'s per-call primitive.

    REUSES ``CCRunner._run_one`` over a one-shot ``CCRequest`` so the isolation
    flag set (``--tools "" --strict-mcp-config --no-session-persistence
    --output-format json``), the JSON parsing, and the usage-limit detection are
    all inherited rather than reinvented. A usage-window exhaustion surfaces as
    the same ``UsageLimitExhausted`` cc.py raises; a per-call error surfaces as a
    ``RuntimeError`` carrying the cc error detail (so a malformed/failed
    distillation does not silently cache an empty string).

    Imported lazily so importing ``distill.py`` (and ``compress.py``) never pulls
    in the claude runtime.
    """
    from .cc import CCRequest, CCRunner

    runner = CCRunner()

    def call(prompt: str) -> str:
        # _run_one raises UsageLimitExhausted on a usage-window stop (let it
        # propagate so the caller persists progress and re-runs); otherwise it
        # returns a CCResult. A non-succeeded result is a hard error here — we
        # must not cache an empty distillation as if it were valid.
        res = runner._run_one(CCRequest(custom_id="distill", system=_DISTILL_SYSTEM, prompt=prompt))
        if res.status != "succeeded":
            raise RuntimeError(f"distiller claude call failed: {res.error}")
        return res.text

    return call


class Distiller:
    """Distill rendered context via a ``call(prompt)->str`` callable, with an
    optional disk cache.

    ``call`` is injected at construction (tests pass a deterministic fake); when
    omitted it defaults to the cc.py-backed production call (lazy, so importing
    this module needs no claude runtime). ``cache`` defaults to the
    ``$LME_DISTILL_CACHE`` / ``~/.cache/lme-qa-distill`` disk cache; pass
    ``cache=None`` to disable caching entirely.
    """

    def __init__(
        self,
        call: Callable[[str], str] | None = None,
        cache: Cache | None = ...,  # type: ignore[assignment]
    ) -> None:
        self.call = call if call is not None else _default_call()
        # Sentinel ``...`` distinguishes "not passed" (use the default disk
        # cache) from an explicit ``cache=None`` (disable caching).
        self.cache = _default_cache() if cache is ... else cache

    def distill(
        self,
        context_text: str,
        *,
        query: str,
        query_mode: str,
        output_form: str,
        budget: int,
    ) -> str:
        """Distill ``context_text`` to ``budget`` tokens; cache the result.

        On a cache hit the stored (already-rendered) text is returned without a
        claude call. On a miss: build the prompt, call the distiller, render a
        structured response to a bullet list (prose is returned stripped),
        cache, and return.
        """
        key = Cache.key_for(
            compressor="llm_distill",
            query_mode=query_mode,
            output_form=output_form,
            budget=budget,
            context_text=context_text,
        )
        if self.cache is not None:
            hit = self.cache.get(key)
            if hit is not None:
                return hit

        prompt = build_prompt(
            context_text,
            query=query,
            query_mode=query_mode,
            output_form=output_form,
            budget=budget,
        )
        raw = self.call(prompt)
        text = render_structured(raw) if output_form == "structured" else raw.strip()

        if self.cache is not None:
            self.cache.put(key, text)
        return text
