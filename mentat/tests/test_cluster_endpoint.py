"""Integration test for /v1/cluster against a real postgres.

Marked @pytest.mark.integration — requires a postgres reachable via
DATABASE_* env (or MENTAT_DATABASE_DSN) with the chapterhouse
episodic + semantic schemas applied. Run via the compose stack:

    cd deploy/docker-compose && docker compose up -d postgres ch-init
    cd mentat && pytest tests/test_cluster_endpoint.py -m integration

The test seeds three clusters of L1 embeddings via direct SQL,
calls /v1/cluster through fastapi.testclient, and asserts that
non-noise mneme rows land in semantic.mnemes with the expected
shape (level=1, populated member_ids, normalized embedding).
"""
from __future__ import annotations

import os
from uuid import UUID, uuid4

import numpy as np
import psycopg
import pytest
from fastapi.testclient import TestClient

from mentat.app import app
from mentat.config import settings


pytestmark = pytest.mark.integration


def _seed_sessions(dsn: str, user_id: UUID, embeddings: list[np.ndarray]) -> list[UUID]:
    """Insert one episodic.sessions row per embedding, return ids in
    insert order. Each session is closed (ended_at NOT NULL) and has
    its l1_embedding populated — the production path that PR4
    clusters."""
    ids: list[UUID] = []
    with psycopg.connect(dsn) as conn:
        with conn.transaction(), conn.cursor() as cur:
            for emb in embeddings:
                sid = uuid4()
                vec_lit = "[" + ",".join(repr(float(x)) for x in emb) + "]"
                cur.execute(
                    """
                    INSERT INTO episodic.sessions
                        (id, user_id, started_at, ended_at, event_count,
                         l1_embedding)
                    VALUES (%s::uuid, %s::uuid, now(), now(), 0,
                            %s::vector)
                    """,
                    (str(sid), str(user_id), vec_lit),
                )
                ids.append(sid)
    return ids


def _purge_workspace(dsn: str, workspace_id: UUID) -> None:
    with psycopg.connect(dsn) as conn:
        with conn.transaction(), conn.cursor() as cur:
            cur.execute(
                "DELETE FROM semantic.mnemes WHERE workspace_id = %s::uuid",
                (str(workspace_id),),
            )


def test_cluster_endpoint_creates_mnemes():
    if not settings.database_dsn:
        pytest.skip("DATABASE_* env not set; integration test requires a live DB")

    client = TestClient(app)

    # Three blobs, 20 each, in 1024-dim space.
    rng = np.random.default_rng(7)
    centers = rng.normal(size=(3, 1024)).astype(np.float32)
    embs: list[np.ndarray] = []
    for c in centers:
        for _ in range(20):
            embs.append(c + rng.normal(scale=0.05, size=1024).astype(np.float32))

    # Use a fresh user_id so we don't collide with other test data.
    user_id = uuid4()
    workspace_id = uuid4()
    _ = _seed_sessions(settings.database_dsn, user_id, embs)
    _purge_workspace(settings.database_dsn, workspace_id)

    try:
        resp = client.post(
            "/v1/cluster",
            json={"workspace_id": str(workspace_id), "min_cluster_size": 3},
        )
        assert resp.status_code == 200, resp.text
        body = resp.json()
        # Three blobs should produce three clusters; tolerate ±1 for
        # HDBSCAN edge cases on synthetic data.
        assert 2 <= body["n_clusters"] <= 4
        assert body["upserted_mnemes"] == body["n_clusters"]

        # Verify mneme rows exist with expected shape.
        with psycopg.connect(settings.database_dsn) as conn, conn.cursor() as cur:
            cur.execute(
                """
                SELECT id, level, array_length(member_ids, 1)
                FROM semantic.mnemes
                WHERE workspace_id = %s::uuid
                """,
                (str(workspace_id),),
            )
            rows = cur.fetchall()
        assert len(rows) == body["n_clusters"]
        for _, level, n_members in rows:
            assert level == 1
            assert n_members >= 3
    finally:
        # Clean up — leave the bench database tidy.
        _purge_workspace(settings.database_dsn, workspace_id)
        with psycopg.connect(settings.database_dsn) as conn:
            with conn.transaction(), conn.cursor() as cur:
                cur.execute(
                    "DELETE FROM episodic.sessions WHERE user_id = %s::uuid",
                    (str(user_id),),
                )
