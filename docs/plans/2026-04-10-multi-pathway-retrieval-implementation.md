# Multi-Pathway Retrieval Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace FTS-gated HNSW recall with four parallel retrieval pathways (semantic, lexical, entity, cluster) where no pathway silences another, and rename the Hebbian worker to consolidation worker.

**Architecture:** Four CTE-based retrieval pathways UNION'd in recall_inner. Entity extraction expanded with individual token splitting for partial GIN matching. Cluster centroids stored in a new table, computed by the consolidation worker, assigned per-mneme by the gating worker. linfa crate for k-means.

**Tech Stack:** Rust, pgrx 0.17.0, PostgreSQL 18, pgvector, linfa (k-means), cargo-pgrx

**Design doc:** `docs/plans/2026-04-10-multi-pathway-retrieval-design.md`

---

### Task 1: Rename Hebbian worker to Consolidation worker

**Files:**
- Modify: `src/lib.rs` (worker name in BackgroundWorkerBuilder, module comment)
- Modify: `src/worker.rs` (all log messages, comments, function doc)
- Modify: `src/worker_stats.rs` (table name `worker_stats` -> `consolidation_worker_stats`, function name `get_worker_stats` -> `get_consolidation_worker_stats`, type name `worker_status` -> `consolidation_worker_status`)
- Modify: `src/schema.rs` (if any references)
- Modify: `src/integration_tests.rs` (table name and function references)

**Step 1: Rename in lib.rs**

Change `BackgroundWorkerBuilder::new("pg_ghola Hebbian Worker")` to `BackgroundWorkerBuilder::new("pg_ghola Consolidation Worker")`.

Change `pub mod worker;` to `pub mod consolidation_worker;`.

**Step 2: Rename the file**

```bash
mv src/worker.rs src/consolidation_worker.rs
```

**Step 3: Update all log messages in src/consolidation_worker.rs**

Replace all `"pg_ghola worker:"` with `"pg_ghola consolidation worker:"` in log messages.

Rename `worker_main` to `consolidation_worker_main`. Update the `set_function` call in lib.rs.

**Step 4: Rename worker_stats table and functions in src/worker_stats.rs**

- Table: `worker_stats` -> `consolidation_worker_stats`
- Type: `worker_status` -> `consolidation_worker_status`
- Function: `get_worker_stats` -> `get_consolidation_worker_stats`
- All SQL references in the file

**Step 5: Update src/consolidation_worker.rs to reference new table name**

All `ghola.worker_stats` references become `ghola.consolidation_worker_stats`.

**Step 6: Update integration_tests.rs**

- `test_all_tables_exist`: replace `"worker_stats"` with `"consolidation_worker_stats"` in the table list
- `test_worker_stats_table_in_schema`: update table name
- `test_get_worker_stats_returns_initial_state`: update function call
- Any other references

**Step 7: Run tests**

Run: `cd ~/pg_ghola && cargo test 2>&1 | tail -5`
Expected: Same pass count as before (83+), no new failures.

**Step 8: Commit**

```bash
git add -A
git commit -m "refactor: rename Hebbian worker to consolidation worker

Reflects expanded role: Hebbian learning, decay, pruning, archival,
and soon clustering/rebalancing. Neuroscience analog: sleep consolidation."
```

---

### Task 2: Add linfa dependency and cluster_centroids table

**Files:**
- Modify: `Cargo.toml` (add linfa-clustering, linfa, ndarray)
- Modify: `src/schema.rs` (add cluster_centroids table + index)

**Step 1: Add linfa dependencies to Cargo.toml**

```toml
[dependencies]
pgrx = "=0.17.0"
linfa = "0.7"
linfa-clustering = "0.7"
ndarray = "0.16"
```

**Step 2: Add cluster_centroids table to schema.rs**

Add a new extension_sql! block after the gating_worker_stats table:

```sql
CREATE TABLE cluster_centroids (
    id           serial PRIMARY KEY,
    workspace_id uuid NOT NULL,
    centroid     vector(1024) NOT NULL,
    member_count integer NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX cluster_centroids_workspace_idx
    ON cluster_centroids (workspace_id);
```

Name: `"create_cluster_centroids_table"`, requires: `["create_mnemes_table"]`.

**Step 3: Add test for cluster_centroids table**

In schema.rs tests, add `test_cluster_centroids_table_insert`:

```rust
#[pg_test]
fn test_cluster_centroids_table_insert() {
    let emb = zero_embedding_literal();
    Spi::run(&format!(
        "INSERT INTO ghola.cluster_centroids (workspace_id, centroid, member_count) \
         VALUES (gen_random_uuid(), '{emb}'::vector, 10)"
    ))
    .expect("inserting into cluster_centroids should succeed");
}
```

**Step 4: Verify compilation**

Run: `cd ~/pg_ghola && cargo test 2>&1 | tail -5`
Expected: Compiles, new test may fail due to vector dim mismatch (pre-existing), no regressions.

**Step 5: Commit**

```bash
git add Cargo.toml Cargo.lock src/schema.rs
git commit -m "feat: add linfa dependency and cluster_centroids table"
```

---

### Task 3: Update entity extraction to store compound + individual tokens

**Files:**
- Modify: `src/gating_worker.rs` (add `expand_entity_tokens` function, update `extract_entities`)

**Step 1: Write failing tests for token expansion**

Add to gating_worker tests:

```rust
#[test]
fn test_extract_entities_individual_tokens() {
    let entities = extract_entities("I met Sarah Chen at the conference.");
    assert!(entities.contains(&"sarah".to_string()),
        "should contain individual token 'sarah', got: {:?}", entities);
    assert!(entities.contains(&"chen".to_string()),
        "should contain individual token 'chen', got: {:?}", entities);
    assert!(entities.contains(&"sarah chen".to_string()),
        "should contain compound form 'sarah chen', got: {:?}", entities);
}

#[test]
fn test_extract_entities_title_prefix_tokens() {
    let entities = extract_entities("I saw Dr. Smith at the clinic.");
    assert!(entities.contains(&"dr. smith".to_string()),
        "should contain compound 'dr. smith', got: {:?}", entities);
    assert!(entities.contains(&"smith".to_string()),
        "should contain individual 'smith', got: {:?}", entities);
}

#[test]
fn test_extract_entities_single_word_no_duplicate_token() {
    let entities = extract_entities("I talked to Sarah about the project.");
    // "sarah" is already a single word, so only one entry (no compound to split)
    let sarah_count = entities.iter().filter(|e| e.contains("sarah")).count();
    assert_eq!(sarah_count, 1, "single-word entity should appear once, got: {:?}", entities);
}
```

**Step 2: Run tests to verify RED**

Run: `cd ~/pg_ghola && cargo test gating_worker::tests::test_extract_entities_individual 2>&1 | tail -5`
Expected: FAIL (no "chen" token in output, only "sarah chen").

**Step 3: Add token expansion to extract_entities**

After the existing `entities.sort(); entities.dedup();` at the end of `extract_entities`, add expansion:

```rust
    // Expand compound entities into individual tokens
    let mut expanded = Vec::new();
    for entity in &entities {
        expanded.push(entity.clone());
        let words: Vec<&str> = entity.split_whitespace().collect();
        if words.len() >= 2 {
            for word in &words {
                let w = word.trim_matches(|c: char| !c.is_alphanumeric() && c != '.' && c != '@');
                if !w.is_empty() && !COMMON_STARTERS.contains(&w.to_lowercase().as_str()) {
                    expanded.push(w.to_lowercase());
                }
            }
        }
    }
    expanded.sort();
    expanded.dedup();
    expanded
```

Replace the final `entities` return with `expanded`.

**Step 4: Run tests to verify GREEN**

Run: `cd ~/pg_ghola && cargo test gating_worker::tests 2>&1 | tail -5`
Expected: All gating_worker tests pass, including new ones.

**Step 5: Commit**

```bash
git add src/gating_worker.rs
git commit -m "feat: expand entity extraction to store compound + individual tokens

'Sarah Chen' now produces ['sarah chen', 'sarah', 'chen'].
Enables partial matching via GIN array overlap."
```

---

### Task 4: Replace FTS-gated recall with four-pathway union

**Files:**
- Modify: `src/recall.rs:184-255` (replace the gated CTE with four-pathway union)

**Step 1: Replace the candidate generation SQL**

Replace everything from `let min_gated_candidates = 50;` through the end of the CTE `combined"` string with the four-pathway union:

```rust
    // Step 1: Fetch candidate pool via multi-pathway retrieval
    // Four independent pathways, all additive, none blocks another

    // Extract entity tokens from query for entity pathway
    let query_entities = crate::gating_worker::extract_entities(&escaped_text);

    // Build entity pathway CTE (only if query has entities)
    let entity_cte = if !query_entities.is_empty() {
        let ent_literals: Vec<String> = query_entities
            .iter()
            .map(|e| format!("'{}'", e.replace('\'', "''")))
            .collect();
        format!(
            "entity_matches AS ( \
                SELECT id, concept, content, confidence::float8, access_count, \
                       GREATEST(EXTRACT(EPOCH FROM (now() - last_access)) / 86400.0, {min_age})::float8 AS age_days, \
                       (1.0 - (embedding <=> '{emb}'::vector))::float8 AS cosine_sim, \
                       ts_rank(search_vector, plainto_tsquery('english', '{qt}'))::float8 AS fts_rank, \
                       memory_type, session_id::text \
                FROM ghola.mnemes \
                WHERE workspace_id = '{ws}' \
                  AND state = 'active' \
                  AND confidence >= {min_conf} \
                  {filters} \
                  AND entities && ARRAY[{ents}]::text[] \
                ORDER BY embedding <=> '{emb}'::vector \
                LIMIT {pool} \
            ), ",
            min_age = ONE_MINUTE_DAYS, emb = query_embedding_text, qt = escaped_text,
            ws = workspace_id, min_conf = min_confidence, filters = extra_filters,
            ents = ent_literals.join(","), pool = pool_size,
        )
    } else {
        String::new()
    };
    let entity_union = if !query_entities.is_empty() {
        "UNION ALL SELECT * FROM entity_matches "
    } else {
        ""
    };

    // Build cluster pathway CTE (only if centroids exist)
    let cluster_k = 3; // tunable: number of nearest clusters to search
    let cluster_cte = format!(
        "nearest_clusters AS ( \
            SELECT id FROM ghola.cluster_centroids \
            WHERE workspace_id = '{ws}' \
            ORDER BY centroid <=> '{emb}'::vector \
            LIMIT {k} \
        ), \
        cluster_matches AS ( \
            SELECT m.id, m.concept, m.content, m.confidence::float8, m.access_count, \
                   GREATEST(EXTRACT(EPOCH FROM (now() - m.last_access)) / 86400.0, {min_age})::float8 AS age_days, \
                   (1.0 - (m.embedding <=> '{emb}'::vector))::float8 AS cosine_sim, \
                   ts_rank(m.search_vector, plainto_tsquery('english', '{qt}'))::float8 AS fts_rank, \
                   m.memory_type, m.session_id::text \
            FROM ghola.mnemes m \
            WHERE m.workspace_id = '{ws}' \
              AND m.state = 'active' \
              AND m.confidence >= {min_conf} \
              {filters} \
              AND m.cluster_id IN (SELECT id FROM nearest_clusters) \
              AND (SELECT count(*) FROM nearest_clusters) > 0 \
            ORDER BY m.embedding <=> '{emb}'::vector \
            LIMIT {pool} \
        ), ",
        ws = workspace_id, emb = query_embedding_text, qt = escaped_text,
        min_age = ONE_MINUTE_DAYS, min_conf = min_confidence, filters = extra_filters,
        k = cluster_k, pool = pool_size,
    );

    let candidates: Vec<Candidate> = Spi::connect(|client| {
        let query = format!(
            "WITH \
            semantic AS ( \
                SELECT id, concept, content, confidence::float8, access_count, \
                       GREATEST(EXTRACT(EPOCH FROM (now() - last_access)) / 86400.0, {min_age})::float8 AS age_days, \
                       (1.0 - (embedding <=> '{emb}'::vector))::float8 AS cosine_sim, \
                       ts_rank(search_vector, plainto_tsquery('english', '{qt}'))::float8 AS fts_rank, \
                       memory_type, session_id::text \
                FROM ghola.mnemes \
                WHERE workspace_id = '{ws}' \
                  AND state = 'active' \
                  AND confidence >= {min_conf} \
                  {filters} \
                ORDER BY embedding <=> '{emb}'::vector \
                LIMIT {pool} \
            ), \
            lexical AS ( \
                SELECT id, concept, content, confidence::float8, access_count, \
                       GREATEST(EXTRACT(EPOCH FROM (now() - last_access)) / 86400.0, {min_age})::float8 AS age_days, \
                       (1.0 - (embedding <=> '{emb}'::vector))::float8 AS cosine_sim, \
                       ts_rank(search_vector, plainto_tsquery('english', '{qt}'))::float8 AS fts_rank, \
                       memory_type, session_id::text \
                FROM ghola.mnemes \
                WHERE workspace_id = '{ws}' \
                  AND state = 'active' \
                  AND confidence >= {min_conf} \
                  {filters} \
                  AND search_vector @@ plainto_tsquery('english', '{qt}') \
                ORDER BY ts_rank(search_vector, plainto_tsquery('english', '{qt}')) DESC \
                LIMIT {pool} \
            ), \
            {entity_cte} \
            {cluster_cte} \
            SELECT DISTINCT ON (id) id, concept, content, confidence, access_count, \
                   age_days, cosine_sim, fts_rank, memory_type, session_id \
            FROM ( \
                SELECT * FROM semantic \
                UNION ALL \
                SELECT * FROM lexical \
                {entity_union} \
                UNION ALL \
                SELECT * FROM cluster_matches \
            ) combined",
            ws = workspace_id,
            qt = escaped_text,
            emb = query_embedding_text,
            min_conf = min_confidence,
            min_age = ONE_MINUTE_DAYS,
            pool = pool_size,
            filters = extra_filters,
            entity_cte = entity_cte,
            cluster_cte = cluster_cte,
            entity_union = entity_union,
        );
```

The row parsing code after the query remains unchanged.

**Step 2: Run tests**

Run: `cd ~/pg_ghola && cargo test 2>&1 | tail -5`
Expected: All previous passing tests still pass. No regressions.

**Step 3: Commit**

```bash
git add src/recall.rs
git commit -m "feat: replace FTS-gated recall with four-pathway union

Semantic (HNSW), lexical (FTS), entity (GIN), and cluster pathways
all run in parallel. No pathway silences another. Scoring unchanged."
```

---

### Task 5: Add cluster assignment to gating worker

**Files:**
- Modify: `src/gating_worker.rs` (add cluster assignment after entity/date/intent extraction in `process_one_gating_item`)

**Step 1: Add cluster assignment in process_one_gating_item**

After the existing entity/date/intent extraction and UPDATE, add:

```rust
    // Assign cluster_id if centroids exist for this workspace
    let cluster_assignment = Spi::get_one::<i32>(&format!(
        "SELECT c.id FROM ghola.cluster_centroids c \
         WHERE c.workspace_id = '{ws_id}' \
         ORDER BY c.centroid <=> (SELECT embedding FROM ghola.mnemes WHERE id = '{mneme_id}') \
         LIMIT 1"
    ));

    if let Ok(Some(cluster_id)) = cluster_assignment {
        // Assign cluster_id to mneme
        Spi::run(&format!(
            "UPDATE ghola.mnemes SET cluster_id = {cluster_id} WHERE id = '{mneme_id}'"
        )).unwrap_or_else(|e| log!("gating worker: cluster assign failed: {e}"));

        // Incrementally update centroid (running average)
        Spi::run(&format!(
            "UPDATE ghola.cluster_centroids SET \
                 centroid = centroid + ((SELECT embedding FROM ghola.mnemes WHERE id = '{mneme_id}') - centroid) / (member_count + 1), \
                 member_count = member_count + 1, \
                 updated_at = now() \
             WHERE id = {cluster_id}"
        )).unwrap_or_else(|e| log!("gating worker: centroid update failed: {e}"));
    }
```

**Step 2: Run tests**

Run: `cd ~/pg_ghola && cargo test 2>&1 | tail -5`
Expected: All tests pass. Cluster assignment is a no-op when no centroids exist.

**Step 3: Commit**

```bash
git add src/gating_worker.rs
git commit -m "feat: add cluster assignment to gating worker

Assigns nearest centroid as cluster_id, updates centroid incrementally.
No-op when no centroids exist (cold start graceful degradation)."
```

---

### Task 6: Add initial clustering and rebalancing to consolidation worker

**Files:**
- Modify: `src/consolidation_worker.rs` (add clustering check to periodic maintenance, add k-means functions)

**Step 1: Add clustering constants and imports**

At the top of consolidation_worker.rs, add:

```rust
use linfa::prelude::*;
use linfa_clustering::KMeans;
use ndarray::Array2;
```

Add constants:

```rust
const CLUSTERING_CHECK_INTERVAL: Duration = Duration::from_secs(3600); // 1 hour
const REBALANCE_INTERVAL: Duration = Duration::from_secs(86400); // 24 hours
const MIN_MNEMES_FOR_CLUSTERING: i64 = 500; // don't cluster below this
```

**Step 2: Implement compute_initial_clusters**

```rust
fn compute_initial_clusters(workspace_id: &str) {
    // Count active mnemes
    let count = Spi::get_one::<i64>(&format!(
        "SELECT count(*) FROM ghola.mnemes \
         WHERE workspace_id = '{workspace_id}' AND state = 'active'"
    )).unwrap_or(Some(0)).unwrap_or(0);

    if count < MIN_MNEMES_FOR_CLUSTERING {
        return;
    }

    let k = ((count as f64 / 10.0).sqrt()).max(2.0) as usize;
    log!("pg_ghola consolidation worker: computing initial clusters, k={k}, mnemes={count}");

    // Read all embeddings (this is the expensive part)
    // Each embedding is 1024 floats
    let dims = 1024_usize; // TODO: read from config table
    let mut data = Vec::new();
    let mut mneme_ids = Vec::new();

    Spi::connect(|client| {
        let rows = client.select(&format!(
            "SELECT id::text, embedding::text FROM ghola.mnemes \
             WHERE workspace_id = '{workspace_id}' AND state = 'active'"
        ), None, &[]).expect("failed to read embeddings");

        for row in rows {
            let id: String = row.get::<String>(1).expect("err").expect("null");
            let emb_text: String = row.get::<String>(2).expect("err").expect("null");
            // Parse "[0.1,0.2,...]" format
            let values: Vec<f64> = emb_text
                .trim_matches(|c| c == '[' || c == ']')
                .split(',')
                .filter_map(|s| s.trim().parse().ok())
                .collect();
            if values.len() == dims {
                data.extend_from_slice(&values);
                mneme_ids.push(id);
            }
        }
    });

    let n = mneme_ids.len();
    if n < k * 10 {
        return; // insufficient data
    }

    // Run k-means via linfa
    let array = Array2::from_shape_vec((n, dims), data)
        .expect("failed to create ndarray");
    let dataset = linfa::DatasetBase::from(array);

    let model = KMeans::params(k)
        .max_n_iterations(100)
        .tolerance(1e-4)
        .fit(&dataset)
        .expect("k-means failed");

    let centroids = model.centroids();
    let assignments = model.predict(&dataset);

    // Write centroids to cluster_centroids table
    Spi::run(&format!(
        "DELETE FROM ghola.cluster_centroids WHERE workspace_id = '{workspace_id}'"
    )).expect("failed to clear old centroids");

    for i in 0..k {
        let centroid_vec: Vec<String> = centroids.row(i)
            .iter().map(|v| format!("{v}")).collect();
        let centroid_str = format!("[{}]", centroid_vec.join(","));
        let member_count = assignments.iter().filter(|&&a| a == i).count();

        Spi::run(&format!(
            "INSERT INTO ghola.cluster_centroids (workspace_id, centroid, member_count) \
             VALUES ('{workspace_id}', '{centroid_str}'::vector, {member_count})"
        )).expect("failed to insert centroid");
    }

    // Assign cluster_ids to all mnemes
    // Re-read centroid IDs (serial, so they're sequential)
    let centroid_ids: Vec<i32> = Spi::connect(|client| {
        let rows = client.select(&format!(
            "SELECT id FROM ghola.cluster_centroids \
             WHERE workspace_id = '{workspace_id}' ORDER BY id"
        ), None, &[]).expect("failed");
        rows.into_iter().map(|r| r.get::<i32>(1).expect("err").expect("null")).collect()
    });

    for (idx, mneme_id) in mneme_ids.iter().enumerate() {
        let cluster_idx = assignments[idx];
        let cluster_db_id = centroid_ids[cluster_idx];
        Spi::run(&format!(
            "UPDATE ghola.mnemes SET cluster_id = {cluster_db_id} WHERE id = '{mneme_id}'::uuid"
        )).unwrap_or_else(|e| log!("cluster assign failed: {e}"));
    }

    log!("pg_ghola consolidation worker: initial clustering complete, {k} clusters, {n} mnemes assigned");
}
```

**Step 3: Implement rebalance_clusters**

Same as compute_initial_clusters but uses existing centroid count as k. Called during the 24-hour rebalance cycle.

```rust
fn rebalance_clusters(workspace_id: &str) {
    let existing_k = Spi::get_one::<i64>(&format!(
        "SELECT count(*) FROM ghola.cluster_centroids WHERE workspace_id = '{workspace_id}'"
    )).unwrap_or(Some(0)).unwrap_or(0);

    if existing_k == 0 {
        return; // nothing to rebalance
    }

    log!("pg_ghola consolidation worker: rebalancing clusters, k={existing_k}");
    // Reuse compute_initial_clusters logic with existing k
    // (for v1, full recomputation is the rebalance strategy)
    compute_initial_clusters(workspace_id);
}
```

**Step 4: Add clustering checks to the main worker loop**

In the consolidation worker's main loop, after the existing periodic maintenance checks, add:

```rust
    // Periodic: check if initial clustering needed (every hour)
    if last_clustering_check.elapsed() >= CLUSTERING_CHECK_INTERVAL {
        BackgroundWorker::transaction(|| {
            // Get distinct workspace_ids
            let workspaces: Vec<String> = Spi::connect(|client| {
                let rows = client.select(
                    "SELECT DISTINCT workspace_id::text FROM ghola.mnemes WHERE state = 'active'",
                    None, &[]
                ).expect("failed");
                rows.into_iter()
                    .map(|r| r.get::<String>(1).expect("err").expect("null"))
                    .collect()
            });

            for ws in &workspaces {
                let has_centroids = Spi::get_one::<i64>(&format!(
                    "SELECT count(*) FROM ghola.cluster_centroids WHERE workspace_id = '{ws}'"
                )).unwrap_or(Some(0)).unwrap_or(0);

                if has_centroids == 0 {
                    compute_initial_clusters(ws);
                }
            }
        });
        last_clustering_check = Instant::now();
    }

    // Periodic: rebalance clusters (every 24 hours)
    if last_rebalance.elapsed() >= REBALANCE_INTERVAL {
        BackgroundWorker::transaction(|| {
            let workspaces: Vec<String> = Spi::connect(|client| {
                let rows = client.select(
                    "SELECT DISTINCT workspace_id::text FROM ghola.cluster_centroids",
                    None, &[]
                ).expect("failed");
                rows.into_iter()
                    .map(|r| r.get::<String>(1).expect("err").expect("null"))
                    .collect()
            });

            for ws in &workspaces {
                rebalance_clusters(ws);
            }
        });
        last_rebalance = Instant::now();
    }
```

Add `last_clustering_check` and `last_rebalance` as `Instant::now()` in the variable initialization before the main loop.

**Step 5: Run tests**

Run: `cd ~/pg_ghola && cargo test 2>&1 | tail -5`
Expected: All tests pass. Clustering functions are only called when centroids don't exist and enough mnemes are present.

**Step 6: Commit**

```bash
git add src/consolidation_worker.rs Cargo.toml
git commit -m "feat: add k-means clustering to consolidation worker

Initial clustering triggered hourly when mneme count > 500 and no
centroids exist. Full k-means rebalancing every 24 hours. Uses linfa."
```

---

### Task 7: Update integration tests

**Files:**
- Modify: `src/integration_tests.rs`

**Step 1: Add test for multi-pathway recall (graceful degradation, no clusters)**

```rust
#[pg_test]
fn test_recall_multi_pathway_no_clusters() {
    // Verify recall works with no clusters (cluster pathway is no-op)
    // This tests the graceful degradation path
    let (ws_id, _) = setup_recall_test_data();
    let emb = make_query_embedding();

    let count = Spi::get_one::<i64>(&format!(
        "SELECT count(*) FROM ghola.recall(\
            '{ws_id}'::uuid, 'kubernetes pod scheduling', '{emb}'::vector, \
            10, 0.0, NULL)"
    )).expect("query failed").expect("null");

    assert!(count > 0, "recall should return results even without clusters");
}
```

**Step 2: Add test for cluster_centroids table in test_all_tables_exist**

Add `"cluster_centroids"` to the table list.

**Step 3: Add test verifying entity pathway adds candidates**

```rust
#[pg_test]
fn test_recall_entity_pathway_surfaces_matches() {
    // Insert a mneme with known entities, verify it appears in recall
    // when the query mentions those entities
    // (This test validates the entity GIN pathway contributes candidates)
    Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector");

    let ws = "00000000-0000-0000-0000-000000000077";
    let emb = make_query_embedding();

    // Insert a mneme with entities set (simulating gating worker output)
    Spi::run(&format!(
        "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding, entities) \
         VALUES ('{ws}', 'meeting notes', 'discussed project timeline with team', \
                 '{emb}'::vector, ARRAY['sarah', 'sarah chen']::text[])"
    )).expect("insert");

    // Query mentioning sarah -- entity pathway should find it
    let count = Spi::get_one::<i64>(&format!(
        "SELECT count(*) FROM ghola.recall(\
            '{ws}'::uuid, 'what did Sarah say', '{emb}'::vector, \
            10, 0.0, NULL)"
    )).expect("query").expect("null");

    assert!(count > 0, "entity pathway should surface mneme matching query entities");
}
```

**Step 4: Run tests, commit**

```bash
git add src/integration_tests.rs
git commit -m "test: add multi-pathway recall and entity pathway integration tests"
```

---

### Task 8: Live migration and deployment

**Step 1: Build Docker image**

```bash
cd ~/pg_ghola
docker build --no-cache -f Dockerfile.cnpg -t cnpg-pg18-ghola:18.1-ghola-0.0.5 .
```

**Step 2: Transfer to NUC**

```bash
docker save cnpg-pg18-ghola:18.1-ghola-0.0.5 -o /tmp/cnpg-ghola-0.0.5.tar
scp /tmp/cnpg-ghola-0.0.5.tar nuc:/tmp/
# User runs: ssh -t nuc 'sudo k3s ctr images import /tmp/cnpg-ghola-0.0.5.tar && rm /tmp/cnpg-ghola-0.0.5.tar'
```

**Step 3: Run live database migration**

```sql
BEGIN;

-- 1. Create cluster_centroids table
CREATE TABLE IF NOT EXISTS ghola.cluster_centroids (
    id           serial PRIMARY KEY,
    workspace_id uuid NOT NULL,
    centroid     vector(1024) NOT NULL,
    member_count integer NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS cluster_centroids_workspace_idx
    ON ghola.cluster_centroids (workspace_id);

-- 2. Rename worker_stats to consolidation_worker_stats
ALTER TABLE ghola.worker_stats RENAME TO consolidation_worker_stats;

-- 3. Update recall and recall_inner function signatures
-- (detach from extension, drop, recreate with new signatures)
-- Same pattern as the 0.0.4 migration

-- 4. Grant permissions
GRANT SELECT, INSERT, UPDATE, DELETE ON ghola.cluster_centroids TO memory_api;
GRANT USAGE, SELECT ON SEQUENCE ghola.cluster_centroids_id_seq TO memory_api;
GRANT SELECT, INSERT, UPDATE, DELETE ON ghola.consolidation_worker_stats TO memory_api;

COMMIT;
```

**Step 4: Update ArgoCD manifest**

Change image tag to `18.1-ghola-0.0.5` in `~/ai/homelab-k3s/chapterhouse/postgres-cluster.yaml`. Commit and push.

**Step 5: Verify deployment**

```bash
# Check all 3 workers running
kubectl exec -n ch-system memory-db-1 -- psql -U postgres -d memories \
    -c "SELECT pid, backend_type FROM pg_stat_activity WHERE backend_type LIKE '%ghola%'"

# Verify consolidation worker name
# Expected: pg_ghola Consolidation Worker (not Hebbian Worker)

# Check cluster_centroids table exists
kubectl exec -n ch-system memory-db-1 -- psql -U postgres -d memories \
    -c "SELECT count(*) FROM ghola.cluster_centroids"
```

**Step 6: Run benchmark**

```bash
cd ~/longmemeval-ghola
.venv/bin/python run.py all --backend ghola_mcp --dataset s
```

Compare R@5 against:
- Pre-gating baseline: 19.4%
- FTS-gated (regression): 13.6%
- Multi-pathway (this release): ???

---

## Task Dependency Summary

```
Task 1 (rename worker)
  └→ Task 6 (clustering in consolidation worker)

Task 2 (linfa + cluster_centroids table)
  ├→ Task 4 (four-pathway recall)
  ├→ Task 5 (cluster assignment in gating worker)
  └→ Task 6 (clustering in consolidation worker)

Task 3 (entity token expansion)
  └→ Task 4 (four-pathway recall)

Task 4 (recall rewrite) ─→ Task 7 (integration tests)

Tasks 1-7 ─→ Task 8 (build + deploy)
```

Tasks 1, 2, 3 are independent of each other (can parallelize).
Task 4 depends on 2 and 3.
Task 5 depends on 2.
Task 6 depends on 1, 2.
Task 7 depends on 4.
Task 8 depends on everything.
