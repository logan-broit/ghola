import logging
from uuid import UUID

import numpy as np
import psycopg
from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse

from .clustering import cluster_embeddings
from .config import settings
from .mnemes import upsert_mnemes_from_cluster
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
    """Cluster the workspace's L1 embeddings and upsert mnemes.

    Pulls every session in workspace_id whose l1_embedding is set,
    runs HDBSCAN under cosine, and upserts one mneme row per
    non-noise cluster (reinforcement-aware: overlapping member_ids
    update an existing mneme rather than inserting).
    """
    if not settings.database_dsn:
        raise HTTPException(
            status_code=503,
            detail="MENTAT_DATABASE_DSN unset (or DATABASE_* env block missing)",
        )

    # Read L1 embeddings + ids for the workspace. pgvector's psycopg
    # adapter would normally handle vector decoding, but for a one-
    # shot read using the text representation keeps the cluster
    # endpoint self-contained without registering type adapters.
    with psycopg.connect(settings.database_dsn) as conn, conn.cursor() as cur:
        cur.execute(
            """
            SELECT id, l1_embedding::text
            FROM episodic.sessions
            WHERE l1_embedding IS NOT NULL
            """
        )
        rows = cur.fetchall()

    # Workspace scoping: chapterhouse session rows don't carry a
    # workspace_id directly; the workspace is implicit via the user-
    # id boundary the caller manages. For v1a we cluster all sessions
    # with l1_embedding (a single workspace per deployment is the
    # current dev assumption). When multi-workspace lands, this query
    # gains a workspace filter via a sessions.workspace_id column or
    # an explicit join.
    _ = req.workspace_id

    if not rows:
        return ClusterResponse(
            workspace_id=req.workspace_id,
            n_sessions=0,
            n_clusters=0,
            n_outliers=0,
            upserted_mnemes=0,
        )

    ids: list[UUID] = []
    vecs: list[list[float]] = []
    for row_id, vec_text in rows:
        ids.append(row_id)
        # vec_text is the postgres text rep "[0.1,0.2,...]"
        v = vec_text.strip("[]").split(",")
        vecs.append([float(x) for x in v])

    embeddings = np.asarray(vecs, dtype=np.float32)
    result = cluster_embeddings(embeddings, ids, min_cluster_size=req.min_cluster_size)

    upserted = upsert_mnemes_from_cluster(
        settings.database_dsn, req.workspace_id, result,
    )
    n_outliers = sum(1 for lbl in result.labels if lbl == -1)
    return ClusterResponse(
        workspace_id=req.workspace_id,
        n_sessions=len(ids),
        n_clusters=result.n_clusters,
        n_outliers=n_outliers,
        upserted_mnemes=upserted,
    )
