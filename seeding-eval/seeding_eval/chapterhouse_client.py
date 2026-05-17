"""Thin chapterhouse /v1/episodic/query client.

For the H3.c narrow experiment — bypasses ghola's RRF fan-out so we
can isolate the effect of the tags_any filter on a single tier. If the
experiment shows lift, productionize by plumbing the filter through
ghola's recall path. If not, throw this away.

Wire shape (queryRequest in handler/episodic.go):
    {user_id, workspace_id, query_text, query_embedding,
     limit?, include_shared?, w_semantic?, w_fts?,
     filters?, tags_any?}

The dev compose stack runs auth_provider=default with the default user
fixed to AUTH_DEFAULT_USER (00000000-0000-0000-0000-000000000001). No
Bearer header required there. We keep an Authorization hook for
deployments that wrap the mux in Bearer middleware.

Embeddings are computed via the OpenAI-compatible guild endpoint that
ch-server itself uses (EMBEDDING_URL = http://guild:8082 inside the
compose network; localhost:8082 from the host). Same model the server
ingested with (qwen3-embedding) so the vector and FTS pathways match.
"""
from __future__ import annotations

import os

import httpx


class GuildEmbeddingClient:
    """Wraps guild's OpenAI-compatible /v1/embeddings.

    Matches the model + endpoint the dev compose stack writes events
    with, so query embeddings live in the same space the stored
    vectors do. Single-shot — no batching, no caching.
    """

    def __init__(
        self,
        base_url: str = "http://localhost:8082",
        model: str = "qwen3-embedding",
        timeout: float = 60.0,
    ):
        self._client = httpx.Client(base_url=base_url, timeout=timeout)
        self._model = model

    def embed(self, text: str) -> list[float]:
        body = {"model": self._model, "input": text}
        resp = self._client.post("/v1/embeddings", json=body)
        resp.raise_for_status()
        data = resp.json()["data"]
        if not data:
            raise RuntimeError("guild returned empty embeddings array")
        return list(data[0]["embedding"])

    def close(self) -> None:
        self._client.close()

    def __enter__(self) -> GuildEmbeddingClient:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()


class ChapterhouseClient:
    """chapterhouse HTTP /v1/episodic/query client. localhost:8080 by default."""

    def __init__(
        self,
        base_url: str = "http://localhost:8080",
        api_key: str | None = None,
        timeout: float = 30.0,
    ):
        headers: dict[str, str] = {}
        if api_key is not None:
            headers["Authorization"] = f"Bearer {api_key}"
        elif env_key := os.environ.get("CHAPTERHOUSE_API_KEY"):
            headers["Authorization"] = f"Bearer {env_key}"
        self._client = httpx.Client(
            base_url=base_url,
            timeout=timeout,
            headers=headers,
        )

    def query_episodic(
        self,
        *,
        query_text: str,
        query_embedding: list[float],
        workspace_id: str,
        user_id: str,
        limit: int = 20,
        tags_any: list[str] | None = None,
    ) -> list[dict]:
        """Run the hybrid vector+FTS query and return ranked `hits`.

        Args:
            query_text: FTS query (also surfaces as the literal text in
                logs). Empty string short-circuits the FTS pathway via
                the `$4 <> ''` guard but the vector pathway still runs.
            query_embedding: Dense vector — required for the vector
                pathway. Empty list short-circuits the vector pathway.
            workspace_id: Required UUID. candidate pool = sessions
                joined to session_workspaces for this id.
            user_id: UUID. Must match the authenticated caller (the dev
                stack's default-user provider lets the default UUID
                through with no auth header).
            limit: Top-K returned. Default 20.
            tags_any: Optional. When non-empty, only events whose
                `tags` array overlaps this list are returned (`&&`).

        Returns:
            Parsed `hits` list. Each element is a dict with at least
            `id`, `score`, `tier`, `tags`, `text`.
        """
        body: dict = {
            "query_text": query_text,
            "query_embedding": query_embedding,
            "workspace_id": workspace_id,
            "user_id": user_id,
            "limit": limit,
        }
        if tags_any:
            body["tags_any"] = tags_any
        resp = self._client.post("/v1/episodic/query", json=body)
        resp.raise_for_status()
        return resp.json().get("hits") or []

    def close(self) -> None:
        self._client.close()

    def __enter__(self) -> ChapterhouseClient:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()
