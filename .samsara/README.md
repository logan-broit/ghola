# pg_ghola Samsara Loop

Eval-driven development of neuroscience-grounded memory retrieval primitives.

## Docs

| File | Description |
|------|-------------|
| prompt.md | Iteration prompt -- piped to agent each loop |
| STATE.md | Compact state: baselines, iteration table, what's next (<100 lines) |
| iterations/NNN.md | Full analysis for each iteration (read on demand) |
| spec/PROJECT.md | Project context, architecture, neuroscience foundations |
| spec/EVAL.md | Eval workflow -- how to run benchmarks, analyze, improve |
| spec/DEPLOY.md | Build, transfer, migrate, verify pipeline |

## Analysis scripts (Think in Code)

| Script | Purpose |
|--------|---------|
| analysis/diagnose_category.py | Diagnose failures for a category -- prints compact summary |
| analysis/compare_runs.py | Compare two benchmark runs -- shows per-category delta + gained/lost queries |
| analysis/variance_report.py | Analyze variance across N benchmark runs -- Jaccard, stable core, per-category |
| analysis/benchmark_reset.sh | Full retrieval-time state reset (access_count + associations + queue) |
| analysis/benchmark_run.sh | Orchestrate N retrieve-only runs with reset between each |
| analysis/benchmark_restore.sh | Restore pinned database from binary COPY dumps in benchmark-data/ |
| analysis/gold_rank_analysis.py | Where do gold mnemes rank in semantic pathway? Pool vs scoring bottleneck |
| analysis/fts_diagnostic.py | FTS match patterns for failing queries -- gold vs competitor FTS rates |

Write new scripts in `analysis/` for any investigation. Keep raw data out of context.

## Design docs (outside .samsara/)

| File | Description |
|------|-------------|
| docs/plans/2026-04-10-multi-pathway-retrieval-design.md | Simplex spec with constraints |
| docs/plans/2026-04-10-multi-pathway-retrieval-implementation.md | Implementation plan with outcomes |

## Blog dashboard

| File | Description |
|------|-------------|
| ~/.openclaw/workspace/projects/blog/preview/src/MemoryEval.tsx | Public-facing results dashboard |
