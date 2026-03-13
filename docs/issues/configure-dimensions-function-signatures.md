# configure_dimensions does not update function signatures

## Status: Open (pre-existing)

## Problem

`configure_dimensions()` in `src/schema.rs` alters the `mnemes.embedding` column
type and recreates the HNSW index with the new dimension, but does not recreate
any SQL function signatures that reference `vector(768)`.

After calling `configure_dimensions(3072)`, the `recall()` function still has
`query_embedding vector(768)` in its signature. pgvector rejects the dimension
mismatch at call time.

## Affected functions

Currently:
- `recall()` — `src/recall.rs:30`

The thousand-brains-concepts specs introduce additional vector-parameter functions
that will have the same issue:
- `recall_voted()`
- `project_embedding()`
- `recall_by_analogy()`
- `recall_by_displacement()`
- `auto_project_recall()`
- `compute_displacement()`

## Fix options

**A. `configure_dimensions` drops and recreates function signatures**
Each function with a vector parameter is re-created with the new dimension.
Fragile — `configure_dimensions` must know about every vector-parameter function.

**B. Use untyped `vector` (no dimension) in function signatures**
pgvector allows `vector` without a dimension specifier. The column constraint
on `mnemes.embedding` still enforces dimensions at insert/query time. Less
type-safe at the function boundary but zero maintenance as functions are added.

**C. Store dims in config, generate wrappers dynamically**
Over-engineered for this use case.

## Recommendation

Option B is the simplest. The column type on `mnemes.embedding` is the real
enforcement point. Function signatures accepting untyped `vector` would
eliminate this entire class of bug.
