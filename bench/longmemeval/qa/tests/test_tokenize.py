"""Budgeting tokenizer: char-ratio default. Approximate by design (the plotted
rate is the reader's real usage.input_tokens, not this estimate)."""

from __future__ import annotations

from lme_qa.tokenize import CharRatioTokenizer


def test_count_char_ratio():
    tok = CharRatioTokenizer(chars_per_token=4)
    assert tok.count("a" * 40) == 10


def test_truncate_to_budget():
    tok = CharRatioTokenizer(chars_per_token=4)
    out = tok.truncate("a" * 100, 10)  # 10 tokens -> 40 chars
    assert len(out) == 40
    assert tok.count(out) <= 10


def test_truncate_noop_when_under_budget():
    tok = CharRatioTokenizer(chars_per_token=4)
    assert tok.truncate("short", 100) == "short"
