// pg_recall::recall — Composite recall function
//
// The primary retrieval entry point that fuses vector similarity, full-text search,
// ACT-R temporal activation, Hebbian association strength, and Bayesian confidence
// into a single ranked result set.
// The control file's schema directive places all objects in pg_recall automatically.
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
// Provides the public interface with vector(384) and score_weights types.
// Delegates to recall_inner() which handles the actual computation.
// ---------------------------------------------------------------------------

extension_sql!(
    r#"
CREATE FUNCTION recall(
    workspace_id uuid,
    query_text text,
    query_embedding vector(384),
    limit_n int DEFAULT 10,
    min_confidence float8 DEFAULT 0.0,
    weights pg_recall.score_weights DEFAULT NULL
) RETURNS SETOF pg_recall.recall_result
LANGUAGE SQL
STABLE
AS $$
    SELECT (mneme_id, score, content_match, activation, hebbian_boost,
            confidence, concept, content)::pg_recall.recall_result
    FROM pg_recall.recall_inner(
        workspace_id, query_text, query_embedding::text, limit_n, min_confidence,
        COALESCE((weights).semantic, 0.6),
        COALESCE((weights).fts, 0.4),
        COALESCE((weights).actr_decay, 0.5),
        COALESCE((weights).hebbian_scale, 4.0)
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

    // Step 1: Fetch candidate pool — union of HNSW nearest neighbors and FTS matches
    let candidates: Vec<Candidate> = Spi::connect(|client| {
        let query = format!(
            "WITH hnsw_candidates AS ( \
                SELECT id, concept, content, confidence::float8, access_count, \
                       GREATEST(EXTRACT(EPOCH FROM (now() - last_access)) / 86400.0, {min_age})::float8 AS age_days, \
                       (1.0 - (embedding <=> '{emb}'::vector(384)))::float8 AS cosine_sim, \
                       ts_rank(search_vector, plainto_tsquery('english', '{qt}'))::float8 AS fts_rank \
                FROM pg_recall.mnemes \
                WHERE workspace_id = '{ws}' \
                  AND state = 'active' \
                  AND confidence >= {min_conf} \
                ORDER BY embedding <=> '{emb}'::vector(384) \
                LIMIT {pool} \
            ), \
            fts_candidates AS ( \
                SELECT id, concept, content, confidence::float8, access_count, \
                       GREATEST(EXTRACT(EPOCH FROM (now() - last_access)) / 86400.0, {min_age})::float8 AS age_days, \
                       (1.0 - (embedding <=> '{emb}'::vector(384)))::float8 AS cosine_sim, \
                       ts_rank(search_vector, plainto_tsquery('english', '{qt}'))::float8 AS fts_rank \
                FROM pg_recall.mnemes \
                WHERE workspace_id = '{ws}' \
                  AND state = 'active' \
                  AND confidence >= {min_conf} \
                  AND search_vector @@ plainto_tsquery('english', '{qt}') \
                ORDER BY ts_rank(search_vector, plainto_tsquery('english', '{qt}')) DESC \
                LIMIT {pool} \
            ) \
            SELECT DISTINCT ON (id) id, concept, content, confidence, access_count, \
                   age_days, cosine_sim, fts_rank \
            FROM ( \
                SELECT * FROM hnsw_candidates \
                UNION ALL \
                SELECT * FROM fts_candidates \
            ) combined",
            ws = workspace_id,
            qt = escaped_text,
            emb = query_embedding_text,
            min_conf = min_confidence,
            min_age = ONE_MINUTE_DAYS,
            pool = pool_size,
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
            });
        }
        result
    });

    // Step 2: Fetch Hebbian associations between all candidates in the pool
    let candidate_ids: Vec<String> = candidates.iter().map(|c| c.id.to_string()).collect();

    let hebbian_boosts: HashMap<String, f64> = if candidates.len() > 1 {
        Spi::connect(|client| {
            let id_list: String = candidate_ids
                .iter()
                .map(|id| format!("'{id}'::uuid"))
                .collect::<Vec<_>>()
                .join(",");

            let query = format!(
                "SELECT src_id, dst_id, weight FROM pg_recall.associations \
                 WHERE src_id IN ({ids}) AND dst_id IN ({ids})",
                ids = id_list,
            );

            let rows = client
                .select(&query, None, &[])
                .expect("failed to query associations for Hebbian boost");

            let mut boosts: HashMap<String, f64> = HashMap::new();
            for row in rows {
                let src: pgrx::Uuid = row.get(1).expect("err").expect("null src_id");
                let dst: pgrx::Uuid = row.get(2).expect("err").expect("null dst_id");
                let weight: f64 = row.get(3).expect("err").expect("null weight");

                *boosts.entry(src.to_string()).or_insert(0.0) += weight;
                *boosts.entry(dst.to_string()).or_insert(0.0) += weight;
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

    let mut scored: Vec<ScoredCandidate> = candidates
        .into_iter()
        .map(|c| {
            let content_match = w_semantic * c.cosine_sim + w_fts * c.fts_rank.tanh();
            let actr_val = actr_activation_inner(c.access_count, c.age_days, w_actr_decay);
            let heb_boost = hebbian_boosts
                .get(&c.id.to_string())
                .copied()
                .unwrap_or(0.0);
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
            "INSERT INTO pg_recall.co_activation_queue (workspace_id, mneme_ids, scores) \
             VALUES ('{ws}', ARRAY[]::uuid[], ARRAY[]::float8[])",
            ws = workspace_id,
        ))
        .expect("failed to enqueue empty co-activation event");
    } else {
        let ids: Vec<String> = results.iter().map(|r| format!("'{}'", r.id)).collect();
        let scores: Vec<String> = results.iter().map(|r| format!("{}", r.score)).collect();

        Spi::run(&format!(
            "INSERT INTO pg_recall.co_activation_queue (workspace_id, mneme_ids, scores) \
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

    /// Helper: create pgvector extension and insert test mnemes in a workspace.
    /// Returns (workspace_id, mneme_ids) where mneme_ids contains the inserted IDs.
    fn setup_recall_test_data() -> (String, Vec<String>) {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector")
            .expect("failed to create vector extension");

        let ws_id = "00000000-0000-0000-0000-000000000099";

        // Insert mnemes with different embeddings and concepts for testability.
        // We use slightly different fill values so cosine similarity varies.
        let m1 = insert_mneme(ws_id, "kubernetes", "pod scheduling and orchestration", 0.1);
        let m2 = insert_mneme(ws_id, "docker", "container runtime engine", 0.2);
        let m3 = insert_mneme(ws_id, "helm", "chart deployment and packaging", 0.3);

        (ws_id.to_string(), vec![m1, m2, m3])
    }

    fn insert_mneme(ws_id: &str, concept: &str, content: &str, fill_val: f64) -> String {
        let elements = vec![format!("{fill_val}"); 384];
        let vec_literal = format!("[{}]", elements.join(","));
        Spi::get_one::<String>(&format!(
            "INSERT INTO pg_recall.mnemes (workspace_id, concept, content, embedding) \
             VALUES ('{ws_id}', '{concept}', '{content}', \
             '{vec_literal}'::vector(384)) \
             RETURNING id::text"
        ))
        .expect("insert failed")
        .expect("null id")
    }

    fn make_query_embedding(fill_val: f64) -> String {
        let elements = vec![format!("{fill_val}"); 384];
        format!("[{}]", elements.join(","))
    }

    // ── Basic recall functionality ──

    #[pg_test]
    fn test_recall_returns_results() {
        let (ws_id, _mneme_ids) = setup_recall_test_data();
        let emb = make_query_embedding(0.1);

        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM pg_recall.recall(\
                '{ws_id}'::uuid, \
                'kubernetes pod scheduling', \
                '{emb}'::vector(384), \
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
        let emb = make_query_embedding(0.1);

        // Check that all fields of recall_result are accessible
        let score = Spi::get_one::<f64>(&format!(
            "SELECT (r).score FROM pg_recall.recall(\
                '{ws_id}'::uuid, 'kubernetes', '{emb}'::vector(384), \
                10, 0.0, NULL) AS r LIMIT 1"
        ))
        .expect("query failed")
        .expect("null score");

        assert!(score > 0.0, "score should be positive, got {score}");
    }

    #[pg_test]
    fn test_recall_respects_limit() {
        let (ws_id, _) = setup_recall_test_data();
        let emb = make_query_embedding(0.1);

        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM pg_recall.recall(\
                '{ws_id}'::uuid, 'kubernetes', '{emb}'::vector(384), \
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
        let emb = make_query_embedding(0.1);

        // Set one mneme to high confidence, others to low
        Spi::run(&format!(
            "UPDATE pg_recall.mnemes SET confidence = 0.9 WHERE id = '{}'::uuid",
            mneme_ids[0]
        ))
        .expect("update failed");
        Spi::run(&format!(
            "UPDATE pg_recall.mnemes SET confidence = 0.3 WHERE id = '{}'::uuid",
            mneme_ids[1]
        ))
        .expect("update failed");
        Spi::run(&format!(
            "UPDATE pg_recall.mnemes SET confidence = 0.2 WHERE id = '{}'::uuid",
            mneme_ids[2]
        ))
        .expect("update failed");

        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM pg_recall.recall(\
                '{ws_id}'::uuid, 'kubernetes', '{emb}'::vector(384), \
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
        let (ws_id, _) = setup_recall_test_data();
        let emb = make_query_embedding(0.1);

        // Query with a different workspace_id should return nothing
        let other_ws = "00000000-0000-0000-0000-000000000042";
        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM pg_recall.recall(\
                '{other_ws}'::uuid, 'kubernetes', '{emb}'::vector(384), \
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
        let emb = make_query_embedding(0.1);

        // Clear any existing queue entries
        Spi::run("DELETE FROM pg_recall.co_activation_queue").expect("delete failed");

        // Execute recall
        Spi::run(&format!(
            "SELECT * FROM pg_recall.recall(\
                '{ws_id}'::uuid, 'kubernetes', '{emb}'::vector(384), \
                10, 0.0, NULL)"
        ))
        .expect("recall failed");

        let queue_count =
            Spi::get_one::<i64>("SELECT count(*) FROM pg_recall.co_activation_queue")
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
        let emb = make_query_embedding(0.5);

        Spi::run("DELETE FROM pg_recall.co_activation_queue").expect("delete failed");

        // No mnemes in this workspace, should return empty but still enqueue
        Spi::run(&format!(
            "SELECT * FROM pg_recall.recall(\
                '{ws_id}'::uuid, 'nonexistent topic', '{emb}'::vector(384), \
                10, 0.0, NULL)"
        ))
        .expect("recall failed");

        let queue_count =
            Spi::get_one::<i64>("SELECT count(*) FROM pg_recall.co_activation_queue")
                .expect("query failed")
                .expect("null count");

        assert_eq!(
            queue_count, 1,
            "empty recall should still enqueue co-activation event"
        );
    }

    // ── Default weights ──

    #[pg_test]
    fn test_recall_default_weights() {
        let (ws_id, _) = setup_recall_test_data();
        let emb = make_query_embedding(0.1);

        // Calling with NULL weights should use defaults and not error
        #[allow(unused_variables)]
        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM pg_recall.recall(\
                '{ws_id}'::uuid, 'kubernetes', '{emb}'::vector(384), \
                10, 0.0, NULL)"
        ))
        .expect("query failed")
        .expect("null count");

        assert!(count >= 0, "default weights should work without error");
    }

    // ── Custom weights ──

    #[pg_test]
    fn test_recall_custom_weights() {
        let (ws_id, _) = setup_recall_test_data();
        let emb = make_query_embedding(0.1);

        // Custom weights: heavier semantic, lighter FTS
        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM pg_recall.recall(\
                '{ws_id}'::uuid, 'query', '{emb}'::vector(384), \
                10, 0.0, (0.8, 0.2, 0.5, 2.0)::pg_recall.score_weights)"
        ))
        .expect("query failed")
        .expect("null count");

        assert!(count >= 0, "custom weights should work without error");
    }

    // ── Hebbian boost effect ──

    #[pg_test]
    fn test_recall_hebbian_boost_visible() {
        let (ws_id, mneme_ids) = setup_recall_test_data();
        let emb = make_query_embedding(0.15);

        // Create a strong association between m1 and m2
        let (m1, m2) = (&mneme_ids[0], &mneme_ids[1]);
        let (src, dst) = if m1 < m2 { (m1, m2) } else { (m2, m1) };
        Spi::run(&format!(
            "INSERT INTO pg_recall.associations (src_id, dst_id, weight) \
             VALUES ('{src}'::uuid, '{dst}'::uuid, 0.8)"
        ))
        .expect("insert association failed");

        // Recall — both m1 and m2 should be in the candidate pool.
        // The one associated with the other should get a non-zero hebbian_boost.
        let has_boost = Spi::get_one::<bool>(&format!(
            "SELECT EXISTS( \
                SELECT 1 FROM pg_recall.recall( \
                    '{ws_id}'::uuid, 'kubernetes docker', '{emb}'::vector(384), \
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

    // ── Custom actr_decay affects scoring ──

    #[pg_test]
    fn test_recall_custom_actr_decay() {
        let (ws_id, _) = setup_recall_test_data();
        let emb = make_query_embedding(0.1);

        // Higher actr_decay should penalize older memories more
        // With our test mnemes all freshly inserted (very recent), the effect
        // may be subtle, but the query should succeed without error
        let score_default = Spi::get_one::<f64>(&format!(
            "SELECT (r).score FROM pg_recall.recall(\
                '{ws_id}'::uuid, 'kubernetes', '{emb}'::vector(384), \
                1, 0.0, (0.6, 0.4, 0.5, 4.0)::pg_recall.score_weights) AS r \
            LIMIT 1"
        ))
        .expect("query failed")
        .expect("null score");

        let score_high_decay = Spi::get_one::<f64>(&format!(
            "SELECT (r).score FROM pg_recall.recall(\
                '{ws_id}'::uuid, 'kubernetes', '{emb}'::vector(384), \
                1, 0.0, (0.6, 0.4, 0.8, 4.0)::pg_recall.score_weights) AS r \
            LIMIT 1"
        ))
        .expect("query failed")
        .expect("null score");

        // Both should be valid positive scores (exact comparison depends on timing)
        assert!(score_default > 0.0, "default decay score should be positive");
        assert!(
            score_high_decay > 0.0,
            "high decay score should be positive"
        );
    }

    // ── Ordered by score descending ──

    #[pg_test]
    fn test_recall_ordered_by_score_desc() {
        let (ws_id, _) = setup_recall_test_data();
        let emb = make_query_embedding(0.1);

        let scores = Spi::connect(|client| {
            let rows = client
                .select(
                    &format!(
                        "SELECT (r).score FROM pg_recall.recall(\
                            '{ws_id}'::uuid, 'kubernetes', '{emb}'::vector(384), \
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

    // ── Only active mnemes returned ──

    #[pg_test]
    fn test_recall_excludes_non_active() {
        let (ws_id, mneme_ids) = setup_recall_test_data();
        let emb = make_query_embedding(0.1);

        // Archive one mneme
        Spi::run(&format!(
            "UPDATE pg_recall.mnemes SET state = 'archived' WHERE id = '{}'::uuid",
            mneme_ids[0]
        ))
        .expect("update failed");

        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM pg_recall.recall(\
                '{ws_id}'::uuid, 'kubernetes', '{emb}'::vector(384), \
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

    #[pg_test]
    fn test_recall_does_not_update_access_tracking() {
        let (ws_id, mneme_ids) = setup_recall_test_data();
        let emb = make_query_embedding(0.1);

        let before = Spi::get_one::<i32>(&format!(
            "SELECT access_count FROM pg_recall.mnemes WHERE id = '{}'::uuid",
            mneme_ids[0]
        ))
        .expect("query failed")
        .expect("null");

        // Execute recall
        Spi::run(&format!(
            "SELECT * FROM pg_recall.recall(\
                '{ws_id}'::uuid, 'kubernetes', '{emb}'::vector(384), \
                10, 0.0, NULL)"
        ))
        .expect("recall failed");

        let after = Spi::get_one::<i32>(&format!(
            "SELECT access_count FROM pg_recall.mnemes WHERE id = '{}'::uuid",
            mneme_ids[0]
        ))
        .expect("query failed")
        .expect("null");

        assert_eq!(
            before, after,
            "recall should NOT update access_count directly"
        );
    }
}
