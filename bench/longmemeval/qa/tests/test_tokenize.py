"""Budgeting + rate tokenizers.

The char-ratio impl is the always-available fallback (what CI runs — tiktoken is
an optional [rate] extra). The tiktoken impl, when installed, gives a real
cl100k token count that proxies Claude's tokenizer for the rate axis. Both sit
behind the same Tokenizer Protocol and expose a ``.name`` for provenance, so the
reader can record WHICH tokenizer produced the rate it stamped.
"""

from __future__ import annotations

import importlib.util

import pytest

from lme_qa.tokenize import CharRatioTokenizer, TiktokenTokenizer, default_tokenizer

# tiktoken is an optional extra; CI installs [dev] only, so the tiktoken-backed
# tests skip there and the char-ratio fallback path is what always runs.
_HAS_TIKTOKEN = importlib.util.find_spec("tiktoken") is not None


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


def test_char_ratio_name():
    # Provenance: the name encodes the ratio so a recorded rate is interpretable.
    assert CharRatioTokenizer(chars_per_token=4).name == "char-ratio:4"


# --- TiktokenTokenizer (guarded: optional [rate] extra) ----------------------


@pytest.mark.skipif(not _HAS_TIKTOKEN, reason="tiktoken not installed ([rate] extra)")
def test_tiktoken_count_nonzero():
    tok = TiktokenTokenizer()
    # A real BPE tokenizer counts a non-trivial sentence as several tokens.
    assert tok.count("Hello, world! This is a test.") > 0


@pytest.mark.skipif(not _HAS_TIKTOKEN, reason="tiktoken not installed ([rate] extra)")
def test_tiktoken_truncate_cuts_on_token_boundary():
    tok = TiktokenTokenizer()
    text = "the quick brown fox jumps over the lazy dog " * 20
    out = tok.truncate(text, 10)
    # Cut to the budget exactly (token-boundary slice, not a byte cut).
    assert tok.count(out) == 10
    # And the truncated text is a real prefix-decode, shorter than the input.
    assert len(out) < len(text)


@pytest.mark.skipif(not _HAS_TIKTOKEN, reason="tiktoken not installed ([rate] extra)")
def test_tiktoken_truncate_noop_when_under_budget():
    tok = TiktokenTokenizer()
    assert tok.truncate("short", 1000) == "short"


@pytest.mark.skipif(not _HAS_TIKTOKEN, reason="tiktoken not installed ([rate] extra)")
def test_tiktoken_name():
    assert TiktokenTokenizer().name == "cl100k"


# --- default_tokenizer selection (works either way) --------------------------


def test_default_tokenizer_is_usable():
    # Whichever impl is selected, count() returns a non-negative int on real text
    # and a .name is exposed — so the reader always has a working rate tokenizer.
    tok = default_tokenizer()
    assert tok.count("some context text") >= 0
    assert isinstance(tok.name, str) and tok.name


def test_default_tokenizer_picks_tiktoken_when_available():
    tok = default_tokenizer()
    if _HAS_TIKTOKEN:
        assert tok.name == "cl100k"
    else:
        assert tok.name == "char-ratio:4"
