# Schema migration strategy for thousand-brains features

## Status: Open

## Problem

The four thousand-brains specs introduce schema changes that need a migration
strategy for existing pg_ghola installations:

### New columns on existing tables
- `associations.displacement` — vector column, NULL default (backwards compatible)
- `associations.magnitude` — float column, NULL default

### New tables
- `contexts` — context embeddings with HNSW index
- `context_evidence` — per-context evidence accumulation (PK: mneme_id, context_id)
- `context_shift` — per-context shift vectors (from learn_context_shift)
- `workspace_projection` — workspace-wide U/V matrices (from learn_workspace_projection)
- `projection_training_event` — training data for projection learning

### New indexes needed
- HNSW index on `context.embedding` for deduplication in register_context
- Index on `context_evidence(context_id, last_observed)` for decay_context_evidence
- Index on `projection_training_event(context_id, created_at)` for learn_context_shift

## Current schema mechanism

pg_ghola uses `extension_sql!` macros in `src/schema.rs` with `requires` for
ordering. Schema is created at `CREATE EXTENSION` time. There is no versioned
migration system — the extension creates all objects from scratch.

The background worker (registered in `_PG_init`) does not create schema. It
assumes tables exist and uses SPI to process co-activation batches.

## What needs to be decided

1. **Extension version bump**: These changes warrant a version bump (e.g., 0.4 → 0.5).
   The control file needs `default_version` updated.

2. **Upgrade script**: PostgreSQL extensions support versioned upgrades via
   `ALTER EXTENSION pg_ghola UPDATE TO '0.5'`. This requires a migration SQL
   file (e.g., `pg_ghola--0.4--0.5.sql`) that:
   - ALTERs the associations table to add displacement/magnitude columns
   - CREATEs the new tables with proper constraints and indexes
   - CREATEs the new SQL functions (recall_voted, recall_by_analogy, etc.)

3. **Background worker during migration**: The existing Hebbian worker should
   continue to function during and after migration. New worker tasks (evidence
   decay, projection training) should be added to the worker loop with
   feature-detection: check if tables exist before attempting to process them.
   This allows the worker to handle both pre- and post-migration schemas.

4. **extension_sql! ordering**: New macros need `requires` declarations:
   - `create_context_table` requires `create_mnemes_table`
   - `create_context_evidence_table` requires `create_mnemes_table`, `create_context_table`
   - `create_context_shift_table` requires `create_context_table`
   - `create_workspace_projection_table` (standalone)
   - `create_projection_training_event_table` requires `create_context_table`
   - `alter_associations_add_displacement` requires `create_associations_table`

## Recommendation

1. Add `extension_sql!` macros for all new tables with proper `requires` ordering
2. Create a migration SQL file for the version upgrade path
3. Feature-detect in the background worker: check for table existence before
   processing displacement/evidence/projection tasks
4. NULL defaults on new columns ensure existing associations continue to work
   without migration (the `backwards_compatible` constraint in each spec)
