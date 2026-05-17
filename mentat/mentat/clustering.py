"""HDBSCAN over pre-pooled L1 embeddings for Stage C of predictive
replay. Inputs are session-level vectors (output of mentat's pool
endpoint, persisted to episodic.sessions.l1_embedding); the cluster
results are upserted into semantic.mnemes via mnemes.py.

Distance metric: cosine, computed on L2-normalized vectors as
1 - normalized_inner_product. HDBSCAN requires precomputed when
cosine isn't a built-in option in the local hdbscan binding.

Parameters tuned per the design doc: min_cluster_size >= 3 keeps
spurious 2-session pairs out of the cluster set.
"""
from __future__ import annotations

from dataclasses import dataclass
from uuid import UUID

import hdbscan
import numpy as np


@dataclass
class ClusterResult:
    """Result of one HDBSCAN run.

    labels[i] == -1 marks an outlier (HDBSCAN's "noise" class); they
    don't appear in member_ids_by_label or centroids_by_label. Callers
    upsert one mneme row per non-noise cluster.
    """

    labels: list[int]
    n_clusters: int
    member_ids_by_label: dict[int, list[UUID]]
    centroids_by_label: dict[int, np.ndarray]


def cluster_embeddings(
    embeddings: np.ndarray,
    ids: list[UUID],
    min_cluster_size: int = 3,
) -> ClusterResult:
    """Cluster (N, D) embeddings under cosine distance with HDBSCAN.

    embeddings.shape == (N, D); ids is the parallel list of UUIDs
    describing each row's source session. min_cluster_size <= 2 is
    rejected by HDBSCAN; the design-doc default is 3.
    """
    if embeddings.shape[0] == 0:
        return ClusterResult(labels=[], n_clusters=0, member_ids_by_label={}, centroids_by_label={})
    if embeddings.shape[0] != len(ids):
        raise ValueError(
            f"embeddings rows ({embeddings.shape[0]}) must match ids length ({len(ids)})"
        )

    # Cosine distance via L2-normalized inner product. clip to [0, 2]
    # — floating-point noise can produce tiny negatives or values
    # slightly above 2; HDBSCAN's precomputed metric assumes a proper
    # distance.
    norms = np.linalg.norm(embeddings, axis=1, keepdims=True).clip(min=1e-12)
    normed = embeddings / norms
    # HDBSCAN's precomputed-metric path is Cython-typed for float64.
    # Force the distance matrix dtype here; embeddings stay float32
    # to match pgvector's column type (no upstream change required).
    dist = np.clip(1.0 - normed @ normed.T, 0.0, 2.0).astype(np.float64)

    labels = hdbscan.HDBSCAN(
        min_cluster_size=min_cluster_size, metric="precomputed"
    ).fit_predict(dist).tolist()

    members: dict[int, list[UUID]] = {}
    for i, lbl in enumerate(labels):
        if lbl >= 0:
            members.setdefault(lbl, []).append(ids[i])

    centroids: dict[int, np.ndarray] = {}
    for lbl, mids in members.items():
        idxs = [i for i, l in enumerate(labels) if l == lbl]
        c = embeddings[idxs].mean(axis=0)
        n = float(np.linalg.norm(c))
        if n < 1e-12:
            n = 1e-12
        centroids[lbl] = c / n

    return ClusterResult(
        labels=labels,
        n_clusters=len(members),
        member_ids_by_label=members,
        centroids_by_label=centroids,
    )
