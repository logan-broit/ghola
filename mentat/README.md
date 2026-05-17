# mentat

Predictive-replay service for the Chapterhouse v0.3 tiered memory system.
Replaces the v0.2 LLM-distillation pipeline with a JEPA-style pooler +
predictor pair. PR1 ships the cold-start path; later PRs (PR4 cluster,
PR5 train, PR6 predict-with-weights, PR8 attention pool) replace pieces
of this with trained behavior.

The cold-start path is real production behavior, not a stub:

- Pool: deterministic type-weighted mean of event embeddings, L2 normalized.
  Weights `{user: 1.0, assistant: 0.5, tool_result: 0.1, system: 0.0}` with
  uniform fallback when the weighted sum is zero.
- Predict: identity (return last L1 of history).
- Health: reads `${MENTAT_WEIGHTS_ROOT}/current` symlink; reports
  `cold_start=true` when no weights have been published yet.

## Endpoints

- `GET  /v1/health`  — weights version + cold-start flag + embedding dim
- `POST /v1/pool`    — pool a session's event embeddings into one L1
- `POST /v1/predict` — predict the next L1 from a history of L1s

## Run

```bash
python -m venv .venv && source .venv/bin/activate
pip install -e .[dev]
pytest -v
uvicorn mentat.app:app --host 0.0.0.0 --port 8084
```

## Config (env)

- `EMBEDDING_DIM` (default 1024)
- `MENTAT_WEIGHTS_ROOT` (default `/weights`)
