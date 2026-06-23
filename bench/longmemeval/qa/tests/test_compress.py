"""Compressor registry + baselines. Pure-function tests on context.Session
fixtures: identity for full, budget respected for truncate_tokens, unknown name
raises."""

from __future__ import annotations

import os
import urllib.error
import urllib.request

import pytest

from lme_qa import compress, context
from lme_qa.tokenize import CharRatioTokenizer


def _sessions() -> list[context.Session]:
    return [
        context.Session(
            "s1", "2023/05/20 (Sat) 02:21", [{"role": "user", "content": "alpha"}]
        ),
        context.Session(
            "s2",
            "2023/05/21 (Sun) 02:21",
            [{"role": "assistant", "content": "beta gamma delta"}],
        ),
    ]


def test_full_is_identity_render():
    out = compress.compress(
        "full", _sessions(), query="q", target_tokens=None, tokenizer=None
    )
    # Same text build_context renders for these sessions.
    assert "alpha" in out and "beta gamma delta" in out
    assert "Session dated 2023/05/20" in out


def test_truncate_respects_budget():
    tok = CharRatioTokenizer(4)
    out = compress.compress(
        "truncate_tokens", _sessions(), query="q", target_tokens=3, tokenizer=tok
    )
    assert tok.count(out) <= 3


def test_unknown_compressor_raises():
    with pytest.raises(KeyError):
        compress.compress(
            "nope", _sessions(), query="q", target_tokens=10, tokenizer=None
        )


def _four_sessions() -> list[context.Session]:
    # Four chronologically-ordered sessions, each a single short turn.
    return [
        context.Session(
            "s1", "2023/05/01 (Mon) 00:00", [{"role": "user", "content": "first"}]
        ),
        context.Session(
            "s2", "2023/05/02 (Tue) 00:00", [{"role": "user", "content": "second"}]
        ),
        context.Session(
            "s3", "2023/05/03 (Wed) 00:00", [{"role": "user", "content": "third"}]
        ),
        context.Session(
            "s4", "2023/05/04 (Thu) 00:00", [{"role": "user", "content": "fourth"}]
        ),
    ]


def test_topk_keeps_chronological_prefix_within_budget():
    sessions = _four_sessions()
    tok = CharRatioTokenizer(4)
    # Budget that fits the first two whole sessions but not the third.
    full_text, _ = context.render_sessions(sessions)
    two_text, _ = context.render_sessions(sessions[:2])
    three_text, _ = context.render_sessions(sessions[:3])
    budget = tok.count(three_text) - 1  # below the cost of three sessions
    assert tok.count(two_text) <= budget < tok.count(full_text)

    out = compress.compress(
        "topk_sessions", sessions, query="q", target_tokens=budget, tokenizer=tok
    )
    # First two kept whole; later sessions dropped.
    assert "first" in out and "second" in out
    assert "third" not in out and "fourth" not in out
    # Kept sessions are intact (never split) and in chronological order.
    assert out == two_text


def test_topk_no_budget_keeps_all():
    sessions = _four_sessions()
    tok = CharRatioTokenizer(4)
    full_text, _ = context.render_sessions(sessions)
    out = compress.compress(
        "topk_sessions", sessions, query="q", target_tokens=None, tokenizer=tok
    )
    assert out == full_text


# --- extractive_relevance (Task 5) ---------------------------------------


def _sessions_with_many_turns() -> list[context.Session]:
    """Two chronological sessions; the relevant turn sits in the SECOND session
    so a correct extractive keep crosses session boundaries (proving turns, not
    whole sessions, are the unit) and the dropped filler turns share a session
    with the kept one."""
    return [
        context.Session(
            "s1",
            "2023/05/20 (Sat) 02:21",
            [
                {"role": "user", "content": "filler one"},
                {"role": "assistant", "content": "filler two"},
            ],
        ),
        context.Session(
            "s2",
            "2023/05/21 (Sun) 02:21",
            [
                {"role": "user", "content": "filler three"},
                {"role": "assistant", "content": "relevant turn"},
            ],
        ),
    ]


def _fake_scorer(scores: dict[str, float]):
    """A scorer callable keyed on turn TEXT (the unit test's controlled signal),
    adapted to the (id, text) item interface the compressor passes."""

    def _score(query: str, items):
        return {i: scores[t] for i, t in items}

    return _score


def test_extractive_keeps_highest_relevance_turns_within_budget():
    sessions = _sessions_with_many_turns()
    tok = CharRatioTokenizer(4)
    fake_scores = {
        "filler one": 0.1,
        "filler two": 0.1,
        "filler three": 0.1,
        "relevant turn": 0.9,
    }
    # Budget tight enough that only the single highest-scored turn (plus its own
    # session's date header) fits -- the filler turns must be dropped.
    kept_only, _ = context.render_sessions(
        [context.Session("s2", "2023/05/21 (Sun) 02:21",
                         [{"role": "assistant", "content": "relevant turn"}])]
    )
    budget = tok.count(kept_only) + 1

    out = compress.compress(
        "extractive_relevance",
        sessions,
        query="q",
        target_tokens=budget,
        tokenizer=tok,
        scorer=_fake_scorer(fake_scores),
    )
    assert "relevant turn" in out
    assert "filler" not in out  # low-relevance turns dropped to fit


def test_extractive_preserves_chronological_order_and_whole_turns():
    sessions = _sessions_with_many_turns()
    tok = CharRatioTokenizer(4)
    # All turns equally relevant; a budget that fits every turn. The render must
    # then equal the full render -- turns regrouped under their sessions in the
    # original chronological order, each turn kept whole.
    fake_scores = {
        "filler one": 0.5,
        "filler two": 0.5,
        "filler three": 0.5,
        "relevant turn": 0.5,
    }
    full_text, _ = context.render_sessions(sessions)
    out = compress.compress(
        "extractive_relevance",
        sessions,
        query="q",
        target_tokens=tok.count(full_text) + 10,
        tokenizer=tok,
        scorer=_fake_scorer(fake_scores),
    )
    assert out == full_text


def test_extractive_no_budget_keeps_all():
    sessions = _sessions_with_many_turns()
    tok = CharRatioTokenizer(4)
    full_text, _ = context.render_sessions(sessions)
    out = compress.compress(
        "extractive_relevance",
        sessions,
        query="q",
        target_tokens=None,
        tokenizer=tok,
        scorer=_fake_scorer({}),  # not consulted when budget is None
    )
    assert out == full_text


# --- extractive_relevance_expanded (neighbor expansion) ------------------

def test_expanded_no_budget_keeps_all():
    """With no budget, expanded == extractive == full render."""
    sessions = _sessions_with_many_turns()
    tok = CharRatioTokenizer(4)
    full_text, _ = context.render_sessions(sessions)
    out = compress.compress(
        "extractive_relevance_expanded",
        sessions,
        query="q",
        target_tokens=None,
        tokenizer=tok,
        scorer=_fake_scorer({}),
    )
    assert out == full_text


def test_expanded_zero_reserve_equals_extractive():
    """With expansion_reserve=0, the greedy phase gets the full budget and the
    expansion phase has no room to add anything — so the output must equal
    plain extractive_relevance at the same budget."""
    sessions = _sessions_with_many_turns()
    tok = CharRatioTokenizer(4)
    fake_scores = {
        "filler one": 0.1,
        "filler two": 0.1,
        "filler three": 0.1,
        "relevant turn": 0.9,
    }
    budget = tok.count(
        context.render_sessions(
            [context.Session(
                "s2", "2023/05/21 (Sun) 02:21",
                [{"role": "assistant", "content": "relevant turn"}],
            )]
        )[0]
    ) + 5

    plain = compress.compress(
        "extractive_relevance",
        sessions,
        query="q",
        target_tokens=budget,
        tokenizer=tok,
        scorer=_fake_scorer(fake_scores),
    )
    expanded = compress.compress(
        "extractive_relevance_expanded",
        sessions,
        query="q",
        target_tokens=budget,
        tokenizer=tok,
        scorer=_fake_scorer(fake_scores),
        expansion_reserve=0.0,
    )
    assert expanded == plain


def test_expanded_admits_neighbors_of_kept_turn():
    """The expansion pass must admit the immediate predecessor and successor
    of a kept turn when budget allows. One session, five turns; the relevant
    turn is in the middle (index 2). Greedy selection with the reduced budget
    keeps only it; expansion admits turns 1 and 3 (the neighbors), but NOT
    turn 0 or turn 4 (two-hop away, not neighbors).

    Uses CharRatioTokenizer(1) (1 char = 1 token) for character-level budget
    precision so test budgets land on exact boundaries, not 4x-coarse grained
    ones. Token counts: relevant-only=69, +one neighbor=87, +both neighbors=104,
    +one far turn=121, all 5=137."""
    sessions = [
        context.Session(
            "s1",
            "2023/05/20 (Sat) 02:21",
            [
                {"role": "user", "content": "far before"},
                {"role": "user", "content": "near before"},
                {"role": "assistant", "content": "relevant turn"},
                {"role": "user", "content": "near after"},
                {"role": "user", "content": "far after"},
            ],
        ),
    ]
    tok = CharRatioTokenizer(1)
    fake_scores = {
        "far before": 0.1,
        "near before": 0.1,
        "relevant turn": 0.9,
        "near after": 0.1,
        "far after": 0.1,
    }
    # Greedy gets 70% of budget. Need:
    #   - greedy_budget >= 69 (relevant-only) so greedy keeps the relevant turn
    #   - greedy_budget < 87 (relevant + one neighbor) so greedy does NOT pick
    #     up any neighbor during phase 1
    #   - full budget >= 104 (relevant + both neighbors) so expansion admits both
    #   - full budget < 121 (relevant + neighbors + one far turn) so far turns
    #     don't slip in
    # 70% of budget in [69, 87) -> budget in [99, 124)
    # full budget in [104, 121) -> budget in [104, 121)
    # Intersection: [104, 121). Pick 110.
    budget = 110

    out = compress.compress(
        "extractive_relevance_expanded",
        sessions,
        query="q",
        target_tokens=budget,
        tokenizer=tok,
        scorer=_fake_scorer(fake_scores),
    )
    assert "relevant turn" in out
    assert "near before" in out and "near after" in out
    assert "far before" not in out and "far after" not in out


def test_expanded_single_hop_no_recursive_fill():
    """The expansion is single-hop: admitting a neighbor does NOT trigger
    expansion of that neighbor. At a budget that fits turns {1, 2, 3} but not
    {0, 1, 2, 3}, the expanded output should have turns {1, 2, 3} -- turn 0 is
    NOT pulled in as a second-hop neighbor of turn 1 (which was itself admitted
    as a neighbor of turn 2).

    Token counts (CharRatioTokenizer(1)): relevant-only=69, 3 turns
    {1,2,3}=101, 4 turns {0,1,2,3}=117."""
    sessions = [
        context.Session(
            "s1",
            "2023/05/20 (Sat) 02:21",
            [
                {"role": "user", "content": "turn zero"},
                {"role": "user", "content": "turn one"},
                {"role": "assistant", "content": "relevant turn"},
                {"role": "user", "content": "turn three"},
                {"role": "user", "content": "turn four"},
            ],
        ),
    ]
    tok = CharRatioTokenizer(1)
    fake_scores = {
        "turn zero": 0.1,
        "turn one": 0.1,
        "relevant turn": 0.9,
        "turn three": 0.1,
        "turn four": 0.1,
    }
    # Greedy gets 70% of budget. Need:
    #   - greedy_budget >= 69 (relevant-only) so greedy keeps the relevant turn
    #   - greedy_budget < 86 (relevant + turn_one) so greedy doesn't pick up
    #     turn_one or turn_three during phase 1
    #   - full budget >= 101 (3 turns {1,2,3}) so expansion admits both neighbors
    #   - full budget < 117 (4 turns {0,1,2,3}) so turn_zero isn't pulled in
    # 70% of budget in [69, 86) -> budget in [99, 123)
    # full budget in [101, 117) -> budget in [101, 117)
    # Intersection: [101, 117). Pick 110.
    budget = 110

    out = compress.compress(
        "extractive_relevance_expanded",
        sessions,
        query="q",
        target_tokens=budget,
        tokenizer=tok,
        scorer=_fake_scorer(fake_scores),
    )
    assert "turn one" in out and "relevant turn" in out and "turn three" in out
    assert "turn zero" not in out and "turn four" not in out


def test_expanded_preserves_chronological_order():
    """Kept + expanded turns render in original chronological order, not in
    admission order. The relevant turn is at index 3; the predecessor at index 2
    (a long connective) is admitted during expansion. The render must place
    turn 2 before turn 3.

    Uses a long connective so the greedy phase cannot afford it with its reduced
    budget — only the expansion pass (against the full budget) admits it.
    ``expansion_reserve=0.5`` gives the expansion half the budget so it has
    enough room for the expensive neighbor. The non-neighbor turns (aaa, bbb)
    score the same as the connective (0.1) but are tried first by the greedy
    phase (chronological tie-break); they don't fit the reduced greedy budget,
    so greedy keeps only the relevant turn — leaving the connective for
    expansion."""
    long_connective = "this is a long connective " * 3
    sessions = [
        context.Session(
            "s1",
            "2023/05/20 (Sat) 02:21",
            [
                {"role": "user", "content": "aaa"},
                {"role": "user", "content": "bbb"},
                {"role": "user", "content": long_connective},
                {"role": "assistant", "content": "relevant turn"},
            ],
        ),
    ]
    tok = CharRatioTokenizer(1)
    fake_scores = {
        "aaa": 0.1,
        "bbb": 0.1,
        long_connective: 0.1,
        "relevant turn": 0.9,
    }
    # Token counts: relevant-only=69, connective+relevant=154.
    # With reserve=0.5: greedy_budget = 0.5 * budget.
    # Need greedy_budget in [69, 79) so greedy keeps only relevant (adding aaa
    # at 79 would exceed it). => budget in [138, 158).
    # Need full budget >= 154 so expansion admits connective. => budget >= 154.
    # Intersection: [154, 158). Pick 156.
    budget = 156

    out = compress.compress(
        "extractive_relevance_expanded",
        sessions,
        query="q",
        target_tokens=budget,
        tokenizer=tok,
        scorer=_fake_scorer(fake_scores),
        expansion_reserve=0.5,
    )
    assert "connective" in out and "relevant turn" in out
    assert "aaa" not in out and "bbb" not in out  # non-neighbors not admitted
    # Chronological order: connective (turn 2) before relevant turn (turn 3).
    assert out.index("connective") < out.index("relevant turn")


def test_expanded_neighbor_does_not_cross_session_boundary():
    """Neighbor expansion is within-session only. The kept turn is the first
    turn of session 2 (index 0); the expansion admits its in-session successor
    but must NOT pull in any turn from session 1 (different session).

    Token counts (CharRatioTokenizer(1)): s2-relevant-only=64, s2-relevant+
    after=76, both-sessions=147."""
    sessions = [
        context.Session(
            "s1",
            "2023/05/20 (Sat) 02:21",
            [{"role": "user", "content": "other session turn"}],
        ),
        context.Session(
            "s2",
            "2023/05/21 (Sun) 02:21",
            [
                {"role": "user", "content": "relevant turn"},
                {"role": "user", "content": "after"},
            ],
        ),
    ]
    tok = CharRatioTokenizer(1)
    fake_scores = {
        "other session turn": 0.1,
        "relevant turn": 0.9,
        "after": 0.1,
    }
    # Greedy gets 70% of budget. Need:
    #   - greedy_budget >= 64 (s2-relevant-only) so greedy keeps the relevant
    #   - greedy_budget < 76 (s2-relevant+after) so greedy doesn't pick up
    #     after -- leaving it for expansion
    #   - full budget >= 76 so expansion admits after
    #   - full budget < 147 (both sessions) so other_session_turn isn't picked
    # 70% of budget in [64, 76) -> budget in [92, 109)
    # full budget in [76, 147) -> budget in [76, 147)
    # Intersection: [92, 109). Pick 100.
    budget = 100

    out = compress.compress(
        "extractive_relevance_expanded",
        sessions,
        query="q",
        target_tokens=budget,
        tokenizer=tok,
        scorer=_fake_scorer(fake_scores),
    )
    assert "relevant turn" in out
    assert "after" in out  # in-session successor: admitted by expansion
    assert "other session turn" not in out  # cross-session: NOT admitted


@pytest.mark.parametrize("budget", [50, 100, 200, 500, 1000])
def test_expanded_never_exceeds_budget(budget):
    """The expanded output must never exceed target_tokens — the same property
    extractive_relevance has. Phase 2 re-renders against target_tokens, so it
    should hold, but it is unasserted without this test. Parametric across a
    range of budgets to catch any edge case."""
    sessions = [
        context.Session(
            "s1",
            "2023/05/20 (Sat) 02:21",
            [
                {"role": "user", "content": f"turn {i} content here"}
                for i in range(20)
            ],
        ),
        context.Session(
            "s2",
            "2023/05/21 (Sun) 02:21",
            [
                {"role": "assistant", "content": f"response {i} with some text"}
                for i in range(15)
            ],
        ),
    ]
    tok = CharRatioTokenizer(1)
    fake_scores = {
        f"turn {i} content here": 0.9 if i in (5, 10) else 0.1
        for i in range(20)
    }
    fake_scores.update(
        {f"response {i} with some text": 0.9 if i in (3, 7) else 0.1 for i in range(15)}
    )
    out = compress.compress(
        "extractive_relevance_expanded",
        sessions,
        query="q",
        target_tokens=budget,
        tokenizer=tok,
        scorer=_fake_scorer(fake_scores),
    )
    assert tok.count(out) <= budget, (
        f"expanded output {tok.count(out)} exceeded budget {budget}"
    )


def _truthsayer_up() -> bool:
    """True if a truthsayer reranker answers at the configured URL. Probes the
    real /v1/rerank surface so the live test only runs against a working
    service (CI has none -> the test SKIPS, never fails)."""
    base = os.environ.get("TRUTHSAYER_URL", "http://localhost:8085")
    import json

    body = json.dumps(
        {"query": "ping", "candidates": [{"id": "x", "text": "ping"}]}
    ).encode()
    req = urllib.request.Request(
        base.rstrip("/") + "/v1/rerank",
        data=body,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=2) as resp:
            return resp.status == 200
    except (urllib.error.URLError, OSError):
        return False


@pytest.mark.skipif(not _truthsayer_up(), reason="truthsayer not reachable at :8085")
def test_extractive_with_live_truthsayer_scores_real_turns():
    """End-to-end against the live truthsayer reranker (default scorer). Skips
    cleanly when the service is down -- CI has no truthsayer."""
    from lme_qa.scorer import make_scorer

    sessions = [
        context.Session(
            "s1",
            "2023/05/20 (Sat) 02:21",
            [
                {"role": "user", "content": "I adopted a tabby cat named Luna."},
                {"role": "assistant", "content": "The stock market fell today."},
            ],
        ),
    ]
    tok = CharRatioTokenizer(4)
    full_text, _ = context.render_sessions(sessions)
    # Budget that forces dropping ~one turn so the ranking actually matters.
    budget = tok.count(full_text) - 5
    out = compress.compress(
        "extractive_relevance",
        sessions,
        query="What is my cat's name?",
        target_tokens=budget,
        tokenizer=tok,
        scorer=make_scorer("truthsayer"),
    )
    # The cat turn is the relevant one for the cat query; it should survive.
    assert "Luna" in out
