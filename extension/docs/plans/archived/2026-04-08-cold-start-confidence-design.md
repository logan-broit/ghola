# Cold-Start Confidence Fix

**Date:** 2026-04-08
**Status:** Approved

## Problem

pg_ghola's recall scoring formula multiplies `content_match x temporal_weight x confidence`. Default confidence is 0.5, meaning every new memory gets a 0.5x score penalty before it receives any feedback. This causes pg_ghola to underperform vanilla pgvector (which has no confidence penalty) on cold-loaded data.

LongMemEval-S results: pg_ghola 16.4% R@5 vs pgvector 34.6% R@5. The 0.5x confidence multiplier on all cold memories is the primary cause.

## Fix

Change default confidence from 0.5 to 1.0. New memories are trusted until evidence says otherwise (truth bias). Bayesian updates adjust from there.

## Changes

1. `src/schema.rs:28` -- `DEFAULT 0.5` -> `DEFAULT 1.0`
2. Update integration tests asserting initial confidence == 0.5
3. SQL migration for existing deployed memories

## Migration

```sql
UPDATE ghola.mnemes SET confidence = 1.0
WHERE confidence = 0.5
  AND id NOT IN (
    SELECT DISTINCT mneme_id FROM ghola.associations
    WHERE association_type = 'contradicts'
  );
```

## Verification

Re-run LongMemEval-S benchmark with ghola_mcp backend after deploying the fix. Compare against previous run and pgvector baseline.
