// pg_ghola::recall — Composite recall function
//
// The primary retrieval entry point that fuses vector similarity, full-text search,
// ACT-R temporal activation, Hebbian association strength, and Bayesian confidence
// into a single ranked result set.
// The control file's schema directive places all objects in pg_ghola automatically.
//
// Owned by: implement_composite_recall task

use pgrx::prelude::*;
use std::collections::HashMap;

use crate::scoring::{actr_activation_inner, softplus_inner};

/// One-minute floor in days (1/1440), prevents division by zero for very recent accesses.
const ONE_MINUTE_DAYS: f64 = 1.0 / 1440.0;

// ---------------------------------------------------------------------------
// SQL wrapper: recall()
//
// Provides the public interface with vector and score_weights types.
// Delegates to recall_inner() which handles the actual computation.
// Uses untyped vector to support any embedding dimension.
// ---------------------------------------------------------------------------

extension_sql!(
    r#"
CREATE FUNCTION recall(
    workspace_id uuid,
    query_text text,
    query_embedding vector,
    limit_n int DEFAULT 10,
    min_confidence float8 DEFAULT 0.0,
    weights ghola.score_weights DEFAULT NULL,
    memory_type text DEFAULT NULL,
    scope text DEFAULT NULL,
    tags text[] DEFAULT NULL,
    session_id uuid DEFAULT NULL,
    filter_entities text[] DEFAULT NULL,
    filter_intent text DEFAULT NULL
) RETURNS SETOF ghola.recall_result
LANGUAGE SQL
STABLE
AS $$
    SELECT (mneme_id, score, content_match, activation, hebbian_boost,
            confidence, concept, content)::ghola.recall_result
    FROM ghola.recall_inner(
        workspace_id, query_text, query_embedding::text, limit_n, min_confidence,
        COALESCE((weights).semantic, 0.6),
        COALESCE((weights).fts, 0.4),
        COALESCE((weights).actr_decay, 0.5),
        COALESCE((weights).hebbian_scale, 4.0),
        memory_type, scope, tags, session_id,
        filter_entities, filter_intent
    );
$$;
"#,
    name = "create_recall_wrapper",
    requires = [
        recall_inner,
        "create_type_recall_result",
        "create_type_score_weights",
        "create_mnemes_table",
        "create_associations_table",
        "create_co_activation_queue_table",
    ],
);

// ---------------------------------------------------------------------------
// Internal recall implementation
// ---------------------------------------------------------------------------

/// Internal candidate data fetched from the database.
struct Candidate {
    id: pgrx::Uuid,
    concept: String,
    content: String,
    confidence: f64,
    access_count: i32,
    age_days: f64,
    cosine_sim: f64,
    fts_rank: f64,
    memory_type: String,
    candidate_session_id: Option<String>,
}

/// Scored candidate ready for output.
struct ScoredCandidate {
    id: pgrx::Uuid,
    score: f64,
    content_match: f64,
    activation: f64,
    hebbian_boost: f64,
    confidence: f64,
    concept: String,
    content: String,
}

/// Internal implementation of the recall function.
///
/// Accepts decomposed parameters (weights as individual floats, embedding as text)
/// to avoid pgrx limitations with pgvector and composite types as parameters.
#[pg_extern(stable)]
fn recall_inner(
    workspace_id: pgrx::Uuid,
    query_text: &str,
    query_embedding_text: &str,
    limit_n: default!(i32, 10),
    min_confidence: default!(f64, 0.0),
    w_semantic: default!(f64, 0.6),
    w_fts: default!(f64, 0.4),
    w_actr_decay: default!(f64, 0.5),
    w_hebbian_scale: default!(f64, 4.0),
    filter_memory_type: default!(Option<String>, "NULL"),
    filter_scope: default!(Option<String>, "NULL"),
    filter_tags: default!(Option<Vec<String>>, "NULL"),
    filter_session_id: default!(Option<pgrx::Uuid>, "NULL"),
    filter_entities: default!(Option<Vec<String>>, "NULL"),
    filter_intent: default!(Option<String>, "NULL"),
) -> TableIterator<
    'static,
    (
        name!(mneme_id, pgrx::Uuid),
        name!(score, f64),
        name!(content_match, f64),
        name!(activation, f64),
        name!(hebbian_boost, f64),
        name!(confidence, f64),
        name!(concept, String),
        name!(content, String),
    ),
> {
    let pool_size = 3 * limit_n;
    let escaped_text = query_text.replace('\'', "''");

    // Build optional filter clauses
    let mut extra_filters = String::new();
    if let Some(ref mt) = filter_memory_type {
        extra_filters.push_str(&format!(" AND memory_type = '{}'", mt.replace('\'', "''")));
    }
    if let Some(ref sc) = filter_scope {
        extra_filters.push_str(&format!(" AND scope = '{}'", sc.replace('\'', "''")));
    }
    if let Some(ref tags) = filter_tags {
        if !tags.is_empty() {
            let tag_literals: Vec<String> = tags
                .iter()
                .map(|t| format!("'{}'", t.replace('\'', "''")))
                .collect();
            extra_filters.push_str(&format!(
                " AND tags @> ARRAY[{}]::text[]",
                tag_literals.join(",")
            ));
        }
    }
    if let Some(ref sid) = filter_session_id {
        extra_filters.push_str(&format!(" AND session_id = '{sid}'::uuid"));
    }
    // Tier 2 deep gate: entity and intent filters (graceful degradation)
    // NULL columns (unprocessed mnemes) always pass through
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
    // Always exclude expired working memories
    extra_filters.push_str(
        " AND (expires_at IS NULL OR expires_at > now())"
    );

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

    // Build cluster pathway CTE (graceful: returns empty when no centroids exist)
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
        ) ",
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

        let rows = client
            .select(&query, None, &[])
            .expect("failed to query candidate pool");

        let mut result = Vec::new();
        for row in rows {
            result.push(Candidate {
                id: row.get::<pgrx::Uuid>(1).expect("err").expect("null id"),
                concept: row.get::<String>(2).expect("err").expect("null concept"),
                content: row.get::<String>(3).expect("err").expect("null content"),
                confidence: row.get::<f64>(4).expect("err").expect("null confidence"),
                access_count: row
                    .get::<i32>(5)
                    .expect("err")
                    .expect("null access_count"),
                age_days: row.get::<f64>(6).expect("err").unwrap_or(ONE_MINUTE_DAYS),
                cosine_sim: row.get::<f64>(7).expect("err").unwrap_or(0.0),
                fts_rank: row.get::<f64>(8).expect("err").unwrap_or(0.0),
                memory_type: row.get::<String>(9).expect("err").unwrap_or("factual".to_string()),
                candidate_session_id: row.get::<String>(10).expect("err"),
            });
        }
        result
    });

    // Step 2: Fetch typed associations between all candidates in the pool
    let candidate_ids: Vec<String> = candidates.iter().map(|c| c.id.to_string()).collect();

    let hebbian_boosts: HashMap<String, f64> = if candidates.len() > 1 {
        Spi::connect(|client| {
            let id_list: String = candidate_ids
                .iter()
                .map(|id| format!("'{id}'::uuid"))
                .collect::<Vec<_>>()
                .join(",");

            let query = format!(
                "SELECT src_id, dst_id, association_type, weight FROM ghola.associations \
                 WHERE src_id IN ({ids}) AND dst_id IN ({ids})",
                ids = id_list,
            );

            let rows = client
                .select(&query, None, &[])
                .expect("failed to query associations for boost");

            let mut boosts: HashMap<String, f64> = HashMap::new();
            for row in rows {
                let src: pgrx::Uuid = row.get(1).expect("err").expect("null src_id");
                let dst: pgrx::Uuid = row.get(2).expect("err").expect("null dst_id");
                let assoc_type: String = row.get::<String>(3).expect("err").expect("null type");
                let weight: f64 = row.get(4).expect("err").expect("null weight");

                // Type-aware boost scaling:
                //   hebbian:     full weight (1.0×)
                //   supports:    moderate boost (0.5×)
                //   session:     mild boost (0.3×)
                //   contradicts: negative boost (-0.5×)
                //   supersedes:  no scoring contribution (archived mnemes excluded)
                let scaled = match assoc_type.as_str() {
                    "hebbian" => weight,
                    "supports" => weight * 0.5,
                    "session" => weight * 0.3,
                    "contradicts" => -weight * 0.5,
                    _ => 0.0, // supersedes and unknown types don't contribute
                };

                *boosts.entry(src.to_string()).or_insert(0.0) += scaled;
                *boosts.entry(dst.to_string()).or_insert(0.0) += scaled;
            }
            boosts
        })
    } else {
        HashMap::new()
    };

    // Step 3: Compute composite scores
    // normalizer = 1 + softplus(0) ensures temporal_weight is well-scaled
    let softplus_0 = softplus_inner(0.0);
    let normalizer = 1.0 + softplus_0;

    // Session ID for session boost (convert to string for comparison)
    let session_str = filter_session_id.map(|s| s.to_string());

    let mut scored: Vec<ScoredCandidate> = candidates
        .into_iter()
        .map(|c| {
            let content_match = w_semantic * c.cosine_sim + w_fts * c.fts_rank.tanh();

            // Type-aware ACT-R decay: working memories decay twice as fast
            let effective_decay = match c.memory_type.as_str() {
                "working" => (w_actr_decay * 2.0).min(1.5),
                _ => w_actr_decay,
            };
            let actr_val = actr_activation_inner(c.access_count, c.age_days, effective_decay);

            let mut heb_boost = hebbian_boosts
                .get(&c.id.to_string())
                .copied()
                .unwrap_or(0.0);

            // Session boost: mnemes from the requested session get a mild boost
            if let Some(ref sid) = session_str {
                if c.candidate_session_id.as_deref() == Some(sid.as_str()) {
                    heb_boost += 0.3;
                }
            }

            let temporal_weight =
                softplus_inner(actr_val + w_hebbian_scale * heb_boost) / normalizer;
            let score = content_match * temporal_weight * c.confidence;

            ScoredCandidate {
                id: c.id,
                score,
                content_match,
                activation: actr_val,
                hebbian_boost: heb_boost,
                confidence: c.confidence,
                concept: c.concept,
                content: c.content,
            }
        })
        .collect();

    // Step 4: Sort by composite score descending and truncate to limit_n
    scored.sort_by(|a, b| {
        b.score
            .partial_cmp(&a.score)
            .unwrap_or(std::cmp::Ordering::Equal)
    });
    scored.truncate(limit_n as usize);

    // Step 5: Enqueue co-activation event for the returned results
    enqueue_co_activation(&workspace_id, &scored);

    // Step 6: Convert to TableIterator output
    let results: Vec<_> = scored
        .into_iter()
        .map(|s| {
            (
                s.id,
                s.score,
                s.content_match,
                s.activation,
                s.hebbian_boost,
                s.confidence,
                s.concept,
                s.content,
            )
        })
        .collect();

    TableIterator::new(results)
}

/// Enqueue a co-activation event for the returned recall results.
/// Records the mneme IDs and their composite scores.
fn enqueue_co_activation(workspace_id: &pgrx::Uuid, results: &[ScoredCandidate]) {
    if results.is_empty() {
        // Still enqueue with empty arrays per spec
        Spi::run(&format!(
            "INSERT INTO ghola.co_activation_queue (workspace_id, mneme_ids, scores) \
             VALUES ('{ws}', ARRAY[]::uuid[], ARRAY[]::float8[])",
            ws = workspace_id,
        ))
        .expect("failed to enqueue empty co-activation event");
    } else {
        let ids: Vec<String> = results.iter().map(|r| format!("'{}'", r.id)).collect();
        let scores: Vec<String> = results.iter().map(|r| format!("{}", r.score)).collect();

        Spi::run(&format!(
            "INSERT INTO ghola.co_activation_queue (workspace_id, mneme_ids, scores) \
             VALUES ('{ws}', ARRAY[{ids}]::uuid[], ARRAY[{scores}]::float8[])",
            ws = workspace_id,
            ids = ids.join(","),
            scores = scores.join(","),
        ))
        .expect("failed to enqueue co-activation event");
    }
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(any(test, feature = "pg_test"))]
#[pg_schema]
mod tests {
    use pgrx::prelude::*;

    /// Create an embedding with 1.0 in dims [start..start+span) and 0.0 elsewhere.
    /// Produces vectors with known cosine similarity based on dimensional overlap.
    fn directional_embedding(start: usize, span: usize) -> String {
        let mut elements = vec!["0".to_string(); 768];
        for i in start..(start + span).min(768) {
            elements[i] = "1".to_string();
        }
        format!("[{}]", elements.join(","))
    }

    /// Helper: create pgvector extension and insert test mnemes with directional
    /// embeddings that have known cosine similarities to each other.
    ///
    /// Layout (768 dims):
    ///   m1 "kubernetes": dims 0..128   — closest to query
    ///   m2 "docker":     dims 64..192  — partial overlap with query (cos ≈ 0.5)
    ///   m3 "helm":       dims 192..320 — orthogonal to query (cos = 0.0)
    ///   query:           dims 0..128   — same direction as m1
    fn setup_recall_test_data() -> (String, Vec<String>) {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector")
            .expect("failed to create vector extension");

        let ws_id = "00000000-0000-0000-0000-000000000099";

        let m1 = insert_mneme_with_embedding(
            ws_id, "kubernetes", "pod scheduling and orchestration",
            &directional_embedding(0, 128),
        );
        let m2 = insert_mneme_with_embedding(
            ws_id, "docker", "container runtime engine",
            &directional_embedding(64, 128),
        );
        let m3 = insert_mneme_with_embedding(
            ws_id, "helm", "chart deployment and packaging",
            &directional_embedding(192, 128),
        );

        (ws_id.to_string(), vec![m1, m2, m3])
    }

    fn insert_mneme_with_embedding(ws_id: &str, concept: &str, content: &str, embedding: &str) -> String {
        Spi::get_one::<String>(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding) \
             VALUES ('{ws_id}', '{concept}', '{content}', \
             '{embedding}'::vector) \
             RETURNING id::text"
        ))
        .expect("insert failed")
        .expect("null id")
    }

    fn make_query_embedding() -> String {
        // Same direction as m1 in setup_recall_test_data
        directional_embedding(0, 128)
    }

    // ── Basic recall functionality ──

    #[pg_test]
    fn test_recall_returns_results() {
        let (ws_id, _mneme_ids) = setup_recall_test_data();
        let emb = make_query_embedding();

        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.recall(\
                '{ws_id}'::uuid, \
                'kubernetes pod scheduling', \
                '{emb}'::vector, \
                10, 0.0, NULL)"
        ))
        .expect("query failed")
        .expect("null count");

        assert!(count > 0, "recall should return at least one result");
        assert!(count <= 3, "recall should return at most 3 results (we inserted 3)");
    }

    #[pg_test]
    fn test_recall_result_fields_populated() {
        let (ws_id, _mneme_ids) = setup_recall_test_data();
        let emb = make_query_embedding();

        // Check that all fields of recall_result are accessible
        let score = Spi::get_one::<f64>(&format!(
            "SELECT (r).score FROM ghola.recall(\
                '{ws_id}'::uuid, 'kubernetes', '{emb}'::vector, \
                10, 0.0, NULL) AS r LIMIT 1"
        ))
        .expect("query failed")
        .expect("null score");

        assert!(score > 0.0, "score should be positive, got {score}");
    }

    #[pg_test]
    fn test_recall_respects_limit() {
        let (ws_id, _) = setup_recall_test_data();
        let emb = make_query_embedding();

        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.recall(\
                '{ws_id}'::uuid, 'kubernetes', '{emb}'::vector, \
                2, 0.0, NULL)"
        ))
        .expect("query failed")
        .expect("null count");

        assert!(count <= 2, "recall should respect limit_n=2, got {count}");
    }

    // ── Confidence filtering ──

    #[pg_test]
    fn test_recall_confidence_filter() {
        let (ws_id, mneme_ids) = setup_recall_test_data();
        let emb = make_query_embedding();

        // Set one mneme to high confidence, others to low
        Spi::run(&format!(
            "UPDATE ghola.mnemes SET confidence = 0.9 WHERE id = '{}'::uuid",
            mneme_ids[0]
        ))
        .expect("update failed");
        Spi::run(&format!(
            "UPDATE ghola.mnemes SET confidence = 0.3 WHERE id = '{}'::uuid",
            mneme_ids[1]
        ))
        .expect("update failed");
        Spi::run(&format!(
            "UPDATE ghola.mnemes SET confidence = 0.2 WHERE id = '{}'::uuid",
            mneme_ids[2]
        ))
        .expect("update failed");

        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.recall(\
                '{ws_id}'::uuid, 'kubernetes', '{emb}'::vector, \
                10, 0.7, NULL)"
        ))
        .expect("query failed")
        .expect("null count");

        assert_eq!(
            count, 1,
            "only 1 mneme has confidence >= 0.7, got {count}"
        );
    }

    // ── Workspace isolation ──

    #[pg_test]
    fn test_recall_workspace_isolation() {
        let (_ws_id, _) = setup_recall_test_data();
        let emb = make_query_embedding();

        // Query with a different workspace_id should return nothing
        let other_ws = "00000000-0000-0000-0000-000000000042";
        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.recall(\
                '{other_ws}'::uuid, 'kubernetes', '{emb}'::vector, \
                10, 0.0, NULL)"
        ))
        .expect("query failed")
        .expect("null count");

        assert_eq!(
            count, 0,
            "different workspace should return 0 results, got {count}"
        );
    }

    // ── Co-activation enqueuing ──

    #[pg_test]
    fn test_recall_enqueues_co_activation() {
        let (ws_id, _) = setup_recall_test_data();
        let emb = make_query_embedding();

        // Clear any existing queue entries
        Spi::run("DELETE FROM ghola.co_activation_queue").expect("delete failed");

        // Execute recall
        Spi::run(&format!(
            "SELECT * FROM ghola.recall(\
                '{ws_id}'::uuid, 'kubernetes', '{emb}'::vector, \
                10, 0.0, NULL)"
        ))
        .expect("recall failed");

        let queue_count =
            Spi::get_one::<i64>("SELECT count(*) FROM ghola.co_activation_queue")
                .expect("query failed")
                .expect("null count");

        assert_eq!(
            queue_count, 1,
            "recall should enqueue exactly one co-activation event, got {queue_count}"
        );
    }

    #[pg_test]
    fn test_recall_empty_results_still_enqueues() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector")
            .expect("failed to create vector extension");

        let ws_id = "00000000-0000-0000-0000-000000000077";
        let emb = make_query_embedding();

        Spi::run("DELETE FROM ghola.co_activation_queue").expect("delete failed");

        // No mnemes in this workspace, should return empty but still enqueue
        Spi::run(&format!(
            "SELECT * FROM ghola.recall(\
                '{ws_id}'::uuid, 'nonexistent topic', '{emb}'::vector, \
                10, 0.0, NULL)"
        ))
        .expect("recall failed");

        let queue_count =
            Spi::get_one::<i64>("SELECT count(*) FROM ghola.co_activation_queue")
                .expect("query failed")
                .expect("null count");

        assert_eq!(
            queue_count, 1,
            "empty recall should still enqueue co-activation event"
        );
    }

    // ── Custom weights change scores ──

    #[pg_test]
    fn test_recall_custom_weights_affect_scores() {
        let (ws_id, _) = setup_recall_test_data();
        let emb = make_query_embedding();

        // Score with default weights (semantic=0.6, fts=0.4)
        let score_default = Spi::get_one::<f64>(&format!(
            "SELECT (r).score FROM ghola.recall(\
                '{ws_id}'::uuid, 'kubernetes', '{emb}'::vector, \
                1, 0.0, NULL) AS r LIMIT 1"
        ))
        .expect("query failed")
        .expect("null score");

        // Score with semantic-only weights (semantic=1.0, fts=0.0)
        let score_semantic_only = Spi::get_one::<f64>(&format!(
            "SELECT (r).score FROM ghola.recall(\
                '{ws_id}'::uuid, 'kubernetes', '{emb}'::vector, \
                1, 0.0, (1.0, 0.0, 0.5, 4.0)::ghola.score_weights) AS r LIMIT 1"
        ))
        .expect("query failed")
        .expect("null score");

        // With text 'kubernetes' matching the concept exactly, FTS contributes positively.
        // Removing FTS weight should change the score.
        assert!(
            (score_default - score_semantic_only).abs() > 1e-6,
            "changing weights should change scores: default={score_default}, semantic_only={score_semantic_only}"
        );
    }

    // ── Hebbian boost effect ──

    #[pg_test]
    fn test_recall_hebbian_boost_visible() {
        let (ws_id, mneme_ids) = setup_recall_test_data();
        let emb = make_query_embedding();

        // Create a strong association between m1 and m2
        let (m1, m2) = (&mneme_ids[0], &mneme_ids[1]);
        let (src, dst) = if m1 < m2 { (m1, m2) } else { (m2, m1) };
        Spi::run(&format!(
            "INSERT INTO ghola.associations (src_id, dst_id, weight) \
             VALUES ('{src}'::uuid, '{dst}'::uuid, 0.8)"
        ))
        .expect("insert association failed");

        // Recall — both m1 and m2 should be in the candidate pool.
        // The one associated with the other should get a non-zero hebbian_boost.
        let has_boost = Spi::get_one::<bool>(&format!(
            "SELECT EXISTS( \
                SELECT 1 FROM ghola.recall( \
                    '{ws_id}'::uuid, 'kubernetes docker', '{emb}'::vector, \
                    10, 0.0, NULL) \
                WHERE hebbian_boost > 0 \
            )"
        ))
        .expect("query failed")
        .expect("null");

        assert!(
            has_boost,
            "at least one result should have hebbian_boost > 0 when associations exist"
        );
    }

    // ── recall does not mutate access_count ──

    #[pg_test]
    fn test_recall_does_not_mutate_access_count() {
        let (ws_id, mneme_ids) = setup_recall_test_data();
        let emb = make_query_embedding();

        let before = Spi::get_one::<i32>(&format!(
            "SELECT access_count FROM ghola.mnemes WHERE id = '{}'::uuid",
            mneme_ids[0]
        ))
        .expect("query failed")
        .expect("null");

        // Execute recall
        Spi::run(&format!(
            "SELECT * FROM ghola.recall(\
                '{ws_id}'::uuid, 'kubernetes', '{emb}'::vector, 10, 0.0, NULL)"
        ))
        .expect("recall failed");

        let after = Spi::get_one::<i32>(&format!(
            "SELECT access_count FROM ghola.mnemes WHERE id = '{}'::uuid",
            mneme_ids[0]
        ))
        .expect("query failed")
        .expect("null");

        assert_eq!(
            before, after,
            "recall should NOT mutate access_count (batch processing does that)"
        );
    }

    // ── Ordered by score descending ──

    #[pg_test]
    fn test_recall_ordered_by_score_desc() {
        let (ws_id, _) = setup_recall_test_data();
        let emb = make_query_embedding();

        let scores = Spi::connect(|client| {
            let rows = client
                .select(
                    &format!(
                        "SELECT (r).score FROM ghola.recall(\
                            '{ws_id}'::uuid, 'kubernetes', '{emb}'::vector, \
                            10, 0.0, NULL) AS r"
                    ),
                    None,
                    &[],
                )
                .expect("query failed");

            let mut result = Vec::new();
            for row in rows {
                let score: f64 = row.get::<f64>(1).expect("err").expect("null");
                result.push(score);
            }
            result
        });

        // Verify descending order
        for window in scores.windows(2) {
            assert!(
                window[0] >= window[1],
                "scores should be in descending order: {} >= {}",
                window[0],
                window[1]
            );
        }
    }

    // ── Ranking quality: vector similarity drives ranking ──

    #[pg_test]
    fn test_recall_ranks_by_vector_similarity() {
        // This is the core behavioral test: mnemes with higher cosine similarity
        // to the query embedding should rank higher, all else being equal.
        //
        // Setup creates 3 mnemes with known similarity to query (dims 0..128):
        //   m1 "kubernetes" (dims 0..128):  cosine ≈ 1.0  — highest
        //   m2 "docker"     (dims 64..192): cosine ≈ 0.5  — medium
        //   m3 "helm"       (dims 192..320): cosine = 0.0  — lowest
        let (ws_id, mneme_ids) = setup_recall_test_data();
        let emb = make_query_embedding();

        let ranked_ids = Spi::connect(|client| {
            let rows = client
                .select(
                    &format!(
                        "SELECT (r).mneme_id::text, (r).score, (r).content_match \
                         FROM ghola.recall( \
                             '{ws_id}'::uuid, 'query', '{emb}'::vector, \
                             10, 0.0, NULL \
                         ) AS r"
                    ),
                    None,
                    &[],
                )
                .expect("recall query failed");

            let mut results = Vec::new();
            for row in rows {
                let id: String = row.get::<String>(1).expect("err").expect("null");
                let score: f64 = row.get::<f64>(2).expect("err").expect("null");
                let cm: f64 = row.get::<f64>(3).expect("err").expect("null");
                results.push((id, score, cm));
            }
            results
        });

        assert!(
            ranked_ids.len() >= 2,
            "should return at least 2 results, got {}",
            ranked_ids.len()
        );

        // m1 (kubernetes, exact match) should be ranked first
        assert_eq!(
            ranked_ids[0].0, mneme_ids[0],
            "m1 (closest embedding) should be ranked first. \
             Got: {:?}",
            ranked_ids.iter().map(|(id, s, cm)| format!("{id}: score={s:.4}, cm={cm:.4}")).collect::<Vec<_>>()
        );

        // First result should have higher content_match than second
        assert!(
            ranked_ids[0].2 > ranked_ids[1].2,
            "closest mneme should have higher content_match: {:.4} > {:.4}",
            ranked_ids[0].2,
            ranked_ids[1].2
        );
    }

    // ── Only active mnemes returned ──

    #[pg_test]
    fn test_recall_excludes_non_active() {
        let (ws_id, mneme_ids) = setup_recall_test_data();
        let emb = make_query_embedding();

        // Archive one mneme
        Spi::run(&format!(
            "UPDATE ghola.mnemes SET state = 'archived' WHERE id = '{}'::uuid",
            mneme_ids[0]
        ))
        .expect("update failed");

        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.recall(\
                '{ws_id}'::uuid, 'kubernetes', '{emb}'::vector, \
                10, 0.0, NULL)"
        ))
        .expect("query failed")
        .expect("null count");

        // Should return at most 2 (the non-archived ones)
        assert!(
            count <= 2,
            "archived mneme should be excluded, got {count} results"
        );
    }

    // ── recall does NOT update access_count or last_access directly ──

}
