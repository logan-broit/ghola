"""Pydantic v2 request/response models for truthsayer's rerank endpoint."""
from pydantic import BaseModel, Field


class Candidate(BaseModel):
    id: str
    text: str


class RerankRequest(BaseModel):
    query: str = Field(min_length=1)
    candidates: list[Candidate] = Field(min_length=1)
    top_k: int | None = None


class ScoredCandidate(BaseModel):
    id: str
    score: float


class RerankResponse(BaseModel):
    scores: list[ScoredCandidate]


class HealthResponse(BaseModel):
    status: str
    model: str
    device: str
