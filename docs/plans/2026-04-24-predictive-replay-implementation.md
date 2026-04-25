# Predictive Replay Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace the v0.2 LLM-distillation replay pipeline with a JEPA-style predictive replay: a new `mentat` Python+PyTorch service that pools event embeddings into L1 session embeddings and predicts future embeddings in representation space, plus a multi-source import tool to bootstrap training data from this machine's agent-tool logs.

**Architecture:** Three-layer change. (1) `pg_ghola` v0.3 reshapes `semantic.mnemes` (drop text columns, add `level`, unified 1024-dim embedding, polymorphic `member_ids`) and adds `episodic.sessions.l1_embedding`. (2) New `mentat` service exposes HTTP endpoints with PyTorch modules behind them (TypeWeightedMeanPool Stage 1 → AttentionPool Stage 2, EventMaskPredictor, SessionPredictor, LeJEPA SIGReg regularizer, HDBSCAN clustering). (3) Chapterhouse gains `internal/mentat/` (HTTP client) and `internal/semantic/` (write + recall glue) replacing `internal/replay/`; a reconciler pools any closed-session that lacks an L1 embedding. Cold-start path is genuinely real code, not a stub — type-weighted mean pool + identity predictor make the system operational before any training finishes.

**Tech Stack:** Go 1.22+ (ghola daemon, chapterhouse, import tool), Python 3.12 + FastAPI + PyTorch 2.x (mentat), Postgres 17 + pgvector + pg_ghola extension (pgrx/Rust), Docker Compose (local dev), HDBSCAN (clustering), `modernc.org/sqlite` (pure-Go SQLite for opencode adapter), Qwen3-Embedding-0.6B served via melange/vLLM (unchanged).

**Prerequisites:** Read the design doc at `docs/plans/2026-04-24-predictive-replay-design.md` in full before Task 1. All schema decisions, metric thresholds, validation gates, and data-flow shapes live there; this plan implements them.

## Plan Hygiene

- **Real code only.** No stub endpoints returning fake job IDs or zero counts. An endpoint either has real behavior or doesn't exist yet. Cold-start behavior (type-weighted mean pool, identity predictor) IS real code per the design doc and stays — it's the documented fallback that makes the system usable before training converges.
- **No mocks, no fakes, no interfaces-for-mockability.** Pass concrete types (`*mentat.Client`, `*repository.Repository`). If we genuinely need to swap implementations later, introduce the interface then.
- **Tests where they pay for themselves.** Schema-shape tests, adapter parsing on real fixtures, regularizer sanity (zero for isotropic, high for collapsed), training metrics on held-out data, end-to-end smoke against the compose stack. Skip shape-only unit tests, round-trip SQL tests, and "does the for-loop iterate" tests.
- **`${EMBEDDING_DIM}`** is substituted at deploy time — never hard-code `1024` in Go, Python, or SQL. Read from the `EMBEDDING_DIM` env var (default 1024). Rule lives in `~/.claude/projects/-home-loganb-ai/memory/feedback_dimension_agnostic.md`.
- **Commit per task.** Each task ends with one commit and leaves the tree in a working state.
- **No emoji** in code, commits, docs, or output.

---

## PR 1 — Vertical Slice (2–3 days)

Ship the minimum set of real endpoints end-to-end: `/v1/health`, `/v1/pool`, `/v1/predict` on mentat; Go HTTP client for those three; write path (reconciler pools closed sessions → `l1_embedding`); read path (semantic query pools context → cosine against `semantic.mnemes`). No `/v1/train` or `/v1/cluster` yet — they arrive in PR 5 and PR 4 when real work is behind them.

### Task 1.1: pg_ghola v0.3 — reshape `semantic.mnemes`

**Files:**
- Create: `_chapterhouse/ch-server/internal/repository/migrations/002_semantic_v03.sql`
- Create: `_chapterhouse/ch-server/internal/repository/semantic_schema_test.go`
- Modify: `deploy/docker-compose/seed.sql`

Write the migration per the design doc's `semantic.mnemes (pg_ghola v0.3)` section:

```sql
BEGIN;

-- v0.3: drop text-summary model, adopt predictive-replay shape.
-- Destructive. The LLM-distillation pipeline never ran in prod.
TRUNCATE TABLE semantic.mnemes;

ALTER TABLE semantic.mnemes
    DROP COLUMN IF EXISTS concept,
    DROP COLUMN IF EXISTS content,
    DROP COLUMN IF EXISTS memory_type,
    DROP COLUMN IF EXISTS tags,
    DROP COLUMN IF EXISTS entities,
    DROP COLUMN IF EXISTS source_episodic_ids;

ALTER TABLE semantic.mnemes
    ADD COLUMN IF NOT EXISTS level              integer NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS member_ids         uuid[]  NOT NULL DEFAULT '{}',
    ADD COLUMN IF NOT EXISTS last_reinforced_at timestamptz NOT NULL DEFAULT now();

DROP INDEX IF EXISTS semantic.mnemes_workspace;
CREATE INDEX IF NOT EXISTS mnemes_by_level
    ON semantic.mnemes (workspace_id, level);
CREATE INDEX IF NOT EXISTS mnemes_member_ids_gin
    ON semantic.mnemes USING gin (member_ids);
CREATE INDEX IF NOT EXISTS mnemes_last_reinforced
    ON semantic.mnemes (last_reinforced_at DESC) WHERE state = 'active';

COMMIT;
```

Update `deploy/docker-compose/seed.sql`: replace the `CREATE TABLE semantic.mnemes` block with the v0.3 shape (copy from the design doc). Drop the stub `semantic.recall` SQL function and `semantic.recall_result` type — the new Go-side `semantic.Querier` does cosine directly via pgx.

**Test (earns its place — migration drift is silent and painful):**

```go
// semantic_schema_test.go
func TestSemanticMnemesV03Shape(t *testing.T) {
    pool := testutil.OpenPool(t)
    ctx := context.Background()

    cols := map[string]string{}
    rows, err := pool.Query(ctx, `
        SELECT column_name, data_type
        FROM information_schema.columns
        WHERE table_schema='semantic' AND table_name='mnemes'`)
    require.NoError(t, err)
    defer rows.Close()
    for rows.Next() {
        var name, typ string
        require.NoError(t, rows.Scan(&name, &typ))
        cols[name] = typ
    }
    for _, required := range []string{"level", "member_ids", "last_reinforced_at"} {
        require.Contains(t, cols, required)
    }
    for _, removed := range []string{"concept", "content", "memory_type", "tags", "entities", "source_episodic_ids"} {
        require.NotContains(t, cols, removed)
    }
}
```

**Commit:**

```bash
git add _chapterhouse/ch-server/internal/repository/migrations/002_semantic_v03.sql \
        _chapterhouse/ch-server/internal/repository/semantic_schema_test.go \
        deploy/docker-compose/seed.sql
git commit -m "feat(pg_ghola): v0.3 semantic.mnemes shape — drop text cols, add level/member_ids"
```

---

### Task 1.2: Add `episodic.sessions.l1_embedding`

**Files:**
- Create: `_chapterhouse/ch-server/internal/repository/migrations/003_sessions_l1.sql`

```sql
BEGIN;
ALTER TABLE episodic.sessions
    ADD COLUMN IF NOT EXISTS l1_embedding vector(${EMBEDDING_DIM});

CREATE INDEX IF NOT EXISTS episodic_sessions_l1_hnsw
    ON episodic.sessions USING hnsw (l1_embedding vector_cosine_ops)
    WHERE l1_embedding IS NOT NULL;
COMMIT;
```

Confirm `${EMBEDDING_DIM}` substitution works by checking how `001_episodic.sql` handles it in `internal/repository/migrate.go` — mirror the pattern.

Fold this column into the existing `TestEpisodicSchema` (or equivalent) if one exists; a standalone test isn't worth a new file.

**Commit:** `feat(pg_ghola): add episodic.sessions.l1_embedding + HNSW index`

---

### Task 1.3: Delete `internal/replay/`

**Files:**
- Delete: `_chapterhouse/ch-server/internal/replay/`
- Modify: `_chapterhouse/ch-server/cmd/ch-server/main.go` (drop replay wiring)
- Modify: `_chapterhouse/ch-server/internal/config/config.go` (drop `Replay` struct)
- Modify: `deploy/docker-compose/docker-compose.yml` (drop `REPLAY_*` + `MENTAT_*` env vars from chapterhouse service; Task 1.6 re-adds `MENTAT_URL` in its new shape)

Blast radius map:

```bash
cd _chapterhouse/ch-server && grep -rn "internal/replay" --include='*.go' .
grep -rn "REPLAY_ENABLED\|REPLAY_WORKSPACE_ID\|MENTAT_URL\|MENTAT_MODEL\|MENTAT_API_KEY" _chapterhouse/ deploy/
```

Delete, then `go build ./... && go test ./...`. If a handler test imports `replay`, it goes too — no "preservation" of the old surface.

**Commit:** `refactor(ch-server): delete internal/replay — v0.2 LLM-distillation pipeline`

---

### Task 1.4: `mentat` service — `/v1/health`, `/v1/pool`, `/v1/predict`

**Files:**
- Create: `mentat/pyproject.toml`
- Create: `mentat/mentat/__init__.py`
- Create: `mentat/mentat/config.py`
- Create: `mentat/mentat/schemas.py`
- Create: `mentat/mentat/pooler.py`
- Create: `mentat/mentat/predictor.py`
- Create: `mentat/mentat/weights.py`
- Create: `mentat/mentat/app.py`
- Create: `mentat/Dockerfile`
- Create: `mentat/tests/test_pooler.py`
- Create: `mentat/tests/test_app.py`
- Create: `mentat/README.md`

**`pyproject.toml`:**

```toml
[build-system]
requires = ["hatchling"]
build-backend = "hatchling.build"

[project]
name = "mentat"
version = "0.1.0"
requires-python = ">=3.12"
dependencies = [
    "fastapi>=0.115",
    "uvicorn[standard]>=0.32",
    "pydantic>=2.9",
    "torch>=2.4",
    "numpy>=1.26",
    "hdbscan>=0.8.38",
    "psycopg[binary]>=3.2",
    "pgvector>=0.3",
    "httpx>=0.27",
    "structlog>=24",
]

[project.optional-dependencies]
dev = ["pytest>=8", "pytest-asyncio>=0.24", "ruff>=0.7"]

[tool.hatch.build.targets.wheel]
packages = ["mentat"]
```

**`config.py`:**

```python
import os

class Settings:
    embedding_dim: int = int(os.environ.get("EMBEDDING_DIM", "1024"))
    weights_root: str = os.environ.get("MENTAT_WEIGHTS_ROOT", "/weights")
    database_dsn: str | None = os.environ.get("MENTAT_DATABASE_DSN")
    melange_url: str = os.environ.get("MELANGE_URL", "http://melange:8082")

settings = Settings()
```

**`schemas.py`:** `Event`, `PoolRequest`, `PoolResponse`, `PredictRequest`, `PredictResponse`, `HealthResponse` — Pydantic models with field validation (non-empty events list, history length ≥ 1). Only schemas for endpoints that exist in PR 1. No `TrainRequest`/`ClusterRequest` yet.

**`pooler.py`:**

```python
"""Stage 1 pooler: deterministic type-weighted mean.

This is the cold-start path and the reference any trained pooler must
beat. It's also the production path until PR 8's AttentionPool ships.
Zero trainable parameters.
"""
import torch
from .schemas import Event

TYPE_WEIGHTS: dict[str, float] = {
    "user":        1.0,
    "assistant":   0.5,
    "tool_result": 0.1,
    "system":      0.0,
}

def type_weighted_mean_pool(events: list[Event]) -> list[float]:
    weights = torch.tensor(
        [TYPE_WEIGHTS.get(e.type, 0.0) for e in events], dtype=torch.float32
    )
    embs = torch.tensor([e.embedding for e in events], dtype=torch.float32)
    wsum = weights.sum()
    if wsum.item() == 0.0:
        # All events were system-type. Fall back to uniform rather than
        # returning a 0-vector that would poison HNSW.
        weights = torch.ones_like(weights)
        wsum = weights.sum()
    pooled = (weights.unsqueeze(1) * embs).sum(dim=0) / wsum
    norm = torch.linalg.vector_norm(pooled).clamp(min=1e-12)
    return (pooled / norm).tolist()
```

**`predictor.py`:**

```python
"""Cold-start predictor: L1_{t+1} := L1_t.

Identity is the correct fallback before Stage B training has produced
weights. PR 6 replaces this with SessionPredictor when weights are present.
"""
def identity_predict(history: list[list[float]]) -> list[float]:
    if not history:
        raise ValueError("history must be non-empty")
    return list(history[-1])
```

**`weights.py`:** real loader for the `/weights/current` symlink pattern (atomic flip via `os.replace`). Even in PR 1 this is real — it just reports `cold_start=True` when nothing is there.

```python
import json, os
from dataclasses import dataclass
from pathlib import Path

@dataclass
class WeightsState:
    root: Path
    version: str | None
    cold_start: bool

class WeightsLoader:
    def __init__(self, root: Path | str):
        self.root = Path(root)

    def load_current(self) -> WeightsState:
        current = self.root / "current"
        if not current.exists():
            return WeightsState(root=self.root, version=None, cold_start=True)
        target = current.resolve()
        meta_path = target / "metadata.json"
        if not meta_path.exists():
            return WeightsState(root=self.root, version=None, cold_start=True)
        meta = json.loads(meta_path.read_text())
        return WeightsState(root=target, version=meta["version"], cold_start=False)

def flip_to(root: Path, new_version: str) -> None:
    root = Path(root)
    tmp = root / f".current.{new_version}.tmp"
    if tmp.exists():
        tmp.unlink()
    tmp.symlink_to(new_version, target_is_directory=True)
    os.replace(tmp, root / "current")  # atomic on POSIX
```

**`app.py`:**

```python
from fastapi import FastAPI
from .config import settings
from .schemas import (
    Event, PoolRequest, PoolResponse,
    PredictRequest, PredictResponse, HealthResponse,
)
from .pooler import type_weighted_mean_pool
from .predictor import identity_predict
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
```

**Tests (narrow — the pooler's type-weight math is the one invariant worth pinning, and the app-level test confirms wiring):**

`tests/test_pooler.py`:

```python
import math
from mentat.schemas import Event
from mentat.pooler import type_weighted_mean_pool, TYPE_WEIGHTS

def test_type_weighted_mean_matches_documented_weights():
    # Three events with known embeddings; check pre-normalization math
    # matches the spec: weights {u:1.0, a:0.5, t:0.1}, sum=1.6.
    events = [
        Event(type="user",        embedding=[1.0, 0.0, 0.0]),
        Event(type="assistant",   embedding=[0.0, 1.0, 0.0]),
        Event(type="tool_result", embedding=[0.0, 0.0, 1.0]),
    ]
    out = type_weighted_mean_pool(events)
    # Pre-norm direction: (1.0, 0.5, 0.1) / 1.6 = (0.625, 0.3125, 0.0625)
    # Post-L2-norm: divide by 0.7053...
    expected = [1.0/1.6, 0.5/1.6, 0.1/1.6]
    n = math.sqrt(sum(x*x for x in expected))
    expected = [x/n for x in expected]
    for a, b in zip(out, expected):
        assert abs(a - b) < 1e-6

def test_all_system_events_fall_back_to_uniform():
    # weight-sum == 0 is the one branch worth pinning; a silent 0-vector
    # would poison HNSW indexes.
    events = [Event(type="system", embedding=[1.0, 2.0, 3.0])]
    out = type_weighted_mean_pool(events)
    assert any(abs(v) > 0 for v in out)
```

`tests/test_app.py`: one end-to-end-style test per endpoint using `TestClient` — exercises real pooler + predictor through the HTTP layer, not mocks.

```python
from fastapi.testclient import TestClient
from mentat.app import app

client = TestClient(app)

def test_health_reports_cold_start_when_no_weights(tmp_path, monkeypatch):
    monkeypatch.setattr("mentat.app.weights_loader",
                        __import__("mentat.weights", fromlist=["WeightsLoader"]).WeightsLoader(tmp_path))
    r = client.get("/v1/health")
    assert r.status_code == 200
    body = r.json()
    assert body["cold_start"] is True
    assert body["weights_version"] is None

def test_pool_endpoint_returns_1024_dim():
    dim = 1024
    r = client.post("/v1/pool", json={
        "workspace_id": "00000000-0000-0000-0000-000000000010",
        "events": [
            {"type": "user",      "embedding": [0.1]*dim},
            {"type": "assistant", "embedding": [0.2]*dim},
        ],
    })
    assert r.status_code == 200
    assert len(r.json()["embedding"]) == dim

def test_predict_cold_start_is_identity():
    last = [0.5] * 1024
    r = client.post("/v1/predict", json={
        "workspace_id": "00000000-0000-0000-0000-000000000010",
        "history": [[0.1]*1024, last],
    })
    assert r.json()["embedding"] == last
```

**Dockerfile:**

```dockerfile
FROM python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends build-essential curl \
    && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY pyproject.toml ./
COPY mentat ./mentat
RUN pip install --no-cache-dir -e .
EXPOSE 8084
CMD ["uvicorn", "mentat.app:app", "--host", "0.0.0.0", "--port", "8084"]
```

Task 5 swaps to a CUDA base image when training arrives.

**Commit:** `feat(mentat): service scaffold + real pool/predict/health (cold-start path)`

---

### Task 1.5: Wire mentat into docker-compose

**Files:**
- Modify: `deploy/docker-compose/docker-compose.yml`

Add service + named volume:

```yaml
  mentat:
    build:
      context: ../../mentat
      dockerfile: Dockerfile
    image: ghola/mentat:dev
    depends_on:
      postgres:
        condition: service_healthy
      melange:
        condition: service_healthy
    environment:
      EMBEDDING_DIM: ${EMBEDDING_DIM:-1024}
      MENTAT_WEIGHTS_ROOT: /weights
      MENTAT_DATABASE_DSN: postgresql://${POSTGRES_USER:-memory_api}:${POSTGRES_PASSWORD:-dev}@postgres:5432/${POSTGRES_DB:-memories}
      MELANGE_URL: http://melange:8082
    volumes:
      - mentat-weights:/weights
    ports:
      - "${MENTAT_PORT:-8084}:8084"
    healthcheck:
      test: ["CMD-SHELL", "curl -fsS http://localhost:8084/v1/health || exit 1"]
      interval: 5s
      timeout: 3s
      retries: 30
      start_period: 30s
```

Add `mentat-weights:` to the `volumes:` block. Add `MENTAT_URL: ${MENTAT_URL:-http://mentat:8084}` to the `chapterhouse` service env.

Manual smoke:

```bash
cd deploy/docker-compose
docker compose build mentat
docker compose up -d postgres melange ch-init mentat
curl -s http://localhost:8084/v1/health | jq .
# expect: status=ok, cold_start=true, embedding_dim=1024
docker compose down
```

**Commit:** `feat(compose): add mentat service + mentat-weights volume`

---

### Task 1.6: `internal/mentat/` Go HTTP client — Pool, Predict, Health only

**Files:**
- Create: `_chapterhouse/ch-server/internal/mentat/client.go`
- Modify: `_chapterhouse/ch-server/internal/config/config.go` (add `MentatURL` setting)

```go
// Package mentat is the Go HTTP client for the mentat service. It is
// intentionally small: one method per endpoint mentat actually serves.
// Train and Cluster land in PRs 5 and 4 respectively, when their mentat-
// side counterparts go live.
package mentat

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "time"

    "github.com/google/uuid"
)

type Event struct {
    Type      string    `json:"type"`
    Embedding []float32 `json:"embedding"`
}

type PoolRequest struct {
    WorkspaceID uuid.UUID `json:"workspace_id"`
    Events      []Event   `json:"events"`
}
type PoolResponse struct{ Embedding []float32 `json:"embedding"` }

type PredictRequest struct {
    WorkspaceID uuid.UUID   `json:"workspace_id"`
    History     [][]float32 `json:"history"`
}
type PredictResponse struct{ Embedding []float32 `json:"embedding"` }

type HealthResponse struct {
    Status         string `json:"status"`
    WeightsVersion string `json:"weights_version"`
    ColdStart      bool   `json:"cold_start"`
    EmbeddingDim   int    `json:"embedding_dim"`
}

type Client struct {
    baseURL string
    http    *http.Client
}

func NewClient(baseURL string, h *http.Client) *Client {
    if h == nil {
        // Semantic recall calls Pool per-query; tight timeout keeps a
        // stuck mentat from wedging user-visible recall.
        h = &http.Client{Timeout: 10 * time.Second}
    }
    return &Client{baseURL: baseURL, http: h}
}

func (c *Client) Pool(ctx context.Context, req PoolRequest) (*PoolResponse, error) {
    if len(req.Events) == 0 {
        return nil, fmt.Errorf("mentat: pool requires at least one event")
    }
    var out PoolResponse
    return &out, c.do(ctx, "/v1/pool", req, &out)
}

func (c *Client) Predict(ctx context.Context, req PredictRequest) (*PredictResponse, error) {
    if len(req.History) == 0 {
        return nil, fmt.Errorf("mentat: predict requires non-empty history")
    }
    var out PredictResponse
    return &out, c.do(ctx, "/v1/predict", req, &out)
}

func (c *Client) Health(ctx context.Context) (*HealthResponse, error) {
    req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/health", nil)
    if err != nil { return nil, err }
    resp, err := c.http.Do(req)
    if err != nil { return nil, err }
    defer resp.Body.Close()
    var out HealthResponse
    return &out, json.NewDecoder(resp.Body).Decode(&out)
}

func (c *Client) do(ctx context.Context, path string, in, out any) error {
    body, err := json.Marshal(in)
    if err != nil { return err }
    req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
    if err != nil { return err }
    req.Header.Set("Content-Type", "application/json")
    resp, err := c.http.Do(req)
    if err != nil { return fmt.Errorf("mentat: %s: %w", path, err) }
    defer resp.Body.Close()
    if resp.StatusCode >= 400 {
        buf, _ := io.ReadAll(resp.Body)
        return fmt.Errorf("mentat: %s: %d: %s", path, resp.StatusCode, string(buf))
    }
    return json.NewDecoder(resp.Body).Decode(out)
}
```

**No unit test here** — testing that JSON round-trips through `encoding/json` over HTTP is testing the Go stdlib. The e2e test in Task 1.10 exercises this client against a real mentat.

**Commit:** `feat(ch-server): internal/mentat HTTP client (Pool/Predict/Health)`

---

### Task 1.7: Write path — `semantic.Writer` + reconciler

**Files:**
- Create: `_chapterhouse/ch-server/internal/semantic/writer.go`
- Create: `_chapterhouse/ch-server/internal/semantic/reconciler.go`
- Modify: `_chapterhouse/ch-server/internal/repository/semantic.go` (new file)
- Modify: `_chapterhouse/ch-server/internal/repository/repository.go` (if method list lives there)
- Modify: `_chapterhouse/ch-server/cmd/ch-server/main.go`

**Rationale for the reconciler over an inline session-close hook:** ghola's daemon HTTP API is an invariant (design doc). Chapterhouse already owns `episodic.sessions` and sees writes land. A reconciler — "find closed sessions whose `l1_embedding IS NULL`, pool them" — keeps the change local and idempotent. The encoding worker on the ghola side is untouched.

**`internal/repository/semantic.go`:**

```go
package repository

import (
    "context"

    "github.com/google/uuid"
    "github.com/pgvector/pgvector-go"
)

type ClosedSession struct {
    ID          uuid.UUID
    WorkspaceID uuid.UUID
}

func (r *Repository) ClosedSessionsMissingL1(ctx context.Context, limit int) ([]ClosedSession, error) {
    rows, err := r.pool.Query(ctx, `
        SELECT id, workspace_id
        FROM episodic.sessions
        WHERE ended_at IS NOT NULL AND l1_embedding IS NULL
        ORDER BY ended_at ASC
        LIMIT $1`, limit)
    if err != nil { return nil, err }
    defer rows.Close()
    var out []ClosedSession
    for rows.Next() {
        var s ClosedSession
        if err := rows.Scan(&s.ID, &s.WorkspaceID); err != nil { return nil, err }
        out = append(out, s)
    }
    return out, rows.Err()
}

type SessionEventRow struct {
    Type      string
    Embedding []float32
}

func (r *Repository) SessionEvents(ctx context.Context, sessionID uuid.UUID) ([]SessionEventRow, error) {
    rows, err := r.pool.Query(ctx, `
        SELECT type, embedding
        FROM episodic.events
        WHERE session_id = $1 AND embedding IS NOT NULL
        ORDER BY ts ASC, id ASC`, sessionID)
    if err != nil { return nil, err }
    defer rows.Close()
    var out []SessionEventRow
    for rows.Next() {
        var row SessionEventRow
        var v pgvector.Vector
        if err := rows.Scan(&row.Type, &v); err != nil { return nil, err }
        row.Embedding = v.Slice()
        out = append(out, row)
    }
    return out, rows.Err()
}

func (r *Repository) UpdateSessionL1Embedding(ctx context.Context, sessionID uuid.UUID, emb []float32) error {
    _, err := r.pool.Exec(ctx,
        `UPDATE episodic.sessions SET l1_embedding = $1 WHERE id = $2`,
        pgvector.NewVector(emb), sessionID)
    return err
}
```

(Verify actual column names in `episodic.events` — `type` and `embedding` are typical but check `001_episodic.sql`. If the column is called `role` instead of `type`, the mentat-side `TYPE_WEIGHTS` keys will still match because we map from the repo side.)

**`internal/semantic/writer.go`:**

```go
// Package semantic is the v0.3 replacement for internal/replay. It owns
// the session → L1 write path, the recall read path, and (in later PRs)
// clustering + training orchestration.
package semantic

import (
    "context"

    "github.com/google/uuid"

    "github.com/thinkwright/chapterhouse/ch-server/internal/mentat"
    "github.com/thinkwright/chapterhouse/ch-server/internal/repository"
)

type Writer struct {
    repo *repository.Repository
    m    *mentat.Client
}

func NewWriter(repo *repository.Repository, m *mentat.Client) *Writer {
    return &Writer{repo: repo, m: m}
}

// PoolSessionToL1 fetches a session's events, pools them via mentat, and
// writes the L1 vector to episodic.sessions.l1_embedding. Idempotent by
// construction: re-running overwrites with the current pooler's output.
func (w *Writer) PoolSessionToL1(ctx context.Context, workspaceID, sessionID uuid.UUID) error {
    events, err := w.repo.SessionEvents(ctx, sessionID)
    if err != nil { return err }
    if len(events) == 0 { return nil }

    req := mentat.PoolRequest{
        WorkspaceID: workspaceID,
        Events:      make([]mentat.Event, len(events)),
    }
    for i, e := range events {
        req.Events[i] = mentat.Event{Type: e.Type, Embedding: e.Embedding}
    }

    resp, err := w.m.Pool(ctx, req)
    if err != nil { return err }
    return w.repo.UpdateSessionL1Embedding(ctx, sessionID, resp.Embedding)
}
```

**`internal/semantic/reconciler.go`:**

```go
package semantic

import (
    "context"
    "log/slog"
    "time"
)

type Reconciler struct {
    writer *Writer
    every  time.Duration
    batch  int
    logger *slog.Logger
}

func NewReconciler(w *Writer, every time.Duration, logger *slog.Logger) *Reconciler {
    if every <= 0 { every = 30 * time.Second }
    if logger == nil { logger = slog.Default() }
    return &Reconciler{writer: w, every: every, batch: 32, logger: logger}
}

func (r *Reconciler) Run(ctx context.Context) error {
    t := time.NewTicker(r.every)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done(): return ctx.Err()
        case <-t.C: r.tick(ctx)
        }
    }
}

func (r *Reconciler) tick(ctx context.Context) {
    sessions, err := r.writer.repo.ClosedSessionsMissingL1(ctx, r.batch)
    if err != nil {
        r.logger.Warn("semantic: closed-sessions query failed", "err", err)
        return
    }
    for _, s := range sessions {
        if err := r.writer.PoolSessionToL1(ctx, s.WorkspaceID, s.ID); err != nil {
            r.logger.Warn("semantic: pool failed", "session_id", s.ID, "err", err)
        }
    }
}
```

Wire in `cmd/ch-server/main.go`: when `cfg.MentatURL != ""`, construct the client + writer + reconciler and start a goroutine.

**No unit tests for the writer or reconciler.** The interesting behavior is "real session in postgres → pool via real mentat → l1_embedding lands." That's the end-to-end test in Task 1.10. Unit tests with fake postgres and fake mentat would test plumbing, not behavior.

**Commit:** `feat(ch-server): semantic.Writer + Reconciler — pool closed sessions to L1`

---

### Task 1.8: Read path — `semantic.Querier` + wire into handler

**Files:**
- Create: `_chapterhouse/ch-server/internal/semantic/query.go`
- Modify: `_chapterhouse/ch-server/internal/repository/semantic.go`
- Modify: `_chapterhouse/ch-server/internal/handler/semantic.go`
- Modify: `_chapterhouse/ch-server/cmd/ch-server/main.go`

Add to `repository/semantic.go`:

```go
type MnemeHit struct {
    ID    uuid.UUID
    Score float64
    Level int
}

func (r *Repository) QueryMnemesByEmbedding(
    ctx context.Context, workspaceID uuid.UUID, emb []float32, limit int,
) ([]MnemeHit, error) {
    v := pgvector.NewVector(emb)
    rows, err := r.pool.Query(ctx, `
        SELECT id,
               1 - (embedding <=> $2) AS score,
               level
        FROM semantic.mnemes
        WHERE workspace_id = $1 AND state = 'active'
        ORDER BY embedding <=> $2
        LIMIT $3`, workspaceID, v, limit)
    if err != nil { return nil, err }
    defer rows.Close()
    var out []MnemeHit
    for rows.Next() {
        var h MnemeHit
        if err := rows.Scan(&h.ID, &h.Score, &h.Level); err != nil { return nil, err }
        out = append(out, h)
    }
    return out, rows.Err()
}
```

**`internal/semantic/query.go`:**

```go
package semantic

import (
    "context"
    "log/slog"

    "github.com/google/uuid"

    "github.com/thinkwright/chapterhouse/ch-server/internal/mentat"
    "github.com/thinkwright/chapterhouse/ch-server/internal/repository"
)

type RecallRequest struct {
    WorkspaceID    uuid.UUID
    QueryEmbedding []float32
    RecentContext  []mentat.Event
    Limit          int
}

type Querier struct {
    repo   *repository.Repository
    m      *mentat.Client
    logger *slog.Logger
}

func NewQuerier(repo *repository.Repository, m *mentat.Client, logger *slog.Logger) *Querier {
    if logger == nil { logger = slog.Default() }
    return &Querier{repo: repo, m: m, logger: logger}
}

// Recall pools (recent context + query) into an L1 probe vector, then
// runs HNSW cosine against semantic.mnemes. If mentat is unreachable,
// returns zero hits and logs — the design invariant is that a semantic-
// tier failure never breaks user-visible recall.
func (q *Querier) Recall(ctx context.Context, req RecallRequest) ([]repository.MnemeHit, error) {
    events := make([]mentat.Event, 0, len(req.RecentContext)+1)
    events = append(events, req.RecentContext...)
    events = append(events, mentat.Event{Type: "user", Embedding: req.QueryEmbedding})

    pooled, err := q.m.Pool(ctx, mentat.PoolRequest{
        WorkspaceID: req.WorkspaceID,
        Events:      events,
    })
    if err != nil {
        q.logger.Warn("semantic: mentat pool failed; returning 0 hits",
            "workspace_id", req.WorkspaceID, "err", err)
        return nil, nil
    }
    return q.repo.QueryMnemesByEmbedding(ctx, req.WorkspaceID, pooled.Embedding, req.Limit)
}
```

In `handler/semantic.go`: the existing `Query` method hits the legacy `semantic.recall` SQL function. Replace with a call to `*semantic.Querier`. Inject the Querier via `SemanticHandler` constructor in main.go. Keep the request/response JSON shape backward-compatible so ghola-mcp clients don't break.

Existing `handler/semantic_test.go` will need updating — its expectations about `concept`/`content` fields in responses must go, and whatever remains should exercise the real Querier against the compose stack (move to e2e if needed).

**Commit:** `feat(ch-server): semantic.Querier — recall via mentat pool + cosine`

---

### Task 1.9: Full-stack e2e smoke

**Files:**
- Create: `_chapterhouse/ch-server/test/e2e/predictive_vertical_test.go`
- Modify: `Makefile` (root or ch-server — whichever hosts existing `smoke-*` targets)

This is the one test that earns its place for PR 1's plumbing: it brings up the real stack and exercises every wire.

```go
//go:build e2e

package e2e

import (
    "context"
    "testing"
    "time"

    "github.com/google/uuid"
    "github.com/stretchr/testify/require"
)

// Requires: make smoke-predictive-up (compose up postgres melange mentat ch-init chapterhouse)
func TestPredictiveReplayVerticalSlice(t *testing.T) {
    ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
    defer cancel()

    ch := newTestChapterhouseClient(t) // existing helper if present, else inline
    wsid := uuid.MustParse("00000000-0000-0000-0000-000000000010")
    uid  := uuid.MustParse("00000000-0000-0000-0000-000000000001")

    sid := ch.CreateSession(t, ctx, wsid, uid)
    ch.AppendEvent(t, ctx, sid, "user",      "hello world")
    ch.AppendEvent(t, ctx, sid, "assistant", "hi!")
    ch.AppendEvent(t, ctx, sid, "user",      "thanks")
    ch.CloseSession(t, ctx, sid)

    // Reconciler runs every 30s; poll up to 60s for l1_embedding.
    deadline := time.Now().Add(60 * time.Second)
    for {
        if ch.SessionHasL1(t, ctx, sid) { break }
        require.True(t, time.Now().Before(deadline), "l1_embedding never landed")
        time.Sleep(2 * time.Second)
    }

    // Recall returns 200 with 0 hits (no mnemes clustered yet), not an error.
    hits := ch.SemanticQuery(t, ctx, wsid, "thanks")
    require.Empty(t, hits)
}
```

Makefile:

```make
smoke-predictive:
	cd deploy/docker-compose && docker compose up -d --build postgres melange mentat ch-init chapterhouse
	cd _chapterhouse/ch-server && go test -tags=e2e ./test/e2e/ -run TestPredictiveReplayVerticalSlice -v
	cd deploy/docker-compose && docker compose down
```

Run `make smoke-predictive` locally. Iterate any plumbing bugs surfaced.

**Commit + open PR 1:**

```bash
git commit -m "test(ch-server): e2e smoke for predictive-replay vertical slice"
gh pr create --title "feat: predictive-replay v1a — vertical slice"
```

---

## PR 2 — Import Tool + JSONL-Family Adapter (1–2 days)

### Task 2.1: `cmd/import-logs` skeleton + source-agnostic ingestor

**Files:**
- Create: `cmd/import-logs/main.go`
- Create: `internal/importlogs/adapter.go`
- Create: `internal/importlogs/ingestor.go`

The interesting thing in this package is `DeriveSessionID` — the idempotency invariant. Write a one-line sanity test for that; skip tests for flag parsing or CLI wiring.

**`adapter.go`:**

```go
// Package importlogs is the multi-source bootstrap tool's shared core:
// the Adapter interface, the NormalizedSession shape every adapter
// produces, and idempotent chapterhouse ingestion. Per-source adapters
// (jsonl-family, augment, codex-cli, hermes, cline, opencode) live
// under internal/importlogs/adapters/.
package importlogs

import (
    "crypto/sha256"
    "encoding/hex"
    "iter"
    "time"

    "github.com/google/uuid"
)

var namespaceOID = uuid.MustParse("6ba7b812-9dad-11d1-80b4-00c04fd430c8")

// DeriveSessionID is a pure function of (tool, raw bytes) so --resume
// can cheaply detect "already imported."
func DeriveSessionID(sourceTool string, rawBytes []byte) uuid.UUID {
    h := sha256.Sum256(rawBytes)
    return uuid.NewSHA1(namespaceOID, []byte(sourceTool+"|"+hex.EncodeToString(h[:])))
}

type SessionFile struct {
    Path    string
    RawSize int64
}

type NormalizedEvent struct {
    Type      string // user | assistant | tool_result | system
    Text      string
    Timestamp time.Time
    Metadata  map[string]string
}

type NormalizedSession struct {
    SourceTool, SourceMachine string
    SessionID                 uuid.UUID
    UserID                    uuid.UUID
    StartedAt                 time.Time
    EndedAt                   *time.Time
    Cwd, GitBranch            *string
    AgentKind                 string
    Events                    []NormalizedEvent
}

type Adapter interface {
    Name() string
    Walk(root string) iter.Seq[SessionFile]
    Parse(sf SessionFile) (*NormalizedSession, error)
}
```

**One test, because the invariant is load-bearing:**

```go
// adapter_test.go
func TestDeriveSessionID(t *testing.T) {
    raw := []byte(`{"some":"session"}`)
    require.Equal(t, DeriveSessionID("claude-code", raw), DeriveSessionID("claude-code", raw),
        "same input → same uuid")
    require.NotEqual(t, DeriveSessionID("claude-code", raw), DeriveSessionID("openclaw", raw),
        "different tool → different uuid")
}
```

**`ingestor.go`:** streams `adapter.Walk`, calls `Parse`, for each normalized session checks `GET /v1/episodic/sessions/{id}` → 404 means "ingest," 200 means "skip (resume)." Batches POSTs to `/v1/episodic/sessions` + `/v1/episodic/events/batch`. Verify actual endpoint names by reading `handler/episodic.go` first.

**`main.go`:** flag parsing + dispatch. `--source=kind:path` joined form (cleaner than pair-matching). `--dry-run` parses and counts without writing. `--resume` is the default (opt-out via `--resume=false`). `--batch-size` gates how many sessions per HTTP POST.

**Commit:** `feat(import-logs): skeleton + adapter contract + source-agnostic ingestor`

---

### Task 2.2: `jsonl-family` adapter

**Files:**
- Create: `internal/importlogs/adapters/jsonlfamily/adapter.go`
- Create: `internal/importlogs/adapters/jsonlfamily/adapter_test.go`
- Create: `internal/importlogs/adapters/jsonlfamily/testdata/*.jsonl`

Inspect the actual format first — these have varied historically:

```bash
ls bootstrap-data/ai-session-logs-2026-04-23/
head -5 bootstrap-data/ai-session-logs-2026-04-23/<sample>/conversation.jsonl
ls ~/.claude/projects/ | head
head -5 ~/.claude/projects/<first>/<first>.jsonl
```

Document the observed shape at the top of `adapter.go` with 3–5 example lines commented in. This comment ages better than a prose description.

Implement:
- First line often has `type: session` / `session_meta` header. If missing, derive `SessionID` from file path + file mtime + raw bytes hash.
- Subsequent lines are events. Map JSONL `role`/`type` onto the 4 normalized types (`user`, `assistant`, `tool_result`, `system`).
- Timestamps: prefer an explicit field; fall back to line-order-derived synthetic timestamps if absent.

**Test (earns its place — format drift is real and caught late without fixtures):**

```go
func TestJSONLFamily_ParsesCanonicalSession(t *testing.T) {
    a := &Adapter{}
    got, err := a.Parse(importlogs.SessionFile{Path: "testdata/canonical.jsonl"})
    require.NoError(t, err)
    require.Equal(t, "claude-code", got.SourceTool)
    require.GreaterOrEqual(t, len(got.Events), 2)
    require.Equal(t, "user", got.Events[0].Type)
    require.False(t, got.Events[0].Timestamp.IsZero())
}
```

Fixture: copy a real-but-small session from bootstrap-data; scrub secrets via manual inspection.

Register the adapter in the `main.go` registry.

**Smoke run (manual, before committing):**

```bash
go run ./cmd/import-logs \
  --source=jsonl-family:/home/loganb/ghola/bootstrap-data/ai-session-logs-2026-04-23 \
  --workspace=00000000-0000-0000-0000-000000000010 \
  --user=00000000-0000-0000-0000-000000000001 \
  --dry-run
```

Verify sensible session/event counts. No writes yet. Then run without `--dry-run` against the compose stack; re-run once to confirm `--resume` skips everything.

**Commit:** `feat(import-logs): jsonl-family adapter for claude-code/openclaw/pi logs`

---

### Task 2.3: Real-data integration test (non-optional gate for PR 2)

**Files:**
- Create: `cmd/import-logs/integration_test.go`

```go
//go:build integration

func TestImport_Bootstrap2026_04_23(t *testing.T) {
    // Requires: compose up + bootstrap-data present on disk.
    // Run importer, assert:
    //   - N sessions inserted > some baseline from `find ... | wc -l`
    //   - 0 errors
    //   - Re-run with --resume inserts 0
    //   - After reconciler grace period, some sessions have l1_embedding NOT NULL
}
```

Open PR 2.

---

## PR 3 — Remaining Adapters (2–3 days; split possible)

One commit per adapter. Each is the same template: inspect real format, document observed shape in adapter.go, implement, one test against a scrubbed real fixture, register, manual smoke.

### Task 3.1: `augment` — `sessions/*.json`, `chatHistory[].exchange.{request,response}`. Skip `checkpoint-documents/`.

### Task 3.2: `codex-cli` — JSONL with `session_meta`. Likely reuses helpers from jsonl-family; extract to `internal/importlogs/jsonl/` only if the duplication is real, not speculative.

### Task 3.3: `hermes` — single JSON per session, `messages` array (OpenAI-chat-style).

### Task 3.4: `cline` — JSON array of Anthropic-messages-API shape.

### Task 3.5: `opencode` — SQLite (`session ⟵ message ⟵ part`). Pure-Go driver:

```
require modernc.org/sqlite v1.33.0
```

Parse query:

```sql
SELECT s.id, s.started_at, s.ended_at, s.cwd, m.role, m.ts, p.content_text
FROM session s
JOIN message m ON m.session_id = s.id
JOIN part    p ON p.message_id = m.id
WHERE p.content_text IS NOT NULL
ORDER BY s.id, m.ts, p.seq
```

Walk by `session.id` groups.

### Task 3.6: Bootstrap inventory

After all five adapters land, run `--dry-run` across every source present on the machine. Record counts in `docs/predictive-replay/bootstrap-inventory.md`. Provenance for validation later.

**Commit:** `docs: v1a bootstrap inventory`

Open PR 3.

---

## PR 4 — Stage C: Real Clustering (1 day)

### Task 4.1: HDBSCAN in mentat + `/v1/cluster` endpoint (goes live here, not earlier)

**Files:**
- Create: `mentat/mentat/clustering.py`
- Create: `mentat/mentat/mnemes.py`
- Create: `mentat/tests/test_clustering.py`
- Modify: `mentat/mentat/app.py` (add `/v1/cluster` + its schemas)
- Modify: `mentat/mentat/schemas.py` (add `ClusterRequest`, `ClusterResponse`)

**`clustering.py`:** HDBSCAN on precomputed cosine-distance matrix. See design doc for parameters (`min_cluster_size ≥ 3`).

```python
import hdbscan
import numpy as np
from dataclasses import dataclass
from uuid import UUID

@dataclass
class ClusterResult:
    labels: list[int]
    n_clusters: int
    member_ids_by_label: dict[int, list[UUID]]
    centroids_by_label: dict[int, np.ndarray]

def cluster_embeddings(
    embeddings: np.ndarray, ids: list[UUID], min_cluster_size: int = 3,
) -> ClusterResult:
    norms = np.linalg.norm(embeddings, axis=1, keepdims=True).clip(min=1e-12)
    normed = embeddings / norms
    dist = np.clip(1.0 - normed @ normed.T, 0.0, 2.0)
    labels = hdbscan.HDBSCAN(
        min_cluster_size=min_cluster_size, metric="precomputed"
    ).fit_predict(dist).tolist()

    members: dict[int, list[UUID]] = {}
    for i, lbl in enumerate(labels):
        if lbl >= 0:
            members.setdefault(lbl, []).append(ids[i])
    centroids: dict[int, np.ndarray] = {}
    for lbl, mids in members.items():
        idxs = [i for i, l in enumerate(labels) if l == lbl]
        c = embeddings[idxs].mean(axis=0)
        centroids[lbl] = c / np.linalg.norm(c).clip(min=1e-12)
    return ClusterResult(labels, len(members), members, centroids)
```

**One test, because HDBSCAN params are easy to get wrong in a way that silently degrades:**

```python
def test_cluster_embeddings_finds_three_blobs():
    rng = np.random.default_rng(0)
    centers = rng.normal(size=(3, 1024))
    embs = []
    ids = []
    for c in centers:
        for _ in range(20):
            embs.append(c + rng.normal(scale=0.1, size=1024))
            ids.append(uuid4())
    res = cluster_embeddings(np.array(embs, dtype=np.float32), ids)
    assert res.n_clusters == 3
    for mids in res.member_ids_by_label.values():
        assert len(mids) >= 3
```

**`mnemes.py`:** reinforcement-aware upsert. For each new cluster, find any existing `level=1` mneme whose `member_ids` overlap the new cluster — if found, UPDATE (new centroid, extend member_ids, bump `last_reinforced_at`, bump confidence via Bayesian rule); else INSERT.

```python
import psycopg
from pgvector.psycopg import register_vector
from uuid import UUID
from .clustering import ClusterResult

def upsert_mnemes_from_cluster(dsn: str, workspace_id: UUID, result: ClusterResult) -> int:
    with psycopg.connect(dsn) as conn:
        register_vector(conn)
        with conn.transaction(), conn.cursor() as cur:
            upserted = 0
            for lbl, member_ids in result.member_ids_by_label.items():
                centroid = result.centroids_by_label[lbl]
                mids = [str(m) for m in member_ids]
                cur.execute("""
                    SELECT id FROM semantic.mnemes
                    WHERE workspace_id = %s AND level = 1
                      AND member_ids && %s::uuid[]
                    ORDER BY array_length(member_ids & %s::uuid[], 1) DESC NULLS LAST
                    LIMIT 1
                """, (str(workspace_id), mids, mids))
                row = cur.fetchone()
                if row:
                    cur.execute("""
                        UPDATE semantic.mnemes
                        SET embedding = %s, member_ids = %s::uuid[],
                            last_reinforced_at = now(),
                            confidence = LEAST(0.99, confidence + 0.05)
                        WHERE id = %s
                    """, (centroid, mids, row[0]))
                else:
                    cur.execute("""
                        INSERT INTO semantic.mnemes
                            (workspace_id, level, embedding, member_ids, confidence)
                        VALUES (%s, 1, %s, %s::uuid[], 0.5)
                    """, (str(workspace_id), centroid, mids))
                upserted += 1
            return upserted
```

**`/v1/cluster`:** pulls `episodic.sessions.l1_embedding` for the workspace via psycopg + pgvector's type adapter, feeds into `cluster_embeddings`, calls `upsert_mnemes_from_cluster`. Returns real counts.

**Integration test, earns its place — the whole point of the endpoint is "cluster produces usable mnemes":**

`tests/test_cluster_endpoint.py` (`@pytest.mark.integration`) — requires postgres from compose stack. Seeds L1 embeddings via direct SQL, calls `/v1/cluster`, asserts mneme rows created with correct shape.

### Task 4.2: Go client method + scheduler

**Files:**
- Modify: `_chapterhouse/ch-server/internal/mentat/client.go` (add `Cluster` method + types)
- Create: `_chapterhouse/ch-server/internal/semantic/scheduler.go`
- Modify: `cmd/ch-server/main.go` (wire scheduler)

Scheduler runs daily at configured hour:min (design doc says 02:00), calls `mentat.Client.Cluster` for each configured workspace. Use `time.Timer` + `context.Context`; don't invent a testable abstraction — scheduler correctness is verified by the next day's mneme rows appearing.

**Commit + open PR 4.**

---

## PR 5 — Stage A: Event-Mask Pretraining (3–5 days)

Largest PR. Heavy CUDA use. Runs on this workstation's Blackwell GPU.

### Task 5.1: `EventMaskPredictor` PyTorch module

**File:** `mentat/mentat/models/event_mask.py`

```python
import torch
import torch.nn as nn

class EventMaskPredictor(nn.Module):
    """MLP dim→hidden→dim with residual + LayerNorm.

    Predicts a masked event's embedding from the pooled visible context.
    ~1.5M params at (1024, 512).
    """
    def __init__(self, dim: int = 1024, hidden: int = 512):
        super().__init__()
        self.fc1 = nn.Linear(dim, hidden)
        self.act = nn.GELU()
        self.fc2 = nn.Linear(hidden, dim)
        self.ln  = nn.LayerNorm(dim)

    def forward(self, x: torch.Tensor) -> torch.Tensor:
        h = self.fc2(self.act(self.fc1(x)))
        return self.ln(x + h)
```

No separate unit test — the module is simple and Task 5.4's training-loop validation is what matters.

**Commit:** `feat(mentat): EventMaskPredictor module`

### Task 5.2: LeJEPA SIGReg regularizer

**File:** `mentat/mentat/models/sigreg.py` (reference: arXiv 2511.08544 appendix — ~30 LOC)

Implement per the paper: normalize representations, penalize variance deviation from 1 (isotropy), penalize off-diagonal of the per-dim correlation matrix (decorrelation), weighted sum.

**One test, because getting the regularizer wrong silently makes training look-fine-but-collapses:**

```python
def test_sigreg_low_for_isotropic_batch():
    torch.manual_seed(0)
    x = torch.randn(512, 64)  # isotropic Gaussian
    assert sigreg(x).item() < 0.1

def test_sigreg_high_for_collapsed_batch():
    x = torch.ones(512, 64)  # fully collapsed
    assert sigreg(x).item() > 1.0
```

**Fallback approved by design doc:** if LeJEPA underperforms during Stage A training, drop in `vicreg.py` (arXiv 2105.04906). Identical interface (scalar loss), so only the import in the trainer changes. Budget 2 extra days if triggered.

**Commit:** `feat(mentat): LeJEPA SIGReg regularizer`

### Task 5.3: Stage A trainer — data loader + training loop

**Files:**
- Create: `mentat/mentat/training/__init__.py`
- Create: `mentat/mentat/training/data_pg.py`
- Create: `mentat/mentat/training/stage_a.py`

`data_pg.py`: PyTorch `Dataset` reading sessions from postgres. Yields `(pooled_visible, masked_target)` pairs. 40% mask per session, mixed 20% contiguous + 20% scattered. Type-weighted mask bias (prefer masking user/assistant events over system). 10% session-level event dropout.

`stage_a.py`:

```python
import torch
from ..models.event_mask import EventMaskPredictor
from ..models.sigreg import sigreg

class StageATrainer:
    def __init__(self, dim=1024, hidden=512, lambda_sigreg=0.1, device="cuda"):
        self.predictor = EventMaskPredictor(dim, hidden).to(device)
        self.opt = torch.optim.AdamW(self.predictor.parameters(), lr=1e-4)
        self.device = device
        self.lambda_sigreg = lambda_sigreg

    def step(self, batch):
        pooled, masked = batch["pooled"].to(self.device), batch["masked"].to(self.device)
        pred = self.predictor(pooled)
        cs = torch.nn.functional.cosine_similarity(pred, masked, dim=-1).mean()
        inv = 1.0 - cs
        reg = sigreg(pred)
        loss = inv + self.lambda_sigreg * reg
        self.opt.zero_grad(); loss.backward(); self.opt.step()
        return {"loss": loss.item(), "inv": inv.item(), "sigreg": reg.item(), "cosine": cs.item()}

    def fit(self, dataset, epochs: int, log_every: int = 100):
        dl = torch.utils.data.DataLoader(dataset, batch_size=64, shuffle=True, drop_last=True)
        for epoch in range(epochs):
            for i, batch in enumerate(dl):
                m = self.step(batch)
                if i % log_every == 0:
                    print(f"epoch {epoch} step {i}: {m}")
```

Note: Stage 1 pooler has zero parameters so it's not in the optimizer. PR 8 changes this when AttentionPool ships.

**No unit test for the trainer** — the thing that matters is Task 5.5's Metric 1 gate against real data. A synthetic-data test for "does the loss go down" would be a weaker version of the same check.

**Commit:** `feat(mentat): Stage A trainer skeleton (predictor + LeJEPA loss)`

### Task 5.4: `/v1/train` endpoint + weights versioning

**Files:**
- Create: `mentat/mentat/training/jobs.py`
- Modify: `mentat/mentat/app.py` (add `/v1/train`, `/v1/training/{job_id}`)
- Modify: `mentat/mentat/schemas.py` (`TrainRequest`, `TrainResponse`, job status schema)

`jobs.py`: in-memory registry keyed by job UUID. POST `/v1/train` kicks off a background `ThreadPoolExecutor` task (PyTorch threads off the event loop). On Stage A completion:

1. `version = f"v{int(time.time())}"`
2. `weights_dir = Path(settings.weights_root) / version; weights_dir.mkdir()`
3. `torch.save(predictor.state_dict(), weights_dir / "event_predictor.pt")`
4. Write `metadata.json` with `version`, `trained_at`, `metric_1_cosine`, `metric_1_baseline`.
5. `weights.flip_to(settings.weights_root, version)`
6. Reload the global predictor under a lock.

GET `/v1/training/{job_id}` returns `{status: queued|running|done|failed, metrics: {...}}`.

**Commit:** `feat(mentat): /v1/train endpoint with weights versioning + atomic flip`

### Task 5.5: Metric 1 validation harness (gates the PR)

**Files:**
- Create: `mentat/mentat/validation/metrics.py`
- Create: `mentat/mentat/validation/cli.py`

Metric 1 per design doc: event-mask reconstruction cosine vs random-event baseline on held-out 20% — reconstruction > random by ≥ 2σ on 100+ sessions.

This is the gate. Run the trainer against real bootstrap data; run the metric harness; assert threshold; ship PR 5 only if it passes. Document the run + numbers in the PR description.

**Commit:** `feat(mentat): Metric 1 harness — Stage A ship gate`

Open PR 5. If LeJEPA fails the gate, fall back to VICReg per Task 5.2; re-run; ship once metric passes.

---

## PR 6 — Stage B: Session-Level Predictor (2–3 days)

### Task 6.1: `SessionPredictor` module

**File:** `mentat/mentat/models/session_pred.py`. Same shape as `EventMaskPredictor` (1024→512→1024 with residual + LayerNorm). Same training interface.

### Task 6.2: Stage B trainer + Metric 2

**Files:**
- Create: `mentat/mentat/training/stage_b.py`
- Modify: `mentat/mentat/training/jobs.py` (add stage B to the pipeline)
- Extend: `mentat/mentat/validation/metrics.py` (add Metric 2)

Pooler is frozen from Stage A (no gradient). Form `(L1_t, L1_{t+1})` pairs from consecutive sessions, ordered by `started_at` within each user. Loss: `1 - cos(pred, target) + λ·sigreg(pred_batch)`.

Save `session_predictor.pt` into the same version directory as Stage A's output. Update `metadata.json` to record both metrics.

Metric 2: next-session cosine vs random-session baseline, ≥ 2σ. Gates the PR.

### Task 6.3: `/v1/predict` uses the trained predictor when weights are present

**File:** modify `mentat/mentat/predictor.py` + `mentat/mentat/app.py`.

Replace the module-level `identity_predict` call with a predictor that checks `weights_loader.load_current()`:
- `cold_start=True` → identity (last history element, unchanged).
- `cold_start=False` → load SessionPredictor state, run on `history[-1]` (or short window; keep v1a simple with just-last).

**Commit + open PR 6.**

---

## PR 7 — Validation Harness + v1a Ship Gate (1–2 days)

### Task 7.1: Metric 3 (cluster coherence)

Extend `validation/metrics.py`: for the top-3 largest clusters, per-cluster median intra-cosine > median inter-cluster cosine (against the nearest other cluster).

### Task 7.2: `--compare-pipelines` A/B in recall path

**Files:**
- Modify: `_chapterhouse/ch-server/internal/handler/semantic.go` (accept `compare_pipelines: true`)
- Modify: `_chapterhouse/ch-server/internal/semantic/query.go` (run both old episodic-cosine path and new L1-predictive path)

Merged response tags each hit `source: "old"|"new"`. Log merged response bodies to a local JSONL at `$CHAPTERHOUSE_AB_LOG`. Not automated; weekly manual review.

### Task 7.3: Dogfooding tags

**Files:**
- Create: migration `004_recall_feedback.sql` (`episodic.recall_feedback` table)
- Modify: `internal/mcp/` in ghola — add four tools: `recall_surprise_positive`, `recall_miss_obvious`, `recall_useful_unprompted`, `recall_noisy`

Each tool writes a row with `(recall_id, tag, user_id, workspace_id, at, note)`. Used for v1b steering; does not gate v1a.

### Task 7.4: v1a ship gate script

**File:** `scripts/ship-gate.sh`

```bash
#!/usr/bin/env bash
set -euo pipefail

echo "== Metric harness (1, 2, 3) =="
python -m mentat.validation --workspace "${WORKSPACE_ID}" --dsn "${MENTAT_DATABASE_DSN}"

echo "== Phase 11 e2e gates 1, 2, 4, 6 =="
cd /home/loganb/ghola
make smoke-predictive
./test/phase11/run.sh gates 1 2 4 6

echo "== Full bootstrap dry-run =="
go run ./cmd/import-logs --config config/v1a-bootstrap.yaml --dry-run

echo "v1a ship gate: GREEN"
```

### Task 7.5: Docs

**Files:**
- Modify: `README.md` (point at the design doc)
- Modify: `_chapterhouse/ch-server/MEMORY_SYSTEM_GUIDE.md` (reflect v0.3 shape — no more `concept`/`content`)
- Modify: `_chapterhouse/ch-server/RUNBOOK.md` (add "re-train weights" and "roll back weights" runbooks)

**Commit + open PR 7.**

---

## PR 8 (post-v1a, conditional) — AttentionPool Stage 2 (2 days)

Ships only if AttentionPool beats Stage 1 pooler on Metric 1/2 with the same data.

### Task 8.1: `AttentionPool` module

**File:** `mentat/mentat/models/attention_pool.py`

```python
import torch
import torch.nn as nn

class AttentionPool(nn.Module):
    """Single learned query vector; softmax attention over event embeddings.

    ~1024 params (the query). Stage 2 pooler; replaces type-weighted mean
    when Metric 1/2 show improvement.
    """
    def __init__(self, dim: int = 1024):
        super().__init__()
        self.query = nn.Parameter(torch.randn(dim) / dim**0.5)

    def forward(self, events: torch.Tensor) -> torch.Tensor:
        # events: (B, N, D) → (B, D)
        scores = torch.einsum("bnd,d->bn", events, self.query)
        weights = torch.softmax(scores, dim=-1)
        pooled = torch.einsum("bn,bnd->bd", weights, events)
        return torch.nn.functional.normalize(pooled, dim=-1)
```

### Task 8.2: Re-run Stage A with AttentionPool jointly in the optimizer

Flag `--pool=mean|attention`. Rerun; compare Metric 1/2. Ship as `v1a.1` if better; drop otherwise.

### Task 8.3: Compose env controls pooler

`MENTAT_POOLER={mean|attention}` in compose; default to whichever won.

---

## Final Checklist

- [ ] PR 1: vertical slice live; `/v1/health` reports `cold_start=true`; reconciler pools closed sessions
- [ ] PR 2: jsonl-family corpus ingested idempotently; integration test green
- [ ] PR 3: all five remaining adapters (augment, codex-cli, hermes, cline, opencode) shipped
- [ ] PR 4: HDBSCAN produces `level=1` mnemes on real L1 data
- [ ] PR 5: Stage A training run completes; Metric 1 ≥ 2σ on held-out
- [ ] PR 6: Stage B training run completes; Metric 2 ≥ 2σ on held-out
- [ ] PR 7: ship-gate script green on full bootstrap; docs updated; compare-pipelines A/B live
- [ ] PR 8 (conditional): AttentionPool Metric 1/2 ≥ Stage 1 → ship v1a.1

**Rollback story:** `current` symlink stays on last-green version; no PR's success depends on the next shipping. `internal/replay/` deletion in PR 1 is irreversible by git alone, but the pipeline never ran in production.

**Realistic wall-clock:** 3–4 weeks per design doc. Add 2 days for VICReg fallback if LeJEPA underperforms in PR 5.
