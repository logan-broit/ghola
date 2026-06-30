from __future__ import annotations

from lme_qa.segment import split_sentences


def test_split_keeps_nonempty_trimmed():
    assert split_sentences("Port is 9931. What now?  ok") == [
        "Port is 9931.",
        "What now?",
        "ok",
    ]


def test_split_empty_returns_empty():
    assert split_sentences("   ") == []
