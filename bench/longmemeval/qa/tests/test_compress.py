"""Compressor registry + baselines. Pure-function tests on context.Session
fixtures: identity for full, budget respected for truncate_tokens, unknown name
raises."""

from __future__ import annotations

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
