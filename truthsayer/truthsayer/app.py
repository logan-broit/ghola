import logging

from fastapi import FastAPI, HTTPException, Request
from fastapi.responses import JSONResponse

from .config import settings
from .schemas import (
    HealthResponse,
    RerankRequest,
    RerankResponse,
    ScoredCandidate,
)
from .scorer import Scorer

logger = logging.getLogger(__name__)

app = FastAPI(title="truthsayer", version="0.1.0")
scorer = Scorer(
    settings.model,
    settings.device,
    settings.max_length,
    settings.chunk_chars,
    settings.dtype,
)


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
    logger.info(
        "truthsayer started model=%s device=%s max_length=%s dtype=%s",
        settings.model, settings.device, settings.max_length, settings.dtype,
    )


@app.exception_handler(ValueError)
async def _value_error_handler(_: Request, exc: ValueError) -> JSONResponse:
    # Bad input — surface as 400 instead of FastAPI's default 500.
    logger.warning("value_error %s", exc)
    return JSONResponse(status_code=400, content={"detail": str(exc)})


@app.exception_handler(Exception)
async def _unhandled_exception_handler(_: Request, exc: Exception) -> JSONResponse:
    # Catch-all for scorer / model failures. HTTPException short-
    # circuits before this handler, so authoritative 4xx paths keep
    # their codes; everything else becomes a structured 500 with a
    # log trail instead of a hang.
    logger.exception("unhandled exception in truthsayer: %s", exc)
    return JSONResponse(status_code=500, content={"detail": "internal error"})


@app.get("/v1/health", response_model=HealthResponse)
def health() -> HealthResponse:
    return HealthResponse(status="ok", model=scorer.model_name, device=scorer.device)


@app.post("/v1/rerank", response_model=RerankResponse)
def rerank(req: RerankRequest) -> RerankResponse:
    try:
        scores = scorer.score(req.query, [c.text for c in req.candidates])
    except (RuntimeError, MemoryError) as exc:
        # OOM / model-load failures — surface as 503 so the Go caller
        # can fall back gracefully instead of treating it as a bug.
        logger.error("scorer failed: %s", exc)
        raise HTTPException(status_code=503, detail=f"reranker unavailable: {exc}") from exc

    paired = [
        ScoredCandidate(id=c.id, score=s) for c, s in zip(req.candidates, scores)
    ]
    paired.sort(key=lambda x: x.score, reverse=True)
    if req.top_k is not None:
        paired = paired[: req.top_k]
    return RerankResponse(scores=paired)
