# pg_recall Extension — Implementation Plan

## Scope

Build the pg_recall Postgres extension. This plan covers **only** the extension itself — a self-contained Rust/pgrx project that ships as `CREATE EXTENSION pg_recall`.

Chapterhouse integration, MCP server work, and deployment are **explicitly out of scope**. The extension is the product.

---

## Toolchain

| Dependency | Version | Notes |
|------------|---------|-------|
| Rust | 1.94+ | Stable toolchain |
| pgrx | 0.17.x | With `pg18` feature flag |
| PostgreSQL | 18.3+ | Target runtime |
| pgvector | 0.8.2+ | Required dependency (`requires = 'vector'`) |

---

## Tasks

### 1. Project Scaffolding

**Goal**: Bootable pgrx project that compiles and passes `CREATE EXTENSION pg_recall`.

- Initialize with `cargo pgrx init`
- Configure `Cargo.toml`: pgrx 0.17.x, `pg18` feature, pgvector crate for Rust interop
- Create `pg_recall.control`: version 0.1.0, schema pg_recall, requires vector
- Create `src/lib.rs` with pgrx extension macro, schema creation, bgworker registration stub
- Verify: `cargo pgrx run` -> `CREATE EXTENSION pg_recall` -> schema exists

**File structure**:
```
pg_recall/
  Cargo.toml
  pg_recall.control
  src/
    lib.rs
    scoring.rs
    recall.rs
    hebbian.rs
    types.rs
```

**Dependencies**: None. First task.

---

### 2. Custom Types

**Goal**: Define `recall_result` and `score_weights` as PostgreSQL composite types.

**`recall_result`**: mneme_id (uuid), score (float), content_match (float), activation (float), hebbian_boost (float), confidence (float), concept (text), content (text)

**`score_weights`**: semantic (float, default 0.6), fts (float, default 0.4), actr_decay (float, default 0.5), hebbian_scale (float, default 4.0)

- Implement in `src/types.rs` with `#[derive(PostgresType)]`
- Verify: `SELECT '(0.6,0.4,0.5,4.0)'::pg_recall.score_weights`

**Dependencies**: Requires 1.

---

### 3. Scoring Primitives

**Goal**: Implement the four cognitive scoring functions. Pure math, no DB access, no side effects.

**Functions**:

- `actr_activation(access_count int, last_access timestamptz) -> float`
  - `B = ln(n+1) - d * ln(max(age_days, 1/1440) / (n+1))` where d=0.5
- `ebbinghaus_decay(last_access timestamptz, access_count int, created_at timestamptz) -> float`
  - Spacing-aware stability, exponential decay, floor at 0.05
- `bayesian_update(prior float, evidence float) -> float`
  - Bayes' rule with Laplace smoothing, result in [0.025, 0.975]
- `softplus(x float) -> float`
  - `ln(1 + exp(x))`, overflow guard at x > 20

All marked `#[pg_extern(immutable, parallel_safe, schema = "pg_recall")]`.

**Tests** (Rust unit tests + pgrx integration tests):
- `actr_activation(13, now() - '10 days') ~ 2.08`
- `actr_activation(0, now() - '1400 days') ~ -3.27`
- `ebbinghaus_decay(now()-'30d', 50, now()-'180d') ~ 0.78`
- `ebbinghaus_decay(now()-'30d', 50, now()-'1d') ~ 0.12`
- `bayesian_update(0.5, 0.95) ~ 0.925`
- `bayesian_update(0.8, 0.10) ~ 0.32`
- `softplus(0.0) ~ 0.6931`
- `softplus(25.0) = 25.0`

**Dependencies**: Requires 1. Parallel with 2 and 4.

---

### 4. Schema

**Goal**: `CREATE EXTENSION pg_recall` produces all tables, indexes, and constraints.

**Tables**:
- `pg_recall.mnemes` — id (uuid PK), workspace_id (uuid), concept (text), content (text), embedding vector(384), search_vector (generated tsvector), confidence (float, default 0.5), access_count (int), last_access (timestamptz), created_at (timestamptz), state (text, CHECK active/archived/dormant)
- `pg_recall.associations` — src_id/dst_id (uuid FK, PK), weight (float), co_activations (int), updated_at (timestamptz), CHECK src_id < dst_id
- `pg_recall.co_activation_queue` — id (bigserial PK), workspace_id (uuid), mneme_ids (uuid[]), scores (float[]), created_at (timestamptz)

**Indexes**:
- HNSW on mnemes(embedding) with vector_cosine_ops
- GIN on mnemes(search_vector)
- B-tree on mnemes(workspace_id, last_access DESC)
- B-tree on associations(dst_id, src_id)

**Dependencies**: Requires 1. Parallel with 2 and 3.

---

### 5. Composite Recall Function

**Goal**: `pg_recall.recall()` — full cognitive retrieval pipeline.

Fuses HNSW nearest neighbors + FTS ts_rank into a candidate pool (3x limit), then scores each candidate:
1. `content_match = 0.6 * vec_cosine + 0.4 * tanh(bm25)`
2. `temporal = softplus(actr_activation + 4.0 * hebbian_boost) / (1 + softplus(0))`
3. `score = content_match * temporal * confidence`

Returns top-n as `SETOF pg_recall.recall_result`. Enqueues co-activation event (fire-and-forget INSERT).

- Implement in `src/recall.rs` using SPI
- Handle NULL weights (use defaults)
- Integration tests: ranking reflects cognitive scoring, co-activation enqueued, min_confidence filtering, workspace isolation

**Dependencies**: Requires 2, 3, 4.

---

### 6. Hebbian Operations (SQL Functions)

**Goal**: Helper functions for association and confidence management.

- `record_co_activation(workspace_id uuid, mneme_ids uuid[], scores float[]) -> void` — validates array lengths, INSERT into queue
- `get_associations(mneme_id uuid, min_weight float DEFAULT 0.01) -> SETOF (related_id, weight)` — both directions, ordered by weight DESC
- `update_confidence(mneme_id uuid, evidence float) -> float` — atomic read-update-write via bayesian_update
- `confirm_recall(mneme_ids uuid[]) -> void` — apply evidence=0.95 to each

**Dependencies**: Requires 3 (for bayesian_update), 4 (for schema).

---

### 7. Background Worker

**Goal**: pgrx background worker for Hebbian learning.

**Behavior**:
- Single worker per Postgres instance, starts on extension load
- Polls `co_activation_queue`, batch size up to 100 rows
- Per batch: generate (i,j) pairs, aggregate signal = sum(score_i * score_j), update association weights in log-space: `min(1.0, exp(ln(current) + signal * ln(1.01)))`, cold start at 0.01
- Single transaction: UPDATE associations, DELETE consumed queue rows, UPDATE mnemes access_count + last_access
- Adaptive polling: active (100ms) -> idle >30s (1s) -> dormant >5min (5s)
- Hourly decay: `weight *= 0.999` where stale > 1 day, prune below 0.001
- On shutdown: drain remaining queue

**`worker_stats() -> record`**: state, queue_depth, batches_processed, pairs_updated, last_batch_at, last_decay_at

**This is the most complex component.** Integration tests: associations form from co-activation, weights follow log-space formula, decay prunes weak links, access tracking updates.

**Dependencies**: Requires 3, 4.

---

### 8. Extension Packaging

**Goal**: pg_recall installs cleanly as a standard Postgres extension.

- Finalize `pg_recall.control`: `default_version = '0.1.0'`, `schema = 'pg_recall'`, `requires = 'vector'`
- Generate or author `sql/pg_recall--0.1.0.sql`
- Full test: `CREATE EXTENSION vector; CREATE EXTENSION pg_recall;` -> schema + worker running
- Document installation steps in README

**Dependencies**: Requires 1-7 complete.

---

## Parallelization

```
1. Scaffolding ──┬── 2. Types ─────────────────┐
                  ├── 3. Scoring Primitives ────┤
                  └── 4. Schema ────────────────┤
                                                ├── 5. Recall Function ──┐
                                                └── 6. Hebbian Ops ─────┤
                                                                        ├── 7. Background Worker
                                                                        └── 8. Packaging
```

After scaffolding, tasks 2/3/4 are fully independent and can run in parallel. Tasks 5 and 6 can also run in parallel once their shared dependencies land. Task 7 is the critical path item. Task 8 is the final gate.

---

## Out of Scope

- Chapterhouse integration (separate project, separate plan)
- MCP server layer
- Container image / CNPG deployment
- Contradiction detection, typed associations, graph traversal (v0.2)
- Configurable vector dimensions (v0.1 hardcodes 384)
- Embedding generation (users bring their own)
