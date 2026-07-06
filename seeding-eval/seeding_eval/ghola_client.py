"""Thin HTTP client for ghola's POST /v1/recall.

Single-shot — no retry, no caching, no aggregation. The eval orchestrator
(D3) drives this; durability concerns belong there, not here.

Wire shape pinned against internal/core/types.go RecallInput / RecallResult:
    request:  {session_id?, user_id, workspace, query_text?, limit?,
               include_shared?, include_sietch?, include_episode?,
               include_semant?, include_timings?}
    response: {hits: [{tier, id, score, content, session_id?,
                       session_chunk_text?}], tier_counts, timings?}

ghola listens loopback-only by default (internal/http/server.go loopback
middleware) and has no Authorization header — auth is via user_id UUID
resolution against AUTH_DEFAULT_USER. We keep an optional Bearer hook
so a future non-loopback deployment doesn't require a client rewrite,
but the dev stack does not require it.

No unit test for this wrapper — observed end-to-end via the D6
integration test, matching the github_client.py pattern (no mocks).
"""
from __future__ import annotations

import os

import httpx


class GholaClient:
    """ghola HTTP recall client. localhost:7421 by default."""

    def __init__(
        self,
        base_url: str = "http://localhost:7421",
        api_key: str | None = None,
        timeout: float = 30.0,
    ):
        headers: dict[str, str] = {}
        # ghola's stock loopback server has no auth, so this is a future
        # hook. If a deployment ever wraps the mux in Bearer middleware,
        # the client already speaks it — no rewrite needed.
        if api_key is not None:
            headers["Authorization"] = f"Bearer {api_key}"
        elif env_key := os.environ.get("GHOLA_API_KEY"):
            headers["Authorization"] = f"Bearer {env_key}"
        self._client = httpx.Client(
            base_url=base_url,
            timeout=timeout,
            headers=headers,
        )

    def recall(
        self,
        *,
        query: str,
        workspace_id: str,
        user_id: str,
        k: int = 20,
        tags_any: list[str] | None = None,
        primitives: bool = False,
        settle: str | None = None,
        settle_params: dict | None = None,
        activation_weight: float | None = None,
    ) -> list[dict]:
        """Run a recall query and return the ranked `hits` list.

        Args:
            query: Free-text query — sent as `query_text` per RecallInput.
            workspace_id: Required UUID — sent as `workspace`. Empty
                workspace is rejected by ghola; scoping is structural.
            user_id: UUID. Empty falls back to AUTH_DEFAULT_USER on the
                server, but we require it explicitly so the eval
                orchestrator stays honest about which identity it ran as.
            k: Maximum rows to return — sent as `limit`. Default 20.
            tags_any: H3.c structural filter. When non-empty, ghola
                propagates the list to chapterhouse's event-grain tiers
                (episodic dense + episodic keyword) — only events whose
                ``tags`` column overlaps the list participate. Omitted
                from the wire body when ``None`` or empty so existing
                callers stay byte-identical.
            primitives: Phase 2a opt-in. When True, ghola asks
                chapterhouse for the 4th ranking sub-list (Hebbian
                primitives) and folds it into RRF as a 6th tier
                (equal-weight, tier-additive). Omitted from the wire
                body when False so the server's omitempty contract
                preserves byte-identical legacy behaviour.
            settle: Settle mode: None/omit (off, default), "expand" (config A:
                spreading activation sub-list), or "channel" (config B: activation
                also participates in score fusion). Maps to RecallInput.Settle.
            settle_params: Optional tuning overrides for the settle pipeline
                (lambda, hop_cap, node_cap, top_m, eps, max_iters). Zero/absent
                fields fall back to chapterhouse's DefaultSettleParams.
            activation_weight: Activation channel weight for channel mode (0, 1].
                Required when settle="channel". Must satisfy rerank_weight +
                activation_weight <= 1 (server default rerank_weight=0.5 implies
                activation_weight < 0.5).

        Returns:
            Parsed `hits` array. Each element is a dict with at least
            `tier`, `id`, `score`, `content`, and optionally `session_id`
            / `session_chunk_text`. Caller decides what to read.

        Raises:
            httpx.HTTPStatusError: non-2xx response.
            httpx.TimeoutException: timeout exceeded.
        """
        body: dict = {
            "query_text": query,
            "workspace": workspace_id,
            "user_id": user_id,
            "limit": k,
        }
        if tags_any:
            body["tags_any"] = list(tags_any)
        if primitives:
            body["primitives"] = True
        if settle:
            body["settle"] = settle
        if settle_params:
            body["settle_params"] = settle_params
        if activation_weight is not None:
            body["activation_weight"] = activation_weight
        resp = self._client.post("/v1/recall", json=body)
        resp.raise_for_status()
        data = resp.json()
        return data.get("hits") or []

    def close(self) -> None:
        self._client.close()

    def __enter__(self) -> GholaClient:
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()
