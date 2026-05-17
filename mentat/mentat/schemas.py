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
    """Request for /v1/cluster.

    workspace_id scopes which sessions feed clustering — mentat reads
    every closed session in the workspace whose l1_embedding is set,
    runs HDBSCAN, and upserts mnemes for that workspace alone.
    min_cluster_size is the HDBSCAN parameter; default 3 keeps
    spurious 2-session pairs out.
    """

    workspace_id: UUID
    min_cluster_size: int = 3


class ClusterResponse(BaseModel):
    workspace_id: UUID
    n_sessions: int
    n_clusters: int
    n_outliers: int
    upserted_mnemes: int
