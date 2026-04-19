# Thalamic Gating Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add two-tier pre-recall filtering (FTS fast gate + async-extracted deep gate) to dramatically narrow the HNSW candidate set before cosine similarity scoring.

**Architecture:** New columns on `mnemes` table for extracted attributes (entities, content_dates, cluster_id, intent). New `gating_queue` table + trigger for async extraction. Modified `recall_inner()` to pre-filter via FTS before the existing HNSW+FTS union. Third background worker for attribute extraction.

**Tech Stack:** Rust, pgrx 0.17.0, PostgreSQL 18, cargo-pgrx

**Design doc:** `docs/plans/2026-04-09-thalamic-gating-design.md`

---

### Task 1: Add gating columns and queue table to schema

**Files:**
- Modify: `src/schema.rs`

**Step 1: Add new columns to the mnemes CREATE TABLE**

In `src/schema.rs`, find the `create_mnemes_table` extension_sql! block. Add after the `expires_at` column:

```sql
    entities        text[] DEFAULT NULL,
    content_dates   timestamptz[] DEFAULT NULL,
    cluster_id      integer DEFAULT NULL,
    intent          text DEFAULT NULL
        CHECK (intent IN ('decision', 'preference', 'fact', 'question', 'plan', 'experience'))
```

**Step 2: Add indexes for gating columns**

In the `create_indexes` extension_sql! block, add after the existing indexes:

```sql
-- Gating indexes (partial -- only index populated rows)
CREATE INDEX mnemes_entities_gin_idx
    ON mnemes USING gin (entities) WHERE entities IS NOT NULL;

CREATE INDEX mnemes_content_dates_gin_idx
    ON mnemes USING gin (content_dates) WHERE content_dates IS NOT NULL;

CREATE INDEX mnemes_cluster_id_idx
    ON mnemes (cluster_id) WHERE cluster_id IS NOT NULL;

CREATE INDEX mnemes_intent_idx
    ON mnemes (intent) WHERE intent IS NOT NULL;
```

**Step 3: Add gating_queue table**

Add a new extension_sql! block (after the contradiction_queue block):

```sql
CREATE TABLE gating_queue (
    id           bigserial PRIMARY KEY,
    workspace_id uuid NOT NULL,
    mneme_id     uuid NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
```

**Step 4: Add gating_worker_stats table**

```sql
CREATE TABLE gating_worker_stats (
    id                integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    state             text NOT NULL DEFAULT 'stopped',
    queue_depth       bigint NOT NULL DEFAULT 0,
    items_processed   bigint NOT NULL DEFAULT 0,
    last_process_at   timestamptz,
    poll_interval_ms  integer NOT NULL DEFAULT 5000,
    started_at        timestamptz,
    updated_at        timestamptz DEFAULT now()
);

INSERT INTO gating_worker_stats (id) VALUES (1) ON CONFLICT DO NOTHING;
```

**Step 5: Update the INSERT trigger to also enqueue to gating_queue**

In `src/contradiction.rs`, find the `contradiction_check_trigger` and add the gating enqueue. Change the trigger function body to:

```sql
CREATE OR REPLACE FUNCTION contradiction_check_trigger()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO @extschema@.contradiction_queue (workspace_id, mneme_id)
    VALUES (NEW.workspace_id, NEW.id);
    INSERT INTO @extschema@.gating_queue (workspace_id, mneme_id)
    VALUES (NEW.workspace_id, NEW.id);
    RETURN NEW;
END;
$$;
```

The trigger requires list needs `"create_gating_queue_table"` added.

**Step 6: Run tests**

Run: `cd ~/pg_ghola && cargo test 2>&1 | tail -10`
Expected: All unit tests pass.

**Step 7: Commit**

```bash
git add src/schema.rs src/contradiction.rs
git commit -m "feat: add gating columns, indexes, queue table, and enqueue trigger"
```

---

### Task 2: Implement FTS pre-filter in recall_inner (Tier 1 fast gate)

**Files:**
- Modify: `src/recall.rs:128-205`

**Context:** The current `recall_inner` function (line 128+) fetches candidates via a CTE that UNION ALLs HNSW candidates with FTS candidates, both scanning the full workspace. The change adds an FTS pre-filter: when the query text produces FTS matches, use those as the candidate set for HNSW reranking instead of scanning the full workspace.

**Step 1: Add a gated candidate CTE**

Replace the existing candidate query (lines 161-205) with a three-stage approach:

```rust
    // Step 1: Fetch candidate pool with thalamic gating
    // Tier 1 (fast gate): FTS pre-filter narrows candidates before HNSW
    // If FTS finds >= min_gated_candidates, HNSW searches only the gated set
    // If FTS finds fewer, fall back to full workspace HNSW scan
    let min_gated_candidates = 50; // Minimum gate size before fallback
    let gate_pool = pool_size * 5; // Cast a wider FTS net for the gate

    let candidates: Vec<Candidate> = Spi::connect(|client| {
        let query = format!(
            "WITH fts_gate AS ( \
                SELECT id \
                FROM ghola.mnemes \
                WHERE workspace_id = '{ws}' \
                  AND state = 'active' \
                  AND confidence >= {min_conf} \
                  {filters} \
                  AND search_vector @@ plainto_tsquery('english', '{qt}') \
                ORDER BY ts_rank_cd(search_vector, plainto_tsquery('english', '{qt}')) DESC \
                LIMIT {gate_pool} \
            ), \
            gated_hnsw AS ( \
                SELECT id, concept, content, confidence::float8, access_count, \
                       GREATEST(EXTRACT(EPOCH FROM (now() - last_access)) / 86400.0, {min_age})::float8 AS age_days, \
                       (1.0 - (embedding <=> '{emb}'::vector))::float8 AS cosine_sim, \
                       ts_rank(search_vector, plainto_tsquery('english', '{qt}'))::float8 AS fts_rank, \
                       memory_type, session_id::text \
                FROM ghola.mnemes \
                WHERE id IN (SELECT id FROM fts_gate) \
                ORDER BY embedding <=> '{emb}'::vector \
                LIMIT {pool} \
            ), \
            ungated_hnsw AS ( \
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
            fts_fallback AS ( \
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
            ) \
            SELECT DISTINCT ON (id) id, concept, content, confidence, access_count, \
                   age_days, cosine_sim, fts_rank, memory_type, session_id \
            FROM ( \
                SELECT * FROM gated_hnsw \
                WHERE (SELECT count(*) FROM fts_gate) >= {min_gate} \
                UNION ALL \
                SELECT * FROM ungated_hnsw \
                WHERE (SELECT count(*) FROM fts_gate) < {min_gate} \
                UNION ALL \
                SELECT * FROM fts_fallback \
            ) combined",
            ws = workspace_id,
            qt = escaped_text,
            emb = query_embedding_text,
            min_conf = min_confidence,
            min_age = ONE_MINUTE_DAYS,
            pool = pool_size,
            gate_pool = gate_pool,
            min_gate = min_gated_candidates,
            filters = extra_filters,
        );
        // ... rest of parsing unchanged
```

The logic:
- `fts_gate`: Find all FTS-matching mneme IDs (cheap, GIN index scan)
- `gated_hnsw`: If gate has >= 50 results, run HNSW only within the gated set
- `ungated_hnsw`: If gate has < 50 results, fall back to full workspace HNSW
- `fts_fallback`: Always include top FTS matches (catches keyword-exact matches that HNSW misses)
- UNION ALL + DISTINCT ON (id): Merge and deduplicate

**Step 2: Run tests**

Run: `cd ~/pg_ghola && cargo test 2>&1 | tail -10`
All existing recall tests must still pass -- the gating is transparent when FTS doesn't match.

**Step 3: Commit**

```bash
git add src/recall.rs
git commit -m "feat: add FTS pre-filter (tier 1 thalamic gate) to recall_inner"
```

---

### Task 3: Create gating_worker module

**Files:**
- Create: `src/gating_worker.rs`
- Modify: `src/lib.rs` (add module declaration)

**Step 1: Create src/gating_worker.rs**

Follow the exact same pattern as `src/contradiction_worker.rs`. The worker:
- Drains `gating_queue` one item at a time
- For each mneme, extracts entities, content_dates, intent
- Updates the mneme row with extracted values
- Uses adaptive polling: 5s active, 60s idle, 300s dormant

Key extraction functions (heuristic, no LLM):

```rust
/// Extract entity-like terms from text.
/// Finds capitalized word sequences (2+ words), quoted terms, @mentions.
fn extract_entities(text: &str) -> Vec<String> {
    let mut entities = Vec::new();
    // Capitalized sequences: "Dr. Sarah Chen", "New York"
    // Simple heuristic: sequences of 2+ words starting with uppercase
    let words: Vec<&str> = text.split_whitespace().collect();
    let mut i = 0;
    while i < words.len() {
        if words[i].chars().next().map_or(false, |c| c.is_uppercase()) {
            let mut entity = vec![words[i]];
            let mut j = i + 1;
            while j < words.len() && words[j].chars().next().map_or(false, |c| c.is_uppercase()) {
                entity.push(words[j]);
                j += 1;
            }
            if entity.len() >= 2 {
                let e = entity.join(" ").trim_matches(|c: char| !c.is_alphanumeric()).to_string();
                if !e.is_empty() {
                    entities.push(e.to_lowercase());
                }
            }
            i = j;
        } else {
            i += 1;
        }
    }
    entities.sort();
    entities.dedup();
    entities
}

/// Extract date-like patterns from text.
/// Returns empty vec if no dates found (not an error).
fn extract_dates(text: &str) -> Vec<String> {
    // ISO dates: 2026-04-09
    // US dates: 04/09/2026, April 9, 2026
    // Relative: "last Tuesday", "3 weeks ago" (skip for v1, too complex)
    let mut dates = Vec::new();
    // ISO pattern
    let iso_re = regex::Regex::new(r"\b(\d{4}-\d{2}-\d{2})\b").unwrap();
    for cap in iso_re.captures_iter(text) {
        dates.push(cap[1].to_string());
    }
    dates
}

/// Classify intent from text content.
fn classify_intent(text: &str) -> Option<String> {
    let lower = text.to_lowercase();
    let checks = [
        ("decision", &["decided", "chose", "picked", "switched to", "went with", "settled on"][..]),
        ("preference", &["prefer", "like", "enjoy", "favorite", "rather", "love to"]),
        ("plan", &["plan to", "going to", "will", "schedule", "intend", "aim to"]),
        ("question", &["?", "how do", "what is", "can you", "wondering"]),
        ("experience", &["went to", "visited", "tried", "attended", "saw", "met"]),
    ];
    let mut best: Option<(&str, usize)> = None;
    for (intent, keywords) in &checks {
        let count = keywords.iter().filter(|kw| lower.contains(**kw)).count();
        if count > 0 {
            if best.is_none() || count > best.unwrap().1 {
                best = Some((intent, count));
            }
        }
    }
    best.map(|(intent, _)| intent.to_string())
        .or(Some("fact".to_string()))
}
```

The worker main function processes one queue item per cycle:

```rust
fn process_one_gating_item() -> i64 {
    let result = Spi::get_two::<pgrx::Uuid, pgrx::Uuid>(
        "WITH d AS ( \
             DELETE FROM ghola.gating_queue \
             WHERE id = (SELECT id FROM ghola.gating_queue ORDER BY id LIMIT 1) \
             RETURNING mneme_id, workspace_id \
         ) SELECT mneme_id, workspace_id FROM d"
    );

    match result {
        Ok((Some(mneme_id), Some(_ws_id))) => {
            // Read the mneme content
            let content = Spi::get_one::<String>(&format!(
                "SELECT content FROM ghola.mnemes WHERE id = '{mneme_id}'"
            ));

            if let Ok(Some(text)) = content {
                let entities = extract_entities(&text);
                let dates = extract_dates(&text);
                let intent = classify_intent(&text);

                // Build UPDATE SET clauses
                let mut sets = Vec::new();
                if !entities.is_empty() {
                    let arr = entities.iter()
                        .map(|e| format!("'{}'", e.replace('\'', "''")))
                        .collect::<Vec<_>>().join(",");
                    sets.push(format!("entities = ARRAY[{arr}]::text[]"));
                }
                if !dates.is_empty() {
                    let arr = dates.iter()
                        .map(|d| format!("'{d}'::timestamptz"))
                        .collect::<Vec<_>>().join(",");
                    sets.push(format!("content_dates = ARRAY[{arr}]::timestamptz[]"));
                }
                if let Some(ref i) = intent {
                    sets.push(format!("intent = '{i}'"));
                }

                if !sets.is_empty() {
                    Spi::run(&format!(
                        "UPDATE ghola.mnemes SET {} WHERE id = '{mneme_id}'",
                        sets.join(", ")
                    )).unwrap_or_else(|e| log!("gating worker: update failed: {e}"));
                }
            }
            1
        }
        _ => 0,
    }
}
```

Include unit tests for `extract_entities`, `extract_dates`, `classify_intent`.

Note: `regex` crate needs to be added to Cargo.toml dependencies.

**Step 2: Add module to lib.rs**

After `pub mod contradiction_worker;`:
```rust
pub mod gating_worker;
```

**Step 3: Run tests**

Run: `cd ~/pg_ghola && cargo test 2>&1 | tail -10`

**Step 4: Commit**

```bash
git add src/gating_worker.rs src/lib.rs Cargo.toml
git commit -m "feat: add gating_worker module with entity/date/intent extraction"
```

---

### Task 4: Register gating worker in _PG_init

**Files:**
- Modify: `src/lib.rs:47-78`

**Step 1: Add third BackgroundWorkerBuilder**

After the Contradiction Worker registration:

```rust
    BackgroundWorkerBuilder::new("pg_ghola Gating Worker")
        .set_function("gating_worker_main")
        .set_library("pg_ghola")
        .set_argument(0i32.into_datum())
        .enable_spi_access()
        .set_start_time(BgWorkerStartTime::RecoveryFinished)
        .set_restart_time(Some(Duration::from_secs(10)))
        .load();
```

**Step 2: Run tests, commit**

```bash
git add src/lib.rs
git commit -m "feat: register gating worker as third background worker"
```

---

### Task 5: Add Tier 2 deep gate filtering to recall_inner

**Files:**
- Modify: `src/recall.rs`

**Context:** After Task 2, recall uses FTS as Tier 1. This task adds entity and intent filtering as Tier 2 when those columns are populated.

**Step 1: Add optional deep gate filters to recall_inner**

Add new parameters to `recall_inner`:

```rust
    filter_entities: default!(Option<Vec<String>>, "NULL"),
    filter_intent: default!(Option<String>, "NULL"),
```

Build additional WHERE clauses:

```rust
    if let Some(ref ents) = filter_entities {
        if !ents.is_empty() {
            let ent_literals: Vec<String> = ents
                .iter()
                .map(|e| format!("'{}'", e.replace('\'', "''")))
                .collect();
            extra_filters.push_str(&format!(
                " AND (entities IS NULL OR entities && ARRAY[{}]::text[])",
                ent_literals.join(",")
            ));
        }
    }
    if let Some(ref intent_val) = filter_intent {
        extra_filters.push_str(&format!(
            " AND (intent IS NULL OR intent = '{}')",
            intent_val.replace('\'', "''")
        ));
    }
```

Note: `entities IS NULL OR entities && ...` ensures unprocessed mnemes (NULL entities) are always included (graceful degradation).

**Step 2: Update the SQL wrapper**

Update the `recall()` SQL wrapper to pass through the new parameters.

**Step 3: Run tests, commit**

```bash
git add src/recall.rs
git commit -m "feat: add entity/intent deep gate (tier 2) to recall_inner"
```

---

### Task 6: Update integration tests

**Files:**
- Modify: `src/integration_tests.rs`

**Step 1: Add test for gating queue enqueue**

Verify INSERT triggers enqueue to both contradiction_queue AND gating_queue.

**Step 2: Add test for FTS gate behavior**

Insert mnemes with known content, verify recall with matching keywords returns them (gated path) and recall with non-matching keywords still returns via ungated HNSW fallback.

**Step 3: Add test for new tables exist**

Update `test_all_tables_exist` to include `gating_queue` and `gating_worker_stats`.

**Step 4: Commit**

```bash
git add src/integration_tests.rs
git commit -m "test: add gating queue and FTS gate integration tests"
```

---

### Task 7: Update benchmark harness for gated recall

**Files:**
- Modify: `~/longmemeval-ghola/backends/ghola_mcp.py`

**Context:** The ghola_mcp backend calls Chapterhouse MCP `recall` tool. Chapterhouse calls `ghola.recall()`. The gating is transparent -- it happens inside the SQL function. No harness changes needed for Tier 1 (FTS gate).

For Tier 2 testing, add a mode that extracts entities from the query and passes them as parameters. This requires Chapterhouse to support entity filter passthrough in the MCP recall tool.

**Step 1: Add entity extraction to the retrieve stage**

In `ghola_mcp.py`, extract entities from query text before calling recall. Pass as tags or a new filter parameter.

**Step 2: Run benchmark**

```bash
cd ~/longmemeval-ghola
.venv/bin/python run.py all --backend ghola_mcp --dataset s
```

Compare R@5 with gating vs previous ungated run (19.4%).

**Step 3: Commit results**

---

### Task 8: Build, deploy, and verify

Same pattern as async contradiction worker deployment:

1. Build Docker image: `docker build --no-cache -f Dockerfile.cnpg -t cnpg-pg18-ghola:18.1-ghola-0.0.4 .`
2. Transfer to NUC: `docker save | ssh nuc`
3. Import: `sudo k3s ctr images import`
4. Update ArgoCD manifest to 0.0.4
5. Manually create new tables/columns in live database (same migration pattern as contradiction worker)
6. Grant permissions to memory_api user
7. Verify all three workers start

---

## Task Dependency Summary

```
Task 1 (schema: columns + queue + trigger)
  ├→ Task 2 (FTS pre-filter in recall_inner)
  │    └→ Task 5 (deep gate in recall_inner)
  │         └→ Task 7 (benchmark harness)
  ├→ Task 3 (gating_worker module)
  │    └→ Task 4 (register in _PG_init)
  └→ Task 6 (integration tests)
       └→ Task 8 (build + deploy)
```

Tasks 2 and 3 are independent of each other (can parallelize).
Tasks 5 depends on both 2 and 3.
Task 7 depends on 5.
Task 6 depends on 1.
Task 8 depends on everything.
