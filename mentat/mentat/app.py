from fastapi import FastAPI

from .config import settings
from .pooler import type_weighted_mean_pool
from .predictor import identity_predict
from .schemas import (
    HealthResponse,
    PoolRequest,
    PoolResponse,
    PredictRequest,
    PredictResponse,
)
from .weights import WeightsLoader

app = FastAPI(title="mentat", version="0.1.0")
weights_loader = WeightsLoader(settings.weights_root)


@app.get("/v1/health", response_model=HealthResponse)
def health() -> HealthResponse:
    state = weights_loader.load_current()
    return HealthResponse(
        status="ok",
        weights_version=state.version,
        cold_start=state.cold_start,
        embedding_dim=settings.embedding_dim,
    )


@app.post("/v1/pool", response_model=PoolResponse)
def pool(req: PoolRequest) -> PoolResponse:
    return PoolResponse(embedding=type_weighted_mean_pool(req.events))


@app.post("/v1/predict", response_model=PredictResponse)
def predict(req: PredictRequest) -> PredictResponse:
    return PredictResponse(embedding=identity_predict(req.history))
