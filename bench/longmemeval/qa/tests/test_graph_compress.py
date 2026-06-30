"""Lexical-graph + Leiden-coverage compressor.

All tests here run WITHOUT igraph/leidenalg installed: ``build_graph`` and
``compress_with_partition`` are pure (stdlib only), and the one test that
touches ``leiden_partition`` asserts it raises a clear ``[graph]``-extra error
when leidenalg is absent (simulated via monkeypatch on find_spec). The pure
helpers are exercised directly so the coverage logic is testable without the
optional graph community-detection dependency.
"""

from __future__ import annotations

import importlib.util

import pytest

from lme_qa import context, graph_compress
from lme_qa.tokenize import CharRatioTokenizer


# --- build_graph (pure) --------------------------------------------------


def test_build_graph_links_similar_not_dissimilar():
    """Edge between two lexically-similar texts (high Jaccard), no edge to a
    dissimilar one. Texts 0 and 1 share most tokens; text 2 shares none."""
    texts = [
        "the cat sat on the warm mat",
        "the cat sat on the cold mat",
        "quantum entanglement violates bell inequalities",
    ]
    n, edges, weights = graph_compress.build_graph(
        texts, edge_metric="jaccard", threshold=0.1
    )
    assert n == 3
    # The similar pair is linked; the dissimilar node has no edge.
    assert (0, 1) in edges
    assert (0, 2) not in edges and (1, 2) not in edges
    # Weight of the kept edge is its jaccard, strictly above the threshold.
    w = dict(zip(edges, weights))
    assert w[(0, 1)] >= 0.1


def test_build_graph_threshold_excludes_weak_edges():
    """A high threshold drops a weak-overlap edge that a low threshold keeps."""
    texts = [
        "alpha beta gamma delta epsilon",
        "alpha zeta eta theta iota",  # shares only "alpha" -> low jaccard
    ]
    _, edges_low, _ = graph_compress.build_graph(texts, threshold=0.05)
    _, edges_high, _ = graph_compress.build_graph(texts, threshold=0.9)
    assert (0, 1) in edges_low
    assert (0, 1) not in edges_high


# --- compress_with_partition (pure, injected partition) ------------------


def _two_community_sessions() -> list[context.Session]:
    """One session, three turns. An injected partition [[0,1],[2]] treats turns
    0 and 1 as one community and turn 2 as another."""
    return [
        context.Session(
            "s1",
            "2023/05/20 (Sat) 02:21",
            [
                {"role": "user", "content": "community A first member here"},
                {"role": "assistant", "content": "community A second member longer text"},
                {"role": "user", "content": "community B lone member"},
            ],
        ),
    ]


def test_compress_with_partition_keeps_one_rep_per_community():
    """With an injected partition [[0,1],[2]] and a budget large enough for one
    representative per community but not all three turns, the output keeps the
    longest member of community A (turn 1) and community B's only turn (turn 2),
    dropping community A's shorter member (turn 0)."""
    sessions = _two_community_sessions()
    tok = CharRatioTokenizer(1)
    partition = [[0, 1], [2]]

    # Budget: the two reps (turn 1, the longest of community A, + turn 2) fit,
    # but adding turn 0 would overflow.
    reps_only, _ = context.render_sessions(
        [context.Session(
            "s1", "2023/05/20 (Sat) 02:21",
            [
                {"role": "assistant", "content": "community A second member longer text"},
                {"role": "user", "content": "community B lone member"},
            ],
        )]
    )
    all_three, _ = context.render_sessions(sessions)
    budget = tok.count(reps_only) + 1
    assert tok.count(reps_only) <= budget < tok.count(all_three)

    out = graph_compress.compress_with_partition(
        sessions, partition, target_tokens=budget, tokenizer=tok
    )
    # One representative per community admitted (longest first within community).
    assert "community A second member longer text" in out
    assert "community B lone member" in out
    # The shorter community-A member is dropped (only one rep per community fits).
    assert "community A first member here" not in out


def test_compress_with_partition_preserves_chronological_order():
    """Admitted turns render in original chronological order, regardless of the
    community ordering (community B has the higher-index turn but its rep should
    still render after community A's rep at index 1)."""
    sessions = _two_community_sessions()
    tok = CharRatioTokenizer(1)
    partition = [[0, 1], [2]]
    all_three, _ = context.render_sessions(sessions)
    # Big budget: everything fits.
    out = graph_compress.compress_with_partition(
        sessions, partition, target_tokens=tok.count(all_three) + 50, tokenizer=tok
    )
    # turn 1 (community A) renders before turn 2 (community B).
    assert out.index("community A second member") < out.index("community B lone")


def test_compress_with_partition_no_budget_keeps_all():
    """No budget -> render everything (the partition is irrelevant)."""
    sessions = _two_community_sessions()
    tok = CharRatioTokenizer(1)
    full_text, _ = context.render_sessions(sessions)
    out = graph_compress.compress_with_partition(
        sessions, [[0, 1], [2]], target_tokens=None, tokenizer=tok
    )
    assert out == full_text


# --- leiden_partition (guarded optional dependency) ----------------------


def test_leiden_partition_raises_when_extra_missing(monkeypatch):
    """When leidenalg is absent, leiden_partition raises a clear RuntimeError
    naming the [graph] extra. Simulate absence via find_spec monkeypatch so the
    test holds regardless of whether the extra happens to be installed."""
    real_find_spec = importlib.util.find_spec

    def _fake_find_spec(name, *args, **kwargs):
        if name in ("igraph", "leidenalg"):
            return None
        return real_find_spec(name, *args, **kwargs)

    monkeypatch.setattr(importlib.util, "find_spec", _fake_find_spec)

    with pytest.raises(RuntimeError, match=r"\[graph\] extra"):
        graph_compress.leiden_partition(2, [(0, 1)], [1.0])
