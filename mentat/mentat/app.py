import logging
from uuid import UUID

import numpy as np
from fastapi import FastAPI, Request
from fastapi.responses import JSONResponse

from .clustering import cluster_embeddings
from .config import settings
from .pooler import type_weighted_mean_pool
from .predictor import identity_predict
from .schemas import (
    ClusterRequest,
    ClusterResponse,
    HealthResponse,
    PoolRequest,
    PoolResponse,
    PredictRequest,
    PredictResponse,
)
from .weights import WeightsLoader

logger = logging.getLogger(__name__)

app = FastAPI(title="mentat", version="0.1.0")
weights_loader = WeightsLoader(settings.weights_root)


@app.on_event("startup")
def _on_startup() -> None:
    # Configure root logging once for the worker process. uvicorn ships
    # its own access logger; this gets *application* lines (exceptions,
    # startup config) into the same JSON stream so the Go caller can
    # actually see why a 5xx came back.
    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s %(message)s",
    )
    state = weights_loader.load_current()
    logger.info(
        "mentat started weights_version=%s cold_start=%s embedding_dim=%s",
        state.version, state.cold_start, settings.embedding_dim,
    )


@app.exception_handler(ValueError)
async def _value_error_handler(_: Request, exc: ValueError) -> JSONResponse:
    # Bad input — surface as 400 instead of FastAPI's default 500.
    logger.warning("value_error path-validation %s", exc)
    return JSONResponse(status_code=400, content={"detail": str(exc)})


@app.exception_handler(Exception)
async def _unhandled_exception_handler(_: Request, exc: Exception) -> JSONResponse:
    # Catch-all so the Go caller gets a structured 500 + log trail
    # rather than a black-box hang. HTTPException already short-circuits
    # before this handler, so authoritative 4xx paths keep their codes.
    logger.exception("unhandled exception in mentat: %s", exc)
    return JSONResponse(status_code=500, content={"detail": "internal error"})


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


@app.post("/v1/cluster", response_model=ClusterResponse)
def cluster(req: ClusterRequest) -> ClusterResponse:
    """Cluster caller-supplied embeddings under cosine HDBSCAN.

    Pure: no DB, no workspace scoping. labels[i] is the cluster label for
    ids[i] (-1 == noise); outliers is the noise sublist. Mismatched
    ids/embeddings lengths raise ValueError -> 400 via the app handler.
    """
    if len(req.ids) != len(req.embeddings):
        raise ValueError(
            f"ids ({len(req.ids)}) and embeddings ({len(req.embeddings)}) must match"
        )
    if not req.ids:
        return ClusterResponse(labels=[], outliers=[])

    uuids = [UUID(i) for i in req.ids]
    embeddings = np.asarray(req.embeddings, dtype=np.float32)
    result = cluster_embeddings(embeddings, uuids, min_cluster_size=req.min_cluster_size)
    outliers = [req.ids[i] for i, lbl in enumerate(result.labels) if lbl == -1]
    return ClusterResponse(labels=result.labels, outliers=outliers)
