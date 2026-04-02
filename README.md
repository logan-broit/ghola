# pg_ghola

Cognitive Memory Primitives for Postgres.

A Postgres extension that implements neuroscience-inspired memory primitives as composable SQL functions over pgvector-enabled tables. Memories decay with time, strengthen through use, form associations automatically through co-activation, and track confidence via Bayesian updating. A typed memory system classifies memories and associations to influence cognitive scoring — working memories decay faster, core memories resist erosion, contradictions apply negative boost, and session context provides episodic recall.

## Cognitive Models

| Model | Function | Purpose |
|-------|----------|---------|
| ACT-R | `actr_activation()` | Base-level activation with power-law temporal decay |
| Ebbinghaus | `ebbinghaus_decay()` | Spacing-aware retention with stability estimation |
| Hebbian | `process_co_activation_batch()` | Association learning through co-activation |
| Bayesian | `bayesian_update()` | Confidence tracking with Laplace-smoothed posteriors |
| Contradiction | `flag_contradictions()` | Detection and flagging of contradicting memories |

## Requirements

| Dependency | Version | Notes |
|------------|---------|-------|
| Rust | 1.94+ | Stable toolchain |
| pgrx | 0.17.x | With `pg18` feature flag |
| PostgreSQL | 18.3+ | Target runtime |
| pgvector | 0.8.2+ | Required dependency |

## Installation

```bash
# Install pgrx if not already installed
cargo install cargo-pgrx --version "=0.17.0"
cargo pgrx init --pg18 $(which pg_config)

# Build and install the extension
cargo pgrx install --release
```

Then in PostgreSQL:

```sql
CREATE EXTENSION vector;       -- pgvector must be installed first
CREATE EXTENSION pg_ghola;    -- installs all objects in the pg_ghola schema

-- Optional: configure embedding dimensions (default 768, must be called before inserting data)
SELECT pg_ghola.configure_dimensions(3072);  -- e.g. for OpenAI text-embedding-3-large
```

## Schema

### Tables

**`pg_ghola.mnemes`** — Primary memory store

| Column | Type | Description |
|--------|------|-------------|
| `id` | `uuid` | Primary key (auto-generated) |
| `workspace_id` | `uuid` | Tenant isolation key |
| `concept` | `text` | Short label for the memory |
| `content` | `text` | Full memory content |
| `embedding` | `vector(768)` | Semantic embedding (dimension configurable) |
| `search_vector` | `tsvector` | Auto-generated from concept (weight A) + content (weight B) |
| `confidence` | `float8` | Bayesian confidence, default 0.5 |
| `access_count` | `int` | Number of retrievals, default 0 |
| `last_access` | `timestamptz` | Last retrieval time |
| `created_at` | `timestamptz` | Creation time |
| `state` | `text` | One of: `active`, `archived`, `dormant` |
| `memory_type` | `text` | One of: `factual`, `experiential`, `working` |
| `scope` | `text` | One of: `personal`, `org` |
| `tier` | `text` | One of: `core`, `index`, `state` |
| `tags` | `text[]` | Free-form tags for filtering |
| `session_id` | `uuid` | Episodic session grouping |
| `expires_at` | `timestamptz` | Expiration time (working memories) |

**`pg_ghola.associations`** — Typed links between mnemes

| Column | Type | Description |
|--------|------|-------------|
| `src_id` | `uuid` | Source mneme |
| `dst_id` | `uuid` | Destination mneme |
| `association_type` | `text` | One of: `hebbian`, `contradicts`, `supersedes`, `supports`, `session` |
| `weight` | `float8` | Association strength [0, 1] |
| `co_activations` | `int` | Number of co-activation events |
| `updated_at` | `timestamptz` | Last update time |

Primary key: `(src_id, dst_id, association_type)` — the same pair can have multiple typed relationships.

**`pg_ghola.co_activation_queue`** — Pending Hebbian processing events

| Column | Type | Description |
|--------|------|-------------|
| `id` | `bigserial` | Auto-incrementing ID |
| `workspace_id` | `uuid` | Tenant key |
| `mneme_ids` | `uuid[]` | Co-activated mneme IDs |
| `scores` | `float8[]` | Corresponding recall scores |
| `created_at` | `timestamptz` | Event time |

**`pg_ghola.config`** — Extension configuration

| Column | Type | Description |
|--------|------|-------------|
| `key` | `text` | Configuration key (primary key) |
| `value` | `text` | Configuration value |

Default entry: `embedding_dims = '768'`.

**`pg_ghola.contradiction_candidates`** — Flagged contradicting mneme pairs

| Column | Type | Description |
|--------|------|-------------|
| `id` | `bigserial` | Primary key |
| `workspace_id` | `uuid` | Tenant key |
| `mneme_a` | `uuid` | Newer mneme (the one that triggered detection) |
| `mneme_b` | `uuid` | Existing mneme |
| `similarity` | `float8` | Cosine similarity between embeddings |
| `concept_overlap` | `boolean` | Whether concepts match exactly |
| `status` | `text` | One of: `pending`, `confirmed`, `dismissed` |
| `created_at` | `timestamptz` | Detection time |
| `resolved_at` | `timestamptz` | Resolution time (null if pending) |

**`pg_ghola.worker_stats`** — Background worker operational state (singleton)

| Column | Type | Description |
|--------|------|-------------|
| `id` | `int` | Always 1 (enforced by CHECK) |
| `state` | `text` | Worker state: `stopped`, `active`, `idle`, `dormant`, `shutdown` |
| `queue_depth` | `bigint` | Current co-activation queue depth |
| `batches_processed` | `bigint` | Total batches processed since start |
| `rows_processed` | `bigint` | Total queue rows processed |
| `pairs_updated` | `bigint` | Total association pairs updated |
| `last_batch_at` | `timestamptz` | Last batch processing time |
| `last_decay_at` | `timestamptz` | Last decay/pruning cycle time |
| `poll_interval_ms` | `int` | Current polling interval |
| `started_at` | `timestamptz` | Worker start time |
| `updated_at` | `timestamptz` | Last stats update |

### Indexes

- HNSW index on `mnemes(embedding)` for approximate nearest neighbor search
- GIN index on `mnemes(search_vector)` for full-text search
- B-tree on `mnemes(workspace_id, last_access DESC)` for temporal queries
- B-tree on `mnemes(workspace_id, memory_type)` for type-filtered queries
- B-tree on `mnemes(session_id)` (partial: where not null)
- GIN on `mnemes(tags)` for tag containment queries
- B-tree on `mnemes(expires_at)` (partial: where not null)
- B-tree on `associations(dst_id, src_id)` for reverse lookups
- B-tree on `contradiction_candidates(workspace_id, status)` for pending lookups

### Composite Types

**`pg_ghola.recall_result`** — Return type for `recall()`

```sql
(mneme_id uuid, score float8, content_match float8, activation float8,
 hebbian_boost float8, confidence float8, concept text, content text)
```

**`pg_ghola.score_weights`** — Tuning parameters for `recall()`

```sql
(semantic float8, fts float8, actr_decay float8, hebbian_scale float8)
-- Defaults: (0.6, 0.4, 0.5, 4.0)
```

**`pg_ghola.contradiction_candidate_result`** — Return type for `check_contradictions()`

```sql
(candidate_id bigint, mneme_a uuid, mneme_b uuid, similarity float8, concept_overlap boolean)
```

**`pg_ghola.contradiction_detail`** — Return type for `get_pending_contradictions()`

```sql
(candidate_id bigint, similarity float8, concept_overlap boolean,
 concept_a text, content_a text, confidence_a float8,
 concept_b text, content_b text, confidence_b float8, created_at timestamptz)
```

## Usage

### Inserting Memories

```sql
INSERT INTO pg_ghola.mnemes (workspace_id, concept, content, embedding,
                              memory_type, tier, tags, session_id)
VALUES (
    'your-workspace-uuid',
    'kubernetes',
    'Pod scheduling uses the kube-scheduler to assign pods to nodes',
    '[0.1, 0.2, ...]'::vector(768),  -- embedding from your model
    'factual',                         -- memory type
    'index',                           -- tier
    ARRAY['k8s', 'scheduling'],        -- tags
    'session-uuid'                     -- optional session grouping
);
```

When a mneme is inserted with a `session_id`, a trigger automatically creates `session` associations to other mnemes in the same workspace and session. A separate trigger flags potential contradictions with existing mnemes.

### Recalling Memories

The `recall()` function fuses vector similarity, full-text search, temporal activation, Hebbian associations, and Bayesian confidence into one ranked result set:

```sql
SELECT * FROM pg_ghola.recall(
    'your-workspace-uuid'::uuid,    -- workspace filter
    'how does pod scheduling work',  -- query text (for FTS)
    '[0.1, 0.2, ...]'::vector(768), -- query embedding (for vector search)
    10,                              -- limit
    0.0,                             -- min_confidence threshold
    NULL                             -- use default weights
);
```

With filters and session context:

```sql
SELECT * FROM pg_ghola.recall(
    'your-workspace-uuid'::uuid,
    'pod scheduling',
    '[0.1, 0.2, ...]'::vector(768),
    10,
    0.0,
    NULL,                           -- default weights
    'factual',                      -- memory_type filter
    'personal',                     -- scope filter
    ARRAY['k8s'],                   -- tag filter (AND semantics)
    'session-uuid'::uuid            -- session filter + boost
);
```

When `session_id` is provided, mnemes from that session receive a +0.3 scoring boost.

### Typed Memory Behavior

Memory types influence cognitive scoring beyond simple filtering:

| Type | Scoring Effect |
|------|---------------|
| `working` | 2x ACT-R decay exponent — fades from results faster |
| `core` tier | Confidence never drops below 0.30, even under contradiction |
| `state` tier | Excluded from Hebbian association learning |
| Expired (`expires_at < now()`) | Excluded from all recall results |

### Association Types

| Type | Direction | Scoring Influence | Created By |
|------|-----------|-------------------|------------|
| `hebbian` | Undirected | Full weight (1.0x) | Co-activation batch processing |
| `supports` | Directed | Moderate boost (0.5x) | `mark_supports()` |
| `session` | Undirected | Mild boost (0.3x) | Auto-trigger on insert |
| `contradicts` | Directed | Negative boost (-0.5x) | `resolve_contradiction('confirmed')` |
| `supersedes` | Directed | No scoring; archives older mneme | `mark_supersedes()` |

### Managing Associations

```sql
-- Mark a newer mneme as superseding an older one (archives the older mneme)
SELECT pg_ghola.mark_supersedes('newer-id'::uuid, 'older-id'::uuid);

-- Mark supporting evidence (boosts supported mneme's confidence)
SELECT pg_ghola.mark_supports('evidence-id'::uuid, 'claim-id'::uuid);

-- Query typed associations for a mneme
SELECT * FROM pg_ghola.get_typed_associations('mneme-id'::uuid);
SELECT * FROM pg_ghola.get_typed_associations('mneme-id'::uuid, 'supports');
SELECT * FROM pg_ghola.get_typed_associations('mneme-id'::uuid, NULL, 0.1);
```

### Processing Hebbian Learning

Each `recall()` call automatically enqueues a co-activation event. The background worker processes these automatically; you can also process them manually:

```sql
-- Process a batch of pending events
SELECT pg_ghola.process_co_activation_batch(100);

-- Or drain the entire queue
SELECT pg_ghola.process_all_pending_co_activations();
```

State-tier mnemes are excluded from Hebbian pair generation — they appear in recall results but do not form associations.

#### Background Worker

The background worker automatically drains the co-activation queue, runs periodic
maintenance, and archives stale memories. To enable it:

```
# postgresql.conf
shared_preload_libraries = 'pg_ghola'
pg_ghola.database = 'memories'          # database where extension is installed
```

Restart PostgreSQL after changing `shared_preload_libraries`. The worker adapts its
polling interval based on queue activity:

| State | Poll Interval | Transition |
|-------|--------------|------------|
| Active | 100ms | No rows for 30s → Idle |
| Idle | 1s | No rows for 5min → Dormant |
| Dormant | 5s | Rows found → Active |

**Periodic maintenance:**

| Job | Interval | Action |
|-----|----------|--------|
| Association decay | 1 hour | 0.1% weight reduction on stale associations (>1 day old) |
| Association pruning | 1 hour | Remove associations with weight < 0.001 |
| Dormant archival | 6 hours | Archive active mnemes with 90+ days inactive and confidence < 0.3 |
| State cleanup | 6 hours | Archive state-tier mnemes with >24 hours inactive |
| Working memory expiration | 10 minutes | Archive working mnemes past their `expires_at` |

**Monitoring:**

```sql
SELECT * FROM pg_ghola.get_worker_stats();
```

Without `shared_preload_libraries`, the extension works fully — process events manually or via `pg_cron`.

### Contradiction Detection

New mnemes are automatically checked for contradictions via an `AFTER INSERT` trigger.
When a new mneme has high cosine similarity (>= 0.85) to an existing active mneme in the
same workspace, a contradiction candidate is flagged for review.

```sql
-- Check for contradictions without flagging (read-only)
SELECT * FROM pg_ghola.check_contradictions('mneme-id'::uuid, 0.85);

-- Flag contradictions (inserts into contradiction_candidates)
SELECT pg_ghola.flag_contradictions('mneme-id'::uuid, 0.85);

-- Review pending contradictions with full mneme details
SELECT * FROM pg_ghola.get_pending_contradictions('workspace-id'::uuid);

-- Scan entire workspace for contradictions (batch)
SELECT pg_ghola.scan_workspace_contradictions('workspace-id'::uuid, 0.85);
```

**Resolving contradictions:**

```sql
-- Confirm: penalizes the newer mneme, weakens Hebbian association,
-- and creates a 'contradicts' typed association (negative recall boost)
SELECT pg_ghola.resolve_contradiction(candidate_id, 'confirmed');

-- Dismiss: marks as dismissed, no side effects
SELECT pg_ghola.resolve_contradiction(candidate_id, 'dismissed');
```

### Confirming Recall

When a user confirms that recalled memories were useful, strengthen their confidence:

```sql
SELECT pg_ghola.confirm_recall(ARRAY['mneme-id-1', 'mneme-id-2']::uuid[]);
```

### Configuring Embedding Dimensions

The default embedding dimension is 768. To use a different model, call `configure_dimensions()` before inserting any data:

```sql
SELECT pg_ghola.configure_dimensions(3072);  -- for text-embedding-3-large
```

This alters the embedding column type, recreates the HNSW index, and updates the config table. Cannot be called once data exists.

### Individual Scoring Functions

All scoring primitives are available as standalone SQL functions:

```sql
SELECT pg_ghola.softplus(2.0);                                    -- ~2.13
SELECT pg_ghola.actr_activation(10, now() - interval '5 days');   -- temporal activation
SELECT pg_ghola.ebbinghaus_decay(                                 -- spacing-aware decay
    now() - interval '30 days', 50, now() - interval '180 days');
SELECT pg_ghola.bayesian_update(0.5, 0.95);                       -- ~0.925
SELECT pg_ghola.update_confidence('mneme-id'::uuid, 0.95);        -- update and persist
```

## Scoring Formula

The composite recall score is computed as:

```
content_match   = semantic_weight * cosine_similarity + fts_weight * tanh(fts_rank)
effective_decay = actr_decay * 2.0 if memory_type = 'working', else actr_decay
activation      = actr_activation(access_count, last_access, effective_decay)

hebbian_boost   = sum(weight * 1.0  where type = 'hebbian')
                + sum(weight * 0.5  where type = 'supports')
                + sum(weight * 0.3  where type = 'session')
                - sum(weight * 0.5  where type = 'contradicts')
                + 0.3 if candidate.session_id matches requested session_id

temporal_weight = softplus(activation + hebbian_scale * hebbian_boost) / (1 + softplus(0))
score           = content_match * temporal_weight * confidence
```

## Multi-Tenancy

All tables include a `workspace_id` column. The extension does not enforce Row-Level Security (RLS) itself but is designed to work with Postgres RLS policies:

```sql
ALTER TABLE pg_ghola.mnemes ENABLE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON pg_ghola.mnemes
    USING (workspace_id = current_setting('app.workspace_id')::uuid);
```

## Development

```bash
# Run tests (requires Postgres 18)
cargo pgrx test pg18

# Generate SQL schema
cargo pgrx schema pg18

# Run interactive Postgres with extension loaded
cargo pgrx run pg18
```

## Documentation

- [Typed Memory System](docs/typed-memory-system.md) — typed mnemes, typed associations, type-aware scoring
- [Contradiction Detection](docs/contradiction-detection.md) — detection strategy, resolution flow, Bayesian integration

## Version

0.4.0

## License

See LICENSE file.
