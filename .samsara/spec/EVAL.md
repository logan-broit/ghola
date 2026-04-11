# Eval Workflow

## Running the Benchmark

The benchmark uses LongMemEval with the Chapterhouse MCP backend.

### Full run (ingest + query)

```bash
# 1. Clean the database
kubectl exec -n ch-system memory-db-1 -- psql -U postgres -d memories -c "
BEGIN;
TRUNCATE ghola.co_activation_queue;
TRUNCATE ghola.contradiction_queue;
TRUNCATE ghola.gating_queue;
TRUNCATE ghola.contradiction_candidates;
TRUNCATE ghola.cluster_centroids;
TRUNCATE ghola.associations CASCADE;
TRUNCATE ghola.mnemes CASCADE;
COMMIT;"

# 2. Run ingest + query
cd ~/longmemeval-ghola && .venv/bin/python run.py all --backend ghola_mcp --dataset s
```

The benchmark ingests ~19K sessions (~19K mnemes with session granularity) then runs 500
retrieval queries across 6 categories. Ingestion takes ~65 minutes via MCP API (~4.8
sessions/second with 50ms throttle). Gating takes ~7 minutes. Queries take ~5 minutes.
Total: ~75-80 minutes.

### Wait for gating worker

After ingest, the gating worker processes the queue (entities, cluster assignment).
Check completion:

```bash
kubectl exec -n ch-system memory-db-1 -- psql -U postgres -d memories -c "
SELECT items_processed, queue_depth FROM ghola.gating_worker_stats WHERE id = 1;
SELECT count(*) FILTER (WHERE entities IS NOT NULL) as with_entities,
       count(*) FILTER (WHERE cluster_id IS NOT NULL) as with_clusters,
       count(*) as total
FROM ghola.mnemes;"
```

The gating worker should drain ~19K items in ~7 minutes at current throughput.
If queue_depth stays at 0 and with_entities ~= total, gating is complete.

### Interpreting Results

```
                         R@1     R@5    R@10     MRR       N
------------------------------------------------------------
Overall                 X.X%   XX.X%   XX.X%   X.XXX     500
```

- **R@k**: Recall at k -- was the correct answer in the top k results?
- **MRR**: Mean Reciprocal Rank -- how high was the correct answer ranked?
- **N**: Number of queries in that category

### Categories

| Category | What it tests | N |
|----------|--------------|---|
| knowledge-update | Retrieving updated facts | 78 |
| multi-session | Cross-session retrieval | 133 |
| single-session-assistant | Retrieving assistant responses | 56 |
| single-session-preference | Retrieving user preferences | 30 |
| single-session-user | Retrieving user statements | 70 |
| temporal-reasoning | Time-based retrieval | 133 |

### Analyzing Failures

To understand WHY a category fails, examine individual queries:

```bash
# Look at the benchmark query data
cd ~/longmemeval-ghola
# The dataset files contain the queries with expected answers
# Compare what recall() returns vs what was expected
```

You can also test specific queries directly:

```bash
kubectl exec -n ch-system memory-db-1 -- psql -U postgres -d memories -c "
SELECT (r).concept, (r).content, (r).score
FROM ghola.recall(
    '00000000-0000-0000-0000-000000000001'::uuid,
    'your test query here',
    (SELECT embedding FROM ghola.mnemes WHERE content LIKE '%relevant text%' LIMIT 1),
    10, 0.0, NULL
) AS r;"
```

## Clean Benchmark Protocol

IMPORTANT: Each benchmark run modifies access_count through co-activation events,
creating rich-get-richer drift that makes iteration-to-iteration comparison unreliable.
Observed 11.6pp R@5 swing between consecutive runs with NO code change (Iter 1).

For fair before/after comparison, ALWAYS start from a clean state:

```bash
# 1. Clean the database (REQUIRED before every benchmark)
kubectl exec -n ch-system memory-db-1 -- psql -U postgres -d memories -c "
BEGIN;
TRUNCATE ghola.co_activation_queue;
TRUNCATE ghola.contradiction_queue;
TRUNCATE ghola.gating_queue;
TRUNCATE ghola.contradiction_candidates;
TRUNCATE ghola.cluster_centroids;
TRUNCATE ghola.associations CASCADE;
TRUNCATE ghola.mnemes CASCADE;
COMMIT;"

# 2. Run full pipeline (ingest + gating + query)
cd ~/longmemeval-ghola && .venv/bin/python run.py all --backend ghola_mcp --dataset s

# 3. Wait for gating worker to complete (~7 min)
# Check: queue_depth = 0 AND with_entities ~= total
```

Adds ~17 minutes but produces reliable comparisons. Skip at your own risk.

**WARNING (Iter 6 finding):** Full pipeline re-ingestion is NON-DETERMINISTIC. TEI CPU
float32 embeddings differ between ingestion runs. Jaccard overlap of top-5 session sets
drops from 0.85 (same ingest, different code) to 0.33 (different ingest, same code).
This means R@k comparisons across full pipeline runs are UNRELIABLE.

## Recommended Benchmark Protocol (post-Iter 6)

For reliable before/after comparison, use RETRIEVE-ONLY on a pinned database:

```bash
# 1. Reset co-activation state (but keep mnemes and embeddings intact)
kubectl exec -n ch-system memory-db-1 -- psql -U postgres -d memories -c "
BEGIN;
TRUNCATE ghola.co_activation_queue;
UPDATE ghola.mnemes SET access_count = 1, last_access = created_at;
COMMIT;"

# 2. Deploy new code (build, transfer, restart, recreate functions)

# 3. Run retrieve-only with the correct workspace ID
cd ~/longmemeval-ghola && .venv/bin/python run.py retrieve \
    --backend ghola_mcp --dataset s \
    --workspace-id 00000000-0000-0000-0000-000000000001

# 4. Evaluate
.venv/bin/python run.py evaluate --run results/<latest>.jsonl
```

IMPORTANT: The MCP server maps all workspace IDs to `00000000-0000-0000-0000-000000000001`.
When using `run.py retrieve` standalone, you MUST pass this workspace ID or the benchmark
will generate a random UUID and query an empty workspace (returning 0% across the board).

## Recording Results

After each benchmark run, update `.samsara/STATE.md`:

1. Add a new entry to the Iteration Log
2. Record the full results table
3. Note which categories improved/regressed
4. Document the hypothesis and whether it was confirmed
