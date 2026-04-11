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
