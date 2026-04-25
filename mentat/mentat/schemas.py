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
