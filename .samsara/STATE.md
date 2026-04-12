# State

## Current Baseline (v0.0.5+relaxed_lexical, Iter 8 re-ingest, 2026-04-12)

Mean of 3 retrieve-only runs with full retrieval-time state reset.
Variance: 4.2pp spread at R@5. Changes need >5pp improvement to be significant.
NOTE: Different embedding set than Iter 7. Not directly comparable.

```
                         R@1     R@5    R@10     MRR       N
------------------------------------------------------------
Overall                 7.5%   24.1%   36.7%   0.147     500
------------------------------------------------------------
knowledge-update       13.3%   41.5%   64.9%   0.262      78
multi-session           3.0%   12.3%   25.1%   0.076     133
single-session-asst    33.3%   72.6%   83.9%   0.489      56
single-session-pref     0.0%    3.3%    7.8%   0.019      30
single-session-user     1.4%    5.7%   11.4%   0.031      70
temporal-reasoning      2.8%   19.5%   31.8%   0.096     133
```

## Iteration History

| Iter | R@5 | Delta | What | Kept? | Details |
|------|-----|-------|------|-------|---------|
| 0 | 18.0% | +4.4 | Four-pathway retrieval (semantic, lexical, entity, cluster) | yes | [000.md](iterations/000.md) |
| 1 | 18.0% | +0.0 | Preference pathway analysis -- root cause is encoding-time | reverted | [001.md](iterations/001.md) |
| 2 | 29.2% | +11.2 | Concept enrichment: user-turn text at weight A in tsvector | yes | [002.md](iterations/002.md) |
| 3 | 28.6% | -0.6 | ts_rank_cd for tie-breaking -- smaller FTS values regressed | reverted | [003.md](iterations/003.md) |
| 4 | 12.4% | -16.8 | OR-based lexical filter -- precision loss > recall gain | reverted | [004.md](iterations/004.md) |
| 5 | 28.4% | -0.8 | Temporal stop word stripping -- displaced marginal hits | reverted | [005.md](iterations/005.md) |
| 6 | 16.6%* | n/a | Additive relaxed lexical fallback -- *benchmark non-deterministic on re-ingest | reverted | [006.md](iterations/006.md) |
| 7 | 10.1% | n/a | Fix benchmark: pin DB, full reset, 3-run variance (new baseline) | yes | [007.md](iterations/007.md) |
| 8 | 24.1% | n/a | Relaxed lexical fallback + re-ingest (new embedding set, new baseline) | yes | [008.md](iterations/008.md) |

**Note**: Iters 0-5 used same ingest (29.2% R@5). Iter 6 re-ingest produced different embeddings.
Iter 7 established baseline on Iter 6 ingest (10.1%). Iter 7 dump was truncated.
Iter 8 re-ingested (new embeddings, 24.1%). Iters 0-7 R@5 NOT comparable to Iter 8+.

## Key Constraints Discovered

- Session-level embeddings for long sessions produce diluted representations (Iter 1)
- Benchmark requires truncate + re-ingest for fair comparison (access_count drift, Iter 1)
- Intent classifier misses 41% of preference sessions (Iter 1)
- concept field was wasted on metadata; enrichment with user-turn text = +11.2pp (Iter 2)
- ts_rank_cd breaks ties but reduces FTS contribution via tanh() (Iter 3)
- OR-based FTS filter floods pool, precision loss > recall gain (Iter 4)
- Modifying existing lexical pathway displaces marginal hits globally (Iter 5)
- Retrieval-time fixes have diminishing returns; encoding-time changes needed (Iter 1-5)
- CRITICAL: Benchmark is non-deterministic on re-ingest. TEI CPU float32 embeddings differ between ingestion runs. (Iter 6)
- Co-activation drift: 478/500 queries differ in top-5 after a single 500-query retrieve run. (Iter 6)
- MCP server maps all workspace_ids to `00000000-...0001`; benchmark workspace_id is cosmetic (Iter 6)
- Bench tag mismatch causes 0% retrieve-only results; must use consistent workspace_id (Iter 7)
- Hebbian associations cause degrading trend across successive runs; must be cleared (Iter 7)
- Full retrieval-time state reset (access_count + associations + queue) stabilizes variance to 2.2pp (Iter 7)
- pg_dump cannot export extension-member tables; use COPY binary format instead (Iter 7)
- Binary COPY dumps must be validated: check 0xffff trailer and row count (Iter 8)
- Force-deleting pods during COPY causes WAL corruption; use graceful shutdown (Iter 8)
- Schema recreation (DROP+CREATE EXTENSION) requires ch-server restart + re-GRANT (Iter 8)
- TEI ingest quality varies 2.4x between runs: same code, R@5 10% vs 24% (Iter 7 vs 8)
- run.py all does not accept --workspace-id; must update bench tags post-ingest (Iter 8)

## What To Try Next

1. **Isolate relaxed lexical effect**: Run 3x on Iter 8 pinned DB WITHOUT relaxed lexical
   (revert code, deploy, benchmark). If delta <5pp, the pathway doesn't help on this ingest.

2. **Multi-granularity encoding** (high impact, high effort): Extract per-turn or per-fact
   sub-mnemes during gating. Helps single-session-user, multi-session, temporal simultaneously.
   Requires schema changes + embedding generation at gating time.

3. **Temporal retrieval pathway** (moderate effort): Use content_dates column in a new CTE.
   Many temporal-reasoning queries include date cues not leveraged today.

## Benchmark Protocol

```bash
# 1. Reset ALL retrieval-time state
./analysis/benchmark_reset.sh

# 2. Deploy new code (build, transfer, restart, recreate functions)

# 3. Run retrieve-only
cd ~/longmemeval-ghola && .venv/bin/python run.py retrieve \
    --backend ghola_mcp --dataset s \
    --workspace-id 00000000-0000-0000-0000-000000000001

# 4. Evaluate
.venv/bin/python run.py evaluate --run results/<latest>.jsonl

# 5. For variance measurement: repeat 3x, analyze with:
python3 ~/pg_ghola/analysis/variance_report.py results/run1.jsonl results/run2.jsonl results/run3.jsonl
```

To restore from pinned database: `./analysis/benchmark_restore.sh`

## Roadmap

- [x] FIX BENCHMARK: Pin embeddings via database dump (Iter 7)
- [x] FIX BENCHMARK: Reset co-activation state between retrieve-only runs (Iter 7)
- [x] Additive fallback lexical pathway (deployed Iter 8, effect not yet isolated)
- [ ] Multi-granularity encoding (per-turn sub-mnemes)
- [ ] Temporal retrieval pathway (content_dates CTE)
- [ ] Session-context boosting (leverage session associations)
- [ ] Fix recall function lifecycle (avoid manual CREATE FUNCTION per deploy)
