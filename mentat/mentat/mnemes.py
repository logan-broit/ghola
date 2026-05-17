"""Upsert ClusterResult into semantic.mnemes with reinforcement
semantics: a new cluster whose member_ids overlap an existing level=1
mneme UPDATEs that mneme (refreshes embedding + member_ids, bumps
last_reinforced_at, nudges confidence up) instead of inserting a
duplicate.

Why overlap-based reinforcement: the same conceptual cluster may be
re-discovered with slight membership changes across daily clustering
runs. Treating overlap as identity keeps the mneme set stable across
time; treating each run as independent INSERTs would inflate the
mneme catalog with near-duplicates.
"""
from __future__ import annotations

from uuid import UUID

import numpy as np
import psycopg
from pgvector.psycopg import register_vector

from .clustering import ClusterResult


def upsert_mnemes_from_cluster(
    dsn: str, workspace_id: UUID, result: ClusterResult,
) -> int:
    """Upsert one mneme row per non-noise cluster in result.

    Returns the number of mneme rows touched (inserts + updates).
    Single transaction so a partial failure rolls back the whole
    cluster batch — partial mneme updates would leave the workspace
    in an inconsistent state for the next clustering run.
    """
    with psycopg.connect(dsn) as conn:
        register_vector(conn)
        with conn.transaction(), conn.cursor() as cur:
            upserted = 0
            for lbl, member_ids in result.member_ids_by_label.items():
                centroid = result.centroids_by_label[lbl]
                if not isinstance(centroid, np.ndarray):
                    centroid = np.asarray(centroid, dtype=np.float32)

                mids = [str(m) for m in member_ids]

                # Find the best-overlapping existing level=1 mneme.
                # Order by intersection cardinality so we pick the
                # largest overlap when several mnemes share members.
                # Postgres has no built-in UUID-array intersection
                # operator, so we count overlap per-row via unnest.
                cur.execute(
                    """
                    SELECT m.id
                    FROM semantic.mnemes m
                    WHERE m.workspace_id = %s::uuid
                      AND m.level = 1
                      AND m.member_ids && %s::uuid[]
                    ORDER BY (
                      SELECT count(*)
                      FROM unnest(m.member_ids) x
                      WHERE x = ANY(%s::uuid[])
                    ) DESC
                    LIMIT 1
                    """,
                    (str(workspace_id), mids, mids),
                )
                row = cur.fetchone()
                if row is not None:
                    cur.execute(
                        """
                        UPDATE semantic.mnemes
                        SET embedding = %s,
                            member_ids = %s::uuid[],
                            last_reinforced_at = now(),
                            confidence = LEAST(0.99, confidence + 0.05)
                        WHERE id = %s
                        """,
                        (centroid, mids, row[0]),
                    )
                else:
                    cur.execute(
                        """
                        INSERT INTO semantic.mnemes
                            (workspace_id, level, embedding, member_ids, confidence)
                        VALUES (%s::uuid, 1, %s, %s::uuid[], 0.5)
                        """,
                        (str(workspace_id), centroid, mids),
                    )
                upserted += 1
            return upserted
