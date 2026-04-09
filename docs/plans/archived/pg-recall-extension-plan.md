# pg_ghola Extension — Merge-Optimized Implementation Plan

## Scope

Build the `pg_ghola` Postgres extension as a self-contained Rust/pgrx project
that ships as `CREATE EXTENSION pg_ghola`.

This plan is optimized for one outcome above all others: **getting work merged
reliably**.

Previous plans were coherent, but created too many merge
hotspots:

- `src/lib.rs`
- extension install wiring
- schema SQL
- cross-module registration
- shared integration tests
- background worker lifecycle code

This version reduces those collisions by:

- minimizing shared ownership
- defining file ownership explicitly
- front-loading stubs and extension wiring
- reserving hotspot edits for one integration task
- deferring the autonomous background worker to v0.2

---

## Toolchain

| Dependency | Version | Notes |
|------------|---------|-------|
| Rust | 1.94+ | Stable toolchain |
| pgrx | 0.17.x | With `pg18` feature flag |
| PostgreSQL | 18.3+ | Target runtime |
| pgvector | 0.8.2+ | Required dependency (`requires = 'vector'`) |

---

## Product Shape for v0.1

v0.1 ships the cognitive memory core without autonomous background execution.

### Included in v0.1

- Extension scaffolding and install packaging
- Schema: `mnemes`, `associations`, `co_activation_queue`
- Composite types: `recall_result`, `score_weights`
- Pure scoring primitives:
  - `actr_activation`
  - `ebbinghaus_decay`
  - `bayesian_update`
  - `softplus`
- Composite retrieval:
  - `pg_ghola.recall()`
- Hebbian helper functions:
  - `record_co_activation(workspace_id uuid, mneme_ids uuid[], scores float[])`
  - `get_associations(mneme_id uuid, min_weight float DEFAULT 0.01)`
  - `update_confidence(mneme_id uuid, evidence float)`
  - `confirm_recall(mneme_ids uuid[])`
  - `process_co_activation_batch(batch_limit int DEFAULT 100)`
  - `process_all_pending_co_activations()`

### Deferred to v0.2

- Autonomous background worker
- `shared_preload_libraries` requirement
- adaptive polling state machine
- graceful shutdown draining
- live worker stats table/function
- hourly decay job inside the extension runtime

This is the main scope cut. It removes the largest source of runtime risk and
the largest source of merge contention while preserving the core retrieval and
Hebbian learning model.

---

## Mergeability Rules

These are part of the plan, not optional conventions.

1. One task creates the final file/module skeleton up front.
2. Downstream tasks fill in owned files instead of creating new wiring.
3. `src/lib.rs` is owned only by bootstrap and final integration.
4. The install SQL file is owned only by schema and final integration.
5. End-to-end tests are owned only by final integration.
6. No task may edit another task's primary owned file unless it is the final
   integration task.
7. The background worker is not part of v0.1.

---

## Final File Structure

The bootstrap task should create the final structure immediately so later tasks
merge into existing files instead of inventing layout independently.

```text
pg_ghola/
  Cargo.toml
  pg_ghola.control
  sql/
    pg_ghola--0.1.0.sql
  src/
    lib.rs
    scoring.rs
    recall.rs
    hebbian.rs
    types.rs
  tests/
    integration_smoke.rs
    scoring_sql.rs
    recall_sql.rs
    hebbian_sql.rs
    schema_sql.rs
```

---

## Tasks

### 1. Bootstrap Skeleton

**Goal**: Create a compiling extension skeleton with the final module graph and
placeholder public surface area.

**Owns**:

- `Cargo.toml`
- `pg_ghola.control`
- `src/lib.rs`
- empty module files under `src/`
- test file stubs under `tests/`

**Responsibilities**:

- Initialize the pgrx project
- Configure `pgrx` + `pg18` + Rust dependencies
- Add module declarations in `src/lib.rs`
- Create stub exports for all public functions and types referenced by later
  tasks
- Ensure the project compiles before any feature work begins

**Done when**:

- `cargo pgrx run` boots
- `CREATE EXTENSION pg_ghola` succeeds with placeholder behavior
- all final files already exist in the repository

**Dependencies**: None. This task must finish first.

---

### 2. Custom Types

**Goal**: Define the SQL-visible composite types.

**Owns**:

- `src/types.rs`
- tests for composite type visibility/casting

**Types**:

**`recall_result`**
- `mneme_id uuid`
- `score float`
- `content_match float`
- `activation float`
- `hebbian_boost float`
- `confidence float`
- `concept text`
- `content text`

**`score_weights`**
- `semantic float` default `0.6`
- `fts float` default `0.4`
- `actr_decay float` default `0.5`
- `hebbian_scale float` default `4.0`

**Done when**:

- types are exposed in schema `pg_ghola`
- SQL casts work
- downstream tasks can import the Rust definitions without changing ownership

**Dependencies**: Requires 1.

---

### 3. Scoring Primitives

**Goal**: Implement the pure cognitive scoring functions.

**Owns**:

- `src/scoring.rs`
- scoring unit tests
- scoring SQL integration tests

**Functions**:

- `actr_activation(access_count int, last_access timestamptz) -> float`
  - `B = ln(n+1) - d * ln(max(age_days, 1/1440) / (n+1))` where `d = 0.5`
- `ebbinghaus_decay(last_access timestamptz, access_count int, created_at timestamptz) -> float`
  - spacing-aware stability, exponential decay, floor at `0.05`
- `bayesian_update(prior float, evidence float) -> float`
  - Bayes' rule with Laplace smoothing, result in `[0.025, 0.975]`
- `softplus(x float) -> float`
  - `ln(1 + exp(x))`, overflow guard at `x > 20`

All marked `#[pg_extern(immutable, parallel_safe, schema = "pg_ghola")]`.

**Reference checks**:

- `actr_activation(13, now() - '10 days') ~ 2.08`
- `actr_activation(0, now() - '1400 days') ~ -3.27`
- `ebbinghaus_decay(now()-'30d', 50, now()-'180d') ~ 0.78`
- `ebbinghaus_decay(now()-'30d', 50, now()-'1d') ~ 0.12`
- `bayesian_update(0.5, 0.95) ~ 0.925`
- `bayesian_update(0.8, 0.10) ~ 0.32`
- `softplus(0.0) ~ 0.6931`
- `softplus(25.0) = 25.0`

**Dependencies**: Requires 1.

---

### 4. Schema SQL

**Goal**: `CREATE EXTENSION pg_ghola` produces all tables, indexes, and
constraints needed for v0.1.

**Owns**:

- `sql/pg_ghola--0.1.0.sql`
- schema verification tests

**Tables**:

- `pg_ghola.mnemes`
  - `id uuid PK`
  - `workspace_id uuid`
  - `concept text`
  - `content text`
  - `embedding vector(384)`
  - `search_vector generated tsvector`
  - `confidence float default 0.5`
  - `access_count int`
  - `last_access timestamptz`
  - `created_at timestamptz`
  - `state text CHECK active/archived/dormant`
- `pg_ghola.associations`
  - `src_id/dst_id uuid FK, PK`
  - `weight float`
  - `co_activations int`
  - `updated_at timestamptz`
  - `CHECK src_id < dst_id`
- `pg_ghola.co_activation_queue`
  - `id bigserial PK`
  - `workspace_id uuid`
  - `mneme_ids uuid[]`
  - `scores float[]`
  - `created_at timestamptz`

**Indexes**:

- HNSW on `mnemes(embedding)` with `vector_cosine_ops`
- GIN on `mnemes(search_vector)`
- B-tree on `mnemes(workspace_id, last_access DESC)`
- B-tree on `associations(dst_id, src_id)`

**Done when**:

- install SQL creates schema cleanly
- constraints and indexes are present
- no worker-specific tables are required for v0.1

**Dependencies**: Requires 1.

---

### 5. Hebbian SQL Helpers

**Goal**: Implement explicit SQL-callable association and queue processing
functions.

**Owns**:

- `src/hebbian.rs`
- Hebbian helper tests

**Functions**:

- `record_co_activation(workspace_id uuid, mneme_ids uuid[], scores float[]) -> void`
  - validates array lengths
  - inserts into `co_activation_queue`
- `get_associations(mneme_id uuid, min_weight float DEFAULT 0.01) -> SETOF (related_id, weight)`
  - returns both directions ordered by weight desc
- `update_confidence(mneme_id uuid, evidence float) -> float`
  - atomic read-update-write via `bayesian_update`
- `confirm_recall(mneme_ids uuid[]) -> void`
  - applies evidence `0.95` to each
- `process_co_activation_batch(batch_limit int DEFAULT 100) -> bigint`
  - pulls a batch from `co_activation_queue`
  - computes canonical `(i, j)` pair signals
  - UPSERTs `associations`
  - updates `mnemes.access_count` and `mnemes.last_access`
  - deletes consumed queue rows
  - returns rows processed
- `process_all_pending_co_activations() -> bigint`
  - repeatedly calls batch processing until queue is empty
  - returns total rows processed

**Processing formula**:

- aggregate pair signal as `sum(score_i * score_j)`
- update weight with log-space reinforcement
- cold start seeds at `0.01`

**Why this exists in v0.1**:

This gives the extension complete Hebbian behavior without requiring a runtime
worker. Operators can invoke the batch processor from SQL, cron, pg_cron, or an
external scheduler later without changing the data model.

**Dependencies**: Requires 3 and 4.

---

### 6. Composite Recall Function

**Goal**: Implement `pg_ghola.recall()` as the primary retrieval entry point.

**Owns**:

- `src/recall.rs`
- recall-specific integration tests

**Behavior**:

Fuses HNSW nearest neighbors and FTS `ts_rank` into a candidate pool
(`3x limit`), then scores each candidate:

1. `content_match = 0.6 * vec_cosine + 0.4 * tanh(bm25)`
2. `temporal = softplus(actr_activation + 4.0 * hebbian_boost) / (1 + softplus(0))`
3. `score = content_match * temporal * confidence`

Returns top-n as `SETOF pg_ghola.recall_result`.

After ranking, it enqueues a co-activation event via `record_co_activation(...)`.

**Requirements**:

- use SPI for internal reads
- handle NULL weights using defaults
- enforce workspace isolation
- filter on `min_confidence`

**Done when**:

- ranking reflects cognitive scoring
- co-activation events are enqueued
- workspace and confidence filtering behave correctly

**Dependencies**: Requires 2, 3, 4, and 5.

---

### 7. Packaging and Final Integration

**Goal**: Merge the independently owned modules into a releasable extension
without reopening broad ownership.

**Owns**:

- final edits to `src/lib.rs`
- final edits to `pg_ghola.control`
- final end-to-end verification
- `README` installation and usage notes
- end-to-end tests only

**Responsibilities**:

- wire the final exported functions/types into the extension surface
- ensure install SQL, Rust modules, and tests agree on names/signatures
- verify `CREATE EXTENSION vector; CREATE EXTENSION pg_ghola;`
- verify a minimal seeded recall flow end-to-end

**Dependencies**: Requires 2, 3, 4, 5, and 6.

---

## Recommended Parallelization

This plan intentionally uses less parallelism than the original plan.

### Wave 1

- Task 1: Bootstrap Skeleton

### Wave 2

- Task 2: Custom Types
- Task 3: Scoring Primitives
- Task 4: Schema SQL

### Wave 3

- Task 5: Hebbian SQL Helpers

### Wave 4

- Task 6: Composite Recall Function

### Wave 5

- Task 7: Packaging and Final Integration

This is slower in theory and faster in practice because it reduces rework,
conflicting edits, and merge failures.

---

## Critical Path

`1 -> 4 -> 5 -> 6 -> 7`

Task 3 is also near-critical because both Hebbian confidence updates and recall
scoring depend on it.

---

## Why the Worker Is Deferred

The autonomous background worker created three problems at once:

1. runtime complexity
2. test harness complexity
3. merge complexity

It forced concurrent edits to:

- bootstrap wiring
- extension loading behavior
- runtime lifecycle code
- queue processing semantics
- operational observability

That is too much surface area for a first merged release.

The worker is still a good feature. It just should not be part of the first
integration milestone.

---

## v0.2 Follow-On Plan

Once v0.1 is merged and stable, v0.2 can add:

1. background worker skeleton
2. automatic queue polling
3. adaptive polling states
4. periodic decay/pruning
5. graceful shutdown draining
6. worker stats table/function

At that point the queue processing logic already exists and is testable. The
worker becomes a thin automation layer over the existing batch processor rather
than the place where the core logic is first invented.

---

## Success Criteria

v0.1 is successful when:

- `CREATE EXTENSION pg_ghola` installs cleanly
- all schema objects and SQL-visible types exist
- scoring primitives are independently callable
- `pg_ghola.recall()` returns ranked results using vector + FTS + temporal +
  confidence scoring
- co-activation events are queued automatically from recall
- queued events can be processed deterministically through SQL functions
- all feature branches can merge without repeated conflict churn in the same
  hotspot files
