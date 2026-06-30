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

from . import context, stats
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


def _score_and_greedy_select(
    sessions: list[context.Session],
    query: str,
    target_tokens: int,
    tokenizer: Tokenizer,
    scorer: Optional[Callable[[str, list[tuple[str, str]]], dict[str, float]]] = None,
) -> set[tuple[int, int]]:
    """Score every turn against the query and greedily keep the highest-
    relevance ones within the budget. Returns the kept set of
    ``(session_index, turn_index)`` pairs.

    Shared core for ``extractive_relevance`` and its expanded variant so the
    scoring + greedy packing logic is not duplicated.
    """
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
        return set()

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
    return kept


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

    kept = _score_and_greedy_select(
        sessions, query, target_tokens, tokenizer, scorer
    )
    if not kept:
        return ""
    text, _ = context.render_sessions(_regroup(sessions, kept))
    return text


def _extractive_relevance_expanded(
    sessions: list[context.Session],
    *,
    query: str,
    target_tokens: Optional[int],
    tokenizer: Optional[Tokenizer],
    scorer: Optional[Callable[[str, list[tuple[str, str]]], dict[str, float]]] = None,
    expansion_reserve: float = 0.3,
) -> str:
    """Extractive selection + one-hop neighbor expansion.

    Two phases:

    1. **Greedy selection** with a *reduced* budget
       (``target_tokens * (1 - expansion_reserve)``). This picks only the
       highest-relevance turns, leaving room for neighbors.

    2. **Neighbor expansion** with the *full* ``target_tokens`` budget.
       For each turn kept by phase 1, try to admit its immediate predecessor
       ``(si, ti-1)`` and successor ``(si, ti+1)`` if they fit. The expansion
       iterates chronologically over the phase-1 kept turns (not the expanded
       set), so it is a single one-hop layer — not a recursive fill.

    Why the budget reserve is necessary: the greedy phase tries every turn
    and keeps every turn that fits. Without a reserve, the greedy phase would
    consume the entire budget with turns ranked by score (including low-
    relevance non-neighbors that happen to be chronologically early), leaving
    no room for the expansion to add anything. The reserve guarantees the
    expansion has budget to work with. ``expansion_reserve`` (default 0.3,
    i.e. 30% of the budget) controls the split; 0 reduces to plain
    ``extractive_relevance``.

    The hypothesis: temporal-reasoning questions need the connective timeline
    between high-relevance turns. Pure extractive selection scores each turn
    in isolation and may drop low-relevance connective turns that establish
    "X happened, then Y, then Z." Neighbor expansion tests whether adding a
    single layer of context around each kept turn recovers that connective
    signal. If the temporal-category dip closes, it was lost connective
    tissue; if not, temporal questions genuinely need the fuller session and
    a different approach (gap-filling within a session) is warranted.

    Out-of-bounds neighbors (first turn's predecessor, last turn's
    successor) are skipped. A turn is never double-admitted (if a neighbor is
    already in the set it is skipped). The expansion never crosses session
    boundaries — neighbors are within-session only.
    """
    if target_tokens is None or tokenizer is None:
        text, _ = context.render_sessions(sessions)
        return text

    if expansion_reserve <= 0:
        # No reserve: degenerates to plain extractive_relevance.
        kept = _score_and_greedy_select(
            sessions, query, target_tokens, tokenizer, scorer
        )
        if not kept:
            return ""
        text, _ = context.render_sessions(_regroup(sessions, kept))
        return text

    # Phase 1: greedy selection with a reduced budget so phase 2 has room.
    greedy_budget = max(1, int(target_tokens * (1 - expansion_reserve)))
    kept = _score_and_greedy_select(
        sessions, query, greedy_budget, tokenizer, scorer
    )
    if not kept:
        return ""

    # Phase 2: neighbor expansion against the full target_tokens budget.
    # Iterate chronologically over the phase-1 kept turns (snapshot before the
    # loop so newly admitted neighbors do NOT trigger further expansion —
    # single-hop, not recursive fill).
    #
    # Admission policy: chronological-first. We iterate sorted(kept) and
    # admit neighbors first-come-first-served against the budget. This is
    # defensible (even temporal coverage across the timeline) but it's an
    # unexamined choice — under a tight budget, the earliest kept turn's
    # neighbors win over a later, higher-relevance anchor's neighbors. An
    # alternative (expand around highest-relevance anchors first) would
    # re-sort kept by descending score; try it as a variant if the result is
    # murky.
    #
    # Confound to keep in mind: a turn rejected by phase 1 (didn't fit the
    # reduced budget) can re-enter here as a neighbor, now against the full
    # budget. This is desirable (relevant + connective), but it means a
    # positive result is partly "extra relevance budget," not purely
    # "connective tissue." Compare against extractive_relevance at the SAME
    # total budget, not just the phase-1 budget, to separate the effects.
    expanded = set(kept)
    for si, ti in sorted(kept):
        for neighbor_ti in (ti - 1, ti + 1):
            # Bounds check: skip out-of-range turns within the same session.
            if neighbor_ti < 0 or neighbor_ti >= len(sessions[si].turns):
                continue
            key = (si, neighbor_ti)
            if key in expanded:
                continue  # already kept or already expanded
            candidate = expanded | {key}
            text, _ = context.render_sessions(_regroup(sessions, candidate))
            if tokenizer.count(text) > target_tokens:
                continue  # doesn't fit the remaining budget; try next neighbor
            expanded = candidate

    text, _ = context.render_sessions(_regroup(sessions, expanded))
    return text


class _SelfInfoScorer:
    """Scorer with the ``scorer(query, items) -> {id: float}`` shape that ranks
    items by IDF self-information -- the "surprise" of their tokens against the
    item corpus -- with no model and no network.

    Per-item score is the MEAN self-information of its tokens (summed IDF divided
    by token count), so a long turn does not out-score a short distinctive one
    merely by accumulating more tokens. ``query_mode="aware"`` adds the item's
    BM25 relevance to the query on top of the surprise signal; ``"agnostic"``
    (the default) ignores the query entirely.
    """

    def __init__(self, query_mode: str = "agnostic") -> None:
        self.query_mode = query_mode
        self._bm25 = stats.BM25Scorer() if query_mode == "aware" else None

    def __call__(
        self, query: str, items: list[tuple[str, str]]
    ) -> dict[str, float]:
        if not items:
            return {}
        idf = stats.idf_map([text for _, text in items])
        bm25 = self._bm25(query, items) if self._bm25 is not None else {}
        out: dict[str, float] = {}
        for sid, text in items:
            n_tokens = len(stats.tokenize_words(text))
            mean_info = stats.self_information(text, idf) / max(1, n_tokens)
            out[sid] = mean_info + bm25.get(sid, 0.0)
        return out


def _statistical_prune(
    sessions: list[context.Session],
    *,
    query: str,
    target_tokens: Optional[int],
    tokenizer: Optional[Tokenizer],
    query_mode: str = "agnostic",
) -> str:
    """Keep the highest-surprise turns within the budget -- IDF self-information
    pruning with no model.

    The free, no-model cousin of ``extractive_relevance``: same greedy-select +
    chronological-regroup core, but the scorer is ``_SelfInfoScorer`` (mean IDF
    surprise, optionally plus BM25 query relevance) instead of a neural reranker.
    ``query_mode="agnostic"`` ranks purely by token rarity; ``"aware"`` folds in
    BM25 relevance to the query.
    """
    if target_tokens is None or tokenizer is None:
        text, _ = context.render_sessions(sessions)
        return text

    kept = _score_and_greedy_select(
        sessions, query, target_tokens, tokenizer, _SelfInfoScorer(query_mode)
    )
    if not kept:
        return ""
    text, _ = context.render_sessions(_regroup(sessions, kept))
    return text


def _lexical_relevance(
    sessions: list[context.Session],
    *,
    query: str,
    target_tokens: Optional[int],
    tokenizer: Optional[Tokenizer],
    **_: object,
) -> str:
    """Keep the turns most BM25-relevant to the query within the budget -- the
    no-reranker twin of ``extractive_relevance``.

    Same greedy-select + chronological-regroup core, but the scorer is
    ``stats.BM25Scorer()`` (Okapi BM25 over the turn corpus) instead of a neural
    reranker. Query-aware by nature: a turn scores zero if it shares no terms
    with the query, so a tight budget keeps the term-overlapping turns.
    """
    if target_tokens is None or tokenizer is None:
        text, _ = context.render_sessions(sessions)
        return text

    kept = _score_and_greedy_select(
        sessions, query, target_tokens, tokenizer, stats.BM25Scorer()
    )
    if not kept:
        return ""
    text, _ = context.render_sessions(_regroup(sessions, kept))
    return text


def _graph_community(
    sessions: list[context.Session],
    *,
    query: str,
    target_tokens: Optional[int],
    tokenizer: Optional[Tokenizer],
    **kwargs: object,
) -> str:
    """Thin wrapper delegating to ``graph_compress.graph_community``.

    The lazy import keeps ``compress.py`` importable without the optional
    ``[graph]`` extra (igraph/leidenalg): only this call path -- reached when the
    ``graph_community`` compressor actually runs -- can need them, and even then
    the pure ``build_graph`` / ``compress_with_partition`` helpers don't (only
    ``leiden_partition`` does, and it raises a clear error when they're absent).
    """
    from . import graph_compress

    return graph_compress.graph_community(
        sessions,
        query=query,
        target_tokens=target_tokens,
        tokenizer=tokenizer,
        **kwargs,
    )


# name -> compressor. compress() dispatches here; KeyError on unknown name.
REGISTRY: dict[str, Callable[..., str]] = {
    "full": _full,
    "truncate_tokens": _truncate_tokens,
    "topk_sessions": _topk_sessions,
    "extractive_relevance": _extractive_relevance,
    "extractive_relevance_expanded": _extractive_relevance_expanded,
    "statistical_prune": _statistical_prune,
    "lexical_relevance": _lexical_relevance,
    "graph_community": _graph_community,
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
