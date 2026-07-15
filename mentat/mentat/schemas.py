"""Pydantic v2 request/response models for mentat's PR1 endpoints."""
from uuid import UUID

from pydantic import BaseModel, Field


class Event(BaseModel):
    type: str
    embedding: list[float]


class PoolRequest(BaseModel):
    workspace_id: UUID
    events: list[Event] = Field(min_length=1)


class PoolResponse(BaseModel):
    embedding: list[float]


class PredictRequest(BaseModel):
    workspace_id: UUID
    history: list[list[float]] = Field(min_length=1)


class PredictResponse(BaseModel):
    embedding: list[float]


class HealthResponse(BaseModel):
    status: str
    weights_version: str | None
    cold_start: bool
    embedding_dim: int


class ClusterRequest(BaseModel):
    """Pure-math cluster request.

    ids and embeddings are parallel lists (same length). The caller
    supplies the UUID strings and their corresponding embedding vectors;
    mentat clusters them under cosine HDBSCAN and returns the label
    assignments. No DB, no workspace scoping — the Go worker owns all
    reads and writes.
    """

    ids: list[str]
    embeddings: list[list[float]]
    min_cluster_size: int = 3


class ClusterResponse(BaseModel):
    """Pure-math cluster response.

    labels[i] is the HDBSCAN cluster label for ids[i]; -1 marks noise.
    outliers is the sublist of ids whose label is -1.
    """

    labels: list[int]
    outliers: list[str]
