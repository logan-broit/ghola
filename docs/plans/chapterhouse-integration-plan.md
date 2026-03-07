# pg_recall Implementation Plan

## Overview

This plan covers two workstreams that converge:

1. **Build pg_recall** — A Postgres extension (Rust/pgrx) implementing cognitive memory primitives as composable SQL functions over pgvector
2. **Integrate with Chapterhouse** — Modify the Go MCP server to replace Qdrant with pg_recall-powered Postgres, using the Bridge architecture

### Architecture Decision: Bridge

pg_recall ships as a general-purpose extension with its own schema (`pg_recall.mnemes`, `pg_recall.associations`, etc.). Chapterhouse integrates by:

- Adding `embedding vector(384)`, `confidence float`, and a generated `search_vector tsvector` column to its existing `memory_blocks` table
- Adding `associations` and `co_activation_queue` tables in the Chapterhouse schema
- Calling pg_recall's scoring functions (`actr_activation`, `ebbinghaus_decay`, `bayesian_update`, `softplus`) in Chapterhouse's recall queries
- Running pg_recall's background worker for Hebbian processing (pointed at Chapterhouse's tables, or reimplemented in Go within Chapterhouse)

pg_recall stays clean and reusable. Chapterhouse keeps its mature schema (versioning, tiers, scopes, tags, sessions, expiration).

### Infrastructure

- **Postgres**: CNPG operator on k3s homelab cluster, custom image based on `ghcr.io/cloudnative-pg/postgresql:18.3-system-trixie` with pgvector 0.8.2 + pg_recall
- **Rust**: 1.94+ stable, pgrx 0.17.x (with `pg18` feature flag)
- **Embeddings**: Together.ai, BAAI/bge-small-en-v1.5 (384 dimensions)
- **MCP Server**: Chapterhouse (Go), `final-refactor` branch, streamable HTTP

---

## Workstream 1: pg_recall Extension

### 1.1 Project Scaffolding

**Goal**: Bootable pgrx project that compiles, installs, and passes `CREATE EXTENSION pg_recall`.

**Tasks**:
- Initialize pgrx project with `cargo pgrx init`
- Configure `Cargo.toml` with pgrx 0.17.x dependency, `pg18` feature flag, pgvector interop (`pgvector` crate for Rust)
- Create `pg_recall.control` file (extension metadata: version 0.1.0, schema pg_recall, requires vector)
- Create `src/lib.rs` with pgrx extension macro, schema creation, background worker registration
- Verify: `cargo pgrx run` → connect → `CREATE EXTENSION pg_recall` → schema exists

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

**Dependencies**: None. Can start immediately.

---

### 1.2 Custom Types

**Goal**: Define `recall_result` and `score_weights` as PostgreSQL composite types via pgrx.

**`recall_result`**:
```
mneme_id      uuid
score         float
content_match float
activation    float
hebbian_boost float
confidence    float
concept       text
content       text
```

**`score_weights`**:
```
semantic      float  (default 0.6)
fts           float  (default 0.4)
actr_decay    float  (default 0.5)
hebbian_scale float  (default 4.0)
```

**Tasks**:
- Define structs in `src/types.rs` with `#[derive(PostgresType)]`
- Register as SQL composite types
- Verify: `SELECT '(0.6,0.4,0.5,4.0)'::pg_recall.score_weights`

**Dependencies**: Requires 1.1 (scaffolding).

---

### 1.3 Scoring Primitives

**Goal**: Implement the four cognitive scoring functions as pure SQL-callable functions. No side effects, no DB access. These are the mathematical core of the extension.

**Functions**:

**`actr_activation(access_count int, last_access timestamptz) -> float`**
```
n = access_count + 1
d = 0.5
age_days = max(days_since(last_access), 1/(24*60))
B = ln(n) - d * ln(age_days / n)
return B
```

**`ebbinghaus_decay(last_access timestamptz, access_count int, created_at timestamptz) -> float`**
```
lifespan_days = days_since(created_at)
avg_spacing = lifespan_days / max(access_count, 1)
base = ln(access_count + 1) * 20.0
spacing = tanh(avg_spacing / 7.0)
stability = clamp(base * (1 + 0.5 * spacing), 14.0, 365.0)
R = max(0.05, exp(-days_since(last_access) / stability))
return R
```

**`bayesian_update(prior float, evidence float) -> float`**
```
posterior = (prior * evidence) / max(prior * evidence + (1-prior)*(1-evidence), 1e-9)
return 0.95 * posterior + 0.025
```

**`softplus(x float) -> float`**
```
if x > 20: return x
return ln(1 + exp(x))
```

**Tasks**:
- Implement in `src/scoring.rs`
- Mark as `#[pg_extern(immutable, parallel_safe, schema = "pg_recall")]`
- Write unit tests in Rust (pure math, no DB needed)
- Write pgrx integration tests:
  - `actr_activation(13, now() - '10 days') ≈ 2.08`
  - `actr_activation(0, now() - '1400 days') ≈ -3.27`
  - `ebbinghaus_decay(now()-'30d', 50, now()-'180d') ≈ 0.78`
  - `ebbinghaus_decay(now()-'30d', 50, now()-'1d') ≈ 0.12`
  - `bayesian_update(0.5, 0.95) ≈ 0.925`
  - `bayesian_update(0.8, 0.10) ≈ 0.32`
  - `softplus(0.0) ≈ 0.6931`
  - `softplus(25.0) = 25.0`

**Dependencies**: Requires 1.1. Can run in parallel with 1.2 and 1.4.

---

### 1.4 Schema (Extension-Internal)

**Goal**: `CREATE EXTENSION pg_recall` produces the `mnemes`, `associations`, and `co_activation_queue` tables with all indexes and constraints.

**Tasks**:
- Implement schema creation in `src/lib.rs` extension init (or via SQL migration file `sql/pg_recall--0.1.0.sql`)
- Tables as defined in the design doc:
  - `pg_recall.mnemes`: id (uuid), workspace_id (uuid), concept, content, embedding vector(384), search_vector (generated tsvector), confidence (float, default 0.5), access_count (int), last_access (timestamptz), created_at, state (text, CHECK active/archived/dormant)
  - `pg_recall.associations`: src_id, dst_id (PK), weight, co_activations, updated_at, CHECK src_id < dst_id
  - `pg_recall.co_activation_queue`: id (bigserial), workspace_id, mneme_ids (uuid[]), scores (float[]), created_at
- Indexes: HNSW on embedding (cosine), GIN on search_vector, B-tree on (workspace_id, last_access DESC), B-tree on associations (dst_id, src_id)
- Verify: tables, indexes, constraints all present after `CREATE EXTENSION`

**Note**: These tables are pg_recall's own schema for standalone use. Chapterhouse will NOT use these tables directly — it will use its own `memory_blocks` table with pg_recall's scoring functions. But the extension must ship complete for other consumers.

**Dependencies**: Requires 1.1. Can run in parallel with 1.2 and 1.3.

---

### 1.5 Composite Recall Function

**Goal**: Implement `pg_recall.recall()` — the full cognitive retrieval pipeline that fuses vector similarity, FTS, ACT-R, Hebbian boost, and confidence.

**Implementation outline** (in `src/recall.rs`):

```
fn recall(workspace_id, query_text, query_embedding, limit_n, min_confidence, weights):
    pool_size = 3 * limit_n

    // 1. Candidate selection: union of HNSW + FTS top-k from mnemes
    hnsw_candidates = SELECT id, 1 - (embedding <=> query_embedding) as vec_score
                      FROM pg_recall.mnemes
                      WHERE workspace_id = $1 AND state = 'active' AND confidence >= min_confidence
                      ORDER BY embedding <=> query_embedding
                      LIMIT pool_size

    fts_candidates = SELECT id, ts_rank(search_vector, plainto_tsquery('english', query_text)) as fts_score
                     FROM pg_recall.mnemes
                     WHERE workspace_id = $1 AND state = 'active' AND confidence >= min_confidence
                           AND search_vector @@ plainto_tsquery('english', query_text)
                     ORDER BY fts_score DESC
                     LIMIT pool_size

    // 2. Merge and deduplicate candidates
    candidates = union(hnsw_candidates, fts_candidates)

    // 3. Score each candidate
    for each candidate:
        content_match = weights.semantic * vec_score + weights.fts * tanh(fts_score)

        activation = actr_activation(candidate.access_count, candidate.last_access)

        hebbian_boost = SUM(weight) FROM associations
                        WHERE (src_id = candidate.id OR dst_id = candidate.id)
                        AND other_id IN (candidate_set)

        temporal = softplus(activation + weights.hebbian_scale * hebbian_boost)
        temporal_norm = temporal / (1 + softplus(0))

        score = content_match * temporal_norm * candidate.confidence

    // 4. Sort, limit, enqueue co-activation
    results = top_n(candidates, by=score)
    INSERT INTO co_activation_queue (workspace_id, mneme_ids, scores)
    return results
```

**Tasks**:
- Implement in `src/recall.rs` using SPI (Server Programming Interface) for internal queries
- Handle NULL weights parameter (use defaults)
- Return `SETOF pg_recall.recall_result`
- Integration tests:
  - Insert test mnemes with known embeddings
  - Verify ranking order reflects cognitive scoring (not just vector similarity)
  - Verify co-activation event is enqueued
  - Verify min_confidence filtering works
  - Verify workspace isolation

**Dependencies**: Requires 1.2 (types), 1.3 (scoring), 1.4 (schema).

---

### 1.6 Hebbian Operations (SQL Functions)

**Goal**: Implement the helper functions for managing associations and confidence.

**Functions**:

**`record_co_activation(workspace_id uuid, mneme_ids uuid[], scores float[]) -> void`**
- Validates array lengths match
- Single INSERT into co_activation_queue

**`get_associations(mneme_id uuid, min_weight float DEFAULT 0.01) -> SETOF (related_id, weight)`**
- Queries both directions (src_id and dst_id)
- Ordered by weight DESC

**`update_confidence(mneme_id uuid, evidence float) -> float`**
- Reads current confidence, applies bayesian_update, writes back
- Returns new confidence

**`confirm_recall(mneme_ids uuid[]) -> void`**
- Calls update_confidence with evidence=0.95 for each

**Tasks**:
- Implement in `src/hebbian.rs` (or split SQL helper functions into separate file)
- Integration tests for each function

**Dependencies**: Requires 1.3 (scoring for bayesian_update), 1.4 (schema).

---

### 1.7 Background Worker

**Goal**: Implement the Hebbian learning background worker as a pgrx bgworker.

**Behavior**:
- Single worker per Postgres instance
- Polls `co_activation_queue`, processes batches of up to 100 rows
- For each batch:
  1. Generate all (i, j) pairs from each event's mneme_ids
  2. Aggregate pairs: signal = sum(score_i * score_j)
  3. Update association weights in log-space: `new = min(1.0, exp(ln(current) + signal * ln(1.01)))`
  4. Cold start: if weight <= 0, seed at 0.01
  5. Single transaction: UPDATE associations, DELETE consumed queue rows, UPDATE mnemes access_count + last_access
- Adaptive polling: active=100ms, idle(>30s)=1s, dormant(>5min)=5s
- Hourly decay: `weight *= 0.999 WHERE updated_at < now() - '1 day'`, prune below 0.001
- On shutdown: drain remaining queue

**`worker_stats() -> record`**:
- Returns: state, queue_depth, batches_processed, pairs_updated, last_batch_at, last_decay_at

**Tasks**:
- Implement in `src/hebbian.rs`
- Use pgrx `BackgroundWorker` API
- Implement adaptive polling state machine
- Implement hourly decay timer
- Implement worker_stats via shared memory or stats table
- Integration tests:
  - Enqueue co-activation events, verify associations form
  - Verify weight updates follow log-space formula
  - Verify decay pass prunes weak associations
  - Verify access_count and last_access are updated

**Dependencies**: Requires 1.4 (schema), 1.3 (scoring). This is the most complex component.

---

### 1.8 Extension Packaging & Container Image

**Goal**: pg_recall installs cleanly via `CREATE EXTENSION` and is packaged in a container image for CNPG.

**Tasks**:
- Finalize `pg_recall.control`: `default_version = '0.1.0'`, `schema = 'pg_recall'`, `requires = 'vector'`
- Generate or author `sql/pg_recall--0.1.0.sql` migration
- Create Dockerfile (multi-stage: Rust 1.94+ build stage → PG18.3 runtime):
  ```dockerfile
  FROM ghcr.io/cloudnative-pg/postgresql:18.3-system-trixie
  # Install pgvector 0.8.2 from source
  # Build and install pg_recall from source (cargo pgrx install --pg18)
  ```
- Test: `CREATE EXTENSION vector; CREATE EXTENSION pg_recall;` → full schema + worker running
- Build and push image to a registry accessible from the k3s cluster

**Dependencies**: Requires all of 1.1–1.7.

---

## Workstream 2: Chapterhouse Modification

### 2.0 Homelab Adaptation

**Goal**: Get the `final-refactor` branch building and running in the homelab environment.

**Context**: The branch is customized for a corporate environment with MITM SSL cert handling and corporate build infrastructure. This needs to be stripped/adapted for the homelab k3s cluster.

**Tasks**:
- Remove corporate SSL/cert handling, proxy configuration
- Update Dockerfile / build pipeline for homelab container registry
- Update Kubernetes manifests / Helm chart for k3s deployment
- Update config to point at CNPG Postgres cluster
- Verify: Chapterhouse builds, deploys to k3s, connects to CNPG Postgres, and responds to MCP tool calls (using existing Qdrant backend initially)

**Dependencies**: None. Can start immediately, in parallel with Workstream 1.

---

### 2.1 Schema Migration: Add Cognitive Columns

**Goal**: Extend `memory_blocks` with the columns needed for pg_recall scoring.

**New migration** (`009_cognitive_columns.sql`):
```sql
-- Requires pgvector extension
CREATE EXTENSION IF NOT EXISTS vector;

-- Add embedding storage (replaces Qdrant)
ALTER TABLE memory_blocks ADD COLUMN embedding vector(384);

-- Add confidence for Bayesian tracking
ALTER TABLE memory_blocks ADD COLUMN confidence float NOT NULL DEFAULT 0.5;

-- Add generated tsvector for FTS (replaces existing full-text approach)
-- Chapterhouse already has a GIN index on value; this adds weighted concept+content
ALTER TABLE memory_blocks ADD COLUMN search_vector tsvector
    GENERATED ALWAYS AS (
        setweight(to_tsvector('english', name), 'A') ||
        setweight(to_tsvector('english', value), 'B')
    ) STORED;

CREATE INDEX ON memory_blocks USING hnsw (embedding vector_cosine_ops)
    WHERE is_current = true;
CREATE INDEX ON memory_blocks USING gin (search_vector);

-- Associations table (Hebbian links between memory blocks)
CREATE TABLE associations (
    src_id bigint NOT NULL REFERENCES memory_blocks(id),
    dst_id bigint NOT NULL REFERENCES memory_blocks(id),
    weight float NOT NULL DEFAULT 0.01,
    co_activations int NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (src_id, dst_id),
    CHECK (src_id < dst_id)
);
CREATE INDEX ON associations (dst_id, src_id);

-- Co-activation queue for background processing
CREATE TABLE co_activation_queue (
    id bigserial PRIMARY KEY,
    user_id uuid NOT NULL,
    block_ids bigint[] NOT NULL,
    scores float[] NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
```

**Design notes**:
- Uses `bigint` (not uuid) for association FKs since `memory_blocks.id` is `bigserial`
- Uses `user_id` (not workspace_id) since Chapterhouse's tenant key is user_id
- HNSW index is partial: only `is_current = true` rows, since historical versions shouldn't appear in recall
- The existing `recall_count` and `last_recalled_at` columns already serve as ACT-R inputs — no new columns needed for that

**Dependencies**: Requires 2.0 (homelab adaptation, working Postgres connection).

---

### 2.2 Embedding Provider: Switch to Together.ai + bge-small

**Goal**: Reconfigure Chapterhouse's embedding provider for Together.ai with bge-small.

**Tasks**:
- Update config defaults / env vars:
  ```
  EMBEDDING_URL=https://api.together.xyz
  EMBEDDING_MODEL=BAAI/bge-small-en-v1.5
  EMBEDDING_DIMENSIONS=384
  EMBEDDING_API_KEY=<together-api-key>
  ```
- Verify the OpenAI-compatible provider (`internal/embedding/openai.go`) works with Together.ai's API (it should — Together.ai implements the OpenAI embeddings endpoint)
- Test: generate embeddings, verify 384 dimensions returned

**Dependencies**: None. Can start immediately.

---

### 2.3 Remember Flow: Embed and Store in Postgres

**Goal**: Modify the `remember` tool to store embeddings directly in `memory_blocks` instead of upserting to Qdrant.

**Current flow** (simplified):
1. Validate, scan for secrets
2. INSERT into memory_blocks
3. Background: embed → check Qdrant for near-duplicates → upsert Qdrant point

**New flow**:
1. Validate, scan for secrets
2. Embed via Together.ai (move to foreground — embedding must complete before INSERT since `embedding` column is NOT NULL after backfill)
3. Check for near-duplicates: `SELECT id, 1 - (embedding <=> $1) as sim FROM memory_blocks WHERE user_id = $2 AND is_current = true ORDER BY embedding <=> $1 LIMIT 3` — if sim >= 0.92, return notice
4. INSERT into memory_blocks (including embedding column)
5. Confidence starts at 0.5 (default)

**Tasks**:
- Modify `internal/handler/memory.go` remember handler
- Remove Qdrant upsert call
- Add embedding to INSERT (update sqlc query `CreateMemoryBlock`)
- Replace Qdrant near-duplicate search with pgvector cosine query
- Update sqlc SQL files and regenerate

**Dependencies**: Requires 2.1 (schema), 2.2 (embedding provider).

---

### 2.4 Recall Flow: Cognitive Retrieval

**Goal**: Replace the Qdrant semantic search + ILIKE keyword search + RRF fusion with a single cognitive recall query using pg_recall's scoring functions.

**Current flow**:
- Semantic mode: embed query → Qdrant cosine search → return
- Keyword mode: ILIKE + FTS in Postgres → return
- Hybrid mode: both → RRF fusion → return
- Background: increment recall_count

**New flow** (single SQL query):
```sql
WITH candidates AS (
    -- HNSW nearest neighbors
    SELECT id, name, value, embedding, confidence, recall_count,
           last_recalled_at, created_at, tier, memory_type, scope, tags, session_id,
           1 - (embedding <=> $query_embedding) AS vec_score,
           0.0 AS fts_score
    FROM memory_blocks
    WHERE user_id = $user_id AND is_current = true
          AND (expires_at IS NULL OR expires_at > now())
          AND confidence >= $min_confidence
          -- access control: personal or org-scoped
          AND (scope = 'personal' OR (scope = 'org' AND user_id IN (SELECT id FROM users WHERE org_id = $org_id)))
          -- optional filters
          AND ($memory_type IS NULL OR memory_type = $memory_type)
          AND ($tags IS NULL OR tags @> $tags)
          AND ($session_id IS NULL OR session_id = $session_id)
    ORDER BY embedding <=> $query_embedding
    LIMIT $pool_size

    UNION

    -- FTS matches
    SELECT id, name, value, embedding, confidence, recall_count,
           last_recalled_at, created_at, tier, memory_type, scope, tags, session_id,
           0.0 AS vec_score,
           ts_rank(search_vector, plainto_tsquery('english', $query_text)) AS fts_score
    FROM memory_blocks
    WHERE user_id = $user_id AND is_current = true
          AND (expires_at IS NULL OR expires_at > now())
          AND confidence >= $min_confidence
          AND search_vector @@ plainto_tsquery('english', $query_text)
          AND (scope = 'personal' OR (scope = 'org' AND user_id IN (SELECT id FROM users WHERE org_id = $org_id)))
          AND ($memory_type IS NULL OR memory_type = $memory_type)
          AND ($tags IS NULL OR tags @> $tags)
          AND ($session_id IS NULL OR session_id = $session_id)
    ORDER BY fts_score DESC
    LIMIT $pool_size
),
merged AS (
    SELECT id, name, value, confidence, recall_count, last_recalled_at, created_at,
           tier, memory_type, scope, tags, session_id,
           MAX(vec_score) AS vec_score,
           MAX(fts_score) AS fts_score
    FROM candidates
    GROUP BY id, name, value, confidence, recall_count, last_recalled_at, created_at,
             tier, memory_type, scope, tags, session_id
),
scored AS (
    SELECT *,
        -- Content match: fuse vector + FTS
        (0.6 * vec_score + 0.4 * tanh(fts_score)) AS content_match,

        -- ACT-R activation (called via pg_recall extension function)
        pg_recall.actr_activation(recall_count, last_recalled_at) AS activation,

        -- Hebbian boost: sum of association weights to other candidates
        COALESCE((
            SELECT SUM(a.weight)
            FROM associations a
            WHERE (a.src_id = merged.id AND a.dst_id IN (SELECT id FROM merged))
               OR (a.dst_id = merged.id AND a.src_id IN (SELECT id FROM merged))
        ), 0.0) AS hebbian_boost
    FROM merged
),
ranked AS (
    SELECT *,
        content_match
        * (pg_recall.softplus(activation + 4.0 * hebbian_boost) / (1.0 + pg_recall.softplus(0.0)))
        * confidence
        AS final_score
    FROM scored
    ORDER BY final_score DESC
    LIMIT $limit
)
SELECT * FROM ranked;
```

After the query returns, enqueue co-activation and update recall counts:

```sql
-- Fire-and-forget co-activation
INSERT INTO co_activation_queue (user_id, block_ids, scores)
VALUES ($user_id, $result_ids, $result_scores);

-- Update access tracking
UPDATE memory_blocks
SET recall_count = recall_count + 1, last_recalled_at = now()
WHERE id = ANY($result_ids) AND is_current = true;
```

**Tasks**:
- Write the cognitive recall SQL query (or implement in Go with query builder)
- Add to sqlc or implement as a raw query via pgx
- Modify `internal/handler/memory.go` recall handler
- Remove Qdrant search calls entirely
- Remove RRF fusion code (now handled in SQL)
- Keep the `mode` parameter for backwards compatibility, but all modes now use the cognitive pipeline (keyword-only mode could skip the HNSW branch)
- Update response formatting to include cognitive scores (activation, hebbian_boost, confidence) — useful for agent transparency
- Background: enqueue co-activation event + update recall counts (keep async pattern)

**Dependencies**: Requires 2.1 (schema), 2.2 (embedding), and pg_recall extension installed (1.8) for scoring functions. However, the scoring functions could be temporarily inlined as SQL until the extension is ready — they're pure math.

---

### 2.5 Hebbian Background Processing

**Goal**: Process the co-activation queue to update association weights and maintain the Hebbian learning loop.

**Two options**:

**Option A — Use pg_recall's background worker**: The extension's built-in bgworker polls `co_activation_queue` and updates `associations`. Requires pointing the worker at Chapterhouse's tables instead of pg_recall's own schema. This may require making the worker schema-configurable.

**Option B — Go goroutine in Chapterhouse**: Implement the Hebbian processing loop as a background goroutine in the MCP server. Simpler integration, no extension worker configuration needed, and aligns with Chapterhouse's existing pattern of background goroutines (e.g., for Qdrant upserts).

**Recommended: Option B** for v0.1. Reasons:
- Chapterhouse already has a `wg` WaitGroup pattern for background work
- The processing logic is straightforward Go code
- Avoids complexity of configuring the pgrx bgworker to operate on a foreign schema
- Can always migrate to the extension worker later if needed

**Go implementation outline**:
```go
func (s *Server) runHebbianWorker(ctx context.Context) {
    // Adaptive polling: active=100ms, idle(>30s)=1s, dormant(>5min)=5s
    // Each cycle:
    //   1. SELECT + DELETE up to 100 rows from co_activation_queue (FOR UPDATE SKIP LOCKED)
    //   2. Generate all (i,j) pairs from each event's block_ids
    //   3. Aggregate: signal[i,j] += score_i * score_j
    //   4. For each pair: UPSERT associations SET weight = min(1.0, exp(ln(weight) + signal * ln(1.01)))
    //   5. UPDATE memory_blocks SET recall_count += 1, last_access = now() (already done in recall, skip here)
    //   6. Hourly: UPDATE associations SET weight *= 0.999 WHERE updated_at < now()-'1 day'
    //              DELETE FROM associations WHERE weight < 0.001
}
```

**Tasks**:
- Implement Hebbian worker as a Go goroutine
- Start on server boot, stop on graceful shutdown
- Implement adaptive polling state machine
- Implement hourly decay pass
- Add worker_stats endpoint (or MCP tool) for monitoring
- Integration test: store memories, recall them together, verify associations form and strengthen

**Dependencies**: Requires 2.1 (schema). Can start once schema is migrated.

---

### 2.6 Confidence Operations

**Goal**: Add MCP tools for confidence management.

**New tools (or modifications to existing)**:

**`confirm_recall`** — New MCP tool. After an agent uses recalled memories successfully, it calls this to boost confidence.
- Input: `block_ids []int64`
- Action: For each ID, `UPDATE memory_blocks SET confidence = pg_recall.bayesian_update(confidence, 0.95) WHERE id = $1`

**`reject_recall`** — New MCP tool. Agent indicates a memory was wrong or unhelpful.
- Input: `block_ids []int64`
- Action: `UPDATE memory_blocks SET confidence = pg_recall.bayesian_update(confidence, 0.05) WHERE id = $1`

**Automatic confidence via co-activation**: The Hebbian worker can also apply mild reinforcement (evidence=0.65) to memories that appear in co-activation events, meaning memories that keep getting recalled together slowly gain confidence.

**Tasks**:
- Add `confirm_recall` and `reject_recall` MCP tools in `internal/mcp/tools.go`
- Add sqlc queries for confidence updates (calling pg_recall.bayesian_update or inlining the formula)
- Update tool registration in server.go

**Dependencies**: Requires 2.1 (schema, confidence column), pg_recall scoring functions or inlined formula.

---

### 2.7 Remove Qdrant

**Goal**: Fully remove Qdrant as a dependency.

**Tasks**:
- Remove `internal/vector/qdrant.go` and `qdrant_test.go`
- Remove `github.com/qdrant/go-client` and `google.golang.org/grpc` from `go.mod`
- Remove Qdrant config fields from `internal/config/config.go`
- Remove `vector_id` column from journal table (or repurpose)
- Remove all references to vectorDB in handlers, server initialization
- Remove Qdrant deployment from Kubernetes manifests
- Update README / documentation

**Dependencies**: Requires 2.3 and 2.4 (remember and recall fully migrated).

---

### 2.8 Data Migration

**Goal**: Migrate existing memories from Qdrant to Postgres embeddings.

**Tasks**:
- Write a migration script (Go CLI command, similar to existing `cmd/reindex/main.go`):
  1. Query all `memory_blocks` where `is_current = true` and `embedding IS NULL`
  2. Batch-embed content via Together.ai (bge-small, 384-dim)
  3. UPDATE `memory_blocks SET embedding = $1 WHERE id = $2`
  4. Set initial confidence = 0.5 for all
  5. Optionally: seed recall_count-based activation by computing initial confidence from existing recall_count (e.g., memories with high recall_count get slightly higher confidence)
- Can reuse or replace the existing `cmd/reindex` command

**Dependencies**: Requires 2.1 (schema), 2.2 (embedding provider).

---

### 2.9 Forget Flow Update

**Goal**: Update the `forget` tool to clean up associations and embeddings.

**Tasks**:
- Remove Qdrant delete call
- Add: `DELETE FROM associations WHERE src_id = $1 OR dst_id = $1` (for each version's block ID)
- The embedding is deleted automatically when the memory_blocks row is deleted
- Update sqlc queries

**Dependencies**: Requires 2.1 (schema).

---

## Workstream 3: Container Image & Deployment

### 3.1 Custom CNPG Postgres Image

**Goal**: Build a Postgres 17 container image with pgvector + pg_recall for CNPG.

**Tasks**:
- Create Dockerfile based on `ghcr.io/cloudnative-pg/postgresql:18.3-system-trixie`
- Install pgvector 0.8.2 from source (critical: fixes CVE-2026-3172 in parallel HNSW builds)
- Build and install pg_recall extension (`cargo pgrx install --pg18`)
- Multi-stage build: Rust build stage → copy .so + .control + .sql into Postgres image
- Push to a container registry accessible from k3s cluster (local registry or GitHub Packages)
- Test: `CREATE EXTENSION vector; CREATE EXTENSION pg_recall;`

**Dependencies**: Requires Workstream 1 complete (1.1–1.7).

---

### 3.2 CNPG Cluster Configuration

**Goal**: Deploy Postgres with pg_recall to the k3s cluster via CNPG operator.

**Tasks**:
- Create or update CNPG `Cluster` manifest:
  - Custom image from 3.1
  - `shared_preload_libraries: 'pg_recall'` (if bgworker needs preload)
  - Appropriate resource limits, storage class
  - Enable pgvector and pg_recall extensions in the bootstrap SQL
- Apply Chapterhouse schema migrations against the CNPG cluster
- Verify: Chapterhouse connects, `recall` and `remember` work end-to-end

**Dependencies**: Requires 3.1.

---

### 3.3 Chapterhouse Deployment

**Goal**: Deploy the modified Chapterhouse to k3s.

**Tasks**:
- Build Chapterhouse container image (Go binary)
- Create/update Kubernetes manifests (Deployment, Service, ConfigMap/Secret for env vars)
- Configure Together.ai API key as a Kubernetes Secret
- Configure Postgres connection string (from CNPG cluster)
- Remove Qdrant deployment manifests
- Verify: MCP tools work end-to-end from a coding agent

**Dependencies**: Requires 3.2, Workstream 2 complete.

---

## Parallelization Map

```
TIME →

Workstream 1 (pg_recall extension):
  1.1 Scaffolding ──┬── 1.2 Types ──────────────────┐
                     ├── 1.3 Scoring Primitives ─────┤
                     └── 1.4 Schema ─────────────────┤
                                                     ├── 1.5 Recall Function ──┐
                                                     └── 1.6 Hebbian Ops ──────┤
                                                                               ├── 1.7 Background Worker
                                                                               └── 1.8 Packaging + Image

Workstream 2 (Chapterhouse):
  2.0 Homelab Adaptation ─── 2.1 Schema Migration ──┬── 2.3 Remember Flow ──┐
                                                     ├── 2.5 Hebbian Worker  ├── 2.7 Remove Qdrant
  2.2 Embedding Provider ───────────────────────────┤├── 2.9 Forget Update   │
                                                     ├── 2.4 Recall Flow ────┘
                                                     └── 2.6 Confidence Ops

  2.8 Data Migration (can run anytime after 2.1 + 2.2)

Workstream 3 (Deployment):
  [After WS1] ── 3.1 Container Image ── 3.2 CNPG Cluster ── 3.3 Chapterhouse Deploy
```

**Maximum parallelism**: Up to 5 independent tracks can run simultaneously:
- 1.2 Types + 1.3 Scoring + 1.4 Schema (within WS1)
- 2.0 Homelab adaptation (WS2)
- 2.2 Embedding provider (WS2)

---

## Key Risks and Mitigations

| Risk | Impact | Mitigation |
|------|--------|------------|
| pgrx bgworker complexity | Worker is the hardest component in the extension | Implement Hebbian processing in Go (Option B) for v0.1; extension worker is nice-to-have |
| pgvector HNSW performance vs Qdrant | Recall latency regression | Partial HNSW index (is_current only), monitor query times, pool_size tuning |
| Together.ai rate limits on embedding | Blocks remember flow | Batch embedding in migration script, single-embed in remember is low volume |
| Custom CNPG image maintenance | Must rebuild on Postgres upgrades | Pin Postgres major version, automate image build in CI |
| Scoring function overhead per-row | Recall query slower than pure vector search | Functions are marked immutable + parallel_safe; Postgres can parallelize. Pool_size limits candidate set |
| Existing memories lose Qdrant vectors | Must re-embed everything with new model | Migration script (2.8) handles this; run before cutover |

---

## What's NOT In Scope

- pg_recall v0.2 features (contradiction detection, typed associations, graph traversal)
- MCP tool API changes visible to agents (same tool names, same parameters — agents don't notice the backend swap)
- Chapterhouse ch-web portal changes (the web admin UI is a separate concern)
- Multi-region / HA beyond what CNPG provides natively
- Embedding model fine-tuning
