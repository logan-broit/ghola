# pg_recall

Cognitive Memory Primitives for Postgres.

A Postgres extension that implements neuroscience-inspired memory primitives as composable SQL functions over pgvector-enabled tables. Memories decay with time, strengthen through use, form associations automatically through co-activation, and track confidence via Bayesian updating.

## Cognitive Models

| Model | Function | Purpose |
|-------|----------|---------|
| ACT-R | `actr_activation()` | Base-level activation with power-law temporal decay |
| Ebbinghaus | `ebbinghaus_decay()` | Spacing-aware retention with stability estimation |
| Hebbian | `process_co_activation_batch()` | Association learning through co-activation |
| Bayesian | `bayesian_update()` | Confidence tracking with Laplace-smoothed posteriors |

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
CREATE EXTENSION pg_recall;    -- installs all objects in the pg_recall schema
```

## Schema

### Tables

**`pg_recall.mnemes`** — Primary memory store

| Column | Type | Description |
|--------|------|-------------|
| `id` | `uuid` | Primary key (auto-generated) |
| `workspace_id` | `uuid` | Tenant isolation key |
| `concept` | `text` | Short label for the memory |
| `content` | `text` | Full memory content |
| `embedding` | `vector(384)` | Semantic embedding |
| `search_vector` | `tsvector` | Auto-generated from concept (weight A) + content (weight B) |
| `confidence` | `float8` | Bayesian confidence, default 0.5 |
| `access_count` | `int` | Number of retrievals, default 0 |
| `last_access` | `timestamptz` | Last retrieval time |
| `created_at` | `timestamptz` | Creation time |
| `state` | `text` | One of: `active`, `archived`, `dormant` |

**`pg_recall.associations`** — Hebbian links between mnemes

| Column | Type | Description |
|--------|------|-------------|
| `src_id` | `uuid` | Source mneme (canonical: src_id < dst_id) |
| `dst_id` | `uuid` | Destination mneme |
| `weight` | `float8` | Association strength [0, 1] |
| `co_activations` | `int` | Number of co-activation events |
| `updated_at` | `timestamptz` | Last update time |

**`pg_recall.co_activation_queue`** — Pending Hebbian processing events

| Column | Type | Description |
|--------|------|-------------|
| `id` | `bigserial` | Auto-incrementing ID |
| `workspace_id` | `uuid` | Tenant key |
| `mneme_ids` | `uuid[]` | Co-activated mneme IDs |
| `scores` | `float8[]` | Corresponding recall scores |
| `created_at` | `timestamptz` | Event time |

### Indexes

- HNSW index on `mnemes(embedding)` for approximate nearest neighbor search
- GIN index on `mnemes(search_vector)` for full-text search
- B-tree on `mnemes(workspace_id, last_access DESC)` for temporal queries
- B-tree on `associations(dst_id, src_id)` for reverse lookups

### Composite Types

**`pg_recall.recall_result`** — Return type for `recall()`

```sql
(mneme_id uuid, score float8, content_match float8, activation float8,
 hebbian_boost float8, confidence float8, concept text, content text)
```

**`pg_recall.score_weights`** — Tuning parameters for `recall()`

```sql
(semantic float8, fts float8, actr_decay float8, hebbian_scale float8)
-- Defaults: (0.6, 0.4, 0.5, 4.0)
```

## Usage

### Inserting Memories

```sql
INSERT INTO pg_recall.mnemes (workspace_id, concept, content, embedding)
VALUES (
    'your-workspace-uuid',
    'kubernetes',
    'Pod scheduling uses the kube-scheduler to assign pods to nodes',
    '[0.1, 0.2, ...]'::vector(384)  -- 384-dim embedding from your model
);
```

### Recalling Memories

The `recall()` function fuses vector similarity, full-text search, temporal activation, Hebbian associations, and Bayesian confidence into one ranked result set:

```sql
SELECT * FROM pg_recall.recall(
    'your-workspace-uuid'::uuid,    -- workspace filter
    'how does pod scheduling work',  -- query text (for FTS)
    '[0.1, 0.2, ...]'::vector(384), -- query embedding (for vector search)
    10,                              -- limit
    0.0,                             -- min_confidence threshold
    NULL                             -- use default weights
);
```

With custom weights:

```sql
SELECT * FROM pg_recall.recall(
    'your-workspace-uuid'::uuid,
    'pod scheduling',
    '[0.1, 0.2, ...]'::vector(384),
    10,
    0.0,
    (0.8, 0.2, 0.5, 2.0)::pg_recall.score_weights  -- heavier semantic, lighter FTS
);
```

### Processing Hebbian Learning

Each `recall()` call automatically enqueues a co-activation event. Process these to form/strengthen associations:

```sql
-- Process a batch of pending events
SELECT pg_recall.process_co_activation_batch(100);

-- Or drain the entire queue
SELECT pg_recall.process_all_pending_co_activations();
```

Schedule with `pg_cron` for automatic processing:

```sql
SELECT cron.schedule('hebbian-learning', '*/5 * * * *',
    'SELECT pg_recall.process_all_pending_co_activations()');
```

### Confirming Recall

When a user confirms that recalled memories were useful, strengthen their confidence:

```sql
SELECT pg_recall.confirm_recall(ARRAY['mneme-id-1', 'mneme-id-2']::uuid[]);
```

### Inspecting Associations

```sql
SELECT * FROM pg_recall.get_associations('mneme-id'::uuid, 0.01);
```

### Individual Scoring Functions

All scoring primitives are available as standalone SQL functions for custom retrieval pipelines:

```sql
-- Smooth activation function (overflow-safe)
SELECT pg_recall.softplus(2.0);  -- ~2.13

-- ACT-R temporal activation
SELECT pg_recall.actr_activation(10, now() - interval '5 days');

-- Ebbinghaus spacing-aware decay
SELECT pg_recall.ebbinghaus_decay(
    now() - interval '30 days',   -- last_access
    50,                            -- access_count
    now() - interval '180 days'   -- created_at
);

-- Bayesian confidence update
SELECT pg_recall.bayesian_update(0.5, 0.95);  -- ~0.925

-- Update a mneme's confidence directly
SELECT pg_recall.update_confidence('mneme-id'::uuid, 0.95);
```

## Multi-Tenancy

All tables include a `workspace_id` column. The extension does not enforce Row-Level Security (RLS) itself but is designed to work with Postgres RLS policies:

```sql
ALTER TABLE pg_recall.mnemes ENABLE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON pg_recall.mnemes
    USING (workspace_id = current_setting('app.workspace_id')::uuid);
```

## Scoring Formula

The composite recall score is computed as:

```
content_match  = semantic_weight * cosine_similarity + fts_weight * tanh(fts_rank)
activation     = actr_activation(access_count, last_access, decay_exponent)
hebbian_boost  = sum of association weights to other candidates
temporal_weight = softplus(activation + hebbian_scale * hebbian_boost) / (1 + softplus(0))
score          = content_match * temporal_weight * confidence
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

## Version

0.1.0

## License

See LICENSE file.
