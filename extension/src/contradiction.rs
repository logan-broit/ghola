// ghola::contradiction — Contradiction detection and flagging
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
                     FROM semantic.mnemes WHERE id = '{mneme_id}'"
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
                     FROM semantic.mnemes \
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
                     FROM semantic.mnemes WHERE id = '{mneme_id}'"
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
        let source_content: String = source_row.get::<String>(4).expect("err").expect("null content");

        // Find similar active mnemes. v2 drops the concept_overlap
        // flag from contradiction_candidates, so we no longer read
        // concept columns for comparison here.
        let candidates = client
            .select(
                &format!(
                    "SELECT id, content, \
                         1 - (embedding <=> '{embedding}'::vector) AS similarity \
                     FROM semantic.mnemes \
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
        }

        let mut to_flag: Vec<Candidate> = Vec::new();
        for row in candidates {
            let candidate_id: pgrx::Uuid = row.get::<pgrx::Uuid>(1).expect("err").expect("null id");
            let candidate_content: String = row.get::<String>(2).expect("err").expect("null content");
            let sim: f64 = row.get::<f64>(3).expect("err").expect("null sim");

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
            });
        }

        let mut flagged: i64 = 0;
        for candidate in &to_flag {
            // v2 contradiction_candidates has (mneme_a, mneme_b,
            // similarity, detected_at, status) and no UNIQUE constraint
            // on (mneme_a, mneme_b). Guard against duplicates with an
            // explicit EXISTS check so repeated scans don't fan out.
            let inserted = client
                .select(
                    &format!(
                        "WITH ins AS ( \
                             INSERT INTO semantic.contradiction_candidates \
                                 (mneme_a, mneme_b, similarity) \
                             SELECT '{mneme_id}'::uuid, '{cid}'::uuid, {sim} \
                             WHERE NOT EXISTS ( \
                                 SELECT 1 FROM semantic.contradiction_candidates \
                                 WHERE mneme_a = '{mneme_id}'::uuid \
                                   AND mneme_b = '{cid}'::uuid \
                                   AND status = 'pending' \
                             ) \
                             RETURNING 1 \
                         ) SELECT count(*) FROM ins",
                        cid = candidate.id,
                        sim = candidate.similarity,
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
                     FROM semantic.contradiction_candidates \
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
                        "SELECT confidence FROM semantic.mnemes \
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
                            "UPDATE semantic.mnemes SET confidence = {posterior} \
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
                        "UPDATE semantic.associations \
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
                        "INSERT INTO semantic.associations \
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

        // Update candidate status. v2 schema drops the resolved_at
        // column — status + detected_at are enough.
        client
            .update(
                &format!(
                    "UPDATE semantic.contradiction_candidates \
                     SET status = '{resolution}' \
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

/// Pending contradiction candidates with full mneme details for
/// review. Workspace scoping is done via JOIN to mnemes rather than a
/// column on the candidate row (v2 contradiction_candidates has no
/// workspace_id).
#[pg_extern]
fn get_pending_contradictions(
    workspace_id: pgrx::Uuid,
    limit_n: default!(i32, 50),
) -> TableIterator<
    'static,
    (
        name!(candidate_id, i64),
        name!(similarity, f64),
        name!(concept_a, String),
        name!(content_a, String),
        name!(confidence_a, f64),
        name!(concept_b, String),
        name!(content_b, String),
        name!(confidence_b, f64),
        name!(detected_at, pgrx::datum::TimestampWithTimeZone),
    ),
> {
    let results = Spi::connect(|client| {
        let rows = client
            .select(
                &format!(
                    "SELECT cc.id, cc.similarity, \
                         a.concept, a.content, a.confidence, \
                         b.concept, b.content, b.confidence, \
                         cc.detected_at \
                     FROM semantic.contradiction_candidates cc \
                     JOIN semantic.mnemes a ON cc.mneme_a = a.id \
                     JOIN semantic.mnemes b ON cc.mneme_b = b.id \
                     WHERE a.workspace_id = '{workspace_id}'::uuid \
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
                row.get::<String>(3).expect("err").expect("null concept_a"),
                row.get::<String>(4).expect("err").expect("null content_a"),
                row.get::<f64>(5).expect("err").expect("null conf_a"),
                row.get::<String>(6).expect("err").expect("null concept_b"),
                row.get::<String>(7).expect("err").expect("null content_b"),
                row.get::<f64>(8).expect("err").expect("null conf_b"),
                row.get::<pgrx::datum::TimestampWithTimeZone>(9).expect("err").expect("null detected"),
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
                    "SELECT id::text FROM semantic.mnemes \
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
            "SELECT semantic.flag_contradictions('{id_str}'::uuid, {similarity_threshold})"
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
CREATE OR REPLACE FUNCTION mneme_insert_enqueue_trigger()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO @extschema@.contradiction_queue (mneme_id)
    VALUES (NEW.id);
    RETURN NEW;
END;
$$;

CREATE TRIGGER mneme_insert_enqueue
    AFTER INSERT ON mnemes
    FOR EACH ROW
    EXECUTE FUNCTION mneme_insert_enqueue_trigger();
"#,
    name = "create_mneme_insert_enqueue_trigger",
    requires = [
        "create_semantic_schema_and_mnemes",
        "create_queue_tables",
    ],
);

