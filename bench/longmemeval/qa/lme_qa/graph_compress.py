"""Lexical-graph + Leiden community-coverage compressor.

The hypothesis: a token budget spent to *cover the distinct topics* in the
retrieved sessions retains more answerable signal than one spent on the single
highest-scoring thread. Build a lexical similarity graph over turns (Jaccard on
word-token sets), detect communities (Leiden modularity), then admit one
representative turn per community round-robin until the budget is spent -- so
every topic gets a seat before any topic gets a second.

Layering:

  - ``build_graph`` and ``compress_with_partition`` are PURE (stdlib + the local
    ``stats``/``context`` modules only). They are unit-tested directly without
    the optional graph dependency.
  - ``leiden_partition`` is the only function that needs igraph/leidenalg. It
    lazy-imports them and raises a clear ``[graph]``-extra error when they are
    absent, so importing this module never requires the extra.
  - ``graph_community`` is the registered compressor: it wires the three
    together and short-circuits the no-budget / no-tokenizer case before any
    graph is built.
"""

from __future__ import annotations

import importlib.util
from typing import Optional

from . import context, stats
from .tokenize import Tokenizer


def build_graph(
    texts: list[str],
    edge_metric: str = "jaccard",
    threshold: float = 0.1,
) -> tuple[int, list[tuple[int, int]], list[float]]:
    """Build a lexical similarity graph over ``texts``. PURE.

    Nodes are the indices ``0..len(texts)-1``. An undirected edge ``(i, j)``
    (with ``i < j``) exists when the similarity of the two texts' word-token
    sets is ``>= threshold``; the edge weight is that similarity.

    Only ``edge_metric="jaccard"`` is implemented (Jaccard of the token sets);
    the parameter is accepted now so future metrics (e.g. cosine over TF) slot
    in without a signature change. An unknown metric raises ``ValueError``.

    Returns ``(n_nodes, edges, weights)`` with ``edges[k]`` weighted by
    ``weights[k]`` -- two parallel lists rather than a dict so the order is
    stable and the result feeds ``leiden_partition`` directly.
    """
    if edge_metric != "jaccard":
        raise ValueError(
            f"unknown edge_metric {edge_metric!r}; only 'jaccard' is implemented"
        )

    n = len(texts)
    # Precompute each node's token SET once (Jaccard is set-based).
    token_sets = [set(stats.tokenize_words(t)) for t in texts]

    edges: list[tuple[int, int]] = []
    weights: list[float] = []
    for i in range(n):
        a = token_sets[i]
        if not a:
            continue
        for j in range(i + 1, n):
            b = token_sets[j]
            if not b:
                continue
            union = a | b
            if not union:
                continue
            jaccard = len(a & b) / len(union)
            if jaccard >= threshold:
                edges.append((i, j))
                weights.append(jaccard)
    return n, edges, weights


def leiden_partition(
    n: int,
    edges: list[tuple[int, int]],
    weights: list[float],
    seed: int = 0,
) -> list[list[int]]:
    """Partition the graph into communities via Leiden modularity.

    Lazy-imports ``igraph`` and ``leidenalg`` (the optional ``[graph]`` extra)
    INSIDE this function so importing the module never requires them. If either
    is missing, raises ``RuntimeError`` naming the extra to install.

    Returns a list of communities, each a list of node indices into the original
    ``texts``. Isolated nodes (no edges) form singleton communities of their own.
    """
    if (
        importlib.util.find_spec("igraph") is None
        or importlib.util.find_spec("leidenalg") is None
    ):
        raise RuntimeError(
            "graph_community needs the [graph] extra: pip install 'lme-qa[graph]'"
        )

    import igraph
    import leidenalg

    g = igraph.Graph(n=n, edges=list(edges))
    partition = leidenalg.find_partition(
        g,
        leidenalg.ModularityVertexPartition,
        weights=list(weights),
        seed=seed,
    )
    return [list(community) for community in partition]


def _flatten_turns(
    sessions: list[context.Session],
) -> list[tuple[tuple[int, int], str]]:
    """Flatten sessions to a chronological turn list of ``((si, ti), text)`` --
    the SAME order ``build_graph`` saw (node index == position in this list)."""
    flat: list[tuple[tuple[int, int], str]] = []
    for si, s in enumerate(sessions):
        for ti, turn in enumerate(s.turns):
            flat.append(((si, ti), str(turn.get("content", ""))))
    return flat


def compress_with_partition(
    sessions: list[context.Session],
    partition: list[list[int]],
    target_tokens: Optional[int],
    tokenizer: Optional[Tokenizer],
    *,
    query: Optional[str] = None,
    query_mode: str = "agnostic",
) -> str:
    """Admit one representative turn per community round-robin within the budget.
    PURE (no leidenalg).

    ``partition`` is a list of communities, each a list of node indices into the
    flattened turn list (the indices ``build_graph`` saw). Community ordering:

      - ``query_mode="agnostic"``: by size descending (biggest topics first).
      - ``query_mode="aware"``: by summed BM25 of the community's member turns
        against ``query``, descending (most query-relevant topics first).

    Round-robin over the ordered communities: each round, each community offers
    its best not-yet-admitted representative (its longest member turn first);
    the turn is admitted if the kept set still fits the budget after adding it.
    Continue until the budget is exhausted or every turn has been admitted.
    Kept turns are regrouped chronologically and rendered -- selection is
    coverage-ordered but the render stays timeline-ordered.
    """
    flat = _flatten_turns(sessions)
    if target_tokens is None or tokenizer is None:
        text, _ = context.render_sessions(sessions)
        return text

    if not flat:
        return ""

    # idx -> (si, ti) and idx -> text, for nodes referenced by the partition.
    idx_key = {idx: key for idx, (key, _) in enumerate(flat)}
    idx_text = {idx: text for idx, (_, text) in enumerate(flat)}

    # Per-community admission order: longest member first (most-informative
    # representative under a coarse length proxy), ties broken by chronological
    # index for determinism.
    def _member_order(members: list[int]) -> list[int]:
        return sorted(members, key=lambda idx: (-len(idx_text[idx]), idx))

    # Community ordering across the round-robin.
    if query_mode == "aware":
        bm25 = stats.BM25Scorer()
        # Score each turn once; a community's priority is the sum over members.
        # The IDF corpus is the full flattened turn set (every turn scored in one
        # pass), so BM25 scores -- and thus community priorities -- are comparable
        # across communities.
        items = [(str(idx), idx_text[idx]) for idx in idx_text]
        per_turn = bm25(query or "", items)

        def _community_priority(members: list[int]) -> tuple[float, int]:
            total = sum(per_turn.get(str(idx), 0.0) for idx in members)
            # Tie-break by smallest member index so ordering is deterministic.
            return (-total, min(members) if members else 0)
    else:

        def _community_priority(members: list[int]) -> tuple[float, int]:
            # Size descending; tie-break by smallest member index.
            return (-len(members), min(members) if members else 0)

    ordered = sorted(
        (list(c) for c in partition if c), key=_community_priority
    )
    # Per-community queue of candidate reps (best first).
    queues = [_member_order(members) for members in ordered]
    cursors = [0] * len(queues)

    kept: set[tuple[int, int]] = set()
    # Round-robin: keep cycling until no community can offer a fitting rep.
    progress = True
    while progress:
        progress = False
        for ci, queue in enumerate(queues):
            while cursors[ci] < len(queue):
                idx = queue[cursors[ci]]
                cursors[ci] += 1
                key = idx_key[idx]
                if key in kept:
                    continue
                candidate = kept | {key}
                text, _ = context.render_sessions(_regroup(sessions, candidate))
                if tokenizer.count(text) > target_tokens:
                    # This rep doesn't fit; try this community's next-best on a
                    # later round (leave cursor advanced past it).
                    continue
                kept = candidate
                progress = True
                break  # one rep per community per round (round-robin coverage)

    if not kept:
        return ""
    text, _ = context.render_sessions(_regroup(sessions, kept))
    return text


def _regroup(
    sessions: list[context.Session],
    kept_turns: set[tuple[int, int]],
) -> list[context.Session]:
    """Rebuild a session list with only ``kept_turns``, preserving chronological
    session + turn order. Mirrors ``compress._regroup`` (imported here to avoid a
    circular import at module load -- ``compress`` lazy-imports this module)."""
    out: list[context.Session] = []
    for si, s in enumerate(sessions):
        turns = [t for ti, t in enumerate(s.turns) if (si, ti) in kept_turns]
        if turns:
            out.append(context.Session(s.session_id, s.date, turns))
    return out


def graph_community(
    sessions: list[context.Session],
    *,
    query: str,
    target_tokens: Optional[int],
    tokenizer: Optional[Tokenizer],
    query_mode: str = "agnostic",
    edge_metric: str = "jaccard",
) -> str:
    """Lexical-graph + Leiden community-coverage compressor (registered name
    ``graph_community``).

    Short-circuits to a full render when there is no budget or no tokenizer
    (before any graph is built). Otherwise: flatten turns -> ``build_graph`` ->
    ``leiden_partition`` (needs the ``[graph]`` extra) -> ``compress_with_partition``.
    """
    if target_tokens is None or tokenizer is None:
        text, _ = context.render_sessions(sessions)
        return text

    flat = _flatten_turns(sessions)
    if not flat:
        return ""

    texts = [text for _, text in flat]
    n, edges, weights = build_graph(texts, edge_metric=edge_metric)
    partition = leiden_partition(n, edges, weights)
    return compress_with_partition(
        sessions,
        partition,
        target_tokens,
        tokenizer,
        query=query,
        query_mode=query_mode,
    )
