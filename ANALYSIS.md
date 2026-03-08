# pg_recall: A Deep Exploration

## 1. Project Purpose and Overview

**pg_recall** is a PostgreSQL extension that implements neuroscience-inspired memory primitives using Rust and pgrx. It treats a relational database as a cognitive memory system rather than just static storage, modeling how memories decay with time, strengthen through use, form associations automatically through co-activation, and track confidence via Bayesian updating.

The extension brings four established cognitive models from psychology research into Postgres:
- **ACT-R** (Adaptive Control of Thought–Rational): Models memory activation based on frequency and recency
- **Ebbinghaus Forgetting Curve**: Tracks retention decay with spacing-aware stability
- **Hebbian Learning**: Forms associations between co-activated memories ("neurons that fire together wire together")
- **Bayesian Inference**: Updates confidence scores as reinforcing or contradicting evidence arrives

## 2. Architecture and Technology Stack

**Language & Framework:**
- **Rust 1.94+** compiled to PostgreSQL extension
- **pgrx 0.17.x** - Rust-to-PostgreSQL bindings (with pg18 feature flag)
- **PostgreSQL 18.3+** target runtime
- **pgvector 0.8.2+** for semantic vector storage and HNSW indexing

**Extension Model:**
- Pure extension (compiled .so library loaded by Postgres, no separate services)
- All objects in `pg_recall` schema
- Version 0.1.0
- Requires pgvector as a dependency (declared in .control file)
- Multi-tenant by design (all tables include workspace_id for isolation)

**Project Structure:**
```
/Users/bran/code/pg_recall/
├── Cargo.toml                      # Rust manifest with pgrx dependencies
├── pg_recall.control               # PostgreSQL control file (schema, version, requires)
├── README.md                        # User-facing documentation
├── spec.simplex                     # Orchestration specification
├── PLEXUS_REPORT.md               # Build report showing 7-agent parallel development
├── docs/
│   └── plans/
│       └── 2026-03-03-pg-recall-design.md  # Design specification
└── src/
    ├── lib.rs                      # Extension entry point, module declarations
    ├── types.rs                    # SQL composite types (recall_result, score_weights)
    ├── scoring.rs                  # Cognitive scoring functions (softplus, actr_activation, ebbinghaus_decay, bayesian_update)
    ├── schema.rs                   # Table and index definitions (mnemes, associations, co_activation_queue)
    ├── recall.rs                   # Main recall() function - composite retrieval engine
    ├── hebbian.rs                  # Association learning and confidence management
    ├── integration_tests.rs        # End-to-end test suite
    └── bin/pgrx_embed_pg_recall.rs # Binary embedding
```

## 3. Directory Structure and Module Responsibilities

**Core Module Breakdown:**

| Module | Responsibility | LOC | Key Functions |
|--------|---|---|---|
| `types.rs` | SQL composite types and Rust struct definitions | ~305 | `RecallResult`, `ScoreWeights` |
| `scoring.rs` | Pure cognitive scoring functions | ~489 | `softplus()`, `actr_activation()`, `ebbinghaus_decay()`, `bayesian_update()` |
| `schema.rs` | Database schema (tables, indexes, constraints) | ~285 | Creates mnemes, associations, co_activation_queue tables; HNSW, GIN, B-tree indexes |
| `recall.rs` | Composite memory retrieval engine | ~738 | `recall()`, `recall_inner()`, candidate ranking, Hebbian boost calculation |
| `hebbian.rs` | Associative learning and confidence tracking | ~818 | `process_co_activation_batch()`, `update_confidence()`, `confirm_recall()`, `get_associations()` |
| `integration_tests.rs` | End-to-end integration tests | ~580 | Full recall-learn-recall cycles, workspace isolation, state filtering |

## 4. Key Data Structures and Types

**SQL Composite Types (defined in types.rs):**

```sql
CREATE TYPE pg_recall.recall_result AS (
    mneme_id      uuid,           -- Memory identifier
    score         float8,         -- Final composite score
    content_match float8,         -- Vector + FTS fusion
    activation    float8,         -- ACT-R base-level activation
    hebbian_boost float8,         -- Association strength sum
    confidence    float8,         -- Bayesian confidence
    concept       text,           -- Short memory label
    content       text            -- Full memory content
);

CREATE TYPE pg_recall.score_weights AS (
    semantic      float8,         -- Weight for vector similarity (default 0.6)
    fts           float8,         -- Weight for full-text search (default 0.4)
    actr_decay    float8,         -- ACT-R decay exponent d (default 0.5)
    hebbian_scale float8          -- Hebbian boost multiplier (default 4.0)
);
```

**Core Tables (defined in schema.rs):**

1. **`mnemes`** - Primary memory store
   - `id` uuid PK, auto-generated
   - `workspace_id` uuid - tenant isolation key
   - `concept` text - short label
   - `content` text - full memory content
   - `embedding` vector(384) - semantic vector
   - `search_vector` tsvector - auto-generated from concept (weight A) + content (weight B)
   - `confidence` float8 default 0.5 - Bayesian confidence
   - `access_count` int default 0 - retrieval count
   - `last_access` timestamptz - last retrieval time
   - `created_at` timestamptz - creation time
   - `state` text - one of: active, archived, dormant (CHECK constraint)

2. **`associations`** - Hebbian links between mnemes
   - `src_id, dst_id` uuid FK to mnemes(id)
   - `weight` float8 default 0.01 - association strength [0, 1]
   - `co_activations` int default 0 - co-activation count
   - `updated_at` timestamptz
   - CHECK constraint: `src_id < dst_id` (canonical ordering)
   - PK: (src_id, dst_id)

3. **`co_activation_queue`** - Pending Hebbian processing events
   - `id` bigserial PK
   - `workspace_id` uuid
   - `mneme_ids` uuid[] - co-activated mneme IDs
   - `scores` float8[] - corresponding recall scores
   - `created_at` timestamptz

**Indexes:**
- HNSW on `mnemes(embedding)` for vector similarity search
- GIN on `mnemes(search_vector)` for full-text search
- B-tree on `mnemes(workspace_id, last_access DESC)` for temporal queries
- B-tree on `associations(dst_id, src_id)` for reverse lookups

## 5. End-to-End System Flow

**The complete "recall-learn-recall cycle":**

```
1. INSERT mnemes
   → Memories stored with embeddings, confidence=0.5, access_count=0

2. CALL recall(workspace_id, query_text, query_embedding, limit, min_confidence, weights)
   a. Fetch candidate pool via UNION:
      - HNSW nearest neighbors on embedding
      - Full-text search matches on search_vector
   b. For each candidate, compute:
      - content_match = w_semantic * cosine_sim + w_fts * tanh(fts_rank)
      - activation = actr_activation(access_count, last_access, w_actr_decay)
      - hebbian_boost = sum of association weights to other candidates
      - temporal_weight = softplus(activation + w_hebbian_scale * hebbian_boost) / normalizer
      - score = content_match * temporal_weight * confidence
   c. Sort by score descending, truncate to limit_n
   d. Enqueue co-activation event (mneme_ids and scores) into co_activation_queue
   e. Return ranked recall_result rows

3. PROCESS co-activation (async or on-demand)
   → process_co_activation_batch(batch_limit) or process_all_pending_co_activations()
   a. Fetch up to batch_limit events from queue
   b. For each event, generate all canonical (i, j) pairs where i < j
   c. Aggregate signal: for each pair, sum(score_i * score_j) from all events
   d. Upsert associations:
      new_weight = min(1.0, exp(ln(current_weight) + signal * ln(1.01)))
   e. Update mneme access_count and last_access
   f. Delete processed queue rows

4. REPEAT recall()
   → Next recall() call now sees non-zero Hebbian boost from formed associations
```

**Key Design Pattern: Immutable Scoring Functions**

All scoring functions are pure, immutable, and parallel-safe:
- `softplus(x)` - Safe overflow-guarded smooth activation ln(1 + exp(x))
- `actr_activation(access_count, last_access)` - Power-law temporal decay
- `ebbinghaus_decay(last_access, access_count, created_at)` - Spacing-aware retention
- `bayesian_update(prior, evidence)` - Probabilistic confidence update

These can be called independently or composed into custom retrieval pipelines.

## 6. Configuration, Build System, and Dependencies

**Build Configuration:**

```toml
[package]
name = "pg_recall"
version = "0.1.0"
edition = "2021"

[lib]
crate-type = ["cdylib", "lib"]  # Shared library for Postgres

[features]
default = ["pg18"]
pg18 = ["pgrx/pg18", "pgrx-tests/pg18"]
pg_test = []

[dependencies]
pgrx = "=0.17.0"

[dev-dependencies]
pgrx-tests = "=0.17.0"

[profile.release]
opt-level = 3
lto = "fat"
```

**Installation:**

```bash
cargo install cargo-pgrx --version "=0.17.0"
cargo pgrx init --pg18 $(which pg_config)
cargo pgrx install --release

# In PostgreSQL:
CREATE EXTENSION vector;
CREATE EXTENSION pg_recall;
```

**Extension Control File (pg_recall.control):**
```
comment = 'pg_recall: Cognitive Memory Primitives for Postgres'
default_version = '0.1.0'
module_pathname = '$libdir/pg_recall'
relocatable = false
superuser = true
schema = 'pg_recall'
requires = 'vector'
```

## 7. Test Coverage and Testing Approach

**Test Structure:**
- **Unit tests** in each module (pure Rust math, no Postgres needed)
- **Integration tests** (require running Postgres instance via pgrx test framework)
- **End-to-end tests** in `integration_tests.rs` (full recall-learn-recall cycles)

**Test Categories:**

**Scoring Functions Tests (scoring.rs):**
- softplus: zero, positive, negative, overflow guard, boundary cases
- bayesian_update: neutral evidence, contradiction, extreme bounds
- actr_activation: frequent access, old unused memories, very recent clamped
- ebbinghaus_decay: high stability, crammed spacing, retention floor, recent access

**Schema Tests (schema.rs):**
- Table existence in pg_recall schema
- Search vector auto-generation from concept + content
- State check constraint validation
- Association canonical ordering (src < dst)
- Index existence (HNSW, GIN, B-tree)
- Default values and constraints

**Recall Tests (recall.rs):**
- Returns ranked results
- Respects limit parameter
- Workspace isolation
- Confidence filtering
- Co-activation enqueuing
- Default and custom weights
- Hebbian boost calculation
- Ordered by score descending
- Excludes non-active mnemes
- Does NOT update access_count directly (deferred to batch processing)

**Hebbian Tests (hebbian.rs):**
- record_co_activation inserts events
- Array length mismatch validation
- get_associations returns both directions
- min_weight filtering
- ordered by weight descending
- update_confidence Bayesian math
- confirm_recall applies evidence=0.95
- process_co_activation_batch creates associations, updates access tracking, reinforces weights
- process_all_pending_co_activations drains queue

**Integration Tests (integration_tests.rs):**
- Extension version 0.1.0
- Schema exists after CREATE EXTENSION
- All tables and types present
- All functions exist
- **Full recall-learn-recall cycle:** insert → recall (no hebbian) → process co-activation → recall (with hebbian)
- **Confidence evolution:** initial 0.5 → confirm_recall increases → evidence decreases
- **Scoring composability:** all primitives callable and composable
- **Workspace isolation:** different workspaces see different results
- **State filtering:** archived/dormant excluded, only active returned
- **Repeated co-activation:** weights strengthen with each round, capped at 1.0
- **Custom weights:** affect ranking

**Total Test Count:** ~80+ test cases across unit and integration

## 8. Notable Design Patterns and Architectural Decisions

**1. Immutable SQL-Callable Functions**
All cognitive primitives are marked `#[pg_extern(immutable, parallel_safe)]`, allowing Postgres query planner to parallelize and optimize them.

**2. Deferred Access Tracking**
`recall()` does NOT update `access_count` or `last_access` directly. These are updated only during `process_co_activation_batch()`, allowing:
- Read-only semantics for `recall()` (can run on read replicas)
- Batch efficiency (amortize writes)
- Clearer separation of concerns (query vs. learning)

**3. Canonical Pair Ordering**
Associations enforce `src_id < dst_id` via CHECK constraint, avoiding duplicate edges and simplifying Hebbian aggregation across recall events.

**4. Log-Space Weight Updates**
Association weights updated in log-space to avoid numeric underflow and allow smooth reinforcement:
```
new_weight = min(1.0, exp(ln(current_weight) + signal * ln(1.01)))
```
New pairs seeded at 0.01, then reinforced based on co-activation signal strength.

**5. Two-Level Candidate Ranking**
`recall()` uses a union of HNSW and FTS candidates (with limit = 3 * limit_n), preventing scenarios where semantic and textual signals produce disjoint result sets.

**6. Softplus Normalization**
Scoring formula normalizes temporal weights by dividing by `1 + softplus(0)`, ensuring well-scaled outputs:
```
temporal_weight = softplus(activation + hebbian_scale * boost) / (1 + softplus(0))
score = content_match * temporal_weight * confidence
```

**7. Multi-Tenancy by Default**
Every query-path function accepts `workspace_id` and filters on it. No RLS enforcement built-in (leaves RLS policy to user), but designed for seamless RLS integration.

**8. Durable Queue Instead of LISTEN/NOTIFY**
Co-activation events stored in `co_activation_queue` table, not LISTEN/NOTIFY, allowing:
- Durability across crashes
- Batch processing on any schedule (cron jobs, manual calls, future background worker)
- No need for connection pooling compatibility concerns
- v0.2 can add autonomous background worker without changing queue interface

**9. Composite Type Wrapping**
Public `recall()` is a thin SQL wrapper around internal `recall_inner()` Rust function, allowing easy parameter handling and future modifications without changing the Postgres function signature.

**10. Workspace Isolation Without Enforcement**
Extension design allows per-workspace Row-Level Security policies:
```sql
ALTER TABLE pg_recall.mnemes ENABLE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON pg_recall.mnemes
    USING (workspace_id = current_setting('app.workspace_id')::uuid);
```

## 9. Development State and Orchestration

**Recent Development History:**
The project was built using a parallel agent orchestration system (Plexus) that managed 7 independent tasks executed concurrently:

1. **bootstrap_extension_skeleton** - Foundation (Cargo.toml, module stubs)
2. **define_composite_types** - SQL types (recall_result, score_weights)
3. **implement_scoring_primitives** - Cognitive functions (softplus, ACT-R, Ebbinghaus, Bayesian)
4. **create_extension_schema** - Tables and indexes
5. **implement_hebbian_helpers** - Association learning and confidence management
6. **implement_composite_recall** - Main retrieval engine
7. **integrate_and_package** - End-to-end tests and README

**Build Report:**
- Total duration: ~46 minutes with 3-way concurrency
- All 7 tasks passed on first attempt
- Clean merges (no conflicts)
- 26,925 lines added across all tasks

## 10. Notable Limitations and Future Work (v0.1 vs v0.2+)

**v0.1 (Current):**
- All scoring primitives (ACT-R, Ebbinghaus, Bayesian)
- Hebbian association learning via co-activation queue
- Composite recall() fusing all signals
- Multi-tenant via workspace_id
- Works with Postgres RLS
- Streaming replication compatible
- Connection pooling compatible

**Deferred to v0.2+:**
- Autonomous background worker (bgworker)
- Contradiction detection
- Predictive transitions
- Typed associations
- Shared preload libraries integration
- Adaptive polling and live stats

## Summary

**pg_recall** is a production-grade cognitive memory system built into PostgreSQL, bringing neuroscience-inspired algorithms (ACT-R, Hebbian, Ebbinghaus, Bayesian) to bear on the problem of intelligent memory retrieval. It fuses vector similarity, full-text search, temporal activation, associative context, and confidence tracking into a single ranked result set. The extension is lightweight, durable (no external services), multi-tenant by design, and fully composable — users can call individual scoring functions or use the high-level `recall()` interface. With comprehensive test coverage (~80+ test cases), it represents a sophisticated but pragmatic implementation of cognitive principles in a relational database.
