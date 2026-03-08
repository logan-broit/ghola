# pg_recall v0.4 — Typed Memory System

## Overview

pg_recall is the cognitive storage layer for Chapterhouse, an MCP memory server
for AI coding agents. v0.4 introduces **typed mnemes** and **typed associations**
— metadata that directly influences cognitive scoring rather than serving as
passive filters.

Every mneme now carries a `memory_type`, `scope`, `tier`, optional `tags`,
optional `session_id`, and optional `expires_at`. Every association carries an
`association_type` that determines its direction, creation mechanism, and
influence on recall scoring.

## Configurable Embedding Dimensions

The embedding dimension defaults to 768 but is configurable per installation.
Different Chapterhouse deployments use different embedding models:

| Deployment | Model | Dims | Notes |
|------------|-------|------|-------|
| Homelab (Together.ai) | `Alibaba-NLP/gte-modernbert-base` | 768 | Best code retrieval; 8K context |
| Corporate (air-gapped) | `openai/text-embedding-3-large` | 3072 | Matryoshka; dimension-truncatable |

### Configuration

The dimension is stored in `pg_recall.config` and defaults to 768. To use a
different dimension, call `configure_dimensions()` on an empty mnemes table:

```sql
SELECT pg_recall.configure_dimensions(3072);
```

This alters the embedding column type, drops and recreates the HNSW index, and
updates the config table. It cannot be called once data exists — changing
dimensions after population requires recreating the extension.

Functions that accept embeddings (`recall()`, `check_contradictions()`) use
`vector(768)` in the SQL wrapper. pgvector rejects dimension mismatches at
runtime.

### Recommended Models

For agentic coding use cases (code snippets, architectural decisions, debugging
insights, technical documentation):

**Together.ai:** `Alibaba-NLP/gte-modernbert-base` (768 dims)
- 8,192 token context (16x more than bge-base's 512)
- Best code retrieval scores on the platform (79.31 COIR, 88+ CodeSearchNet)
- Highest MTEB retrieval score (55.33) despite being smaller than 1024-dim models

**OpenAI:** `text-embedding-3-large` (3072 dims, or 1024 truncated)
- Strong general-purpose performance
- Matryoshka representation learning allows dimension truncation without retraining

## Typed Mnemes

### Columns

```sql
memory_type  text NOT NULL DEFAULT 'factual'
    CHECK (memory_type IN ('factual', 'experiential', 'working'))
scope        text NOT NULL DEFAULT 'personal'
    CHECK (scope IN ('personal', 'org'))
tier         text NOT NULL DEFAULT 'index'
    CHECK (tier IN ('core', 'index', 'state'))
tags         text[] NOT NULL DEFAULT '{}'
session_id   uuid
expires_at   timestamptz
```

### How Types Influence Cognitive Scoring

Types are not passive metadata. The scoring engine adapts its behavior based on
memory type, mirroring how real cognition handles different kinds of knowledge.

#### Memory Type → ACT-R Decay

| Type | Decay Behavior | Rationale |
|------|---------------|-----------|
| `factual` | Standard ACT-R decay exponent | Facts should be stable. |
| `experiential` | Standard | Lessons learned are contextual. |
| `working` | 2x ACT-R decay exponent (capped at 1.5) | Session context is ephemeral by design. |

In `recall_inner()`, working memories receive a doubled decay exponent,
causing them to fade from recall results much faster than factual or
experiential memories.

#### Tier → Confidence Floor

| Tier | Floor | Effect |
|------|-------|--------|
| `core` | 0.30 | Never drops below moderate confidence, even under strong contradictory evidence. Requires explicit deletion, not erosion. |
| `index` | 0.025 | Standard Laplace bound. |
| `state` | 0.025 | Standard bound, but excluded from Hebbian learning. |

Both `update_confidence()` and `confirm_recall()` read the mneme's tier and
clamp the Bayesian posterior to the tier's floor.

#### State-Tier Hebbian Exclusion

State-tier mnemes are filtered out of pair generation in
`process_co_activation_batch()`. They still appear in recall results and get
access tracking updates, but they do not form Hebbian associations — their
relationships are transient by nature.

#### Scope → Recall Filtering

Scope filtering is caller-driven. `recall()` accepts an optional `scope`
parameter. Chapterhouse passes the appropriate filter based on its auth context:

- `personal` — only returned when the caller's user matches the creator
- `org` — returned to any user in the same organization

#### Session ID → Episodic Association

When a mneme is inserted with a `session_id`, a trigger automatically creates
`session` type associations (weight 0.5) to all existing mnemes in the same
workspace and session. This enables episodic recall — querying for one memory
from a session automatically boosts related memories from that same session.

Additionally, when `session_id` is passed to `recall()`, mnemes from that
session receive a direct +0.3 score boost independent of associations.

#### Working Memory Expiration

Mnemes with `expires_at < now()` are excluded from all recall queries. The
background worker archives them (state → `dormant`) every 10 minutes.

#### Tags → Recall Filtering

`recall()` accepts an optional `tags text[]` parameter. When provided, only
mnemes containing ALL specified tags are returned (AND semantics via the
`@>` array containment operator).

### Indexes

```sql
CREATE INDEX mnemes_memory_type_idx ON mnemes (workspace_id, memory_type);
CREATE INDEX mnemes_session_id_idx  ON mnemes (session_id) WHERE session_id IS NOT NULL;
CREATE INDEX mnemes_tags_idx        ON mnemes USING gin (tags);
CREATE INDEX mnemes_expires_at_idx  ON mnemes (expires_at) WHERE expires_at IS NOT NULL;
```

## Typed Associations

### Schema

The `associations` table supports directional, typed relationships. The primary
key is `(src_id, dst_id, association_type)` — the same pair of mnemes can have
multiple typed relationships (e.g., both a `hebbian` link from co-activation
AND a `contradicts` link from contradiction resolution).

```sql
CREATE TABLE associations (
    src_id           uuid NOT NULL REFERENCES mnemes(id) ON DELETE CASCADE,
    dst_id           uuid NOT NULL REFERENCES mnemes(id) ON DELETE CASCADE,
    association_type text NOT NULL DEFAULT 'hebbian'
        CHECK (association_type IN ('hebbian','contradicts','supersedes','supports','session')),
    weight           double precision NOT NULL DEFAULT 0.01,
    co_activations   integer NOT NULL DEFAULT 0,
    updated_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (src_id, dst_id, association_type)
);
```

### Association Types

| Type | Direction | Creation Mechanism | Scoring Influence |
|------|-----------|-------------------|-------------------|
| `hebbian` | Undirected (canonical `src < dst`) | Co-activation batch processing | Full weight (1.0x) |
| `supports` | Directed: src supports dst | `mark_supports()` | Moderate boost (0.5x) |
| `session` | Undirected (canonical `src < dst`) | Auto-trigger on insert with shared `session_id` | Mild boost (0.3x) |
| `contradicts` | Directed: src contradicts dst | `resolve_contradiction('confirmed')` | Negative boost (-0.5x) |
| `supersedes` | Directed: src replaces dst | `mark_supersedes()` | No scoring; archives the older mneme |

For **undirected** types (`hebbian`, `session`), canonical ordering is
maintained by convention in insertion code: `src_id = LEAST(a, b)`,
`dst_id = GREATEST(a, b)`.

For **directed** types (`contradicts`, `supersedes`, `supports`), `src_id` is
the subject and `dst_id` is the object: "src contradicts dst", "src supersedes
dst", "src supports dst".

### Type-Aware Boost Calculation

In `recall_inner()`, the aggregate Hebbian boost for each candidate is:

```
boost = sum(weight * 1.0  WHERE type = 'hebbian')
      + sum(weight * 0.5  WHERE type = 'supports')
      + sum(weight * 0.3  WHERE type = 'session')
      - sum(weight * 0.5  WHERE type = 'contradicts')
      + (0.3 if candidate.session_id matches requested session_id)
```

`supersedes` links contribute nothing to scoring — the older mneme is already
archived and excluded from recall results.

## Functions

### Association Management

#### `mark_supersedes(newer_id uuid, older_id uuid) -> text`

Creates a directed `supersedes` association (weight 1.0) from `newer_id` to
`older_id` and sets the older mneme's state to `archived`. Errors if either
mneme doesn't exist or if called with the same ID for both.

#### `mark_supports(supporting_id uuid, supported_id uuid) -> text`

Creates a directed `supports` association (weight 1.0) and boosts the supported
mneme's confidence via `bayesian_update(confidence, 0.65)`, respecting the
tier confidence floor.

#### `get_typed_associations(mneme_id uuid, association_type text DEFAULT NULL, min_weight float8 DEFAULT 0.01) -> SETOF (related_id, association_type, weight, direction)`

Queries associations for a mneme in both directions. The `direction` field is
`'outgoing'` (mneme is src) or `'incoming'` (mneme is dst). Optional type
filter narrows results to a single association type.

### Updated `recall()` Signature

```sql
CREATE FUNCTION recall(
    workspace_id uuid,
    query_text text,
    query_embedding vector(768),
    limit_n int DEFAULT 10,
    min_confidence float8 DEFAULT 0.0,
    weights pg_recall.score_weights DEFAULT NULL,
    memory_type text DEFAULT NULL,
    scope text DEFAULT NULL,
    tags text[] DEFAULT NULL,
    session_id uuid DEFAULT NULL
) RETURNS SETOF pg_recall.recall_result
```

All new filter parameters are optional. When NULL, no filtering is applied for
that dimension. `session_id` both filters and boosts.

## Background Worker

The v0.2 background worker runs four periodic maintenance jobs:

| Job | Interval | Action |
|-----|----------|--------|
| Association decay | Every 1 hour | `weight *= 0.999` for associations not updated in the last day |
| Association pruning | Every 1 hour | Delete associations with `weight < 0.001` |
| Dormant archival | Every 6 hours | Archive active mnemes with `last_access > 90 days` and `confidence < 0.3` |
| State cleanup | Every 6 hours | Archive state-tier mnemes with `last_access > 24 hours` |
| Working memory expiration | Every 10 minutes | Archive working mnemes past their `expires_at` |

## Contradiction Integration

When a contradiction is confirmed via `resolve_contradiction('confirmed')`:

1. The newer mneme's confidence is penalized (Bayesian update with evidence 0.10)
2. Any existing `hebbian` association between the pair is weakened (weight *= 0.1)
3. A `contradicts` association is created from the newer mneme to the older mneme (weight 1.0)

The `contradicts` association then feeds into recall scoring as a negative
boost (-0.5x weight), actively demoting contradicted mnemes in results.

## Source Layout

| File | Contents |
|------|----------|
| `src/schema.rs` | Tables, indexes, config, `configure_dimensions()`, session trigger |
| `src/recall.rs` | `recall()` wrapper, `recall_inner()` with type-aware scoring |
| `src/hebbian.rs` | `update_confidence()`, `confirm_recall()`, `process_co_activation_batch()` with tier floor and state exclusion |
| `src/associations.rs` | `mark_supersedes()`, `mark_supports()`, `get_typed_associations()` |
| `src/contradiction.rs` | `resolve_contradiction()` with `contradicts` association creation |
| `src/scoring.rs` | Pure math: `bayesian_update_inner`, `actr_activation_inner`, `softplus_inner` |
| `src/worker.rs` | Background worker with expiration and state cleanup jobs |
| `src/integration_tests.rs` | End-to-end tests covering the full v0.4 facility |

## Design Decisions

1. **Configurable embedding dimensions.** Different deployments use different
   models (768 vs 3072). The dimension is set once via `configure_dimensions()`
   and cannot be changed with data present.

2. **Directional associations.** Enables natural relationship semantics
   ("A contradicts B" is different from "B contradicts A"). Undirected types
   maintain canonical ordering by convention.

3. **Multiple association types per pair.** The same mneme pair can have both a
   `hebbian` link and a `contradicts` link. These are independent signals with
   different scoring contributions.

4. **Type-aware scoring in the extension.** Scoring adjustments happen in Rust/SQL
   functions, not in Chapterhouse Go code. This keeps the cognitive model
   cohesive and avoids splitting scoring logic across two codebases.

5. **Working memory expiration.** Handled by the background worker, not by the
   caller. Expired memories are archived, not deleted — they can be recovered
   if needed.

6. **No versioning in pg_recall.** Chapterhouse's `is_current` / `version`
   system is a higher-level concern. Version relationships are modeled via
   `supersedes` associations — the newer mneme supersedes the older one, which
   gets archived.
