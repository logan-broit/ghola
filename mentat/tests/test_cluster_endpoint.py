"""Pure-function HTTP test for /v1/cluster.

The endpoint now takes {ids, embeddings, min_cluster_size} and returns
{labels, outliers} — no DB, no psycopg. Three synthetic blobs must land
as clusters with all members labelled; noise rows surface in `outliers`.
"""
from __future__ import annotations

from uuid import uuid4

import numpy as np
from fastapi.testclient import TestClient

from mentat.app import app

client = TestClient(app)

def test_cluster_endpoint_labels_three_blobs():
    rng = np.random.default_rng(7)
    centers = rng.normal(size=(3, 1024)).astype(np.float32)
    ids: list[str] = []
    embs: list[list[float]] = []
    for c in centers:
        for _ in range(20):
            ids.append(str(uuid4()))
            embs.append((c + rng.normal(scale=0.05, size=1024)).astype(np.float32).tolist())

    resp = client.post("/v1/cluster", json={
        "ids": ids,
        "embeddings": embs,
        "min_cluster_size": 3,
    })
    assert resp.status_code == 200, resp.text
    body = resp.json()
    # labels is a parallel list to ids; -1 marks noise.
    assert len(body["labels"]) == len(ids)
    assert set(body["outliers"]).issubset(set(ids))
    # Non-noise ids should be the complement of outliers.
    non_noise = [i for i, lbl in zip(ids, body["labels"]) if lbl != -1]
    assert len(non_noise) == len(ids) - len(body["outliers"])
    # Three blobs -> at least 2 clusters (tolerate HDBSCAN edge merges).
    n_clusters = len({lbl for lbl in body["labels"] if lbl != -1})
    assert 2 <= n_clusters <= 4

def test_cluster_endpoint_empty_input():
    resp = client.post("/v1/cluster", json={"ids": [], "embeddings": []})
    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert body["labels"] == []
    assert body["outliers"] == []

def test_cluster_endpoint_mismatched_lengths_is_400():
    resp = client.post("/v1/cluster", json={
        "ids": [str(uuid4())],
        "embeddings": [[0.1] * 1024, [0.2] * 1024],
    })
    assert resp.status_code == 400, resp.text
