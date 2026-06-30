"""Approximate sentence segmentation shared by the prune/graph compressors.

This is a deliberately simple, dependency-free splitter (stdlib ``re`` only): it
breaks on terminal punctuation (``. ! ?``) followed by whitespace. It is *not* a
linguistically accurate tokenizer -- abbreviations ("Dr. Smith"), decimals, and
ellipses will be mis-split. That is acceptable: the prune/graph compressors only
need spans small enough to score and select independently, not grammatically
perfect sentences.
"""

from __future__ import annotations

import re

# Split after a sentence terminator that is followed by whitespace. The
# lookbehind keeps the terminator attached to the preceding span.
_SENTENCE_BOUNDARY = re.compile(r"(?<=[.!?])\s+")


def split_sentences(text: str) -> list[str]:
    """Split ``text`` into approximate sentences.

    Splits on ``(?<=[.!?])\\s+``, strips each piece, and drops empties. An
    all-whitespace input yields ``[]``.
    """
    pieces = _SENTENCE_BOUNDARY.split(text)
    return [s for s in (p.strip() for p in pieces) if s]
