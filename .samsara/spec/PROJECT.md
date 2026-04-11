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

## Tech Stack

- Rust, pgrx 0.17.0, PostgreSQL 18, pgvector
- linfa 0.7 (k-means), ndarray 0.15
- Docker (CNPG image), K3s, ArgoCD
- Benchmark: LongMemEval (~/longmemeval-ghola/)
