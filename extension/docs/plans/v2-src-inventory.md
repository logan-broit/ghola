# v2 src inventory — KEEP / SIMPLIFY / DELETE

Snapshot of `extension/src/` before the v2 rewrite. Decisions drive
Phase 1 tasks 1.2 through 1.10. Baseline: pg_ghola had a 12-table
schema named `pg_ghola`, with sub_mnemes, cluster pathway, thalamic
gating columns, and matched_position. v2 keeps the cognitive
primitives but simplifies to a 5-table schema named `semantic`.

## Summary

Starting state: 9,245 lines across 13 `.rs` files + 1 bin stub.
Target state: fewer files, ~40% fewer lines. Single pathway, one
schema rename, no sub_mnemes, no gating.

| File | Lines | Decision | Why |
|---|---|---|---|
| `lib.rs` | 101 | **SIMPLIFY** | Drop `gating_worker` module + `_PG_init` registration. Drop `allow_system_table_mods` from test setup (no longer needed once schema renames from `pg_ghola` → `semantic`). Drop `PG_GHOLA_DATABASE` GUC rename reference if stale. |
| `schema.rs` | 1207 | **REWRITE** | Replace the 12-table DDL with the 5-table v2 DDL from design doc: `semantic.{mnemes, associations, co_activation_queue, contradiction_queue, contradiction_candidates}`. Add `contributor_user_ids uuid[]`, `source_episodic_ids uuid[]` on `mnemes`. Indexes per design (HNSW, GIN FTS, GIN arrays, partial on active). Schema name moves from `pg_ghola` to `semantic`. |
| `recall.rs` | 1046 | **SIMPLIFY** | Drop the sub_mnemes UNION/JOIN. Scan only `semantic.mnemes`. Return 8 columns (no `matched_position`). Drop `filter_session_id` parameter and any param that only existed for sub_mneme attribution. |
| `types.rs` | 199 | **SIMPLIFY** | `recall_result` back to 8 columns (drop `matched_position: i16` from the `extension_sql!` block and the Rust struct). Drop any sub-mneme types. |
| `scoring.rs` | 431 | **KEEP** | ACT-R activation + softplus compositing math is unchanged in v2. Only schema references (`pg_ghola.mnemes` → `semantic.mnemes`) update. |
| `hebbian.rs` | 1115 | **SIMPLIFY** | Schema rename only (`pg_ghola.*` → `semantic.*`). Hebbian weight update logic stays. |
| `associations.rs` | 401 | **SIMPLIFY** | Schema rename only. The `associations` table and its API are unchanged between v1 and v2. |
| `contradiction.rs` | 1021 | **SIMPLIFY** | Schema rename only. Contradiction-candidate scanning logic unchanged. |
| `contradiction_worker.rs` | 266 | **SIMPLIFY** | Schema rename only. |
| `consolidation_worker.rs` | 760 | **SIMPLIFY** | Schema rename only. Decay + archival logic unchanged. The worker no longer needs to coordinate with the gating worker (which is deleted). |
| `gating_worker.rs` | 1101 | **DELETE** | v2 drops thalamic gating columns from `mnemes`. The worker and its registration go with them. |
| `worker_stats.rs` | 204 | **SIMPLIFY** | Schema rename + drop the gating-worker stats row. |
| `integration_tests.rs` | 1393 | **REWRITE** | Replace with v2-surface tests only: insert→recall round-trip, Hebbian update fires from `co_activation_queue`, contradiction_worker flags high-cosine divergent pairs, decay reduces activation over time, archival flips `state='active'` → `'archived'` at age threshold. Drop all sub_mneme/cluster/gating tests. |
| `bin/pgrx_embed_pg_ghola.rs` | — | **KEEP** | pgrx build stub; no decisions needed. |

## Work order (for Phase 1 tasks that follow)

1. Task 1.2: Write failing schema_v2 tests that pin the target shape.
2. Task 1.3: Rewrite `schema.rs` — tests go green for the schema shape.
3. Task 1.4: Rewrite `types.rs` — 8-column `recall_result`.
4. Task 1.5: Rewire `recall.rs` to target `semantic.mnemes` only.
5. Task 1.6: Delete `gating_worker.rs` + unregister in `lib.rs`.
6. Task 1.7: Rewire Hebbian / contradiction / consolidation / scoring /
   associations / worker_stats for the `semantic.*` schema name.
7. Task 1.8: Rewrite `integration_tests.rs` for the v2 surface.
8. Task 1.9: Bump version 0.0.1 → 0.2.0, drop stale migration SQL
   files, regenerate `pg_ghola--0.2.0.sql`, smoke-test fresh
   `CREATE EXTENSION` install.
9. Task 1.10: Rebuild the CNPG image with the new extension.

## Invariants preserved through the rewrite

- Schema dimension is parametric via `${EMBEDDING_DIM}` — never hardcode
  `vector(1024)` in SQL. (Already a property of the v1 schema; verify
  during rewrite.)
- Scoring math (ACT-R + softplus) matches the v1 implementation bit for
  bit. Phase 1 is about schema shape, not retrieval quality.
- No `pg_test` feature runs against a shared Postgres — each test
  function gets its own ephemeral database.
