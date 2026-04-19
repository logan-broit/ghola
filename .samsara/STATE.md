# State

## Current Baseline (v0.0.5, no relaxed lexical, Iter 9, 2026-04-12)

Mean of 3 retrieve-only runs with full retrieval-time state reset.
Variance: 0.8pp spread at R@5. Changes need >2pp improvement to be significant.

```
                         R@1     R@5    R@10     MRR       N
------------------------------------------------------------
Overall                 7.7%   27.5%   39.7%   0.159     500
------------------------------------------------------------
knowledge-update        8.1%   42.3%   68.0%   0.240      78
multi-session           4.0%   15.8%   30.3%   0.097     133
single-session-asst    39.3%   82.1%   94.0%   0.563      56
single-session-pref     0.0%    4.4%    4.4%   0.012      30
single-session-user     1.4%    7.1%   10.9%   0.033      70
temporal-reasoning      3.0%   23.3%   32.1%   0.103     133
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
| 9 | 27.5% | +3.4 | Ablation: relaxed lexical pathway harmful, removed (new baseline) | yes | [009.md](iterations/009.md) |
| 10 | 19.2% | -0.3* | Temporal tokens in concept field dilute lexical pool (all cats regressed) | reverted | [010.md](iterations/010.md) |
| 11 | 16.2%* | n/a | Query decomposition + temporal analysis; bimodal variance (6-17pp) makes comparison impossible | reverted | [011.md](iterations/011.md) |
| 12 | 11.4%* | -16.1 | Re-embed with sentence-transformers HARMFUL (-16pp), reverted. Tooling kept. | reverted | [012.md](iterations/012.md) |
| 13 | 22.5%* | n/a | ts_rank_cd provides real FTS differentiation but amplifies embedding mismatch 34x (27pp variance). Reverted. Analysis scripts kept. | reverted | [013.md](iterations/013.md) |
| 14 | 9.7% | -8.1* | Cluster pathway floods pool with false positives, FTS saturation can't differentiate. Ablation: 17.8% without clusters. | reverted | [014.md](iterations/014.md) |
| 15 | 14.9% | -3.0* | vLLM 0.19.1 Docker embedding server: cross-version mismatch WORSE than sentence-transformers (-3pp). Reverted. | reverted | [015.md](iterations/015.md) |
| 16 | 15.6%* | -11.9 | Multi-granularity: isolated per-turn sub_mneme encoding. Infra kept, encoding reverted. Catastrophic on conversational content (single-session-assistant -55pp, knowledge-update TIED). | reverted (encoding), kept (schema + Go ingest) | [016.md](iterations/016.md) |

**Note**: Iters 0-5 used same ingest (29.2% R@5). Iter 6 re-ingest produced different embeddings.
Iter 7 established baseline on Iter 6 ingest (10.1%). Iter 7 dump was truncated.
Iter 8 re-ingested (new embeddings, 24.1%). Iters 0-7 R@5 NOT comparable to Iter 8+.
Iter 12 re-embedded with sentence-transformers (11.4% R@5, HARMFUL), reverted to vLLM embeddings.
Iter 15 vLLM 0.19.1 tested against stored vLLM embeddings: 14.9% R@5 (worse than ST 17.8%).

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
- Relaxed lexical pathway is harmful: dilutes candidate pool, injects variance (Iter 9)
- Retrieval-time temporal stripping is wrong direction; fix encoding, not query (Iter 9)
- Candidate pool purity matters: fewer high-quality > more low-quality candidates (Iter 9)
- Temporal tokens in concept field dilute lexical pool: month/season names match thousands of mnemes (Iter 10)
- Temporal context needs a DEDICATED retrieval pathway, not the general lexical search (Iter 10)
- Binary COPY fires INSERT triggers; must TRUNCATE worker queues post-restore (Iter 10)
- Corrupted COPY dumps: kubectl stderr mixes into stdout; fix with `tail -c +78` (Iter 10)
- Embedding server config must stay consistent with stored embeddings (Iter 10)
- 0/133 temporal queries contain explicit dates; date-matching retrieval pathways have zero opportunity (Iter 11)
- content_dates unpopulated (1/18917); extract_dates fix in code but pinned DB predates it (Iter 11)
- Single sub-query decomposition = relaxed lexical = harmful; gate on 2+ sub-queries (Iter 11)
- DISTINCT ON dedup is non-deterministic; add ORDER BY id, fts_rank DESC for correctness (Iter 11)
- Bimodal benchmark variance: 1st run after pod restart LOW, 2nd HIGH (6-17pp). HNSW warmup. (Iter 11)
- Sentence-transformers embedding server has higher variance (6-17pp) than TEI (2.2pp) (Iter 11)
- vLLM vs sentence-transformers embedding mismatch causes 7.4pp variance; same model, different engines = different embeddings (Iter 12)
- HNSW cold-start is NOT a significant variance source; no bimodal pattern in 4-run test (Iter 12)
- Embedding server at 192.168.2.6:8082 is ephemeral; use analysis/embed_server.py to start (Iter 12)
- DELETE of 20K+ associations is slow; use TRUNCATE + re-insert supersedes pattern (Iter 12)
- Re-embed with sentence-transformers is HARMFUL: 16pp worse than vLLM for same model. Reverted. (Iter 12)
- vLLM produces 2.5x better embeddings than sentence-transformers for Qwen3-Embedding-0.6B (Iter 12)
- ts_rank saturated at 1.0 after concept enrichment; FTS adds constant 0.305 to all candidates (Iter 13)
- ts_rank_cd provides real differentiation (0-0.46 range) but amplifies embedding mismatch 34x (Iter 13)
- Fixing embedding server is PREREQUISITE for FTS improvements; saturation acts as variance damper (Iter 13)
- 40% of failing gold mnemes are in semantic top-30; scoring issue, not pool size issue (Iter 13)
- 70% of failing gold mnemes lack FTS match; plainto_tsquery AND too strict for paraphrased content (Iter 13)
- cluster_id is 0/18917; cluster pathway is completely dead (no centroids exist) (Iter 13)

- Cluster pathway floods pool with false positives: nearby-cluster candidates have competitive cosine but aren't gold (Iter 14)
- Pool expansion is universally harmful when FTS is saturated: any new pathway needs non-cosine differentiator (Iter 14)
- K-means in dev profile takes 19 min for 19K vectors; needs release profile or external service (Iter 14)
- Baseline drift: current pod measures 17.8% R@5 vs historical 27.5%; embedding mismatch worsening (Iter 14)
- Different vLLM versions produce incompatible embeddings: 0.19.1.dev6 is 3pp WORSE than sentence-transformers (Iter 15)
- Original vLLM embedding engine is irrecoverable: no replacement can reproduce the original embeddings (Iter 15)
- Re-ingest is the ONLY path to stable matched embeddings; neither ST (17.8%) nor vLLM 0.19.1 (14.9%) can approach matched baseline (27.5%) (Iter 15)

## What To Try Next

1. **Re-ingest with sentence-transformers** (HIGHEST PRIORITY): The original vLLM embeddings
   are irrecoverable (Iter 15 confirmed). Re-ingest all sessions using sentence-transformers
   to create matched stored+query embeddings. This is the ONLY path to eliminating the
   embedding mismatch that blocks all retrieval-time improvements.
   NOTE: Iter 12 measured ST-embedded at 11.4% R@5, but that was ST-stored + ST-queried
   DURING the mismatch era. A clean re-ingest + new pinned DB dump is needed.

2. **Multi-granularity encoding** (high impact, combine with re-ingest): Per-turn or
   per-fact sub-mnemes during gating. Can be implemented simultaneously with re-ingest
   to compound improvements. Addresses the root cause of session-level dilution.

3. **Fix FTS saturation** (after re-ingest): FTS adds constant 0.305 to all candidates.
   Only addressable AFTER embedding mismatch is resolved (Iter 13 showed FTS changes
   amplify mismatch). Approaches: reduce concept field weight, phraseto_tsquery, field weighting.

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
- [x] Ablation: relaxed lexical pathway harmful, permanently removed (Iter 9)
- [x] FIX BENCHMARK: Re-embed with sentence-transformers HARMFUL, reverted (Iter 12)
- [x] Optimize benchmark reset (TRUNCATE + re-insert supersedes) (Iter 12)
- [x] Add warmup run to benchmark protocol (Iter 12)
- [ ] Create new pinned DB dump with sentence-transformers embeddings
- [ ] Multi-granularity encoding (per-turn sub-mnemes)
- [ ] Temporal retrieval pathway (content_dates CTE)
- [ ] Session-context boosting (leverage session associations)
- [ ] Fix recall function lifecycle (avoid manual CREATE FUNCTION per deploy)
