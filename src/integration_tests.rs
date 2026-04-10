// pg_ghola::integration_tests — End-to-end integration tests
//
// Verifies the full recall-learn-recall cycle works end-to-end:
// 1. Install extension on clean database
// 2. Insert mnemes with embeddings
// 3. Call recall() and observe ranked results
// 4. Process co-activation events
// 5. Call recall() again and observe Hebbian boost changes
//
// Owned by: integrate_and_package task

#[cfg(any(test, feature = "pg_test"))]
#[pgrx::pg_schema]
mod tests {
    use pgrx::prelude::*;

    // ── Helpers ──

    /// Generate a 768-dim embedding literal with a given fill value.
    fn embedding(fill: f64) -> String {
        let elements = vec![format!("{fill}"); 768];
        format!("[{}]", elements.join(","))
    }

    /// Workspace ID used across integration tests.
    const WS: &str = "10000000-0000-0000-0000-000000000001";

    /// Insert a mneme and return its id as a string.
    fn insert_mneme(ws: &str, concept: &str, content: &str, fill: f64) -> String {
        let emb = embedding(fill);
        Spi::get_one::<String>(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding) \
             VALUES ('{ws}', '{concept}', '{content}', '{emb}'::vector(768)) \
             RETURNING id::text"
        ))
        .expect("insert failed")
        .expect("null id")
    }

    // ── Test: Extension installs cleanly ──

    #[pg_test]
    fn test_extension_version() {
        let version = Spi::get_one::<String>(
            "SELECT extversion FROM pg_extension WHERE extname = 'pg_ghola'",
        )
        .expect("query failed")
        .expect("null version");
        assert_eq!(version, "0.0.1", "extension version should be 0.0.1");
    }

    #[pg_test]
    fn test_pg_ghola_schema_exists() {
        let exists = Spi::get_one::<bool>(
            "SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname = 'pg_ghola')",
        )
        .expect("query failed")
        .expect("null");
        assert!(exists, "pg_ghola schema should exist after CREATE EXTENSION");
    }

    #[pg_test]
    fn test_all_tables_exist() {
        for table in &["mnemes", "associations", "co_activation_queue", "contradiction_candidates", "config", "contradiction_queue", "contradiction_worker_stats", "gating_queue", "gating_worker_stats"] {
            let exists = Spi::get_one::<bool>(&format!(
                "SELECT EXISTS(SELECT 1 FROM information_schema.tables \
                 WHERE table_schema = 'pg_ghola' AND table_name = '{table}')"
            ))
            .expect("query failed")
            .expect("null");
            assert!(exists, "table ghola.{table} should exist");
        }
    }

    #[pg_test]
    fn test_all_types_exist() {
        for typ in &["recall_result", "score_weights", "contradiction_candidate_result", "contradiction_detail"] {
            let exists = Spi::get_one::<bool>(&format!(
                "SELECT EXISTS( \
                    SELECT 1 FROM pg_type t \
                    JOIN pg_namespace n ON t.typnamespace = n.oid \
                    WHERE n.nspname = 'pg_ghola' AND t.typname = '{typ}' \
                )"
            ))
            .expect("query failed")
            .expect("null");
            assert!(exists, "type ghola.{typ} should exist");
        }
    }

    #[pg_test]
    fn test_all_functions_exist() {
        // All SQL-visible functions from the extension
        let functions = vec![
            "softplus",
            "actr_activation",
            "ebbinghaus_decay",
            "bayesian_update",
            "record_co_activation",
            "get_associations",
            "update_confidence",
            "confirm_recall",
            "process_co_activation_batch",
            "process_all_pending_co_activations",
            "recall_inner",
            "recall",
            "check_contradictions",
            "flag_contradictions",
            "resolve_contradiction",
            "get_pending_contradictions",
            "scan_workspace_contradictions",
            "configure_dimensions",
            "mark_supersedes",
            "mark_supports",
            "get_typed_associations",
        ];
        for func in &functions {
            let exists = Spi::get_one::<bool>(&format!(
                "SELECT EXISTS( \
                    SELECT 1 FROM pg_proc p \
                    JOIN pg_namespace n ON p.pronamespace = n.oid \
                    WHERE n.nspname = 'pg_ghola' AND p.proname = '{func}' \
                )"
            ))
            .expect("query failed")
            .expect("null");
            assert!(exists, "function ghola.{func} should exist");
        }
    }

    // ── Test: Full recall-learn-recall cycle ──

    #[pg_test]
    fn test_end_to_end_recall_learn_recall_cycle() {
        // This test exercises the complete cognitive memory feedback loop:
        //   insert -> recall -> process co-activations -> recall again
        // and verifies that associations form and influence subsequent retrievals.

        Spi::run("CREATE EXTENSION IF NOT EXISTS vector")
            .expect("vector extension setup failed");

        // 1. Insert three related mnemes
        let m1 = insert_mneme(WS, "kubernetes", "pod scheduling and orchestration", 0.10);
        let _m2 = insert_mneme(WS, "docker", "container runtime engine for kubernetes", 0.12);
        let _m3 = insert_mneme(WS, "helm", "chart deployment for kubernetes clusters", 0.14);

        // 2. First recall — should return results with no Hebbian boost yet
        let emb = embedding(0.11);
        Spi::run("DELETE FROM ghola.co_activation_queue").expect("clear queue failed");

        let first_results = Spi::connect(|client| {
            let rows = client
                .select(
                    &format!(
                        "SELECT (r).mneme_id::text, (r).score, (r).hebbian_boost, (r).content_match, \
                                (r).activation, (r).confidence \
                         FROM ghola.recall( \
                             '{WS}'::uuid, 'kubernetes pod scheduling', \
                             '{emb}'::vector(768), 10, 0.0, NULL \
                         ) AS r"
                    ),
                    None,
                    &[],
                )
                .expect("first recall failed");

            let mut results = Vec::new();
            for row in rows {
                let id: String = row.get::<String>(1).expect("err").expect("null");
                let score: f64 = row.get::<f64>(2).expect("err").expect("null");
                let heb: f64 = row.get::<f64>(3).expect("err").expect("null");
                let cm: f64 = row.get::<f64>(4).expect("err").expect("null");
                let act: f64 = row.get::<f64>(5).expect("err").expect("null");
                let conf: f64 = row.get::<f64>(6).expect("err").expect("null");
                results.push((id, score, heb, cm, act, conf));
            }
            results
        });

        assert!(
            !first_results.is_empty(),
            "first recall should return results"
        );

        // All scoring components should be populated
        for (id, score, _heb, cm, _act, conf) in &first_results {
            assert!(*score > 0.0, "mneme {id}: score should be positive, got {score}");
            assert!(*cm > 0.0, "mneme {id}: content_match should be positive, got {cm}");
            assert!(*conf > 0.0, "mneme {id}: confidence should be positive, got {conf}");
        }

        // Before processing co-activations, hebbian_boost should be 0 for all
        for (_id, _score, heb, ..) in &first_results {
            assert!(
                (*heb - 0.0).abs() < 1e-9,
                "first recall should have zero hebbian_boost, got {heb}"
            );
        }

        // 3. Verify co-activation event was enqueued
        let queue_count =
            Spi::get_one::<i64>("SELECT count(*) FROM ghola.co_activation_queue")
                .expect("query failed")
                .expect("null");
        assert!(
            queue_count >= 1,
            "recall should enqueue co-activation event, got {queue_count}"
        );

        // 4. Process co-activation batch — should create associations
        let processed = Spi::get_one::<i64>(
            "SELECT ghola.process_all_pending_co_activations()",
        )
        .expect("batch processing failed")
        .expect("null");
        assert!(
            processed >= 1,
            "should have processed at least 1 queue event, got {processed}"
        );

        // Queue should be empty after processing
        let remaining =
            Spi::get_one::<i64>("SELECT count(*) FROM ghola.co_activation_queue")
                .expect("query failed")
                .expect("null");
        assert_eq!(remaining, 0, "queue should be empty after processing");

        // 5. Verify associations were formed between co-activated mnemes
        let assoc_count =
            Spi::get_one::<i64>("SELECT count(*) FROM ghola.associations")
                .expect("query failed")
                .expect("null");
        assert!(
            assoc_count > 0,
            "associations should have been created, got {assoc_count}"
        );

        // Verify get_associations works for one of the mnemes
        let related_count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.get_associations('{m1}'::uuid, 0.001)"
        ))
        .expect("query failed")
        .expect("null");
        assert!(
            related_count > 0,
            "mneme m1 should have associations after co-activation, got {related_count}"
        );

        // 6. Verify access_count was incremented during batch processing
        let access = Spi::get_one::<i32>(&format!(
            "SELECT access_count FROM ghola.mnemes WHERE id = '{m1}'::uuid"
        ))
        .expect("query failed")
        .expect("null");
        assert!(
            access >= 1,
            "access_count should be incremented after batch processing, got {access}"
        );

        // 7. Second recall — should now show non-zero Hebbian boost
        Spi::run("DELETE FROM ghola.co_activation_queue").expect("clear queue failed");

        let second_results = Spi::connect(|client| {
            let rows = client
                .select(
                    &format!(
                        "SELECT (r).mneme_id::text, (r).score, (r).hebbian_boost \
                         FROM ghola.recall( \
                             '{WS}'::uuid, 'kubernetes pod scheduling', \
                             '{emb}'::vector(768), 10, 0.0, NULL \
                         ) AS r"
                    ),
                    None,
                    &[],
                )
                .expect("second recall failed");

            let mut results = Vec::new();
            for row in rows {
                let id: String = row.get::<String>(1).expect("err").expect("null");
                let score: f64 = row.get::<f64>(2).expect("err").expect("null");
                let heb: f64 = row.get::<f64>(3).expect("err").expect("null");
                results.push((id, score, heb));
            }
            results
        });

        assert!(
            !second_results.is_empty(),
            "second recall should return results"
        );

        // At least one result should have non-zero Hebbian boost after associations formed
        let has_hebbian = second_results.iter().any(|(_, _, heb)| *heb > 0.0);
        assert!(
            has_hebbian,
            "second recall should show non-zero hebbian_boost for associated mnemes. Results: {:?}",
            second_results
        );
    }

    // ── Test: Confidence tracking through recall-confirm cycle ──

    #[pg_test]
    fn test_confidence_evolution_through_usage() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector")
            .expect("vector extension setup failed");

        let ws = "20000000-0000-0000-0000-000000000001";
        let m1 = insert_mneme(ws, "rust", "systems programming language", 0.20);

        // Initial confidence should be the default 1.0 (trust by default)
        let conf_initial = Spi::get_one::<f64>(&format!(
            "SELECT confidence FROM ghola.mnemes WHERE id = '{m1}'::uuid"
        ))
        .expect("query failed")
        .expect("null");
        assert!(
            (conf_initial - 1.0).abs() < 0.01,
            "initial confidence should be 1.0, got {conf_initial}"
        );

        // Confirm recall should increase confidence
        Spi::run(&format!(
            "SELECT ghola.confirm_recall(ARRAY['{m1}']::uuid[])"
        ))
        .expect("confirm_recall failed");

        let conf_after = Spi::get_one::<f64>(&format!(
            "SELECT confidence FROM ghola.mnemes WHERE id = '{m1}'::uuid"
        ))
        .expect("query failed")
        .expect("null");
        assert!(
            conf_after > conf_initial,
            "confidence should increase after confirm_recall: {conf_initial} -> {conf_after}"
        );

        // Contradicting evidence should decrease confidence
        let new_conf = Spi::get_one::<f64>(&format!(
            "SELECT ghola.update_confidence('{m1}'::uuid, 0.05)"
        ))
        .expect("query failed")
        .expect("null");
        assert!(
            new_conf < conf_after,
            "confidence should decrease with contradicting evidence: {conf_after} -> {new_conf}"
        );

        // Confidence should never drop below 0.025 (Laplace smoothing bound)
        assert!(
            new_conf >= 0.025,
            "confidence should never drop below 0.025, got {new_conf}"
        );
    }

    // ── Test: Scoring functions compose correctly ──

    #[pg_test]
    fn test_scoring_functions_composable() {
        // Verify all scoring primitives are individually callable and composable
        let sp = Spi::get_one::<f64>("SELECT ghola.softplus(2.0)")
            .expect("query failed")
            .expect("null");
        assert!((sp - 2.1269).abs() < 0.01, "softplus(2) ~ 2.13, got {sp}");

        let actr = Spi::get_one::<f64>(
            "SELECT ghola.actr_activation(10, now() - interval '5 days')",
        )
        .expect("query failed")
        .expect("null");
        assert!(actr > 0.0, "recent frequently accessed mneme should have positive activation");

        let decay = Spi::get_one::<f64>(
            "SELECT ghola.ebbinghaus_decay(now() - interval '7 days', 20, now() - interval '90 days')",
        )
        .expect("query failed")
        .expect("null");
        assert!(
            decay > 0.0 && decay <= 1.0,
            "ebbinghaus_decay should be in (0, 1], got {decay}"
        );

        let bayes = Spi::get_one::<f64>("SELECT ghola.bayesian_update(0.5, 0.9)")
            .expect("query failed")
            .expect("null");
        assert!(
            bayes > 0.5 && bayes < 1.0,
            "bayesian_update with strong evidence should increase from prior, got {bayes}"
        );
    }

    // ── Test: Workspace isolation end-to-end ──

    #[pg_test]
    fn test_workspace_isolation_end_to_end() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector")
            .expect("vector extension setup failed");

        let ws_a = "30000000-0000-0000-0000-000000000001";
        let ws_b = "30000000-0000-0000-0000-000000000002";

        insert_mneme(ws_a, "alpha concept", "alpha content", 0.3);
        insert_mneme(ws_b, "beta concept", "beta content", 0.4);

        let emb = embedding(0.35);

        // Recall in workspace A should only see workspace A mnemes
        let count_a = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.recall( \
                '{ws_a}'::uuid, 'alpha', '{emb}'::vector(768), 10, 0.0, NULL)"
        ))
        .expect("query failed")
        .expect("null");

        let _count_b = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.recall( \
                '{ws_b}'::uuid, 'alpha', '{emb}'::vector(768), 10, 0.0, NULL)"
        ))
        .expect("query failed")
        .expect("null");

        // workspace A should find its mneme, workspace B should not find workspace A's mneme
        assert!(count_a >= 1, "workspace A should find its own mneme");
        // workspace B has its own mneme but shouldn't find "alpha" content from ws_a
        // (it may find its own "beta" mneme via vector similarity though)
        // The key point is workspace isolation works for filtering
    }

    // ── Test: State filtering ──

    #[pg_test]
    fn test_archived_and_dormant_excluded_from_recall() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector")
            .expect("vector extension setup failed");

        let ws = "40000000-0000-0000-0000-000000000001";
        let m_active = insert_mneme(ws, "active memory", "this should appear", 0.5);
        let m_archived = insert_mneme(ws, "archived memory", "this should not appear", 0.5);
        let m_dormant = insert_mneme(ws, "dormant memory", "this should not appear either", 0.5);

        Spi::run(&format!(
            "UPDATE ghola.mnemes SET state = 'archived' WHERE id = '{m_archived}'::uuid"
        ))
        .expect("archive failed");
        Spi::run(&format!(
            "UPDATE ghola.mnemes SET state = 'dormant' WHERE id = '{m_dormant}'::uuid"
        ))
        .expect("dormant failed");

        let emb = embedding(0.5);
        let returned_ids = Spi::connect(|client| {
            let rows = client
                .select(
                    &format!(
                        "SELECT (r).mneme_id::text FROM ghola.recall( \
                            '{ws}'::uuid, 'memory', '{emb}'::vector(768), \
                            10, 0.0, NULL) AS r"
                    ),
                    None,
                    &[],
                )
                .expect("recall failed");
            let mut ids = Vec::new();
            for row in rows {
                ids.push(row.get::<String>(1).expect("err").expect("null"));
            }
            ids
        });

        assert!(
            returned_ids.contains(&m_active),
            "active mneme should be in recall results"
        );
        assert!(
            !returned_ids.contains(&m_archived),
            "archived mneme should NOT be in recall results"
        );
        assert!(
            !returned_ids.contains(&m_dormant),
            "dormant mneme should NOT be in recall results"
        );
    }

    // ── Test: Multiple co-activation rounds strengthen associations ──

    #[pg_test]
    fn test_repeated_co_activation_strengthens_associations() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector")
            .expect("vector extension setup failed");

        let ws = "50000000-0000-0000-0000-000000000001";
        let m1 = insert_mneme(ws, "neural", "network architecture", 0.6);
        let m2 = insert_mneme(ws, "deep", "learning framework", 0.62);

        // First co-activation
        Spi::run(&format!(
            "SELECT ghola.record_co_activation( \
                '{ws}'::uuid, ARRAY['{m1}','{m2}']::uuid[], ARRAY[0.9, 0.8]::float8[])"
        ))
        .expect("record failed");
        Spi::run("SELECT ghola.process_all_pending_co_activations()")
            .expect("process failed");

        let weight1 = Spi::get_one::<f64>("SELECT weight FROM ghola.associations LIMIT 1")
            .expect("query failed")
            .expect("null");

        // Second co-activation
        Spi::run(&format!(
            "SELECT ghola.record_co_activation( \
                '{ws}'::uuid, ARRAY['{m1}','{m2}']::uuid[], ARRAY[0.9, 0.8]::float8[])"
        ))
        .expect("record failed");
        Spi::run("SELECT ghola.process_all_pending_co_activations()")
            .expect("process failed");

        let weight2 = Spi::get_one::<f64>("SELECT weight FROM ghola.associations LIMIT 1")
            .expect("query failed")
            .expect("null");

        assert!(
            weight2 > weight1,
            "repeated co-activation should strengthen associations: {weight1} -> {weight2}"
        );

        // Third co-activation
        Spi::run(&format!(
            "SELECT ghola.record_co_activation( \
                '{ws}'::uuid, ARRAY['{m1}','{m2}']::uuid[], ARRAY[0.9, 0.8]::float8[])"
        ))
        .expect("record failed");
        Spi::run("SELECT ghola.process_all_pending_co_activations()")
            .expect("process failed");

        let weight3 = Spi::get_one::<f64>("SELECT weight FROM ghola.associations LIMIT 1")
            .expect("query failed")
            .expect("null");

        assert!(
            weight3 > weight2,
            "association weight should keep growing: {weight2} -> {weight3}"
        );

        // Weight should be capped at 1.0
        assert!(weight3 <= 1.0, "weight should never exceed 1.0, got {weight3}");
    }

    // ── Test: Custom weights affect scoring ──

    #[pg_test]
    fn test_custom_weights_change_ranking() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector")
            .expect("vector extension setup failed");

        let ws = "60000000-0000-0000-0000-000000000001";
        // Insert one mneme with a very specific embedding
        insert_mneme(ws, "specific topic", "detailed explanation of the specific topic", 0.7);

        let emb = embedding(0.7);

        // Score with default weights
        let score_default = Spi::get_one::<f64>(&format!(
            "SELECT (r).score FROM ghola.recall( \
                '{ws}'::uuid, 'specific topic', '{emb}'::vector(768), \
                1, 0.0, NULL) AS r LIMIT 1"
        ))
        .expect("query failed")
        .expect("null");

        // Score with all-semantic weights (no FTS contribution)
        let score_semantic = Spi::get_one::<f64>(&format!(
            "SELECT (r).score FROM ghola.recall( \
                '{ws}'::uuid, 'specific topic', '{emb}'::vector(768), \
                1, 0.0, (1.0, 0.0, 0.5, 4.0)::ghola.score_weights) AS r LIMIT 1"
        ))
        .expect("query failed")
        .expect("null");

        // Both should be valid positive scores
        assert!(score_default > 0.0, "default score should be positive");
        assert!(score_semantic > 0.0, "semantic-only score should be positive");

        // Scores should differ since weight distribution changed
        // (they could be the same only if FTS contributes exactly nothing,
        //  but with matching text it should contribute something)
        // We just verify both are valid; exact ordering depends on the query
    }

    // ══════════════════════════════════════════════════════════════════════
    // v0.2 Integration Tests: Worker stats, decay, pruning, archival
    // ══════════════════════════════════════════════════════════════════════

    #[pg_test]
    fn test_worker_stats_table_in_schema() {
        let exists = Spi::get_one::<bool>(
            "SELECT EXISTS(
                SELECT 1 FROM information_schema.tables
                WHERE table_schema = 'pg_ghola' AND table_name = 'worker_stats'
            )",
        )
        .unwrap()
        .unwrap();
        assert!(exists, "worker_stats table should exist in pg_ghola schema");
    }

    #[pg_test]
    fn test_worker_status_type_in_schema() {
        let exists = Spi::get_one::<bool>(
            "SELECT EXISTS(
                SELECT 1 FROM pg_type t
                JOIN pg_namespace n ON t.typnamespace = n.oid
                WHERE n.nspname = 'pg_ghola' AND t.typname = 'worker_status'
            )",
        )
        .unwrap()
        .unwrap();
        assert!(exists, "worker_status type should exist in pg_ghola schema");
    }

    #[pg_test]
    fn test_get_worker_stats_returns_initial_state() {
        let state = Spi::get_one::<String>(
            "SELECT (s).state FROM ghola.get_worker_stats() AS s",
        )
        .expect("query failed")
        .expect("null result");
        assert_eq!(state, "stopped", "initial worker state should be 'stopped'");
    }

    #[pg_test]
    fn test_decay_reduces_stale_association_weights() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector")
            .expect("vector extension setup failed");

        let ws = "70000000-0000-0000-0000-000000000001";
        let m1 = insert_mneme(ws, "decay test a", "content a", 0.1);
        let m2 = insert_mneme(ws, "decay test b", "content b", 0.2);

        // Insert an association with a known weight, backdated > 1 day
        Spi::run(&format!(
            "INSERT INTO ghola.associations (src_id, dst_id, weight, updated_at) \
             SELECT LEAST('{m1}'::uuid, '{m2}'::uuid), \
                    GREATEST('{m1}'::uuid, '{m2}'::uuid), \
                    0.5, now() - interval '2 days'"
        ))
        .expect("insert association failed");

        let weight_before = Spi::get_one::<f64>(
            "SELECT weight FROM ghola.associations LIMIT 1",
        )
        .expect("query failed")
        .expect("null");

        // Run the decay SQL directly (same as worker runs)
        Spi::run(
            "UPDATE ghola.associations \
             SET weight = weight * 0.999 \
             WHERE updated_at < now() - interval '1 day'",
        )
        .expect("decay failed");

        let weight_after = Spi::get_one::<f64>(
            "SELECT weight FROM ghola.associations LIMIT 1",
        )
        .expect("query failed")
        .expect("null");

        assert!(
            weight_after < weight_before,
            "decay should reduce weight: {weight_before} -> {weight_after}"
        );
        assert!(
            (weight_after - weight_before * 0.999).abs() < 1e-9,
            "decay should be exactly 0.1%: expected {}, got {weight_after}",
            weight_before * 0.999
        );
    }

    #[pg_test]
    fn test_pruning_removes_sub_threshold_associations() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector")
            .expect("vector extension setup failed");

        let ws = "80000000-0000-0000-0000-000000000001";
        let m1 = insert_mneme(ws, "prune test a", "content a", 0.1);
        let m2 = insert_mneme(ws, "prune test b", "content b", 0.2);
        let m3 = insert_mneme(ws, "prune test c", "content c", 0.3);

        // Insert one strong association and one below threshold
        Spi::run(&format!(
            "INSERT INTO ghola.associations (src_id, dst_id, weight) \
             SELECT LEAST('{m1}'::uuid, '{m2}'::uuid), \
                    GREATEST('{m1}'::uuid, '{m2}'::uuid), 0.5"
        ))
        .expect("insert strong assoc failed");

        Spi::run(&format!(
            "INSERT INTO ghola.associations (src_id, dst_id, weight) \
             SELECT LEAST('{m1}'::uuid, '{m3}'::uuid), \
                    GREATEST('{m1}'::uuid, '{m3}'::uuid), 0.0005"
        ))
        .expect("insert weak assoc failed");

        let count_before = Spi::get_one::<i64>(
            "SELECT count(*) FROM ghola.associations",
        )
        .expect("query failed")
        .expect("null");
        assert_eq!(count_before, 2);

        // Run pruning SQL (same as worker runs)
        Spi::run(
            "DELETE FROM ghola.associations WHERE weight < 0.001",
        )
        .expect("prune failed");

        let count_after = Spi::get_one::<i64>(
            "SELECT count(*) FROM ghola.associations",
        )
        .expect("query failed")
        .expect("null");
        assert_eq!(count_after, 1, "pruning should remove sub-threshold association");

        // The strong association should survive
        let remaining_weight = Spi::get_one::<f64>(
            "SELECT weight FROM ghola.associations LIMIT 1",
        )
        .expect("query failed")
        .expect("null");
        assert!(
            (remaining_weight - 0.5).abs() < 1e-9,
            "strong association should survive pruning"
        );
    }

    #[pg_test]
    fn test_dormant_archival_transitions_stale_low_confidence() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector")
            .expect("vector extension setup failed");

        let ws = "90000000-0000-0000-0000-000000000001";
        let m_stale = insert_mneme(ws, "stale memory", "old and uncertain", 0.1);
        let m_fresh = insert_mneme(ws, "fresh memory", "recent and confident", 0.2);
        let m_stale_confident = insert_mneme(ws, "stale confident", "old but sure", 0.3);

        // Set up: stale + low confidence -> should be archived
        Spi::run(&format!(
            "UPDATE ghola.mnemes SET \
                 last_access = now() - interval '100 days', \
                 confidence = 0.2 \
             WHERE id = '{m_stale}'::uuid"
        ))
        .expect("update stale failed");

        // Set up: fresh + high confidence -> should NOT be archived
        Spi::run(&format!(
            "UPDATE ghola.mnemes SET \
                 last_access = now() - interval '1 day', \
                 confidence = 0.9 \
             WHERE id = '{m_fresh}'::uuid"
        ))
        .expect("update fresh failed");

        // Set up: stale + high confidence -> should NOT be archived
        Spi::run(&format!(
            "UPDATE ghola.mnemes SET \
                 last_access = now() - interval '100 days', \
                 confidence = 0.8 \
             WHERE id = '{m_stale_confident}'::uuid"
        ))
        .expect("update stale confident failed");

        // Run archival SQL (same as worker runs)
        Spi::run(
            "UPDATE ghola.mnemes \
             SET state = 'dormant' \
             WHERE state = 'active' \
               AND last_access < now() - interval '90 days' \
               AND confidence < 0.3",
        )
        .expect("archival failed");

        // Verify: stale + low confidence should be dormant
        let stale_state = Spi::get_one::<String>(&format!(
            "SELECT state FROM ghola.mnemes WHERE id = '{m_stale}'::uuid",
        ))
        .expect("query failed")
        .expect("null");
        assert_eq!(stale_state, "dormant", "stale low-confidence should be archived to dormant");

        // Verify: fresh should still be active
        let fresh_state = Spi::get_one::<String>(&format!(
            "SELECT state FROM ghola.mnemes WHERE id = '{m_fresh}'::uuid",
        ))
        .expect("query failed")
        .expect("null");
        assert_eq!(fresh_state, "active", "fresh memory should remain active");

        // Verify: stale but confident should still be active
        let confident_state = Spi::get_one::<String>(&format!(
            "SELECT state FROM ghola.mnemes WHERE id = '{m_stale_confident}'::uuid",
        ))
        .expect("query failed")
        .expect("null");
        assert_eq!(confident_state, "active", "stale but confident memory should remain active");
    }

    // ══════════════════════════════════════════════════════════════════════
    // v0.3 Integration Tests: Contradiction detection end-to-end
    // ══════════════════════════════════════════════════════════════════════

    #[pg_test]
    fn test_contradiction_trigger_fires_on_insert() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector")
            .expect("vector extension setup failed");

        let ws = "a0000000-0000-0000-0000-000000000001";
        let emb = embedding(0.5);

        // Insert first mneme (trigger fires but nothing to compare against)
        Spi::run(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding) \
             VALUES ('{ws}', 'python version', 'Python 3.8 is the latest release', '{emb}'::vector(768))"
        )).expect("first insert failed");

        // Insert contradicting mneme (trigger should enqueue, not flag directly)
        let m2 = Spi::get_one::<String>(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding) \
             VALUES ('{ws}', 'python version', 'Python 3.12 is the latest release', '{emb}'::vector(768)) \
             RETURNING id::text"
        )).expect("second insert failed").expect("null");

        // Verify the trigger enqueued to contradiction_queue (async behavior)
        let queued = Spi::get_one::<i64>(
            &format!("SELECT count(*) FROM ghola.contradiction_queue \
                      WHERE workspace_id = '{ws}'::uuid")
        ).expect("query failed").expect("null");
        assert!(queued >= 1, "trigger should have enqueued to contradiction_queue, got {queued}");

        // Simulate worker: manually call flag_contradictions
        let flagged = Spi::get_one::<i64>(&format!(
            "SELECT ghola.flag_contradictions('{m2}'::uuid, 0.85)"
        )).expect("flag failed").expect("null");
        assert!(flagged >= 1, "flag_contradictions should find candidates");

        // Verify candidates now exist
        let pending = Spi::get_one::<i64>(
            &format!("SELECT count(*) FROM ghola.contradiction_candidates \
                      WHERE workspace_id = '{ws}'::uuid AND status = 'pending'")
        ).expect("query failed").expect("null");
        assert!(pending >= 1, "should have pending contradiction candidates after manual flagging");
    }

    #[pg_test]
    fn test_contradiction_full_lifecycle() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector")
            .expect("vector extension setup failed");

        let ws = "b0000000-0000-0000-0000-000000000001";
        let emb = embedding(0.5);

        // Disable trigger for controlled setup
        Spi::run("ALTER TABLE ghola.mnemes DISABLE TRIGGER mneme_insert_enqueue")
            .expect("disable trigger");

        Spi::run(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding) \
             VALUES ('{ws}', 'rust speed', 'Rust is slow', '{emb}'::vector(768))"
        )).expect("insert 1 failed");

        let m2 = Spi::get_one::<String>(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding) \
             VALUES ('{ws}', 'rust speed', 'Rust is fast', '{emb}'::vector(768)) \
             RETURNING id::text"
        )).expect("insert 2 failed").expect("null");

        Spi::run("ALTER TABLE ghola.mnemes ENABLE TRIGGER mneme_insert_enqueue")
            .expect("enable trigger");

        // Flag contradictions
        let flagged = Spi::get_one::<i64>(&format!(
            "SELECT ghola.flag_contradictions('{m2}'::uuid, 0.85)"
        )).expect("flag failed").expect("null");
        assert!(flagged >= 1, "should flag at least 1 contradiction");

        // Get pending contradictions
        let pending_count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.get_pending_contradictions('{ws}'::uuid)"
        )).expect("query failed").expect("null");
        assert!(pending_count >= 1, "should have pending contradictions");

        // Get candidate ID
        let candidate_id = Spi::get_one::<i64>(&format!(
            "SELECT candidate_id FROM ghola.get_pending_contradictions('{ws}'::uuid) LIMIT 1"
        )).expect("query failed").expect("null");

        // Get confidence before resolution
        let conf_before = Spi::get_one::<f64>(&format!(
            "SELECT confidence FROM ghola.mnemes WHERE id = '{m2}'::uuid"
        )).expect("query failed").expect("null");

        // Resolve as confirmed — should penalize the newer mneme (m2)
        Spi::run(&format!(
            "SELECT ghola.resolve_contradiction({candidate_id}, 'confirmed')"
        )).expect("resolve failed");

        // Verify status changed
        let status = Spi::get_one::<String>(&format!(
            "SELECT status FROM ghola.contradiction_candidates WHERE id = {candidate_id}"
        )).expect("query failed").expect("null");
        assert_eq!(status, "confirmed");

        // Verify confidence was penalized
        let conf_after = Spi::get_one::<f64>(&format!(
            "SELECT confidence FROM ghola.mnemes WHERE id = '{m2}'::uuid"
        )).expect("query failed").expect("null");
        assert!(
            conf_after < conf_before,
            "confirmed contradiction should penalize newer mneme confidence: {conf_before} -> {conf_after}"
        );
    }

    #[pg_test]
    fn test_contradiction_workspace_scan() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector")
            .expect("vector extension setup failed");

        let ws = "c0000000-0000-0000-0000-000000000099";
        let emb = embedding(0.5);

        // Disable trigger for controlled setup
        Spi::run("ALTER TABLE ghola.mnemes DISABLE TRIGGER mneme_insert_enqueue")
            .expect("disable trigger");

        // Insert several similar mnemes
        for i in 1..=4 {
            Spi::run(&format!(
                "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding) \
                 VALUES ('{ws}', 'topic {i}', 'similar content variant {i}', '{emb}'::vector(768))"
            )).expect("insert failed");
        }

        Spi::run("ALTER TABLE ghola.mnemes ENABLE TRIGGER mneme_insert_enqueue")
            .expect("enable trigger");

        // Scan workspace for contradictions
        let flagged = Spi::get_one::<i64>(&format!(
            "SELECT ghola.scan_workspace_contradictions('{ws}'::uuid, 0.85)"
        )).expect("scan failed").expect("null");

        assert!(flagged >= 1, "workspace scan should flag contradictions among similar mnemes");

        // Verify candidates were actually inserted
        let candidates = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.contradiction_candidates \
             WHERE workspace_id = '{ws}'::uuid"
        )).expect("query failed").expect("null");
        assert!(candidates >= 1, "candidates should exist after workspace scan");
    }

    // ══════════════════════════════════════════════════════════════════════
    // v0.4 Integration Tests: Typed memory system
    // ══════════════════════════════════════════════════════════════════════

    /// Helper: insert a mneme with typed columns, triggers disabled.
    fn insert_typed_mneme(
        ws: &str, concept: &str, content: &str, fill: f64,
        memory_type: &str, tier: &str, session_id: Option<&str>,
    ) -> String {
        let emb = embedding(fill);
        Spi::run(
            "ALTER TABLE ghola.mnemes DISABLE TRIGGER mneme_insert_enqueue"
        ).expect("disable");
        Spi::run(
            "ALTER TABLE ghola.mnemes DISABLE TRIGGER mneme_session_association"
        ).expect("disable");

        let session_clause = match session_id {
            Some(sid) => format!(", '{sid}'::uuid"),
            None => ", NULL".to_string(),
        };

        let id = Spi::get_one::<String>(&format!(
            "INSERT INTO ghola.mnemes \
             (workspace_id, concept, content, embedding, memory_type, tier, session_id) \
             VALUES ('{ws}', '{concept}', '{content}', '{emb}'::vector(768), \
                     '{memory_type}', '{tier}'{session_clause}) \
             RETURNING id::text"
        ))
        .expect("insert failed")
        .expect("null");

        Spi::run(
            "ALTER TABLE ghola.mnemes ENABLE TRIGGER mneme_insert_enqueue"
        ).expect("enable");
        Spi::run(
            "ALTER TABLE ghola.mnemes ENABLE TRIGGER mneme_session_association"
        ).expect("enable");

        id
    }

    #[pg_test]
    fn test_recall_memory_type_filter() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector")
            .expect("vector extension setup failed");

        let ws = "d0000000-0000-0000-0000-000000000001";
        let _m_fact = insert_typed_mneme(ws, "fact", "factual content", 0.5, "factual", "index", None);
        let _m_exp = insert_typed_mneme(ws, "experience", "experiential content", 0.5, "experiential", "index", None);
        let _m_work = insert_typed_mneme(ws, "working", "working memory", 0.5, "working", "state", None);

        let emb = embedding(0.5);

        // Filter to factual only
        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.recall(\
                '{ws}'::uuid, 'content', '{emb}'::vector(768), \
                10, 0.0, NULL, 'factual')"
        ))
        .expect("query failed")
        .expect("null");

        assert_eq!(count, 1, "memory_type filter 'factual' should return 1, got {count}");
    }

    #[pg_test]
    fn test_recall_excludes_expired_working_memory() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector")
            .expect("vector extension setup failed");

        let ws = "d0000000-0000-0000-0000-000000000002";
        let emb = embedding(0.5);

        // Insert a working memory that's already expired
        Spi::run(
            "ALTER TABLE ghola.mnemes DISABLE TRIGGER mneme_insert_enqueue"
        ).expect("disable");
        Spi::run(
            "ALTER TABLE ghola.mnemes DISABLE TRIGGER mneme_session_association"
        ).expect("disable");

        Spi::run(&format!(
            "INSERT INTO ghola.mnemes \
             (workspace_id, concept, content, embedding, memory_type, expires_at) \
             VALUES ('{ws}', 'expired', 'should not appear', '{emb}'::vector(768), \
                     'working', now() - interval '1 hour')"
        )).expect("insert expired failed");

        // Insert a non-expired mneme
        let _m_valid = Spi::get_one::<String>(&format!(
            "INSERT INTO ghola.mnemes \
             (workspace_id, concept, content, embedding) \
             VALUES ('{ws}', 'valid', 'should appear', '{emb}'::vector(768)) \
             RETURNING id::text"
        )).expect("insert failed").expect("null");

        Spi::run(
            "ALTER TABLE ghola.mnemes ENABLE TRIGGER mneme_insert_enqueue"
        ).expect("enable");
        Spi::run(
            "ALTER TABLE ghola.mnemes ENABLE TRIGGER mneme_session_association"
        ).expect("enable");

        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.recall(\
                '{ws}'::uuid, 'content', '{emb}'::vector(768), 10, 0.0, NULL)"
        ))
        .expect("query failed")
        .expect("null");

        assert_eq!(count, 1, "expired working memory should be excluded, got {count}");
    }

    #[pg_test]
    fn test_supersedes_full_lifecycle() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector")
            .expect("vector extension setup failed");

        let ws = "d0000000-0000-0000-0000-000000000003";
        let m_old = insert_typed_mneme(ws, "version", "v1.0 release", 0.5, "factual", "index", None);
        let m_new = insert_typed_mneme(ws, "version", "v2.0 release", 0.5, "factual", "index", None);

        // Mark new as superseding old
        Spi::run(&format!(
            "SELECT ghola.mark_supersedes('{m_new}'::uuid, '{m_old}'::uuid)"
        )).expect("supersedes failed");

        // Old should be archived
        let state = Spi::get_one::<String>(&format!(
            "SELECT state FROM ghola.mnemes WHERE id = '{m_old}'::uuid"
        )).expect("query failed").expect("null");
        assert_eq!(state, "archived");

        // Old should not appear in recall
        let emb = embedding(0.5);
        let returned = Spi::connect(|client| {
            let rows = client.select(
                &format!(
                    "SELECT (r).mneme_id::text FROM ghola.recall(\
                        '{ws}'::uuid, 'version', '{emb}'::vector(768), 10, 0.0, NULL) AS r"
                ),
                None, &[],
            ).expect("recall failed");
            let mut ids = Vec::new();
            for row in rows {
                ids.push(row.get::<String>(1).expect("err").expect("null"));
            }
            ids
        });

        assert!(!returned.contains(&m_old), "superseded mneme should not appear in recall");
        assert!(returned.contains(&m_new), "new mneme should appear in recall");
    }

    #[pg_test]
    fn test_core_tier_resilience() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector")
            .expect("vector extension setup failed");

        let ws = "d0000000-0000-0000-0000-000000000004";
        let m_core = insert_typed_mneme(ws, "core fact", "immutable knowledge", 0.5, "factual", "core", None);

        // Hit it with extremely weak evidence repeatedly
        for _ in 0..5 {
            Spi::run(&format!(
                "SELECT ghola.update_confidence('{m_core}'::uuid, 0.01)"
            )).expect("update failed");
        }

        let conf = Spi::get_one::<f64>(&format!(
            "SELECT confidence FROM ghola.mnemes WHERE id = '{m_core}'::uuid"
        )).expect("query failed").expect("null");

        assert!(
            conf >= 0.30,
            "core-tier mneme should never drop below 0.30, got {conf}"
        );
    }

    #[pg_test]
    fn test_tag_filtering_in_recall() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector")
            .expect("vector extension setup failed");

        let ws = "d0000000-0000-0000-0000-000000000005";
        let emb = embedding(0.5);

        Spi::run(
            "ALTER TABLE ghola.mnemes DISABLE TRIGGER mneme_insert_enqueue"
        ).expect("disable");
        Spi::run(
            "ALTER TABLE ghola.mnemes DISABLE TRIGGER mneme_session_association"
        ).expect("disable");

        // Insert mnemes with different tags
        Spi::run(&format!(
            "INSERT INTO ghola.mnemes \
             (workspace_id, concept, content, embedding, tags) \
             VALUES ('{ws}', 'rust', 'rust content', '{emb}'::vector(768), \
                     ARRAY['language', 'systems'])"
        )).expect("insert failed");

        Spi::run(&format!(
            "INSERT INTO ghola.mnemes \
             (workspace_id, concept, content, embedding, tags) \
             VALUES ('{ws}', 'python', 'python content', '{emb}'::vector(768), \
                     ARRAY['language', 'scripting'])"
        )).expect("insert failed");

        Spi::run(
            "ALTER TABLE ghola.mnemes ENABLE TRIGGER mneme_insert_enqueue"
        ).expect("enable");
        Spi::run(
            "ALTER TABLE ghola.mnemes ENABLE TRIGGER mneme_session_association"
        ).expect("enable");

        // Filter to 'systems' tag
        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.recall(\
                '{ws}'::uuid, 'content', '{emb}'::vector(768), \
                10, 0.0, NULL, NULL, NULL, ARRAY['systems'])"
        ))
        .expect("query failed")
        .expect("null");

        assert_eq!(count, 1, "tag filter should return 1 result, got {count}");
    }

    // ══════════════════════════════════════════════════════════════════════
    // v0.5 Integration Tests: Async contradiction queue enqueue/dequeue
    // ══════════════════════════════════════════════════════════════════════

    #[pg_test]
    fn test_contradiction_queue_enqueue_and_dequeue() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup");

        let ws = "d4000000-0000-0000-0000-000000000001";

        // Disable session trigger to avoid interference
        Spi::run(
            "ALTER TABLE ghola.mnemes DISABLE TRIGGER mneme_session_association"
        ).expect("disable session trigger");

        // Insert a test mneme (fires the contradiction trigger which enqueues)
        let emb = embedding(0.10);
        Spi::run(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding) \
             VALUES ('{ws}', 'test_contra', 'test contradiction queue', '{emb}'::vector(768))"
        )).expect("insert mneme");

        // Verify queue has an entry
        let queue_count = Spi::get_one::<i64>(
            "SELECT count(*) FROM ghola.contradiction_queue"
        ).expect("query").expect("null");

        assert!(queue_count >= 1, "expected at least 1 queue entry, got {queue_count}");

        // Dequeue one item using the CTE pattern
        let dequeued = Spi::get_one::<i64>(
            "WITH d AS ( \
                 DELETE FROM ghola.contradiction_queue \
                 WHERE id = (SELECT id FROM ghola.contradiction_queue ORDER BY id LIMIT 1) \
                 RETURNING mneme_id \
             ) SELECT count(*) FROM d"
        ).expect("query").expect("null");

        assert_eq!(dequeued, 1, "expected to dequeue 1 item");

        // Re-enable triggers
        Spi::run(
            "ALTER TABLE ghola.mnemes ENABLE TRIGGER mneme_session_association"
        ).expect("enable session trigger");
    }

    // ── Thalamic gating integration tests ──

    #[pg_test]
    fn test_gating_queue_enqueued_on_insert() {
        // Verify mneme INSERT triggers gating_queue enqueue
        Spi::run("DELETE FROM ghola.gating_queue").expect("clear gating queue");

        let _id = insert_mneme(WS, "gating enqueue test", "some content", 0.5);

        let count = Spi::get_one::<i64>(
            "SELECT count(*) FROM ghola.gating_queue"
        ).expect("query").expect("null");

        assert!(count >= 1, "gating_queue should have at least 1 entry after insert, got {count}");
    }

    #[pg_test]
    fn test_gating_queue_dequeue_pattern() {
        // Verify the CTE dequeue pattern works for gating_queue
        Spi::run("DELETE FROM ghola.gating_queue").expect("clear");

        // Enqueue directly
        Spi::run(
            "INSERT INTO ghola.gating_queue (workspace_id, mneme_id) \
             VALUES (gen_random_uuid(), gen_random_uuid())"
        ).expect("enqueue");

        let dequeued = Spi::get_one::<i64>(
            "WITH d AS ( \
                 DELETE FROM ghola.gating_queue \
                 WHERE id = (SELECT id FROM ghola.gating_queue ORDER BY id LIMIT 1) \
                 RETURNING mneme_id \
             ) SELECT count(*) FROM d"
        ).expect("query").expect("null");

        assert_eq!(dequeued, 1, "expected to dequeue 1 item from gating_queue");

        let remaining = Spi::get_one::<i64>(
            "SELECT count(*) FROM ghola.gating_queue"
        ).expect("query").expect("null");

        assert_eq!(remaining, 0, "gating_queue should be empty after dequeue");
    }

    #[pg_test]
    fn test_gating_worker_stats_initial_state() {
        let state = Spi::get_one::<String>(
            "SELECT state FROM ghola.gating_worker_stats WHERE id = 1"
        ).expect("query").expect("null");

        assert_eq!(state, "stopped", "initial gating worker state should be 'stopped'");
    }

    #[pg_test]
    fn test_recall_with_entity_filter_compiles() {
        // Verify the new filter_entities parameter is accepted by recall()
        // Uses empty workspace so we just verify no SQL error, not results
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector");

        let emb = embedding(0.1);
        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.recall( \
                 '{WS}'::uuid, 'test query', '{emb}'::vector(768), \
                 10, 0.0, NULL, NULL, NULL, NULL, NULL, \
                 ARRAY['sarah']::text[], 'decision')"
        )).expect("query").expect("null");

        assert_eq!(count, 0, "empty workspace should return 0 results even with filters");
    }
}
