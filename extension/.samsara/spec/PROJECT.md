# pg_ghola -- Cognitive Memory Primitives for PostgreSQL

## What It Is

A pgrx extension (Rust, PostgreSQL 18) that implements neuroscience-inspired memory
retrieval. Part of the Chapterhouse memory system deployed on a K3s homelab cluster.

## Architecture

Three background workers + recall function:

| Component | Role | Neuroscience analog |
|-----------|------|---------------------|
| Gating worker | Per-mneme attribute extraction + cluster assignment | Thalamic encoding |
| Contradiction worker | Similarity scanning + conflict flagging | Conflict detection |
| Consolidation worker | Hebbian learning, decay, archival, k-means clustering | Sleep consolidation |
| recall() | Four-pathway candidate generation + composite scoring | Multi-pathway retrieval |

## Four Retrieval Pathways

| Pathway | Mechanism | Brain Analog | Reference |
|---------|-----------|--------------|-----------|
| Semantic | HNSW nearest neighbors (full workspace) | Hippocampal pattern completion | Rolls, 2013 |
| Lexical | Full-text search on tsvector | Cortical phonological/lexical route | Hickok & Poeppel, 2007 |
| Entity | GIN array overlap on extracted entities | Cue-driven episodic retrieval | Tulving, 1983 |
| Cluster | HNSW within nearest k clusters | Cortical column voting | Hawkins et al., 2024 |

No pathway silences another. All candidates are UNION'd and deduplicated.

## Scoring Formula (frozen)

```
content_match = w_semantic * cosine_sim + w_fts * tanh(fts_rank)
temporal_weight = softplus(actr_activation + w_hebbian * heb_boost) / normalizer
score = content_match * temporal_weight * confidence
```

The scoring formula is intentionally frozen to isolate the effect of candidate
generation changes. Do not modify scoring unless explicitly decided.

## Neuroscience Primitives Available

These are the building blocks. Each improvement should map to one of these:

- **Pattern completion** (hippocampal): partial cue activates full memory trace
- **Encoding specificity** (Tulving): retrieval succeeds when cue matches encoding context
- **Temporal context** (Howard & Kahana): memories encoded nearby in time cluster together
- **Interference** (Anderson): similar memories compete; recent/strong ones win
- **Consolidation** (sleep): offline reorganization strengthens important traces, prunes weak ones
- **Spreading activation** (Collins & Loftus): activating one concept primes related concepts
- **Recency/frequency** (ACT-R): recently/frequently accessed memories are more available
- **Context reinstatement** (Godden & Baddeley): recall improves when retrieval context matches encoding context

## Known Constraints (learned from deployment)

- pgvector has NO scalar multiplication or division on vectors
- Gating worker must batch process (50 items/cycle, 100ms poll) for throughput
- linfa 0.7 requires ndarray 0.15, not 0.16
- recall functions must be manually recreated after extension .so reload
- pgrx pg_test framework has vector dimension mismatch (schema says 1024, tests use 768)
- Session-level embeddings for long sessions (18+ turns) produce diluted representations (Iter 1)
- Benchmark requires truncate + re-ingest for fair comparison (access_count drift otherwise)
- Intent classifier misses 41% of preference sessions due to competing question/plan keywords
- MCP ingestion sets concept to metadata strings (timestamp_xxx_session_yyy), not semantic content (Iter 2)
- Gating worker enriches concept with user-turn text for weight 'A' FTS boost (Iter 2)
- Concept enrichment only applies to newly-gated mnemes; re-queue not supported (Iter 2)
- ts_rank saturates when many sessions share common terms at weight A (Iter 3: switched to ts_rank_cd)
- ts_rank_cd produces values >1.0 for best matches; tanh() saturates similarly to ts_rank for top candidates
- ts_rank_cd produces smaller absolute values for weak matches, reducing FTS contribution via tanh()
- plainto_tsquery uses AND conjunction: all terms must match for @@ to be true (Iter 4)
- Multi-session queries have 5-7 terms; AND filter excludes answer sessions matching most but not all (Iter 4)
- Switching to OR-based @@ filter dilutes the lexical pool with 10K+ weak matches, causing severe regression (Iter 4)
- Two-pass (AND then OR fallback) also regresses: OR tier adds expensive cosine_sim computation for thousands of rows (Iter 4)
- Multi-session answer sessions are thematically indistinguishable from non-answer sessions at the session level (Iter 4)
- 83% of multi-session queries are aggregation queries ("how many/much/total") requiring cross-session synthesis (Iter 4)
- Temporal queries contain framing words ("ago", "weeks", "many", "passed") that fail AND filter: 5/8 terms match but 3 temporal terms missing from answer content (Iter 5)
- Modifying existing lexical pathway filter is UNSAFE: changes pool composition globally, marginal hits at rank 5 shift to rank 6 (Iter 5)
- Safe approach: additive separate pathway (new CTE) rather than modifying existing pathway's filter (Iter 5)
- Retrieval-time improvements are now 0/3 on net gains (Iters 3-5). Encoding-time changes likely needed for further progress.
- BENCHMARK NON-DETERMINISM: TEI CPU float32 embeddings differ between ingestion runs (nomic-embed-text-v1.5 on CPU). Jaccard overlap of top-5 session sets: 0.85 same-ingest vs 0.33 cross-ingest. Cross-ingest R@k comparisons are unreliable. Must pin embeddings via database dump. (Iter 6)
- Co-activation drift after a single 500-query retrieve run changes 478/500 top-5 results. Sequential retrieve-only runs are confounded. Must reset access_count between runs. (Iter 6)
- MCP server maps all benchmark workspace_ids to default UUID `00000000-0000-0000-0000-000000000001`. Workspace isolation is cosmetic. (Iter 6)
- Additive pathway (separate CTE with NOT EXISTS guard) is confirmed safe via EXPLAIN -- no regression mechanism when strict pathway has results. Implementation ready for re-test on pinned database. (Iter 6)
- Binary COPY dumps must be validated (check 0xffff trailer + row count). Iter 7 dump was silently truncated at row 19015/19181. (Iter 8)
- Force-deleting pods during large COPY operations causes WAL corruption (pg_resetwal required). Use graceful shutdown or CNPG hibernation. (Iter 8)
- Schema recreation (DROP SCHEMA + CREATE EXTENSION) invalidates ch-server DB connections. Must restart ch-server and re-GRANT permissions to memory_api role. (Iter 8)
- TEI ingest quality varies dramatically: same code produces R@5 ~10% (Iter 7 ingest) vs ~24% (Iter 8 ingest). 2.4x difference from embedding non-determinism alone. (Iter 8)
- run.py all generates random workspace UUID, creates mismatched bench tags. Must update tags post-ingest for retrieve-only compatibility. (Iter 8)

## Tech Stack

- Rust, pgrx 0.17.0, PostgreSQL 18, pgvector
- linfa 0.7 (k-means), ndarray 0.15
- Docker (CNPG image), K3s, ArgoCD
- Benchmark: LongMemEval (~/longmemeval-ghola/)
