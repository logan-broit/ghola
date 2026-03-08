// pg_recall::hebbian — Hebbian learning and confidence functions
//
// Implements record_co_activation, get_associations, update_confidence,
// confirm_recall, process_co_activation_batch, process_all_pending_co_activations.
// The control file's schema directive places all objects in pg_recall automatically.
//
// Owned by: implement_hebbian_helpers task

use pgrx::prelude::*;

use crate::scoring::bayesian_update_inner;

/// Insert a co-activation event into the queue.
///
/// Validates that mneme_ids and scores arrays have the same length,
/// then inserts a single row into pg_recall.co_activation_queue.
#[pg_extern]
fn record_co_activation(
    workspace_id: pgrx::Uuid,
    mneme_ids: Vec<pgrx::Uuid>,
    scores: Vec<f64>,
) -> &'static str {
    if mneme_ids.len() != scores.len() {
        pgrx::error!(
            "array length mismatch: mneme_ids has {} elements but scores has {}",
            mneme_ids.len(),
            scores.len()
        );
    }

    let ids_literal = uuid_array_literal(&mneme_ids);
    let scores_literal = float_array_literal(&scores);

    Spi::run(&format!(
        "INSERT INTO pg_recall.co_activation_queue (workspace_id, mneme_ids, scores) \
         VALUES ('{workspace_id}', {ids_literal}, {scores_literal})"
    ))
    .expect("failed to insert co-activation event");

    "ok"
}

/// Get associations for a given mneme from both directions (src or dst),
/// returning the other ID as related_id and the weight, ordered by weight descending.
/// Only associations with weight >= min_weight are returned.
#[pg_extern]
fn get_associations(
    mneme_id: pgrx::Uuid,
    min_weight: default!(f64, 0.01),
) -> TableIterator<'static, (name!(related_id, pgrx::Uuid), name!(weight, f64))> {
    let results = Spi::connect(|client| {
        let query = format!(
            "SELECT related_id, weight FROM ( \
                SELECT dst_id AS related_id, weight FROM pg_recall.associations \
                WHERE src_id = '{mneme_id}' AND weight >= {min_weight} \
                UNION ALL \
                SELECT src_id AS related_id, weight FROM pg_recall.associations \
                WHERE dst_id = '{mneme_id}' AND weight >= {min_weight} \
            ) sub \
            ORDER BY weight DESC"
        );

        let tup_table = client
            .select(&query, None, &[])
            .expect("failed to query associations");

        let mut results = Vec::new();
        for row in tup_table {
            let related_id: pgrx::Uuid = row
                .get::<pgrx::Uuid>(1)
                .expect("failed to get related_id")
                .expect("null related_id");
            let weight: f64 = row
                .get::<f64>(2)
                .expect("failed to get weight")
                .expect("null weight");
            results.push((related_id, weight));
        }
        results
    });

    TableIterator::new(results)
}

/// Atomically read a mneme's current confidence, apply bayesian_update with
/// the given evidence, write back the new confidence, and return it.
#[pg_extern]
fn update_confidence(mneme_id: pgrx::Uuid, evidence: f64) -> f64 {
    Spi::connect_mut(|client| {
        // Read current confidence and tier with row lock
        let tup_table = client
            .select(
                &format!(
                    "SELECT confidence, tier FROM pg_recall.mnemes WHERE id = '{mneme_id}' FOR UPDATE"
                ),
                None,
                &[],
            )
            .expect("failed to read mneme confidence");

        let row = tup_table.into_iter().next();
        match row {
            None => {
                pgrx::error!("mneme_id not found in mnemes table: {mneme_id}");
            }
            Some(r) => {
                let prior: f64 = r
                    .get::<f64>(1)
                    .expect("failed to get confidence")
                    .expect("null confidence");
                let tier: String = r
                    .get::<String>(2)
                    .expect("failed to get tier")
                    .expect("null tier");

                let posterior = bayesian_update_inner(prior, evidence);
                let floor = tier_confidence_floor(&tier);
                let clamped = posterior.max(floor);

                client
                    .update(
                        &format!(
                            "UPDATE pg_recall.mnemes SET confidence = {clamped} WHERE id = '{mneme_id}'"
                        ),
                        None,
                        &[],
                    )
                    .expect("failed to update confidence");

                clamped
            }
        }
    })
}

/// Returns the confidence floor for a given tier.
/// Core memories never drop below 0.30; index and state use the Laplace bound (0.025).
fn tier_confidence_floor(tier: &str) -> f64 {
    match tier {
        "core" => 0.30,
        _ => 0.025,
    }
}

/// Confirm recall for multiple mnemes by applying evidence=0.95 to each.
#[pg_extern]
fn confirm_recall(mneme_ids: Vec<pgrx::Uuid>) -> &'static str {
    for id in &mneme_ids {
        Spi::connect_mut(|client| {
            let tup_table = client
                .select(
                    &format!(
                        "SELECT confidence, tier FROM pg_recall.mnemes WHERE id = '{id}' FOR UPDATE"
                    ),
                    None,
                    &[],
                )
                .expect("failed to read mneme confidence");

            let row = tup_table.into_iter().next();
            match row {
                None => {
                    pgrx::error!("mneme_id not found in mnemes table: {id}");
                }
                Some(r) => {
                    let prior: f64 = r
                        .get::<f64>(1)
                        .expect("failed to get confidence")
                        .expect("null confidence");
                    let tier: String = r
                        .get::<String>(2)
                        .expect("failed to get tier")
                        .expect("null tier");

                    let posterior = bayesian_update_inner(prior, 0.95);
                    let floor = tier_confidence_floor(&tier);
                    let clamped = posterior.max(floor);

                    client
                        .update(
                            &format!(
                                "UPDATE pg_recall.mnemes SET confidence = {clamped} WHERE id = '{id}'"
                            ),
                            None,
                            &[],
                        )
                        .expect("failed to update confidence");
                }
            }
        });
    }
    "ok"
}

/// Process up to batch_limit rows from the co_activation_queue in a single transaction.
///
/// For each queue event, generates all canonical (i, j) pairs where i < j from the
/// mneme_ids array. Aggregates pair signals as sum(score_i * score_j). Upserts
/// associations with log-space weight update. Updates access_count and last_access
/// for all referenced mnemes. Deletes consumed queue rows.
///
/// Returns the number of queue rows processed, or 0 when the queue is empty.
#[pg_extern]
fn process_co_activation_batch(batch_limit: default!(i32, 100)) -> i64 {
    use std::collections::{HashMap, HashSet};

    Spi::connect_mut(|client| {
        // 1. Fetch up to batch_limit queue rows
        let rows = client
            .select(
                &format!(
                    "SELECT id, mneme_ids, scores FROM pg_recall.co_activation_queue \
                     ORDER BY id LIMIT {batch_limit}"
                ),
                None,
                &[],
            )
            .expect("failed to read co_activation_queue");

        struct QueueEvent {
            id: i64,
            mneme_ids: Vec<pgrx::Uuid>,
            scores: Vec<f64>,
        }

        let mut events: Vec<QueueEvent> = Vec::new();

        for row in rows {
            let id: i64 = row.get::<i64>(1).expect("failed to get id").expect("null id");
            let mneme_ids: Vec<pgrx::Uuid> = row
                .get::<Vec<pgrx::Uuid>>(2)
                .expect("failed to get mneme_ids")
                .expect("null mneme_ids");
            let scores: Vec<f64> = row
                .get::<Vec<f64>>(3)
                .expect("failed to get scores")
                .expect("null scores");
            events.push(QueueEvent {
                id,
                mneme_ids,
                scores,
            });
        }

        if events.is_empty() {
            return 0i64;
        }

        let num_processed = events.len() as i64;

        // 2. Collect all referenced mneme IDs and identify state-tier mnemes to exclude
        let mut all_mneme_ids: HashSet<String> = HashSet::new();
        let mut consumed_ids: Vec<i64> = Vec::new();

        for event in &events {
            consumed_ids.push(event.id);
            for mid in &event.mneme_ids {
                all_mneme_ids.insert(mid.to_string());
            }
        }

        // Query state-tier mnemes to exclude from Hebbian learning
        let state_tier_ids: HashSet<String> = if !all_mneme_ids.is_empty() {
            let id_list: String = all_mneme_ids
                .iter()
                .map(|id| format!("'{id}'::uuid"))
                .collect::<Vec<_>>()
                .join(",");
            let rows = client
                .select(
                    &format!(
                        "SELECT id::text FROM pg_recall.mnemes \
                         WHERE id IN ({id_list}) AND tier = 'state'"
                    ),
                    None,
                    &[],
                )
                .expect("failed to query state-tier mnemes");
            let mut set = HashSet::new();
            for row in rows {
                if let Some(id) = row.get::<String>(1).expect("err") {
                    set.insert(id);
                }
            }
            set
        } else {
            HashSet::new()
        };

        // Aggregate pair signals across all events, skipping state-tier mnemes
        // Key: (smaller_uuid_str, larger_uuid_str), Value: sum of score_i * score_j
        let mut pair_signals: HashMap<(String, String), f64> = HashMap::new();

        for event in &events {
            let n = event.mneme_ids.len();
            for i in 0..n {
                let a_str = event.mneme_ids[i].to_string();
                if state_tier_ids.contains(&a_str) {
                    continue;
                }
                for j in (i + 1)..n {
                    let b_str = event.mneme_ids[j].to_string();
                    if state_tier_ids.contains(&b_str) {
                        continue;
                    }
                    // Canonical ordering: smaller UUID string first
                    let (src, dst) = if a_str < b_str {
                        (a_str.clone(), b_str)
                    } else {
                        (b_str, a_str.clone())
                    };
                    let signal = event.scores[i] * event.scores[j];
                    *pair_signals.entry((src, dst)).or_insert(0.0) += signal;
                }
            }
        }

        // 3. Upsert associations with log-space weight update
        // new_weight = min(1.0, exp(ln(current_weight) + signal * ln(1.01)))
        // New associations seeded at weight 0.01 before reinforcement
        for ((src, dst), signal) in &pair_signals {
            client
                .update(
                    &format!(
                        "INSERT INTO pg_recall.associations (src_id, dst_id, association_type, weight, co_activations, updated_at) \
                         VALUES ('{src}', '{dst}', 'hebbian', \
                             LEAST(1.0, EXP(LN(0.01) + {signal} * LN(1.01))), \
                             1, now()) \
                         ON CONFLICT (src_id, dst_id, association_type) DO UPDATE SET \
                             weight = LEAST(1.0, EXP(LN(pg_recall.associations.weight) + {signal} * LN(1.01))), \
                             co_activations = pg_recall.associations.co_activations + 1, \
                             updated_at = now()"
                    ),
                    None,
                    &[],
                )
                .expect("failed to upsert association");
        }

        // 4. Update access_count and last_access for all referenced mnemes
        for mid in &all_mneme_ids {
            client
                .update(
                    &format!(
                        "UPDATE pg_recall.mnemes SET access_count = access_count + 1, \
                         last_access = now() WHERE id = '{mid}'"
                    ),
                    None,
                    &[],
                )
                .expect("failed to update mneme access tracking");
        }

        // 5. Delete consumed queue rows
        for qid in &consumed_ids {
            client
                .update(
                    &format!("DELETE FROM pg_recall.co_activation_queue WHERE id = {qid}"),
                    None,
                    &[],
                )
                .expect("failed to delete consumed queue row");
        }

        num_processed
    })
}

/// Repeatedly calls process_co_activation_batch until the queue is fully drained.
/// Returns the total number of queue rows processed across all batches.
#[pg_extern]
fn process_all_pending_co_activations() -> i64 {
    let mut total: i64 = 0;
    loop {
        let processed = Spi::get_one::<i64>(
            "SELECT pg_recall.process_co_activation_batch(100)",
        )
        .expect("failed to call process_co_activation_batch")
        .unwrap_or(0);

        if processed == 0 {
            break;
        }
        total += processed;
    }
    total
}

// ──────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────

/// Format a slice of UUIDs as a Postgres array literal: ARRAY['...','...']::uuid[]
fn uuid_array_literal(ids: &[pgrx::Uuid]) -> String {
    let elements: Vec<String> = ids.iter().map(|id| format!("'{id}'")).collect();
    format!("ARRAY[{}]::uuid[]", elements.join(","))
}

/// Format a slice of f64 as a Postgres array literal: ARRAY[...]::float8[]
fn float_array_literal(vals: &[f64]) -> String {
    let elements: Vec<String> = vals.iter().map(|v| format!("{v}")).collect();
    format!("ARRAY[{}]::float8[]", elements.join(","))
}

// ──────────────────────────────────────────────
// pgrx integration tests (require Postgres)
// ──────────────────────────────────────────────

#[cfg(any(test, feature = "pg_test"))]
#[pg_schema]
mod tests {
    use pgrx::prelude::*;

    /// Helper: install pgvector and insert test mnemes.
    /// Returns (workspace_id, mneme_id_1, mneme_id_2, mneme_id_3) as strings.
    fn setup_test_mnemes() -> (String, String, String, String) {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("failed to create vector extension");

        let ws_id = "00000000-0000-0000-0000-000000000001";

        // Insert three test mnemes with dummy 768-dim embeddings
        let m1 = insert_test_mneme(ws_id, "kubernetes", "pod scheduling", 0.1);
        let m2 = insert_test_mneme(ws_id, "docker", "container runtime", 0.2);
        let m3 = insert_test_mneme(ws_id, "helm", "chart deployment", 0.3);

        (ws_id.to_string(), m1, m2, m3)
    }

    fn insert_test_mneme(ws_id: &str, concept: &str, content: &str, fill_val: f64) -> String {
        // pgvector requires bracket notation: [0.1,0.1,...]
        let elements = vec![format!("{fill_val}"); 768];
        let vec_literal = format!("[{}]", elements.join(","));
        Spi::get_one::<String>(&format!(
            "INSERT INTO pg_recall.mnemes (workspace_id, concept, content, embedding) \
             VALUES ('{ws_id}', '{concept}', '{content}', \
             '{vec_literal}'::vector(768)) \
             RETURNING id::text"
        ))
        .expect("insert failed")
        .expect("null id")
    }

    // ── record_co_activation ──

    #[pg_test]
    fn test_record_co_activation_inserts() {
        let (ws_id, m1, m2, m3) = setup_test_mnemes();

        let result = Spi::get_one::<&str>(&format!(
            "SELECT pg_recall.record_co_activation(\
                '{ws_id}'::uuid, \
                ARRAY['{m1}','{m2}','{m3}']::uuid[], \
                ARRAY[0.9, 0.7, 0.5]::float8[])"
        ))
        .expect("query failed")
        .expect("null result");

        assert_eq!(result, "ok");

        let count = Spi::get_one::<i64>("SELECT count(*) FROM pg_recall.co_activation_queue")
            .expect("count failed")
            .expect("null count");

        assert_eq!(count, 1);
    }

    #[pg_test]
    #[should_panic(expected = "array length mismatch")]
    fn test_record_co_activation_length_mismatch() {
        let (ws_id, m1, m2, ..) = setup_test_mnemes();

        Spi::get_one::<&str>(&format!(
            "SELECT pg_recall.record_co_activation(\
                '{ws_id}'::uuid, \
                ARRAY['{m1}','{m2}']::uuid[], \
                ARRAY[0.9]::float8[])"
        ))
        .expect("query failed");
    }

    // ── get_associations ──

    #[pg_test]
    fn test_get_associations_empty() {
        let (_ws_id, m1, ..) = setup_test_mnemes();

        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM pg_recall.get_associations('{m1}'::uuid, 0.01)"
        ))
        .expect("query failed")
        .expect("null count");

        assert_eq!(count, 0, "should return empty result set when no associations exist");
    }

    #[pg_test]
    fn test_get_associations_both_directions() {
        let (_ws_id, m1, m2, m3) = setup_test_mnemes();

        // Insert associations with canonical ordering
        Spi::run(&format!(
            "INSERT INTO pg_recall.associations (src_id, dst_id, weight) \
             SELECT LEAST('{m1}'::uuid, '{m2}'::uuid), \
                    GREATEST('{m1}'::uuid, '{m2}'::uuid), 0.5"
        ))
        .expect("insert assoc 1 failed");

        Spi::run(&format!(
            "INSERT INTO pg_recall.associations (src_id, dst_id, weight) \
             SELECT LEAST('{m1}'::uuid, '{m3}'::uuid), \
                    GREATEST('{m1}'::uuid, '{m3}'::uuid), 0.3"
        ))
        .expect("insert assoc 2 failed");

        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM pg_recall.get_associations('{m1}'::uuid, 0.01)"
        ))
        .expect("query failed")
        .expect("null count");

        assert_eq!(count, 2, "should return associations from both directions");
    }

    #[pg_test]
    fn test_get_associations_min_weight_filter() {
        let (_ws_id, m1, m2, m3) = setup_test_mnemes();

        Spi::run(&format!(
            "INSERT INTO pg_recall.associations (src_id, dst_id, weight) \
             SELECT LEAST('{m1}'::uuid, '{m2}'::uuid), \
                    GREATEST('{m1}'::uuid, '{m2}'::uuid), 0.5"
        ))
        .expect("insert assoc failed");

        Spi::run(&format!(
            "INSERT INTO pg_recall.associations (src_id, dst_id, weight) \
             SELECT LEAST('{m1}'::uuid, '{m3}'::uuid), \
                    GREATEST('{m1}'::uuid, '{m3}'::uuid), 0.1"
        ))
        .expect("insert assoc failed");

        // min_weight=0.3 should only return the 0.5 association
        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM pg_recall.get_associations('{m1}'::uuid, 0.3)"
        ))
        .expect("query failed")
        .expect("null count");

        assert_eq!(count, 1, "should filter by min_weight");
    }

    #[pg_test]
    fn test_get_associations_ordered_by_weight_desc() {
        let (_ws_id, m1, m2, m3) = setup_test_mnemes();

        Spi::run(&format!(
            "INSERT INTO pg_recall.associations (src_id, dst_id, weight) \
             SELECT LEAST('{m1}'::uuid, '{m2}'::uuid), \
                    GREATEST('{m1}'::uuid, '{m2}'::uuid), 0.3"
        ))
        .expect("insert assoc failed");

        Spi::run(&format!(
            "INSERT INTO pg_recall.associations (src_id, dst_id, weight) \
             SELECT LEAST('{m1}'::uuid, '{m3}'::uuid), \
                    GREATEST('{m1}'::uuid, '{m3}'::uuid), 0.7"
        ))
        .expect("insert assoc failed");

        let first_weight = Spi::get_one::<f64>(&format!(
            "SELECT weight FROM pg_recall.get_associations('{m1}'::uuid, 0.01) LIMIT 1"
        ))
        .expect("query failed")
        .expect("null weight");

        assert!(
            (first_weight - 0.7).abs() < 0.01,
            "first result should be highest weight, got {first_weight}"
        );
    }

    // ── update_confidence ──

    #[pg_test]
    fn test_update_confidence_strong_evidence() {
        let (_ws_id, m1, ..) = setup_test_mnemes();

        // Default confidence is 0.5, apply strong evidence
        let new_conf = Spi::get_one::<f64>(&format!(
            "SELECT pg_recall.update_confidence('{m1}'::uuid, 0.95)"
        ))
        .expect("query failed")
        .expect("null result");

        assert!(
            (new_conf - 0.925).abs() < 0.01,
            "update_confidence(0.5, 0.95) should be ~0.925, got {new_conf}"
        );

        // Verify it's persisted
        let stored = Spi::get_one::<f64>(&format!(
            "SELECT confidence FROM pg_recall.mnemes WHERE id = '{m1}'::uuid"
        ))
        .expect("query failed")
        .expect("null result");

        assert!(
            (stored - new_conf).abs() < 0.001,
            "persisted confidence should match returned value"
        );
    }

    #[pg_test]
    fn test_update_confidence_contradiction() {
        let (_ws_id, m1, ..) = setup_test_mnemes();

        // Set initial confidence to 0.8
        Spi::run(&format!(
            "UPDATE pg_recall.mnemes SET confidence = 0.8 WHERE id = '{m1}'::uuid"
        ))
        .expect("update failed");

        let new_conf = Spi::get_one::<f64>(&format!(
            "SELECT pg_recall.update_confidence('{m1}'::uuid, 0.10)"
        ))
        .expect("query failed")
        .expect("null result");

        assert!(
            (new_conf - 0.32).abs() < 0.02,
            "update_confidence(0.8, 0.10) should be ~0.32, got {new_conf}"
        );
    }

    #[pg_test]
    #[should_panic(expected = "mneme_id not found")]
    fn test_update_confidence_nonexistent() {
        setup_test_mnemes(); // ensure tables exist
        Spi::get_one::<f64>(
            "SELECT pg_recall.update_confidence(\
                '99999999-9999-9999-9999-999999999999'::uuid, 0.5)"
        )
        .expect("query failed");
    }

    // ── confirm_recall ──

    #[pg_test]
    fn test_confirm_recall_updates_all() {
        let (_ws_id, m1, m2, m3) = setup_test_mnemes();

        Spi::run(&format!(
            "SELECT pg_recall.confirm_recall(ARRAY['{m1}','{m2}','{m3}']::uuid[])"
        ))
        .expect("confirm_recall failed");

        // All three should have confidence updated from 0.5 with evidence=0.95
        // bayesian_update(0.5, 0.95) ≈ 0.925
        for mid in &[&m1, &m2, &m3] {
            let conf = Spi::get_one::<f64>(&format!(
                "SELECT confidence FROM pg_recall.mnemes WHERE id = '{mid}'::uuid"
            ))
            .expect("query failed")
            .expect("null confidence");

            assert!(
                (conf - 0.925).abs() < 0.01,
                "mneme {mid} confidence should be ~0.925 after confirm_recall, got {conf}"
            );
        }
    }

    // ── tier-aware confidence floor ──

    #[pg_test]
    fn test_core_tier_confidence_floor() {
        let (_ws_id, m1, ..) = setup_test_mnemes();

        // Set mneme to core tier with moderate confidence
        Spi::run(&format!(
            "UPDATE pg_recall.mnemes SET tier = 'core', confidence = 0.5 WHERE id = '{m1}'::uuid"
        ))
        .expect("update failed");

        // Apply very weak evidence — bayesian_update(0.5, 0.05) ≈ 0.072
        // But core floor is 0.30, so result should be clamped to 0.30
        let new_conf = Spi::get_one::<f64>(&format!(
            "SELECT pg_recall.update_confidence('{m1}'::uuid, 0.05)"
        ))
        .expect("query failed")
        .expect("null result");

        assert!(
            (new_conf - 0.30).abs() < 0.01,
            "core tier should clamp confidence to floor 0.30, got {new_conf}"
        );
    }

    #[pg_test]
    fn test_index_tier_no_elevated_floor() {
        let (_ws_id, m1, ..) = setup_test_mnemes();

        // Default tier is 'index', confidence 0.5
        // Apply weak evidence — bayesian_update(0.5, 0.05) ≈ 0.072
        // Index floor is 0.025, so no clamping
        let new_conf = Spi::get_one::<f64>(&format!(
            "SELECT pg_recall.update_confidence('{m1}'::uuid, 0.05)"
        ))
        .expect("query failed")
        .expect("null result");

        assert!(
            new_conf < 0.10,
            "index tier should allow confidence to drop below 0.10, got {new_conf}"
        );
        assert!(
            new_conf >= 0.025,
            "index tier should not drop below Laplace bound 0.025, got {new_conf}"
        );
    }

    // ── process_co_activation_batch ──

    #[pg_test]
    fn test_process_batch_empty_queue() {
        setup_test_mnemes(); // ensure tables exist

        let processed = Spi::get_one::<i64>(
            "SELECT pg_recall.process_co_activation_batch(100)",
        )
        .expect("query failed")
        .expect("null result");

        assert_eq!(processed, 0, "empty queue should return 0");
    }

    #[pg_test]
    fn test_process_batch_creates_associations() {
        let (ws_id, m1, m2, m3) = setup_test_mnemes();

        // Enqueue a co-activation event
        Spi::run(&format!(
            "SELECT pg_recall.record_co_activation(\
                '{ws_id}'::uuid, \
                ARRAY['{m1}','{m2}','{m3}']::uuid[], \
                ARRAY[0.9, 0.7, 0.5]::float8[])"
        ))
        .expect("record failed");

        let processed = Spi::get_one::<i64>(
            "SELECT pg_recall.process_co_activation_batch(100)",
        )
        .expect("query failed")
        .expect("null result");

        assert_eq!(processed, 1, "should process 1 queue row");

        // Should have 3 associations: (m1,m2), (m1,m3), (m2,m3)
        let assoc_count =
            Spi::get_one::<i64>("SELECT count(*) FROM pg_recall.associations")
                .expect("query failed")
                .expect("null count");

        assert_eq!(assoc_count, 3, "should create 3 associations for 3 mnemes");

        // Queue should be empty now
        let queue_count =
            Spi::get_one::<i64>("SELECT count(*) FROM pg_recall.co_activation_queue")
                .expect("query failed")
                .expect("null count");

        assert_eq!(queue_count, 0, "queue should be empty after processing");
    }

    #[pg_test]
    fn test_process_batch_updates_access_tracking() {
        let (ws_id, m1, m2, ..) = setup_test_mnemes();

        // Record initial access_count
        let initial_count = Spi::get_one::<i32>(&format!(
            "SELECT access_count FROM pg_recall.mnemes WHERE id = '{m1}'::uuid"
        ))
        .expect("query failed")
        .expect("null");

        assert_eq!(initial_count, 0);

        // Enqueue and process
        Spi::run(&format!(
            "SELECT pg_recall.record_co_activation(\
                '{ws_id}'::uuid, \
                ARRAY['{m1}','{m2}']::uuid[], \
                ARRAY[0.9, 0.7]::float8[])"
        ))
        .expect("record failed");

        Spi::run("SELECT pg_recall.process_co_activation_batch(100)")
            .expect("process failed");

        // access_count should be incremented
        let new_count = Spi::get_one::<i32>(&format!(
            "SELECT access_count FROM pg_recall.mnemes WHERE id = '{m1}'::uuid"
        ))
        .expect("query failed")
        .expect("null");

        assert_eq!(new_count, 1, "access_count should be incremented by 1");
    }

    #[pg_test]
    fn test_process_batch_weight_reinforcement() {
        let (ws_id, m1, m2, ..) = setup_test_mnemes();

        // Process first co-activation
        Spi::run(&format!(
            "SELECT pg_recall.record_co_activation(\
                '{ws_id}'::uuid, \
                ARRAY['{m1}','{m2}']::uuid[], \
                ARRAY[0.9, 0.7]::float8[])"
        ))
        .expect("record failed");

        Spi::run("SELECT pg_recall.process_co_activation_batch(100)")
            .expect("process failed");

        let weight1 = Spi::get_one::<f64>("SELECT weight FROM pg_recall.associations LIMIT 1")
            .expect("query failed")
            .expect("null weight");

        // Process second co-activation for same pair
        Spi::run(&format!(
            "SELECT pg_recall.record_co_activation(\
                '{ws_id}'::uuid, \
                ARRAY['{m1}','{m2}']::uuid[], \
                ARRAY[0.9, 0.7]::float8[])"
        ))
        .expect("record failed");

        Spi::run("SELECT pg_recall.process_co_activation_batch(100)")
            .expect("process failed");

        let weight2 = Spi::get_one::<f64>("SELECT weight FROM pg_recall.associations LIMIT 1")
            .expect("query failed")
            .expect("null weight");

        assert!(
            weight2 > weight1,
            "weight should increase with reinforcement: {weight1} -> {weight2}"
        );
    }

    #[pg_test]
    fn test_process_batch_new_association_seeded() {
        let (ws_id, m1, m2, ..) = setup_test_mnemes();

        // Process a co-activation for a new pair
        Spi::run(&format!(
            "SELECT pg_recall.record_co_activation(\
                '{ws_id}'::uuid, \
                ARRAY['{m1}','{m2}']::uuid[], \
                ARRAY[0.9, 0.7]::float8[])"
        ))
        .expect("record failed");

        Spi::run("SELECT pg_recall.process_co_activation_batch(100)")
            .expect("process failed");

        let weight = Spi::get_one::<f64>("SELECT weight FROM pg_recall.associations LIMIT 1")
            .expect("query failed")
            .expect("null weight");

        // Weight should be close to 0.01 (seed) with small reinforcement
        // exp(ln(0.01) + 0.63 * ln(1.01)) ≈ 0.01 * 1.01^0.63 ≈ 0.01006
        assert!(
            weight > 0.009 && weight < 0.02,
            "new association should be seeded near 0.01, got {weight}"
        );
    }

    // ── process_all_pending_co_activations ──

    #[pg_test]
    fn test_process_all_drains_queue() {
        let (ws_id, m1, m2, m3) = setup_test_mnemes();

        // Enqueue multiple events
        for _ in 0..5 {
            Spi::run(&format!(
                "SELECT pg_recall.record_co_activation(\
                    '{ws_id}'::uuid, \
                    ARRAY['{m1}','{m2}','{m3}']::uuid[], \
                    ARRAY[0.9, 0.7, 0.5]::float8[])"
            ))
            .expect("record failed");
        }

        let total = Spi::get_one::<i64>(
            "SELECT pg_recall.process_all_pending_co_activations()",
        )
        .expect("query failed")
        .expect("null result");

        assert_eq!(total, 5, "should process all 5 events");

        // Queue should be empty
        let remaining =
            Spi::get_one::<i64>("SELECT count(*) FROM pg_recall.co_activation_queue")
                .expect("query failed")
                .expect("null count");

        assert_eq!(remaining, 0, "queue should be fully drained");
    }

    #[pg_test]
    fn test_process_all_empty_queue() {
        setup_test_mnemes(); // ensure tables exist

        let total = Spi::get_one::<i64>(
            "SELECT pg_recall.process_all_pending_co_activations()",
        )
        .expect("query failed")
        .expect("null result");

        assert_eq!(total, 0, "empty queue should return 0");
    }

    // ── Weight saturation: LEAST(1.0, ...) cap is enforced ──

    #[pg_test]
    fn test_weight_capped_at_one() {
        let (_ws_id, m1, m2, ..) = setup_test_mnemes();

        // Manually set weight to 0.99 and process one more co-activation
        // to verify LEAST(1.0, ...) prevents exceeding 1.0
        let (src, dst) = if m1 < m2 {
            (&m1, &m2)
        } else {
            (&m2, &m1)
        };
        Spi::run(&format!(
            "INSERT INTO pg_recall.associations (src_id, dst_id, weight, co_activations) \
             VALUES ('{src}'::uuid, '{dst}'::uuid, 0.99, 100)"
        ))
        .expect("insert failed");

        // Process a high-signal co-activation that would push weight past 1.0
        // weight = LEAST(1.0, exp(ln(0.99) + 0.81 * ln(1.01))) ≈ LEAST(1.0, 0.998)
        // Even with many rounds, it should never exceed 1.0
        Spi::run(&format!(
            "INSERT INTO pg_recall.co_activation_queue (workspace_id, mneme_ids, scores) \
             VALUES (gen_random_uuid(), ARRAY['{src}','{dst}']::uuid[], ARRAY[0.99, 0.99]::float8[])"
        ))
        .expect("enqueue failed");
        Spi::run("SELECT pg_recall.process_co_activation_batch(100)")
            .expect("process failed");

        let weight = Spi::get_one::<f64>(
            "SELECT weight FROM pg_recall.associations LIMIT 1",
        )
        .expect("query failed")
        .expect("null");

        assert!(
            weight <= 1.0,
            "weight must never exceed 1.0 even when near saturation, got {weight}"
        );
        assert!(
            weight > 0.98,
            "weight near 1.0 should stay near 1.0 after reinforcement, got {weight}"
        );
    }

    // ── co_activations counter increments correctly ──

    #[pg_test]
    fn test_co_activations_counter_increments() {
        let (ws_id, m1, m2, ..) = setup_test_mnemes();

        // Process 3 co-activation events for the same pair
        for _ in 0..3 {
            Spi::run(&format!(
                "SELECT pg_recall.record_co_activation(\
                    '{ws_id}'::uuid, \
                    ARRAY['{m1}','{m2}']::uuid[], \
                    ARRAY[0.8, 0.7]::float8[])"
            ))
            .expect("record failed");
            Spi::run("SELECT pg_recall.process_co_activation_batch(100)")
                .expect("process failed");
        }

        let co_acts = Spi::get_one::<i32>(
            "SELECT co_activations FROM pg_recall.associations LIMIT 1",
        )
        .expect("query failed")
        .expect("null");

        assert_eq!(
            co_acts, 3,
            "co_activations counter should be 3 after 3 events, got {co_acts}"
        );
    }

    // ── Pair signal computation: verify score_i * score_j ──

    #[pg_test]
    fn test_pair_signal_determines_reinforcement_strength() {
        let (ws_id, m1, m2, m3) = setup_test_mnemes();

        // Strong pair: scores [0.9, 0.9] → signal = 0.81
        // Weak pair:   scores [0.1, 0.1] → signal = 0.01
        // Process one event with m1,m2 at high scores and m1,m3 at low scores
        Spi::run(&format!(
            "SELECT pg_recall.record_co_activation(\
                '{ws_id}'::uuid, \
                ARRAY['{m1}','{m2}']::uuid[], \
                ARRAY[0.9, 0.9]::float8[])"
        ))
        .expect("record failed");

        Spi::run(&format!(
            "SELECT pg_recall.record_co_activation(\
                '{ws_id}'::uuid, \
                ARRAY['{m1}','{m3}']::uuid[], \
                ARRAY[0.1, 0.1]::float8[])"
        ))
        .expect("record failed");

        Spi::run("SELECT pg_recall.process_all_pending_co_activations()")
            .expect("process failed");

        // Get both association weights
        let strong_weight = Spi::get_one::<f64>(&format!(
            "SELECT weight FROM pg_recall.associations \
             WHERE (src_id = LEAST('{m1}'::uuid, '{m2}'::uuid) \
                AND dst_id = GREATEST('{m1}'::uuid, '{m2}'::uuid))"
        ))
        .expect("query failed")
        .expect("null");

        let weak_weight = Spi::get_one::<f64>(&format!(
            "SELECT weight FROM pg_recall.associations \
             WHERE (src_id = LEAST('{m1}'::uuid, '{m3}'::uuid) \
                AND dst_id = GREATEST('{m1}'::uuid, '{m3}'::uuid))"
        ))
        .expect("query failed")
        .expect("null");

        assert!(
            strong_weight > weak_weight,
            "high-score pair should produce stronger association than low-score pair: \
             strong={strong_weight}, weak={weak_weight}"
        );
    }

    // ── state-tier exclusion from Hebbian learning ──

    #[pg_test]
    fn test_state_tier_excluded_from_hebbian() {
        let (ws_id, m1, m2, m3) = setup_test_mnemes();

        // Mark m2 as state-tier
        Spi::run(&format!(
            "UPDATE pg_recall.mnemes SET tier = 'state' WHERE id = '{m2}'::uuid"
        ))
        .expect("update failed");

        // Co-activate all three
        Spi::run(&format!(
            "SELECT pg_recall.record_co_activation(\
                '{ws_id}'::uuid, \
                ARRAY['{m1}','{m2}','{m3}']::uuid[], \
                ARRAY[0.8, 0.8, 0.8]::float8[])"
        ))
        .expect("record failed");

        Spi::run("SELECT pg_recall.process_all_pending_co_activations()")
            .expect("process failed");

        // m1-m3 association should exist (both non-state)
        let m1_m3 = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM pg_recall.associations \
             WHERE src_id = LEAST('{m1}'::uuid, '{m3}'::uuid) \
               AND dst_id = GREATEST('{m1}'::uuid, '{m3}'::uuid) \
               AND association_type = 'hebbian'"
        ))
        .expect("query failed")
        .expect("null");

        assert_eq!(m1_m3, 1, "non-state pair m1-m3 should have an association");

        // m1-m2 and m2-m3 associations should NOT exist (m2 is state-tier)
        let m1_m2 = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM pg_recall.associations \
             WHERE src_id = LEAST('{m1}'::uuid, '{m2}'::uuid) \
               AND dst_id = GREATEST('{m1}'::uuid, '{m2}'::uuid) \
               AND association_type = 'hebbian'"
        ))
        .expect("query failed")
        .expect("null");

        assert_eq!(m1_m2, 0, "state-tier m2 should be excluded from Hebbian pairs");

        let m2_m3 = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM pg_recall.associations \
             WHERE src_id = LEAST('{m2}'::uuid, '{m3}'::uuid) \
               AND dst_id = GREATEST('{m2}'::uuid, '{m3}'::uuid) \
               AND association_type = 'hebbian'"
        ))
        .expect("query failed")
        .expect("null");

        assert_eq!(m2_m3, 0, "state-tier m2 should be excluded from Hebbian pairs");
    }
}
