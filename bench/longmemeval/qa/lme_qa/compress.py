"""Rate-distortion compressors: transform selected sessions down to a token
budget before the reader prompt is built.

A compressor sits between ``build_context``'s session selection and the rendered
reader text. It takes the chronologically-sorted ``context.Session`` list plus
the query and a target token budget, and returns rendered text in the SAME
format ``build_context`` produces (via the shared ``context.render_sessions``).

The budget is approximate by design (see ``tokenize``): it is only the control
knob. The plotted rate is the reader's real ``usage.input_tokens``.
"""

from __future__ import annotations

from typing import Callable, Optional

from . import context
from .tokenize import Tokenizer


def _full(
    sessions: list[context.Session],
    *,
    query: str,
    target_tokens: Optional[int],
    tokenizer: Optional[Tokenizer],
) -> str:
    """Identity: render every session, ignore the budget. Right edge of the
    curve (the current production behavior)."""
    text, _ = context.render_sessions(sessions)
    return text


def _truncate_tokens(
    sessions: list[context.Session],
    *,
    query: str,
    target_tokens: Optional[int],
    tokenizer: Optional[Tokenizer],
) -> str:
    """Render all sessions, then hard-cap the joined text at the budget. The
    relevance-blind strawman: cuts mid-session at the byte boundary."""
    text, _ = context.render_sessions(sessions)
    if target_tokens is None or tokenizer is None:
        return text
    return tokenizer.truncate(text, target_tokens)


def _topk_sessions(
    sessions: list[context.Session],
    *,
    query: str,
    target_tokens: Optional[int],
    tokenizer: Optional[Tokenizer],
) -> str:
    """Keep whole sessions in chronological order until the next would exceed
    the budget; never splits a session (distinct from truncate_tokens, which
    cuts mid-session). Coarse session-granular relevance via the existing K
    knob applied as a token budget."""
    if target_tokens is None or tokenizer is None:
        text, _ = context.render_sessions(sessions)
        return text

    # Accumulate by re-rendering the growing kept prefix so the result is
    # byte-identical to render_sessions(kept): the "\n\n" join cost between
    # sessions is counted, not just per-session body lengths.
    kept: list[context.Session] = []
    for s in sessions:
        candidate, _ = context.render_sessions(kept + [s])
        if tokenizer.count(candidate) > target_tokens:
            break
        kept.append(s)

    text, _ = context.render_sessions(kept)
    return text


# name -> compressor. compress() dispatches here; KeyError on unknown name.
REGISTRY: dict[str, Callable[..., str]] = {
    "full": _full,
    "truncate_tokens": _truncate_tokens,
    "topk_sessions": _topk_sessions,
}


def compress(
    name: str,
    sessions: list[context.Session],
    *,
    query: str,
    target_tokens: Optional[int],
    tokenizer: Optional[Tokenizer],
    **kwargs: object,
) -> str:
    """Dispatch to the named compressor. Raises ``KeyError`` on an unknown name
    so a typo in a sweep settings file fails loudly rather than silently
    falling back to ``full``."""
    try:
        fn = REGISTRY[name]
    except KeyError:
        # Name the bad compressor and list the known ones so a settings typo is
        # diagnosable from the traceback alone.
        raise KeyError(
            f"unknown compressor {name!r}; known: {sorted(REGISTRY)}"
        ) from None
    return fn(
        sessions,
        query=query,
        target_tokens=target_tokens,
        tokenizer=tokenizer,
        **kwargs,
    )
