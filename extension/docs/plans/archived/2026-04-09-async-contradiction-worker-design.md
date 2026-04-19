# Async Contradiction Detection Worker

**Date:** 2026-04-09
**Status:** Approved

## Problem

The `contradiction_check_trigger` fires `flag_contradictions(NEW.id, 0.85)` synchronously on every INSERT. This runs an HNSW scan of top-50 neighbors per row. On live usage (few inserts/day) it's fine. On bulk loads (19K inserts for benchmarking), it causes quadratic behavior and corrupts confidence distributions via co-activation cascades.

## Neuroscience Basis

- **Detection** is fast (<1s): hippocampal mismatch signal fires within 520-735ms (Kumaran & Maguire 2007)
- **Resolution** is slow (hours-days): mPFC schema integration takes 4+ hours for congruent info, full sleep cycle for incongruent (van Kesteren 2012)

The brain optimizes for fast encoding, slow consolidation. pg_ghola should too.

## Design

### New table: `ghola.contradiction_queue`

```sql
CREATE TABLE contradiction_queue (
    id           bigserial PRIMARY KEY,
    workspace_id uuid NOT NULL,
    mneme_id     uuid NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
```

### Modified trigger (lightweight enqueue)

Replace the HNSW scan with a queue insert:

```sql
CREATE OR REPLACE FUNCTION contradiction_check_trigger()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO @extschema@.contradiction_queue (workspace_id, mneme_id)
    VALUES (NEW.workspace_id, NEW.id);
    RETURN NEW;
END;
$$;
```

### New background worker: "pg_ghola Contradiction Worker"

- Registered in `_PG_init()` as second `BackgroundWorkerBuilder`
- Entry point: `contradiction_worker_main`
- Adaptive polling: 5s (Active) / 30s (Idle) / 300s (Dormant)
- Active->Idle: 30s with zero work. Idle->Dormant: 300s with zero work. Any->Active: on work found.

Per cycle:
1. Dequeue one row from `contradiction_queue` (ORDER BY id LIMIT 1)
2. If empty, return 0
3. Call `flag_contradictions(mneme_id, 0.85)` (existing function, unchanged)
4. Delete consumed queue row
5. Return 1

### Worker stats: `ghola.contradiction_worker_stats`

```sql
CREATE TABLE contradiction_worker_stats (
    state            text NOT NULL DEFAULT 'stopped',
    queue_depth      bigint NOT NULL DEFAULT 0,
    scans_completed  bigint NOT NULL DEFAULT 0,
    candidates_found bigint NOT NULL DEFAULT 0,
    last_scan_at     timestamptz,
    poll_interval_ms integer NOT NULL DEFAULT 5000
);
```

## What doesn't change

- `flag_contradictions()` -- same HNSW scan logic, just called async
- `contradiction_candidates` table -- same schema
- `resolve_contradiction()` -- still explicit manual/agent resolution
- `check_contradictions()` -- read-only, unchanged
- Session association trigger -- stays synchronous (lightweight INSERT)
- Hebbian worker -- untouched, separate process

## Data flow

```
INSERT mneme
  ├─ session_association_trigger (sync) -> link session peers
  └─ contradiction_check_trigger (sync) -> enqueue to contradiction_queue

... 5-300 seconds later ...

Contradiction Worker
  └─ dequeue one mneme_id
  └─ flag_contradictions(mneme_id, 0.85) -> HNSW scan, insert candidates
  └─ delete queue row
```

## Files to modify

- `src/schema.rs` -- add `contradiction_queue` and `contradiction_worker_stats` tables, modify trigger DDL
- `src/lib.rs` -- register second BackgroundWorkerBuilder in `_PG_init()`
- `src/worker.rs` -- extract shared worker infrastructure (StateMachine, stats), keep Hebbian-specific logic
- `src/contradiction_worker.rs` (new) -- contradiction worker main loop, queue drain, stats
- `src/contradiction.rs` -- no changes to `flag_contradictions`, just called from different context
- `src/integration_tests.rs` -- update trigger tests, add worker queue tests

## Testing

1. INSERT a mneme -> verify it appears in `contradiction_queue`
2. Call `process_contradiction_queue_item()` manually -> verify it drains and flags
3. Bulk insert 100 mnemes -> verify queue has 100 rows, INSERT performance is fast
4. Worker integration: start worker, insert mneme, wait, verify candidate appears
