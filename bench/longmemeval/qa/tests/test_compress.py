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
