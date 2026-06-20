"""Relevance scorer clients for the ``extractive_relevance`` compressor.

A scorer takes a query and a list of ``(id, text)`` items and returns
``{id: float}`` -- a relevance score per item. The compressor uses these scores
to greedily keep the most relevant turns within a token budget.

Two backends, both behind the same callable interface so the compressor (and
``make_scorer``) can swap them without code change:

  - ``TruthsayerScorer`` (default): the already-running truthsayer reranker.
    ``POST {base_url}/v1/rerank`` with
    ``{"query": str, "candidates": [{"id": str, "text": str}]}`` ->
    ``{"scores": [{"id": str, "score": float}]}``. This is the production path:
    no new model serving, the reranker is already up at :8085.

  - ``GuildCosineScorer`` (fallback): the guild embeddings service. OpenAI-style
    ``POST {base_url}/v1/embeddings`` -> cosine(query_emb, text_emb). Used when
    truthsayer is unavailable; embeds query + items in one batch call.

Both use stdlib ``urllib`` only (no new dependency). A non-2xx response or a
malformed body raises ``RuntimeError`` rather than being swallowed -- a silent
empty score map would make every turn equally (ir)relevant and quietly degrade
the compressor to arbitrary selection.
"""

from __future__ import annotations

import json
import math
import os
import urllib.error
import urllib.request
from typing import Sequence

# Defaults match the live stack; the sweep overrides via env or base_url kwarg.
DEFAULT_TRUTHSAYER_URL = os.environ.get("TRUTHSAYER_URL", "http://localhost:8085")
DEFAULT_EMBEDDING_URL = os.environ.get("EMBEDDING_URL", "http://localhost:8082")
DEFAULT_EMBEDDING_MODEL = os.environ.get("EMBEDDING_MODEL", "qwen3-embedding")

_Item = tuple[str, str]


def _post_json(url: str, body: dict, timeout: float) -> dict:
    """POST ``body`` as JSON to ``url``; return the parsed JSON response.

    Any non-2xx status, transport error, or unparseable body raises
    ``RuntimeError`` -- callers must not see a silent empty result.
    """
    data = json.dumps(body).encode("utf-8")
    req = urllib.request.Request(
        url, data=data, headers={"Content-Type": "application/json"}, method="POST"
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
    except urllib.error.HTTPError as e:
        # Non-2xx: surface the status (and a short body snippet) loudly.
        snippet = b""
        try:
            snippet = e.read()[:200]
        except Exception:
            pass
        raise RuntimeError(
            f"scorer request to {url} failed: HTTP {e.code} {snippet!r}"
        ) from e
    except urllib.error.URLError as e:
        raise RuntimeError(f"scorer request to {url} failed: {e.reason}") from e
    try:
        return json.loads(raw)
    except json.JSONDecodeError as e:
        raise RuntimeError(
            f"scorer response from {url} was not valid JSON: {raw[:200]!r}"
        ) from e


class TruthsayerScorer:
    """Score items via the truthsayer ``/v1/rerank`` endpoint."""

    def __init__(
        self, base_url: str = DEFAULT_TRUTHSAYER_URL, timeout: float = 30.0
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.timeout = timeout

    def score(self, query: str, items: Sequence[_Item]) -> dict[str, float]:
        if not items:
            # No candidates -> no request, empty map. The compressor treats this
            # as "nothing to select" without hitting the network.
            return {}
        body = {
            "query": query,
            "candidates": [{"id": i, "text": t} for i, t in items],
        }
        resp = _post_json(self.base_url + "/v1/rerank", body, self.timeout)
        scores = resp.get("scores")
        if not isinstance(scores, list):
            raise RuntimeError(
                f"truthsayer response missing 'scores' list: {resp!r}"
            )
        return {row["id"]: float(row["score"]) for row in scores}

    # The compressor calls scorers as a bare ``scorer(query, items)`` callable.
    def __call__(self, query: str, items: Sequence[_Item]) -> dict[str, float]:
        return self.score(query, items)


def _cosine(a: Sequence[float], b: Sequence[float]) -> float:
    dot = sum(x * y for x, y in zip(a, b))
    na = math.sqrt(sum(x * x for x in a))
    nb = math.sqrt(sum(y * y for y in b))
    if na == 0.0 or nb == 0.0:
        return 0.0
    return dot / (na * nb)


class GuildCosineScorer:
    """Score items via guild embeddings + cosine(query, text).

    Embeds the query and all item texts in a single batched call (input[0] is
    the query) then scores each item by cosine similarity to the query vector.
    """

    def __init__(
        self,
        base_url: str = DEFAULT_EMBEDDING_URL,
        model: str = DEFAULT_EMBEDDING_MODEL,
        timeout: float = 60.0,
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.model = model
        self.timeout = timeout

    def score(self, query: str, items: Sequence[_Item]) -> dict[str, float]:
        if not items:
            return {}
        ids = [i for i, _ in items]
        texts = [t for _, t in items]
        body = {"input": [query, *texts], "model": self.model}
        resp = _post_json(self.base_url + "/v1/embeddings", body, self.timeout)
        data = resp.get("data")
        if not isinstance(data, list) or len(data) != len(texts) + 1:
            raise RuntimeError(
                f"guild response 'data' missing or wrong length: {resp!r}"
            )
        embeddings = [row["embedding"] for row in data]
        query_emb, item_embs = embeddings[0], embeddings[1:]
        return {
            sid: _cosine(query_emb, emb) for sid, emb in zip(ids, item_embs)
        }

    def __call__(self, query: str, items: Sequence[_Item]) -> dict[str, float]:
        return self.score(query, items)


# name -> scorer class. truthsayer is the default (production path).
_SCORERS = {
    "truthsayer": TruthsayerScorer,
    "guild": GuildCosineScorer,
}


def make_scorer(name: str = "truthsayer", **kwargs):
    """Factory: ``make_scorer("truthsayer")`` (default) or ``"guild"``.

    ``**kwargs`` (e.g. ``base_url=``) flow to the scorer constructor so the
    sweep can point the client at the live stack. Unknown names raise
    ``ValueError`` so a settings typo fails loudly.
    """
    try:
        cls = _SCORERS[name]
    except KeyError:
        raise ValueError(
            f"unknown scorer {name!r}; known: {sorted(_SCORERS)}"
        ) from None
    return cls(**kwargs)
