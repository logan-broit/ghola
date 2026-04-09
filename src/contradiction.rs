// pg_ghola::contradiction — Contradiction detection and flagging
//
// Detects when a new mneme potentially contradicts existing mnemes in the same
// workspace based on high semantic similarity. Surfaces candidates for review
// without autonomously mutating stored memories. Resolution is always explicit.
//
// Detection uses HNSW nearest-neighbor search to find similar active mnemes,
// then flags pairs above a configurable cosine similarity threshold.
//
// On confirmed contradiction:
// - The newer mneme (mneme_a) receives a Bayesian confidence penalty (evidence=0.10)
// - Any Hebbian association between the pair is weakened (weight *= 0.1)
// This enforces burden of proof on new information.

use pgrx::prelude::*;

use crate::scoring::bayesian_update_inner;

// ---------------------------------------------------------------------------
// check_contradictions: read-only candidate detection
// ---------------------------------------------------------------------------

/// Check a specific mneme against all other active mnemes in the same workspace.
/// Returns candidate pairs above the similarity threshold. Does NOT insert into
/// the candidates table — this is a read-only query for inspection.
#[pg_extern]
fn check_contradictions(
    mneme_id: pgrx::Uuid,
    similarity_threshold: default!(f64, 0.85),
) -> TableIterator<
    'static,
    (
        name!(mneme_b, pgrx::Uuid),
        name!(similarity, f64),
        name!(concept_overlap, bool),
    ),
> {
    let results = Spi::connect(|client| {
        // Get the source mneme's workspace, embedding, concept, and content
        let source = client
            .select(
                &format!(
                    "SELECT workspace_id::text, embedding::text, concept, content \
                     FROM ghola.mnemes WHERE id = '{mneme_id}'"
                ),
                None,
                &[],
            )
            .expect("failed to read source mneme");

        let source_row = source.into_iter().next();
        if source_row.is_none() {
            pgrx::error!("mneme_id not found: {mneme_id}");
        }
        let source_row = source_row.unwrap();
        let workspace_id: String = source_row.get::<String>(1).expect("err").expect("null ws");
        let embedding: String = source_row.get::<String>(2).expect("err").expect("null emb");
        let source_concept: String = source_row.get::<String>(3).expect("err").expect("null concept");
        let source_content: String = source_row.get::<String>(4).expect("err").expect("null content");

        // Find similar active mnemes via HNSW cosine similarity
        // 1 - cosine_distance = cosine_similarity
        let candidates = client
            .select(
                &format!(
                    "SELECT id, concept, content, \
                         1 - (embedding <=> '{embedding}'::vector) AS similarity \
                     FROM ghola.mnemes \
                     WHERE workspace_id = '{workspace_id}'::uuid \
                       AND id != '{mneme_id}'::uuid \
                       AND state = 'active' \
                     ORDER BY embedding <=> '{embedding}'::vector \
                     LIMIT 50"
                ),
                None,
                &[],
            )
            .expect("failed to search for similar mnemes");

        let mut results = Vec::new();
        for row in candidates {
            let candidate_id: pgrx::Uuid = row.get::<pgrx::Uuid>(1).expect("err").expect("null id");
            let candidate_concept: String = row.get::<String>(2).expect("err").expect("null concept");
            let candidate_content: String = row.get::<String>(3).expect("err").expect("null content");
            let sim: f64 = row.get::<f64>(4).expect("err").expect("null sim");

            // Filter by threshold
            if sim < similarity_threshold {
                continue;
            }

            // Skip exact duplicate content (not a contradiction)
            if candidate_content == source_content {
                continue;
            }

            let concept_overlap = candidate_concept == source_concept;
            results.push((candidate_id, sim, concept_overlap));
        }
        results
    });

    TableIterator::new(results)
}

// ---------------------------------------------------------------------------
// flag_contradictions: detection + insert into candidates table
// ---------------------------------------------------------------------------

/// Same detection logic as check_contradictions, but inserts results into the
/// contradiction_candidates table. Returns the number of candidates flagged.
/// Skips pairs that are already flagged (respects UNIQUE constraint).
#[pg_extern]
fn flag_contradictions(
    mneme_id: pgrx::Uuid,
    similarity_threshold: default!(f64, 0.85),
) -> i64 {
    Spi::connect_mut(|client| {
        // Get the source mneme's workspace, embedding, concept, and content
        let source = client
            .select(
                &format!(
                    "SELECT workspace_id::text, embedding::text, concept, content \
                     FROM ghola.mnemes WHERE id = '{mneme_id}'"
                ),
                None,
                &[],
            )
            .expect("failed to read source mneme");

        let source_row = source.into_iter().next();
        if source_row.is_none() {
            pgrx::error!("mneme_id not found: {mneme_id}");
        }
        let source_row = source_row.unwrap();
        let workspace_id: String = source_row.get::<String>(1).expect("err").expect("null ws");
        let embedding: String = source_row.get::<String>(2).expect("err").expect("null emb");
        let source_concept: String = source_row.get::<String>(3).expect("err").expect("null concept");
        let source_content: String = source_row.get::<String>(4).expect("err").expect("null content");

        // Find similar active mnemes
        let candidates = client
            .select(
                &format!(
                    "SELECT id, concept, content, \
                         1 - (embedding <=> '{embedding}'::vector) AS similarity \
                     FROM ghola.mnemes \
                     WHERE workspace_id = '{workspace_id}'::uuid \
                       AND id != '{mneme_id}'::uuid \
                       AND state = 'active' \
                     ORDER BY embedding <=> '{embedding}'::vector \
                     LIMIT 50"
                ),
                None,
                &[],
            )
            .expect("failed to search for similar mnemes");

        struct Candidate {
            id: pgrx::Uuid,
            similarity: f64,
            concept_overlap: bool,
        }

        let mut to_flag: Vec<Candidate> = Vec::new();
        for row in candidates {
            let candidate_id: pgrx::Uuid = row.get::<pgrx::Uuid>(1).expect("err").expect("null id");
            let candidate_concept: String = row.get::<String>(2).expect("err").expect("null concept");
            let candidate_content: String = row.get::<String>(3).expect("err").expect("null content");
            let sim: f64 = row.get::<f64>(4).expect("err").expect("null sim");

            if sim < similarity_threshold {
                continue;
            }

            // Skip exact duplicate content
            if candidate_content == source_content {
                continue;
            }

            to_flag.push(Candidate {
                id: candidate_id,
                similarity: sim,
                concept_overlap: candidate_concept == source_concept,
            });
        }

        let mut flagged: i64 = 0;
        for candidate in &to_flag {
            // Use INSERT ... ON CONFLICT DO NOTHING with RETURNING to detect actual inserts
            let inserted = client
                .select(
                    &format!(
                        "WITH ins AS ( \
                             INSERT INTO ghola.contradiction_candidates \
                                 (workspace_id, mneme_a, mneme_b, similarity, concept_overlap) \
                             VALUES ('{workspace_id}'::uuid, '{mneme_id}'::uuid, '{cid}'::uuid, \
                                     {sim}, {overlap}) \
                             ON CONFLICT (mneme_a, mneme_b) DO NOTHING \
                             RETURNING 1 \
                         ) SELECT count(*) FROM ins",
                        cid = candidate.id,
                        sim = candidate.similarity,
                        overlap = candidate.concept_overlap,
                    ),
                    None,
                    &[],
                )
                .expect("failed to flag contradiction candidate");

            if let Some(row) = inserted.into_iter().next() {
                let count: i64 = row.get::<i64>(1).unwrap_or(Some(0)).unwrap_or(0);
                flagged += count;
            }
        }

        flagged
    })
}

// ---------------------------------------------------------------------------
// resolve_contradiction: confirm or dismiss with appropriate side effects
// ---------------------------------------------------------------------------

/// Resolve a contradiction candidate.
///
/// - 'confirmed': penalizes the newer mneme (mneme_a) with bayesian_update(conf, 0.10)
///   and weakens any Hebbian association between the pair (weight *= 0.1).
/// - 'dismissed': closes the candidate with no side effects.
#[pg_extern]
fn resolve_contradiction(candidate_id: i64, resolution: &str) -> &'static str {
    match resolution {
        "confirmed" | "dismissed" => {}
        _ => pgrx::error!("resolution must be 'confirmed' or 'dismissed', got: {resolution}"),
    }

    Spi::connect_mut(|client| {
        // Fetch the candidate
        let row = client
            .select(
                &format!(
                    "SELECT mneme_a::text, mneme_b::text, status \
                     FROM ghola.contradiction_candidates \
                     WHERE id = {candidate_id}"
                ),
                None,
                &[],
            )
            .expect("failed to read candidate");

        let candidate = row.into_iter().next();
        if candidate.is_none() {
            pgrx::error!("contradiction candidate not found: {candidate_id}");
        }
        let candidate = candidate.unwrap();
        let mneme_a: String = candidate.get::<String>(1).expect("err").expect("null mneme_a");
        let mneme_b: String = candidate.get::<String>(2).expect("err").expect("null mneme_b");
        let status: String = candidate.get::<String>(3).expect("err").expect("null status");

        if status != "pending" {
            pgrx::error!("candidate {candidate_id} is already resolved (status: {status})");
        }

        if resolution == "confirmed" {
            // Penalize the newer mneme (mneme_a) — burden of proof
            let prior_row = client
                .select(
                    &format!(
                        "SELECT confidence FROM ghola.mnemes \
                         WHERE id = '{mneme_a}'::uuid FOR UPDATE"
                    ),
                    None,
                    &[],
                )
                .expect("failed to read mneme_a confidence");

            if let Some(r) = prior_row.into_iter().next() {
                let prior: f64 = r.get::<f64>(1).expect("err").expect("null confidence");
                let posterior = bayesian_update_inner(prior, 0.10);

                client
                    .update(
                        &format!(
                            "UPDATE ghola.mnemes SET confidence = {posterior} \
                             WHERE id = '{mneme_a}'::uuid"
                        ),
                        None,
                        &[],
                    )
                    .expect("failed to update mneme_a confidence");
            }

            // Weaken any Hebbian association between the pair (weight *= 0.1)
            // Handle both orderings since hebbian associations use canonical src < dst
            client
                .update(
                    &format!(
                        "UPDATE ghola.associations \
                         SET weight = weight * 0.1, updated_at = now() \
                         WHERE (src_id = LEAST('{mneme_a}'::uuid, '{mneme_b}'::uuid) \
                            AND dst_id = GREATEST('{mneme_a}'::uuid, '{mneme_b}'::uuid) \
                            AND association_type = 'hebbian')"
                    ),
                    None,
                    &[],
                )
                .expect("failed to weaken association");

            // Create a 'contradicts' association (directional: newer → older)
            client
                .update(
                    &format!(
                        "INSERT INTO ghola.associations \
                         (src_id, dst_id, association_type, weight, co_activations, updated_at) \
                         VALUES ('{mneme_a}'::uuid, '{mneme_b}'::uuid, 'contradicts', 1.0, 1, now()) \
                         ON CONFLICT (src_id, dst_id, association_type) DO UPDATE SET \
                             weight = 1.0, updated_at = now()"
                    ),
                    None,
                    &[],
                )
                .expect("failed to create contradicts association");
        }

        // Update candidate status
        client
            .update(
                &format!(
                    "UPDATE ghola.contradiction_candidates \
                     SET status = '{resolution}', resolved_at = now() \
                     WHERE id = {candidate_id}"
                ),
                None,
                &[],
            )
            .expect("failed to update candidate status");
    });

    "ok"
}

// ---------------------------------------------------------------------------
// get_pending_contradictions: review query with full mneme details
// ---------------------------------------------------------------------------

/// Returns pending contradiction candidates with full mneme details for review.
/// Ordered by similarity descending (highest similarity = most likely contradiction).
#[pg_extern]
fn get_pending_contradictions(
    workspace_id: pgrx::Uuid,
    limit_n: default!(i32, 50),
) -> TableIterator<
    'static,
    (
        name!(candidate_id, i64),
        name!(similarity, f64),
        name!(concept_overlap, bool),
        name!(concept_a, String),
        name!(content_a, String),
        name!(confidence_a, f64),
        name!(concept_b, String),
        name!(content_b, String),
        name!(confidence_b, f64),
        name!(created_at, pgrx::datum::TimestampWithTimeZone),
    ),
> {
    let results = Spi::connect(|client| {
        let rows = client
            .select(
                &format!(
                    "SELECT cc.id, cc.similarity, cc.concept_overlap, \
                         a.concept, a.content, a.confidence, \
                         b.concept, b.content, b.confidence, \
                         cc.created_at \
                     FROM ghola.contradiction_candidates cc \
                     JOIN ghola.mnemes a ON cc.mneme_a = a.id \
                     JOIN ghola.mnemes b ON cc.mneme_b = b.id \
                     WHERE cc.workspace_id = '{workspace_id}'::uuid \
                       AND cc.status = 'pending' \
                     ORDER BY cc.similarity DESC \
                     LIMIT {limit_n}"
                ),
                None,
                &[],
            )
            .expect("failed to query pending contradictions");

        let mut results = Vec::new();
        for row in rows {
            results.push((
                row.get::<i64>(1).expect("err").expect("null id"),
                row.get::<f64>(2).expect("err").expect("null sim"),
                row.get::<bool>(3).expect("err").expect("null overlap"),
                row.get::<String>(4).expect("err").expect("null concept_a"),
                row.get::<String>(5).expect("err").expect("null content_a"),
                row.get::<f64>(6).expect("err").expect("null conf_a"),
                row.get::<String>(7).expect("err").expect("null concept_b"),
                row.get::<String>(8).expect("err").expect("null content_b"),
                row.get::<f64>(9).expect("err").expect("null conf_b"),
                row.get::<pgrx::datum::TimestampWithTimeZone>(10).expect("err").expect("null created"),
            ));
        }
        results
    });

    TableIterator::new(results)
}

// ---------------------------------------------------------------------------
// scan_workspace_contradictions: bulk scan for existing contradictions
// ---------------------------------------------------------------------------

/// Checks all active mnemes in a workspace against each other for contradiction
/// candidates. Returns the number of new candidates flagged.
///
/// This is an expensive operation — use infrequently for bulk review.
#[pg_extern]
fn scan_workspace_contradictions(
    workspace_id: pgrx::Uuid,
    similarity_threshold: default!(f64, 0.85),
) -> i64 {
    // Get all active mneme IDs in the workspace
    let mneme_ids = Spi::connect(|client| {
        let rows = client
            .select(
                &format!(
                    "SELECT id::text FROM ghola.mnemes \
                     WHERE workspace_id = '{workspace_id}'::uuid \
                       AND state = 'active' \
                     ORDER BY created_at"
                ),
                None,
                &[],
            )
            .expect("failed to list workspace mnemes");

        let mut ids = Vec::new();
        for row in rows {
            ids.push(row.get::<String>(1).expect("err").expect("null id"));
        }
        ids
    });

    let mut total_flagged: i64 = 0;
    for id_str in &mneme_ids {
        let flagged = Spi::get_one::<i64>(&format!(
            "SELECT ghola.flag_contradictions('{id_str}'::uuid, {similarity_threshold})"
        ))
        .unwrap_or(Some(0))
        .unwrap_or(0);
        total_flagged += flagged;
    }

    total_flagged
}

// ---------------------------------------------------------------------------
// Insert trigger: automatically flag contradictions on new mneme
// ---------------------------------------------------------------------------

extension_sql!(
    r#"
CREATE OR REPLACE FUNCTION contradiction_check_trigger()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO @extschema@.contradiction_queue (workspace_id, mneme_id)
    VALUES (NEW.workspace_id, NEW.id);
    RETURN NEW;
END;
$$;

CREATE TRIGGER mneme_contradiction_check
    AFTER INSERT ON mnemes
    FOR EACH ROW
    EXECUTE FUNCTION contradiction_check_trigger();
"#,
    name = "create_contradiction_trigger",
    requires = [
        "create_mnemes_table",
        "create_contradiction_candidates_table",
        "create_contradiction_queue_table",
    ],
);

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(any(test, feature = "pg_test"))]
#[pg_schema]
mod tests {
    use pgrx::prelude::*;

    fn embedding(fill: f64) -> String {
        let elements = vec![format!("{fill}"); 768];
        format!("[{}]", elements.join(","))
    }

    /// Create an embedding with 1.0 in dimensions [start..start+span) and 0.0 elsewhere.
    /// Useful for creating orthogonal vectors that have low cosine similarity.
    fn directional_embedding(start: usize, span: usize) -> String {
        let mut elements = vec!["0".to_string(); 768];
        for i in start..(start + span).min(768) {
            elements[i] = "1".to_string();
        }
        format!("[{}]", elements.join(","))
    }

    fn insert_mneme_no_trigger(ws: &str, concept: &str, content: &str, fill: f64) -> String {
        // Disable the trigger temporarily to control when detection runs
        Spi::run(
            "ALTER TABLE ghola.mnemes DISABLE TRIGGER mneme_contradiction_check"
        ).expect("failed to disable trigger");

        let emb = embedding(fill);
        let id = Spi::get_one::<String>(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding) \
             VALUES ('{ws}', '{concept}', '{content}', '{emb}'::vector) \
             RETURNING id::text"
        ))
        .expect("insert failed")
        .expect("null id");

        Spi::run(
            "ALTER TABLE ghola.mnemes ENABLE TRIGGER mneme_contradiction_check"
        ).expect("failed to enable trigger");

        id
    }

    #[pg_test]
    fn test_contradiction_candidates_table_exists() {
        let count = Spi::get_one::<i64>(
            "SELECT count(*) FROM information_schema.tables \
             WHERE table_schema = 'pg_ghola' AND table_name = 'contradiction_candidates'",
        )
        .expect("query failed")
        .expect("null");
        assert_eq!(count, 1);
    }

    #[pg_test]
    fn test_contradiction_types_exist() {
        for type_name in &["contradiction_candidate_result", "contradiction_detail"] {
            let exists = Spi::get_one::<bool>(&format!(
                "SELECT EXISTS(
                    SELECT 1 FROM pg_type t
                    JOIN pg_namespace n ON t.typnamespace = n.oid
                    WHERE n.nspname = 'pg_ghola' AND t.typname = '{type_name}'
                )"
            ))
            .unwrap()
            .unwrap();
            assert!(exists, "{type_name} type should exist");
        }
    }

    #[pg_test]
    fn test_check_contradictions_finds_similar() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup failed");

        let ws = "c1000000-0000-0000-0000-000000000001";
        // Two mnemes with very similar embeddings but different content
        let _m1 = insert_mneme_no_trigger(ws, "python version", "Python 3.8 is the latest", 0.5);
        let m2 = insert_mneme_no_trigger(ws, "python version", "Python 3.12 is the latest", 0.5);

        // Identical embeddings = similarity 1.0, should be found
        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.check_contradictions('{m2}'::uuid, 0.85)"
        ))
        .expect("query failed")
        .expect("null");

        assert!(count >= 1, "should find at least one contradiction candidate");
    }

    #[pg_test]
    fn test_check_contradictions_skips_exact_duplicates() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup failed");

        let ws = "c2000000-0000-0000-0000-000000000001";
        // Same content = not a contradiction
        let _m1 = insert_mneme_no_trigger(ws, "fact", "the sky is blue", 0.5);
        let m2 = insert_mneme_no_trigger(ws, "fact", "the sky is blue", 0.5);

        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.check_contradictions('{m2}'::uuid, 0.85)"
        ))
        .expect("query failed")
        .expect("null");

        assert_eq!(count, 0, "exact duplicate content should not be flagged");
    }

    #[pg_test]
    fn test_check_contradictions_skips_dissimilar() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup failed");

        let ws = "c3000000-0000-0000-0000-000000000001";
        // Orthogonal embeddings — cosine similarity ≈ 0
        let emb_a = directional_embedding(0, 50);    // dims 0..50
        let emb_b = directional_embedding(200, 50);  // dims 200..250

        Spi::run(
            "ALTER TABLE ghola.mnemes DISABLE TRIGGER mneme_contradiction_check"
        ).expect("disable trigger");

        let _m1 = Spi::get_one::<String>(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding) \
             VALUES ('{ws}', 'topic a', 'content about dogs', '{emb_a}'::vector) \
             RETURNING id::text"
        )).expect("insert failed").expect("null");

        let m2 = Spi::get_one::<String>(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding) \
             VALUES ('{ws}', 'topic b', 'content about quantum physics', '{emb_b}'::vector) \
             RETURNING id::text"
        )).expect("insert failed").expect("null");

        Spi::run(
            "ALTER TABLE ghola.mnemes ENABLE TRIGGER mneme_contradiction_check"
        ).expect("enable trigger");

        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.check_contradictions('{m2}'::uuid, 0.85)"
        ))
        .expect("query failed")
        .expect("null");

        assert_eq!(count, 0, "dissimilar mnemes should not be flagged");
    }

    #[pg_test]
    fn test_flag_contradictions_inserts_candidates() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup failed");

        let ws = "c4000000-0000-0000-0000-000000000001";
        let _m1 = insert_mneme_no_trigger(ws, "python", "Python 3.8 is latest", 0.5);
        let m2 = insert_mneme_no_trigger(ws, "python", "Python 3.12 is latest", 0.5);

        let flagged = Spi::get_one::<i64>(&format!(
            "SELECT ghola.flag_contradictions('{m2}'::uuid, 0.85)"
        ))
        .expect("query failed")
        .expect("null");

        assert!(flagged >= 1, "should flag at least one candidate");

        // Verify it's in the table
        let count = Spi::get_one::<i64>(
            "SELECT count(*) FROM ghola.contradiction_candidates WHERE status = 'pending'",
        )
        .expect("query failed")
        .expect("null");
        assert!(count >= 1);
    }

    #[pg_test]
    fn test_flag_contradictions_no_duplicate_flagging() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup failed");

        let ws = "c5000000-0000-0000-0000-000000000001";
        let _m1 = insert_mneme_no_trigger(ws, "rust", "Rust is fast", 0.5);
        let m2 = insert_mneme_no_trigger(ws, "rust", "Rust is slow", 0.5);

        // Flag twice
        Spi::run(&format!(
            "SELECT ghola.flag_contradictions('{m2}'::uuid, 0.85)"
        )).expect("first flag failed");

        let second = Spi::get_one::<i64>(&format!(
            "SELECT ghola.flag_contradictions('{m2}'::uuid, 0.85)"
        ))
        .expect("query failed")
        .expect("null");

        assert_eq!(second, 0, "second flag should return 0 (already flagged)");
    }

    #[pg_test]
    fn test_flag_contradictions_concept_overlap() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup failed");

        let ws = "c6000000-0000-0000-0000-000000000001";
        let _m1 = insert_mneme_no_trigger(ws, "python version", "Python 3.8", 0.5);
        let m2 = insert_mneme_no_trigger(ws, "python version", "Python 3.12", 0.5);

        Spi::run(&format!(
            "SELECT ghola.flag_contradictions('{m2}'::uuid, 0.85)"
        )).expect("flag failed");

        let overlap = Spi::get_one::<bool>(
            "SELECT concept_overlap FROM ghola.contradiction_candidates LIMIT 1",
        )
        .expect("query failed")
        .expect("null");

        assert!(overlap, "same concept should set concept_overlap = true");
    }

    #[pg_test]
    fn test_resolve_confirmed_penalizes_newer() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup failed");

        let ws = "c7000000-0000-0000-0000-000000000001";
        let m1 = insert_mneme_no_trigger(ws, "fact", "old fact content", 0.5);
        let m2 = insert_mneme_no_trigger(ws, "fact", "new contradicting fact", 0.5);

        // Flag and get candidate ID
        Spi::run(&format!(
            "SELECT ghola.flag_contradictions('{m2}'::uuid, 0.85)"
        )).expect("flag failed");

        let candidate_id = Spi::get_one::<i64>(
            "SELECT id FROM ghola.contradiction_candidates LIMIT 1",
        )
        .expect("query failed")
        .expect("null");

        // Get confidence before resolution
        let conf_before = Spi::get_one::<f64>(&format!(
            "SELECT confidence FROM ghola.mnemes WHERE id = '{m2}'::uuid"
        ))
        .expect("query failed")
        .expect("null");

        // Resolve as confirmed
        Spi::run(&format!(
            "SELECT ghola.resolve_contradiction({candidate_id}, 'confirmed')"
        )).expect("resolve failed");

        // Newer mneme (m2 = mneme_a) should have reduced confidence
        let conf_after = Spi::get_one::<f64>(&format!(
            "SELECT confidence FROM ghola.mnemes WHERE id = '{m2}'::uuid"
        ))
        .expect("query failed")
        .expect("null");

        assert!(
            conf_after < conf_before,
            "confirmed contradiction should reduce newer mneme confidence: {conf_before} -> {conf_after}"
        );

        // Older mneme (m1 = mneme_b) should be unchanged
        let old_conf = Spi::get_one::<f64>(&format!(
            "SELECT confidence FROM ghola.mnemes WHERE id = '{m1}'::uuid"
        ))
        .expect("query failed")
        .expect("null");
        assert!(
            (old_conf - 0.5).abs() < 0.001,
            "older mneme confidence should be unchanged, got {old_conf}"
        );

        // Status should be confirmed
        let status = Spi::get_one::<String>(&format!(
            "SELECT status FROM ghola.contradiction_candidates WHERE id = {candidate_id}"
        ))
        .expect("query failed")
        .expect("null");
        assert_eq!(status, "confirmed");
    }

    #[pg_test]
    fn test_resolve_dismissed_no_side_effects() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup failed");

        let ws = "c8000000-0000-0000-0000-000000000001";
        let m1 = insert_mneme_no_trigger(ws, "info", "content a", 0.5);
        let m2 = insert_mneme_no_trigger(ws, "info", "content b", 0.5);

        Spi::run(&format!(
            "SELECT ghola.flag_contradictions('{m2}'::uuid, 0.85)"
        )).expect("flag failed");

        let candidate_id = Spi::get_one::<i64>(
            "SELECT id FROM ghola.contradiction_candidates LIMIT 1",
        )
        .expect("query failed")
        .expect("null");

        Spi::run(&format!(
            "SELECT ghola.resolve_contradiction({candidate_id}, 'dismissed')"
        )).expect("resolve failed");

        // Both mnemes should have original confidence
        for mid in &[&m1, &m2] {
            let conf = Spi::get_one::<f64>(&format!(
                "SELECT confidence FROM ghola.mnemes WHERE id = '{mid}'::uuid"
            ))
            .expect("query failed")
            .expect("null");
            assert!(
                (conf - 0.5).abs() < 0.001,
                "dismissed should not change confidence, got {conf}"
            );
        }

        let status = Spi::get_one::<String>(&format!(
            "SELECT status FROM ghola.contradiction_candidates WHERE id = {candidate_id}"
        ))
        .expect("query failed")
        .expect("null");
        assert_eq!(status, "dismissed");
    }

    #[pg_test]
    fn test_resolve_confirmed_weakens_association() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup failed");

        let ws = "c9000000-0000-0000-0000-000000000001";
        let m1 = insert_mneme_no_trigger(ws, "topic", "content alpha", 0.5);
        let m2 = insert_mneme_no_trigger(ws, "topic", "content beta", 0.5);

        // Create an association between them
        Spi::run(&format!(
            "INSERT INTO ghola.associations (src_id, dst_id, weight) \
             SELECT LEAST('{m1}'::uuid, '{m2}'::uuid), \
                    GREATEST('{m1}'::uuid, '{m2}'::uuid), 0.8"
        )).expect("insert assoc failed");

        // Flag and confirm
        Spi::run(&format!(
            "SELECT ghola.flag_contradictions('{m2}'::uuid, 0.85)"
        )).expect("flag failed");

        let candidate_id = Spi::get_one::<i64>(
            "SELECT id FROM ghola.contradiction_candidates LIMIT 1",
        )
        .expect("query failed")
        .expect("null");

        Spi::run(&format!(
            "SELECT ghola.resolve_contradiction({candidate_id}, 'confirmed')"
        )).expect("resolve failed");

        // Hebbian association weight should be weakened to 0.08 (0.8 * 0.1)
        let weight = Spi::get_one::<f64>(
            "SELECT weight FROM ghola.associations WHERE association_type = 'hebbian' LIMIT 1",
        )
        .expect("query failed")
        .expect("null");

        assert!(
            (weight - 0.08).abs() < 0.01,
            "hebbian association should be weakened to ~0.08, got {weight}"
        );

        // A 'contradicts' association should also be created (newer → older)
        let contradicts_count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.associations \
             WHERE src_id = '{m2}'::uuid AND dst_id = '{m1}'::uuid \
               AND association_type = 'contradicts'"
        ))
        .expect("query failed")
        .expect("null");

        assert_eq!(
            contradicts_count, 1,
            "confirmed contradiction should create a 'contradicts' association"
        );

        let contradicts_weight = Spi::get_one::<f64>(&format!(
            "SELECT weight FROM ghola.associations \
             WHERE src_id = '{m2}'::uuid AND dst_id = '{m1}'::uuid \
               AND association_type = 'contradicts'"
        ))
        .expect("query failed")
        .expect("null");

        assert!(
            (contradicts_weight - 1.0).abs() < 0.001,
            "contradicts association should have weight 1.0, got {contradicts_weight}"
        );
    }

    #[pg_test]
    fn test_workspace_isolation_contradictions() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup failed");

        let ws_a = "ca000000-0000-0000-0000-000000000001";
        let ws_b = "ca000000-0000-0000-0000-000000000002";

        let _m1 = insert_mneme_no_trigger(ws_a, "fact", "workspace A fact", 0.5);
        let m2 = insert_mneme_no_trigger(ws_b, "fact", "workspace B fact", 0.5);

        // Checking m2 should not find m1 (different workspace)
        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.check_contradictions('{m2}'::uuid, 0.85)"
        ))
        .expect("query failed")
        .expect("null");

        assert_eq!(count, 0, "different workspaces should not cross-flag");
    }

    #[pg_test]
    fn test_dormant_excluded_from_detection() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup failed");

        let ws = "cb000000-0000-0000-0000-000000000001";
        let m1 = insert_mneme_no_trigger(ws, "fact", "dormant fact", 0.5);
        let m2 = insert_mneme_no_trigger(ws, "fact", "active fact", 0.5);

        // Mark m1 as dormant
        Spi::run(&format!(
            "UPDATE ghola.mnemes SET state = 'dormant' WHERE id = '{m1}'::uuid"
        )).expect("update failed");

        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.check_contradictions('{m2}'::uuid, 0.85)"
        ))
        .expect("query failed")
        .expect("null");

        assert_eq!(count, 0, "dormant mnemes should be excluded from detection");
    }

    #[pg_test]
    fn test_get_pending_contradictions() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup failed");

        let ws = "cc000000-0000-0000-0000-000000000001";
        let _m1 = insert_mneme_no_trigger(ws, "lang", "Go is typed", 0.5);
        let m2 = insert_mneme_no_trigger(ws, "lang", "Go is untyped", 0.5);

        Spi::run(&format!(
            "SELECT ghola.flag_contradictions('{m2}'::uuid, 0.85)"
        )).expect("flag failed");

        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.get_pending_contradictions('{ws}'::uuid, 50)"
        ))
        .expect("query failed")
        .expect("null");

        assert!(count >= 1, "should return pending contradictions");
    }

    #[pg_test]
    fn test_trigger_auto_flags() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup failed");

        let ws = "cd000000-0000-0000-0000-000000000001";
        // Insert first mneme with trigger disabled
        let _m1 = insert_mneme_no_trigger(ws, "db", "PostgreSQL is relational", 0.5);

        // Insert second mneme WITH trigger enabled — should auto-flag
        let emb = embedding(0.5);
        Spi::run(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding) \
             VALUES ('{ws}', 'db', 'PostgreSQL is document-oriented', '{emb}'::vector)"
        )).expect("insert with trigger failed");

        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.contradiction_candidates \
             WHERE workspace_id = '{ws}'::uuid AND status = 'pending'"
        ))
        .expect("query failed")
        .expect("null");

        assert!(count >= 1, "trigger should auto-flag contradiction candidate");
    }

    #[pg_test]
    fn test_cascade_delete_cleans_candidates() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup failed");

        let ws = "ce000000-0000-0000-0000-000000000001";
        let _m1 = insert_mneme_no_trigger(ws, "item", "content one", 0.5);
        let m2 = insert_mneme_no_trigger(ws, "item", "content two", 0.5);

        Spi::run(&format!(
            "SELECT ghola.flag_contradictions('{m2}'::uuid, 0.85)"
        )).expect("flag failed");

        // Delete mneme_a — candidate should cascade delete
        Spi::run(&format!(
            "DELETE FROM ghola.mnemes WHERE id = '{m2}'::uuid"
        )).expect("delete failed");

        let count = Spi::get_one::<i64>(
            "SELECT count(*) FROM ghola.contradiction_candidates",
        )
        .expect("query failed")
        .expect("null");

        assert_eq!(count, 0, "cascade delete should clean up candidates");
    }

    #[pg_test]
    #[should_panic(expected = "resolution must be")]
    fn test_resolve_invalid_resolution() {
        Spi::run(
            "SELECT ghola.resolve_contradiction(1, 'invalid')"
        ).expect("should have failed");
    }

    #[pg_test]
    #[should_panic(expected = "not found")]
    fn test_resolve_nonexistent_candidate() {
        Spi::run(
            "SELECT ghola.resolve_contradiction(99999, 'confirmed')"
        ).expect("should have failed");
    }
}
