# seeding-eval/data — P4 Eval Bed

## Source corpus

| Parameter    | Value             |
|-------------|-------------------|
| Repo         | vercel/next.js    |
| Strategy     | merged-prs        |
| n            | 50 resolved threads |
| Split        | 80/20 held-out (content-derived sha256, HELD_OUT_FRACTION_PERCENT=20) |

## Ingest identifier

| Field        | Value |
|-------------|-------|
| Workspace UUID | e256f781-c7cc-42c4-8539-ed4e944158c0 |
| User UUID   | 00000000-0000-0000-0000-000000000001 |
| Generated   | 2026-07-05 |

Fresh UUID per bed, never re-used. Chapterhouse upserts by event id so
re-importing the same bundle is idempotent, but different beds are
isolated by workspace.

## May-vs-now caveat

The original May 2026 eval bed was wiped from /tmp. This bed was
regenerated from the same source params (vercel/next.js, merged-prs,
n=50) against the current pipeline:

- v2-m3 reranker (BAAI/bge-reranker-base, fp16, :8085)
- websearch FTS (D1 websearch operator in chapterhouse keyword tier)
- events now carry workspace_id (schema drift from May)

The bridge ("structural-miss") set approximates but does not equal the
original 32 misses. It is re-derived under today's pipeline; P4 ships
against this set.

## Artifacts

| Path | Committed | Description |
|------|-----------|-------------|
| phase0-driver.sh | yes | Full pipeline: extract, build-cases, ingest, wait, baselines, miss-set |
| .gitignore | yes | Excludes large generated files |
| README.md | yes | This file |
| cache/ | no | PyGithub API cache (seeding-extract cache-first) |
| bundle-dir/bundle.jsonl | no | GitHub-bundle JSONL (one thread per line) |
| cases.jsonl | no | EvalCase records (all cases, held-out flag in each) |
| event_buckets.json | no | event_id -> module_bucket for H1 entropy |
| results-baseline-noprim/ | no | seeding-eval-run --k 20 (primitives off) |
| results-baseline-prim/ | no | seeding-eval-run --k 20 --primitives |
| bridge-misses.txt | committed after run | case_ids missing top-5 under BOTH baselines |
| import-logs-imported.txt | no | import-logs resume-state |
| phase0.log | no | Full pipeline log |

## Regeneration

```bash
cd /home/loganb/ghola/.claude/worktrees/p4
bash seeding-eval/data/phase0-driver.sh
```

Requires:
- GITHUB_TOKEN (or `gh auth login`; driver calls `gh auth token`)
- Docker stack up: postgres :5432, chapterhouse :8080, ghola :7421
- seeding-eval/.venv present (`cd seeding-eval && python3 -m venv .venv && .venv/bin/pip install -e .`)
- Go toolchain for `go run ./cmd/import-logs`

## Task 8 run matrix

Three run configurations for the P4 evaluation sweep:

| Config | `--settle` | `--activation-weight` | Notes |
|--------|------------|----------------------|-------|
| baseline | off (default) | n/a | Pre-P4 pipeline; byte-identical wire shape |
| expand | expand | n/a | Config A: spreading activation sub-list appended to rerank pool with zero RRF mass |
| channel@0.2 | channel | 0.2 | Config B: activation as third score-fusion channel (harness default) |

**Score > 1.0 note**: The fallback path in `FuseScores` emits `rrfNorm + wActivation*actNorm`; when activation is high and RRF rank is strong, scores can exceed 1.0 and outscore fully-reranked hits. This is intentional — the "trust the RRF prior" convention extended to the activation channel. Measurement output containing scores > 1.0 is expected.

**ActivationWeight bound**: `RerankWeight + ActivationWeight` must not exceed 1 (server default `RerankWeight = 0.5` implies `ActivationWeight < 0.5`). The harness default `--activation-weight 0.2` satisfies this bound.
