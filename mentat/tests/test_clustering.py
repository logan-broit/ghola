"""Smoke tests for HDBSCAN clustering. The plan calls out one
synthetic test because HDBSCAN params are easy to get wrong in a way
that silently degrades — three blobs in feature space MUST land as
three clusters with all members assigned. The mnemes upsert path is
exercised by the integration test (test_cluster_endpoint.py) against
a real postgres."""
from uuid import uuid4

import numpy as np

from mentat.clustering import cluster_embeddings


def test_cluster_embeddings_finds_three_blobs():
    rng = np.random.default_rng(0)
    centers = rng.normal(size=(3, 1024)).astype(np.float32)

    embs: list[np.ndarray] = []
    ids = []
    for c in centers:
        for _ in range(20):
            embs.append(c + rng.normal(scale=0.1, size=1024).astype(np.float32))
            ids.append(uuid4())

    embeddings = np.asarray(embs, dtype=np.float32)
    res = cluster_embeddings(embeddings, ids, min_cluster_size=3)

    assert res.n_clusters == 3, f"expected 3 clusters, got {res.n_clusters}"
    for lbl, mids in res.member_ids_by_label.items():
        assert len(mids) >= 3, f"cluster {lbl} has {len(mids)} members, < min_cluster_size"
    # Centroids are L2-normalized.
    for c in res.centroids_by_label.values():
        assert abs(np.linalg.norm(c) - 1.0) < 1e-5


def test_cluster_embeddings_handles_empty_input():
    res = cluster_embeddings(np.zeros((0, 1024), dtype=np.float32), [])
    assert res.n_clusters == 0
    assert res.labels == []
    assert res.member_ids_by_label == {}
    assert res.centroids_by_label == {}
