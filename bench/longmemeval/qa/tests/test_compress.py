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
