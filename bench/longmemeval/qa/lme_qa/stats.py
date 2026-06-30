"""Corpus statistics for the free, no-model compressors: IDF self-information
pruning and BM25 relevance scoring.

All pure (stdlib ``re``, ``math``, ``collections`` only -- no network, no model
serving). The numbers here are what the IDF-prune and BM25 compressors will use
to decide which spans to keep within a token budget.

  - ``tokenize_words``  -- lowercase alphanumeric word tokens.
  - ``idf_map``         -- BM25-style (always-positive) inverse document
                           frequency over a corpus.
  - ``self_information``-- summed IDF of a span's tokens; the "surprise" of a
                           span, used by the prune compressor to rank spans.
  - ``BM25Scorer``      -- a callable query/items relevance scorer with the same
                           shape as the scorers in ``scorer.py`` so it can drop
                           into ``compress._score_and_greedy_select``.
"""

from __future__ import annotations

import math
import re
from collections import Counter
from typing import Sequence

_WORD = re.compile(r"[a-z0-9]+")

_Item = tuple[str, str]


def tokenize_words(text: str) -> list[str]:
    """Lowercase ``text`` and return its alphanumeric word tokens.

    Punctuation and whitespace are separators, so ``"Mauve-DB"`` -> ``["mauve",
    "db"]``. Shared by every function here so IDF and BM25 agree on what a token
    is.
    """
    return _WORD.findall(text.lower())


def idf_map(docs: list[str]) -> dict[str, float]:
    """BM25-style inverse document frequency for every token in ``docs``.

    Document frequency counts each token at most once per doc (``set`` of its
    tokens). The ``+1`` inside the log keeps the value strictly positive even
    for a token that appears in every document -- the textbook BM25 IDF can go
    negative for very common terms, which would make a common term *lower* a
    span's self-information below zero; we avoid that.

    ``idf = log((N - df + 0.5) / (df + 0.5) + 1)``  where ``N = len(docs)``.
    """
    n = len(docs)
    df: Counter[str] = Counter()
    for doc in docs:
        for tok in set(tokenize_words(doc)):
            df[tok] += 1
    return {
        tok: math.log((n - d + 0.5) / (d + 0.5) + 1.0) for tok, d in df.items()
    }


def self_information(
    span: str, idf: dict[str, float], default: float | None = None
) -> float:
    """Summed IDF ("surprise") of the tokens in ``span``.

    An unknown token (not in ``idf``) is treated as *maximally surprising*: it
    falls back to ``default`` if given, else the max IDF in the map (else 0.0
    for an empty map). Rationale: a token the corpus has never seen carries at
    least as much information as the rarest token we have measured, so the prune
    compressor should not under-value a span just because it contains novel
    words.
    """
    if default is not None:
        fallback = default
    elif idf:
        fallback = max(idf.values())
    else:
        fallback = 0.0
    return sum(idf.get(tok, fallback) for tok in tokenize_words(span))


class BM25Scorer:
    """Relevance scorer with the ``scorer(query, items) -> {id: float}`` shape.

    Builds an IDF map from the item texts (they *are* the corpus), then scores
    each item by Okapi BM25 of the query against that item's text. Same callable
    interface as ``TruthsayerScorer`` / ``GuildCosineScorer`` in ``scorer.py``,
    so it drops into ``compress._score_and_greedy_select`` unchanged -- but with
    no model and no network.
    """

    def __init__(self, k1: float = 1.5, b: float = 0.75) -> None:
        self.k1 = k1
        self.b = b

    def score(self, query: str, items: Sequence[_Item]) -> dict[str, float]:
        if not items:
            # No corpus -> no scores. Mirrors the HTTP scorers' empty-items path.
            return {}
        docs = [tokenize_words(text) for _, text in items]
        idf = idf_map([text for _, text in items])
        avgdl = sum(len(d) for d in docs) / len(docs)
        query_terms = tokenize_words(query)
        out: dict[str, float] = {}
        for (sid, _), doc in zip(items, docs):
            tf = Counter(doc)
            dl = len(doc)
            score = 0.0
            for term in query_terms:
                f = tf.get(term, 0)
                if f == 0:
                    continue
                denom = f + self.k1 * (1.0 - self.b + self.b * dl / avgdl)
                score += idf.get(term, 0.0) * (f * (self.k1 + 1.0)) / denom
            out[sid] = score
        return out

    def __call__(self, query: str, items: Sequence[_Item]) -> dict[str, float]:
        return self.score(query, items)
