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


def _regroup(
    sessions: list[context.Session],
    kept_turns: set[tuple[int, int]],
) -> list[context.Session]:
    """Rebuild a session list containing only the kept turns, preserving the
    original chronological session order and within-session turn order.

    ``kept_turns`` is a set of ``(session_index, turn_index)`` into ``sessions``.
    Sessions with no kept turn are dropped entirely (no empty date header). A
    turn is the atomic unit -- it is kept or dropped whole, never split.
    """
    out: list[context.Session] = []
    for si, s in enumerate(sessions):
        turns = [t for ti, t in enumerate(s.turns) if (si, ti) in kept_turns]
        if turns:
            out.append(context.Session(s.session_id, s.date, turns))
    return out


def _extractive_relevance(
    sessions: list[context.Session],
    *,
    query: str,
    target_tokens: Optional[int],
    tokenizer: Optional[Tokenizer],
    scorer: Optional[Callable[[str, list[tuple[str, str]]], dict[str, float]]] = None,
) -> str:
    """Keep the turns most relevant to the query within the budget.

    Flatten sessions to individual turns, score each turn's text against the
    query via the injected ``scorer`` (default: the truthsayer reranker), then
    greedily admit turns in descending score order, accumulating the rendered
    token cost until the next turn would exceed the budget. Kept turns are
    regrouped under their sessions in the original chronological order before
    rendering -- the selection is relevance-ranked but the render stays
    timeline-ordered (matters for temporal-reasoning questions).

    Distinct from topk_sessions (whole-session granularity) and truncate_tokens
    (relevance-blind byte cut): this is per-turn, relevance-aware selection.
    """
    if target_tokens is None or tokenizer is None:
        text, _ = context.render_sessions(sessions)
        return text

    # Flatten to ((session_index, turn_index), text). The turn id encodes its
    # position so we can regroup in original order regardless of score order.
    # The relevance signal is the turn's raw content, not the rendered
    # "ROLE: content" line -- the ROLE prefix is presentation, and the reranker
    # should score what was said, not the speaker label.
    flat: list[tuple[tuple[int, int], str]] = []
    for si, s in enumerate(sessions):
        for ti, turn in enumerate(s.turns):
            flat.append(((si, ti), str(turn.get("content", ""))))

    if not flat:
        return ""

    # urllib/JSON ids must be strings; map back after scoring.
    str_id = {f"{si}_{ti}": (si, ti) for (si, ti), _ in flat}
    items = [(f"{si}_{ti}", text) for (si, ti), text in flat]

    if scorer is None:
        # Deferred import: keep the pure-function compressors importable without
        # the scorer's urllib stack, and let tests inject a fake scorer.
        from .scorer import make_scorer

        scorer = make_scorer("truthsayer")

    raw_scores = scorer(query, items)
    # Greedy by descending relevance; ties broken by chronological order
    # (session_index, turn_index) so the result is deterministic.
    ordered = sorted(
        str_id.values(),
        key=lambda key: (-raw_scores.get(f"{key[0]}_{key[1]}", 0.0), key),
    )

    kept: set[tuple[int, int]] = set()
    for key in ordered:
        candidate = kept | {key}
        text, _ = context.render_sessions(_regroup(sessions, candidate))
        if tokenizer.count(text) > target_tokens:
            # Skip this turn but keep trying lower-ranked turns: a smaller turn
            # may still fit even when a larger higher-ranked one did not.
            continue
        kept = candidate

    text, _ = context.render_sessions(_regroup(sessions, kept))
    return text


# name -> compressor. compress() dispatches here; KeyError on unknown name.
REGISTRY: dict[str, Callable[..., str]] = {
    "full": _full,
    "truncate_tokens": _truncate_tokens,
    "topk_sessions": _topk_sessions,
    "extractive_relevance": _extractive_relevance,
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
