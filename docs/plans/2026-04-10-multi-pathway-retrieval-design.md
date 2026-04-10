# Multi-Pathway Retrieval Design

Simplex v0.5 specification for neuroscience-grounded memory retrieval in pg_ghola.

## Neuroscience Basis

Memory retrieval in the brain uses multiple parallel pathways that converge and compete.
No single pathway silences another. The thalamus modulates signal gain, it does not filter.
Cluster reorganization happens during consolidation (offline), not during encoding.

| Pathway | Brain Analog | Reference |
|---|---|---|
| Semantic (HNSW) | Hippocampal pattern completion | Rolls, 2013 |
| Lexical (FTS) | Cortical phonological/lexical route | Hickok & Poeppel, 2007 |
| Entity (GIN) | Cue-driven episodic retrieval | Tulving, 1983 (encoding specificity) |
| Cluster (HNSW-within-cluster) | Cortical column voting | Hawkins et al., 2024 (Thousand Brains) |

## Worker Architecture

Three background workers, renamed to reflect their neuroscience roles:

| Worker | Role | Neuroscience analog |
|---|---|---|
| **Gating worker** | Per-mneme attribute extraction + cluster assignment | Thalamic encoding |
| **Contradiction worker** | Per-mneme similarity scanning + flagging | Conflict detection |
| **Consolidation worker** (renamed from Hebbian worker) | Hebbian learning, decay, archival, clustering, rebalancing | Sleep consolidation |

## Design Decisions

1. **Entity matching**: store both compound and individual tokens. "Sarah Chen" -> ["sarah chen", "sarah", "chen"]. GIN array overlap matches partial entity mentions from queries.
2. **Cluster count**: 3 nearest clusters searched. Tunable parameter, measured before committing.
3. **Scoring**: unchanged from existing formula. Source pathway added as debug field in RecallResult to analyze which pathway contributes winning candidates.
4. **K-means implementation**: use the `linfa` Rust crate. Anticipate needing other algorithms (hierarchical clustering, community detection) soon.
5. **Rebalancing**: full k-means recomputation for v1 (no merge/split heuristics). Simpler, avoids convergence issues.
6. **Clustering trigger**: consolidation worker checks hourly. If mneme count > threshold and no centroids exist, run initial clustering. Rebalancing runs every 24 hours.

---

DATA: Candidate
  id: uuid
  concept: string
  content: string
  confidence: float8
  access_count: integer
  age_days: float8
  cosine_sim: float8, similarity to query embedding
  fts_rank: float8, full-text search rank against query
  memory_type: factual | experiential | working
  session_id: uuid, optional
  source_pathway: semantic | lexical | entity | cluster, which pathway contributed this candidate

DATA: ClusterCentroid
  id: integer, sequential
  workspace_id: uuid
  centroid: vector, mean embedding of cluster members
  member_count: integer, number of mnemes assigned
  created_at: timestamptz
  updated_at: timestamptz

DATA: RecallResult
  mneme_id: uuid
  score: float8, composite score
  content_match: float8
  activation: float8
  hebbian_boost: float8
  confidence: float8
  concept: string
  content: string

---

CONSTRAINT: no_pathway_silences_another
  every retrieval pathway contributes candidates to the union pool
  no pathway's presence or absence prevents another from running
  the semantic (HNSW) pathway always runs regardless of other pathway results

CONSTRAINT: graceful_degradation
  when cluster centroids do not exist, the cluster pathway is a no-op
  when query text produces no entity mentions, the entity pathway is a no-op
  when query text produces no FTS matches, the lexical pathway contributes zero candidates
  in all degraded cases, remaining pathways operate normally

CONSTRAINT: scoring_unchanged
  the composite scoring formula (content_match * temporal_weight * confidence) is not modified
  candidate generation changes; candidate scoring does not
  this isolates the effect of multi-pathway retrieval from scoring changes

CONSTRAINT: entity_tokenization
  entities are stored as both compound forms and individual tokens
  "Sarah Chen" produces ["sarah chen", "sarah", "chen"]
  query-time entity extraction uses the same tokenization
  GIN array overlap (&&) matches on any shared token

---

FUNCTION: recall_multi_pathway(workspace_id, query_text, query_embedding, limit_n, min_confidence, weights, filters) -> list of RecallResult

  BASELINE:
    reference: recall_inner as of commit 2625107 (FTS-gated HNSW)
    preserve:
      - workspace isolation (different workspace returns zero results)
      - confidence filtering (min_confidence excludes low-confidence mnemes)
      - co-activation enqueue (every recall enqueues results for Hebbian processing)
      - expired working memory exclusion
      - composite scoring formula produces identical scores for identical candidates
      - memory_type, scope, tags, session_id filtering
    evolve:
      - candidate generation: from FTS-gated HNSW to four-pathway union
      - FTS behavior: from blocking gate to additive pathway
      - new entity retrieval pathway using extracted entities column
      - new cluster retrieval pathway using cluster_id and cluster_centroids

  EVAL:
    preserve: pass^1
    evolve: pass@1
    grading: code

  RULES:
    - extract entity mentions from query_text using the same heuristic as the gating worker (with individual token expansion)
    - find nearest cluster centroids to query_embedding (top 3, tunable) if cluster_centroids table is populated for this workspace
    - run four retrieval pathways in parallel, each returning up to pool_size candidates:
      - semantic: HNSW nearest neighbors across full workspace (no restrictions)
      - lexical: FTS matches on search_vector, ranked by ts_rank
      - entity: GIN scan on entities column where query entity tokens overlap mneme entity tokens
      - cluster: HNSW nearest neighbors restricted to mnemes in the nearest clusters
    - union all pathway results, deduplicate by mneme id (keep highest cosine_sim on collision)
    - fetch Hebbian association boosts for all candidates in the pool
    - compute composite scores using existing formula unchanged
    - sort by score descending, truncate to limit_n
    - enqueue co-activation event for returned results

  DONE_WHEN:
    - all four pathways attempted (entity and cluster may return zero candidates)
    - candidates from all pathways merged and deduplicated
    - results scored, ranked, and truncated to limit_n
    - co-activation event enqueued

  EXAMPLES:
    -- preserved: workspace isolation
    (workspace_A, "query", embedding, 10, 0.0, defaults) -> results only from workspace_A

    -- preserved: confidence filtering
    (ws, "query", emb, 10, 0.9, defaults) -> all results have confidence >= 0.9

    -- preserved: co-activation enqueue
    (ws, "query", emb, 10, 0.0, defaults) -> co_activation_queue has one new entry after call

    -- preserved: empty workspace returns empty, still enqueues
    (empty_ws, "query", emb, 10, 0.0, defaults) -> [] and co_activation_queue has one new entry

    -- evolved: entity pathway adds candidates
    -- query mentions "Sarah", mneme has entity tokens including "sarah"
    -- entity pathway surfaces it via GIN overlap, scoring ranks it
    (ws_with_sarah, "what did Sarah say about the project", emb, 10, 0.0, defaults)
      -> results include the sarah mneme if it scores well

    -- evolved: entity partial match works
    -- query extracts "sarah", mneme has ["sarah chen", "sarah", "chen"]
    -- GIN overlap on "sarah" matches
    (ws_with_sarah_chen, "ask Sarah about it", emb, 10, 0.0, defaults)
      -> entity pathway finds mnemes with "sarah" token

    -- evolved: cluster pathway adds candidates
    -- query about cooking, cooking cluster exists, mneme in cooking cluster
    -- is not globally top-N by HNSW but is top within cooking cluster
    (ws_with_clusters, "recipe for pasta", emb, 10, 0.0, defaults)
      -> results include cooking-cluster mnemes alongside global HNSW results

    -- evolved: graceful degradation, no clusters
    (ws_no_clusters, "query", emb, 10, 0.0, defaults)
      -> results from semantic + lexical + entity pathways only

    -- evolved: graceful degradation, no entity mentions in query
    (ws, "how does it work", emb, 10, 0.0, defaults)
      -> results from semantic + lexical + cluster pathways only

  ERRORS:
    - embedding dimension mismatch -> fail with "expected N dimensions, not M"
    - invalid memory_type filter -> fail with "invalid memory_type: {value}"
    - any unhandled condition -> fail with descriptive message

  NOT_ALLOWED:
    - exclude candidates from the semantic pathway based on FTS, entity, or cluster signals
    - modify the composite scoring formula
    - skip co-activation enqueue

---

FUNCTION: compute_initial_clusters(workspace_id, k) -> list of ClusterCentroid

  RULES:
    - select all active mneme embeddings in the workspace
    - if fewer than k * 10 mnemes exist, return empty (insufficient data for meaningful clusters)
    - run k-means clustering on the embeddings with k centroids using linfa
    - k defaults to sqrt(N / 10) where N is the active mneme count
    - store centroids in cluster_centroids table with workspace_id
    - assign each mneme its nearest centroid as cluster_id
    - record member_count for each centroid
    - triggered by consolidation worker when mneme count > threshold and no centroids exist

  DONE_WHEN:
    - cluster_centroids table has k rows for this workspace
    - every active mneme in the workspace has a non-null cluster_id
    - each centroid's member_count matches the actual count of assigned mnemes

  EXAMPLES:
    (workspace with 19000 mnemes, k=default) -> ~44 centroids, all mnemes assigned
    (workspace with 100 mnemes, k=default) -> empty, insufficient data (100 < 10 * sqrt(10))
    (workspace with 500 mnemes, k=7) -> 7 centroids, all 500 mnemes assigned

  ERRORS:
    - workspace has zero active mnemes -> return empty list
    - k <= 0 -> fail with "k must be positive"
    - linfa k-means fails to converge -> log warning, retry with k-1
    - any unhandled condition -> fail with descriptive message

---

FUNCTION: assign_cluster(mneme_id) -> cluster_id or null

  RULES:
    - read the mneme's embedding and workspace_id
    - if no centroids exist for this workspace, return null
    - find the nearest centroid by cosine distance
    - assign that centroid's id as the mneme's cluster_id
    - update the centroid incrementally: new_centroid = old + (point - old) / (n + 1)
    - increment the centroid's member_count
    - called by the gating worker as part of per-mneme extraction

  DONE_WHEN:
    - mneme's cluster_id column is set (or null if no centroids)
    - centroid vector and member_count updated if assignment occurred

  EXAMPLES:
    (mneme in workspace with centroids) -> cluster_id set, centroid updated
    (mneme in workspace without centroids) -> cluster_id remains null

  ERRORS:
    - mneme does not exist -> log warning, return null
    - any unhandled condition -> fail with descriptive message

  UNCERTAIN:
    - if centroid update would produce a degenerate vector (all zeros) -> skip centroid update, log warning

---

FUNCTION: rebalance_clusters(workspace_id) -> rebalance stats

  RULES:
    - read all active mneme embeddings in the workspace
    - re-run full k-means using linfa with current centroid count as k
    - replace all rows in cluster_centroids for this workspace with new centroids
    - reassign all mnemes to their nearest new centroid
    - update member_counts
    - runs as a periodic job in the consolidation worker (every 24 hours)

  DONE_WHEN:
    - all centroids recomputed from current data distribution
    - all mnemes reassigned to nearest centroid
    - cluster_centroids member_counts are accurate

  EXAMPLES:
    (workspace with 44 clusters, data distribution shifted) -> 44 new centroids, all mnemes reassigned
    (workspace with no centroids) -> no-op

  ERRORS:
    - no centroids exist for workspace -> no-op, return empty stats
    - linfa k-means fails to converge -> log warning, keep existing centroids
    - any unhandled condition -> fail with descriptive message

---

CONSTRAINT: cluster_cold_start
  the system must produce correct results with zero clusters
  cluster_id = null is a valid and expected state for all mnemes before initial clustering
  initial clustering is triggered by the consolidation worker checking hourly

CONSTRAINT: incremental_centroid_accuracy
  centroid drift from incremental updates is corrected by periodic full rebalancing (every 24h)
  the system tolerates moderate drift between rebalances
  rebalancing frequency is a tuning parameter, not a correctness requirement

CONSTRAINT: worker_naming
  the Hebbian worker is renamed to consolidation worker throughout codebase
  the function name worker_main and BackgroundWorkerBuilder name are updated
  the worker_stats table is renamed to consolidation_worker_stats
  all log messages use "consolidation worker" prefix
