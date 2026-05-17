// ghola::integration_tests — v2 end-to-end pg_test coverage
//
// Five tests, one per behavior that the greenfield design doc calls
// out as load-bearing for Phase 1:
//
//   1. insert_then_recall_roundtrip
//      — a mneme inserted into semantic.mnemes resurfaces from
//        semantic.recall.
//
//   2. hebbian_weight_update_from_queue
//      — recall enqueues (src_id, dst_id) pairs into
//        semantic.co_activation_queue; process_co_activation_batch
//        folds them into semantic.associations with type='hebbian'
//        and weight > the 0.01 seed.
//
//   3. flag_contradictions_detects_divergent_pairs
//      — two mnemes with identical embeddings but different content
//        are flagged; a pending row appears in
//        semantic.contradiction_candidates.
//
//   4. actr_activation_decays_with_age
//      — SELECT semantic.actr_activation(n, ts) returns a smaller
//        value for older timestamps, same access_count.
//
//   5. archival_flips_stale_low_confidence_mnemes
//      — the archival SQL from consolidation_worker::run_archival
//        flips state 'active' -> 'archived' for old + low-confidence
//        rows, while leaving recent or confident rows active.

#[cfg(any(test, feature = "pg_test"))]
#[pgrx::pg_schema]
mod tests {
    use pgrx::prelude::*;

    // v2 default embedding dim (Qwen3-Embedding).
    const DIMS: usize = 1024;

    /// An all-zero 1024-dim embedding — cheapest valid vector literal.
    fn zero_embedding() -> String {
        let zeros: Vec<&str> = vec!["0"; DIMS];
        format!("[{}]", zeros.join(","))
    }

    /// A 1024-dim embedding with 1.0 in dims [start..start+span) and
    /// 0.0 elsewhere. Produces vectors with known cosine similarity
    /// based on dimensional overlap.
    fn spike_embedding(start: usize, span: usize) -> String {
        let mut v = vec!["0".to_string(); DIMS];
        for i in start..(start + span).min(DIMS) {
            v[i] = "1".to_string();
        }
        format!("[{}]", v.join(","))
    }

    /// Install pgvector (prerequisite) and return the workspace id.
    /// Each test uses its own workspace so run order doesn't matter.
    fn ws_setup(ws: &str) -> &str {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector")
            .expect("failed to create vector extension");
        ws
    }

    /// Insert a mneme with the given embedding and return its id.
    fn insert_mneme(ws: &str, concept: &str, content: &str, embedding: &str) -> String {
        Spi::get_one::<String>(&format!(
            "INSERT INTO semantic.mnemes (workspace_id, concept, content, embedding) \
             VALUES ('{ws}', '{concept}', '{content}', '{embedding}'::vector) \
             RETURNING id::text"
        ))
        .expect("insert failed")
        .expect("null id")
    }

    // ------------------------------------------------------------------
    // 1. insert -> recall round-trip
    // ------------------------------------------------------------------

    #[pg_test]
    fn insert_then_recall_roundtrip() {
        let ws = ws_setup("00000000-0000-0000-0000-000000000001");
        let emb = spike_embedding(0, 128);

        let _id = insert_mneme(ws, "kubernetes", "pod scheduling", &emb);

        let count: i64 = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM semantic.recall( \
                 '{ws}'::uuid, 'kubernetes pod scheduling', '{emb}'::vector, 10)"
        ))
        .expect("recall failed")
        .expect("null count");

        assert!(count >= 1, "recall should surface the inserted mneme, got {count}");
    }

    // ------------------------------------------------------------------
    // 2. Hebbian weight update fires from co_activation_queue
    // ------------------------------------------------------------------

    #[pg_test]
    fn hebbian_weight_update_from_queue() {
        let ws = ws_setup("00000000-0000-0000-0000-000000000002");

        // Three mnemes that all surface as candidates: their embeddings
        // overlap the query direction in varying degrees, so recall
        // pulls at least two of them, which means at least one pair
        // lands in semantic.co_activation_queue.
        let _m1 = insert_mneme(ws, "k8s", "pods and services", &spike_embedding(0, 128));
        let _m2 = insert_mneme(ws, "docker", "containers", &spike_embedding(32, 128));
        let _m3 = insert_mneme(ws, "helm", "charts", &spike_embedding(64, 128));

        let query_emb = spike_embedding(0, 128);

        // Drive recall so it enqueues pairs.
        Spi::run(&format!(
            "SELECT * FROM semantic.recall( \
                 '{ws}'::uuid, 'query', '{query_emb}'::vector, 10)"
        ))
        .expect("recall failed");

        let enqueued: i64 =
            Spi::get_one::<i64>("SELECT count(*) FROM semantic.co_activation_queue")
                .expect("queue count failed")
                .expect("null");
        assert!(enqueued >= 1, "recall should enqueue at least one pair, got {enqueued}");

        // Drain the queue via the Hebbian batch processor.
        let processed: i64 = Spi::get_one::<i64>(
            "SELECT semantic.process_co_activation_batch(100)",
        )
        .expect("process_co_activation_batch failed")
        .expect("null");
        assert!(processed >= 1, "batch should process at least one pair, got {processed}");

        // Now we should see hebbian associations with weight > seed.
        let assoc_count: i64 = Spi::get_one::<i64>(
            "SELECT count(*) FROM semantic.associations \
             WHERE association_type = 'hebbian' AND weight > 0.01",
        )
        .expect("assoc count failed")
        .expect("null");
        assert!(
            assoc_count >= 1,
            "should see at least one hebbian row with weight > 0.01, got {assoc_count}"
        );

        // Queue should be empty after drain.
        let remaining: i64 =
            Spi::get_one::<i64>("SELECT count(*) FROM semantic.co_activation_queue")
                .expect("remaining count failed")
                .expect("null");
        assert_eq!(remaining, 0, "queue should be drained, {remaining} rows left");
    }

    // ------------------------------------------------------------------
    // 3. flag_contradictions surfaces high-cosine divergent pairs
    // ------------------------------------------------------------------

    #[pg_test]
    fn flag_contradictions_detects_divergent_pairs() {
        let ws = ws_setup("00000000-0000-0000-0000-000000000003");

        // Same embedding (cosine = 1.0), different content (so the
        // "skip exact duplicate content" guard in flag_contradictions
        // doesn't filter them out). This matches the design doc's
        // "same topic, contradicting claim" case.
        let emb = spike_embedding(0, 128);
        let _m1 = insert_mneme(ws, "capital", "the capital of France is Paris", &emb);
        let m2 = insert_mneme(ws, "capital", "the capital of France is Lyon", &emb);

        let flagged: i64 = Spi::get_one::<i64>(&format!(
            "SELECT semantic.flag_contradictions('{m2}'::uuid, 0.85)"
        ))
        .expect("flag_contradictions failed")
        .expect("null");

        assert!(flagged >= 1, "should flag at least one contradiction, got {flagged}");

        let pending: i64 = Spi::get_one::<i64>(
            "SELECT count(*) FROM semantic.contradiction_candidates WHERE status = 'pending'",
        )
        .expect("pending count failed")
        .expect("null");
        assert_eq!(pending, 1, "expected exactly 1 pending candidate, got {pending}");
    }

    // ------------------------------------------------------------------
    // 4. ACT-R activation decays with age
    // ------------------------------------------------------------------

    #[pg_test]
    fn actr_activation_decays_with_age() {
        // Fresh activation: last_access is now.
        let fresh: f64 = Spi::get_one::<f64>("SELECT semantic.actr_activation(5, now())")
            .expect("fresh actr_activation failed")
            .expect("null");

        // Old activation: same access_count, last_access 30 days ago.
        let old: f64 = Spi::get_one::<f64>(
            "SELECT semantic.actr_activation(5, now() - interval '30 days')",
        )
        .expect("old actr_activation failed")
        .expect("null");

        assert!(
            fresh > old,
            "fresh activation ({fresh}) should exceed 30-day-old activation ({old})"
        );
    }

    // ------------------------------------------------------------------
    // 5. Archival flips stale low-confidence mnemes
    // ------------------------------------------------------------------

    #[pg_test]
    fn archival_flips_stale_low_confidence_mnemes() {
        let ws = ws_setup("00000000-0000-0000-0000-000000000005");
        let emb = zero_embedding();

        // Three mnemes covering the archival decision branches:
        //   stale         — old + low confidence: archive
        //   recent        — low confidence but recently accessed: keep
        //   confident     — stale but confident: keep
        let stale_id = Spi::get_one::<String>(&format!(
            "INSERT INTO semantic.mnemes \
                 (workspace_id, concept, content, embedding, confidence, last_access) \
             VALUES ('{ws}', 'old', 'stale low-conf', '{emb}'::vector, 0.2, now() - interval '91 days') \
             RETURNING id::text"
        ))
        .expect("insert stale failed")
        .expect("null");

        let recent_id = Spi::get_one::<String>(&format!(
            "INSERT INTO semantic.mnemes \
                 (workspace_id, concept, content, embedding, confidence, last_access) \
             VALUES ('{ws}', 'recent', 'low-conf but fresh', '{emb}'::vector, 0.2, now()) \
             RETURNING id::text"
        ))
        .expect("insert recent failed")
        .expect("null");

        let confident_id = Spi::get_one::<String>(&format!(
            "INSERT INTO semantic.mnemes \
                 (workspace_id, concept, content, embedding, confidence, last_access) \
             VALUES ('{ws}', 'old-confident', 'stale but trusted', '{emb}'::vector, 0.9, now() - interval '91 days') \
             RETURNING id::text"
        ))
        .expect("insert confident failed")
        .expect("null");

        // Same SQL the consolidation worker runs on its six-hour
        // archival tick (see consolidation_worker::run_archival).
        Spi::run(
            "UPDATE semantic.mnemes \
             SET state = 'archived' \
             WHERE state = 'active' \
               AND last_access < now() - interval '90 days' \
               AND confidence < 0.3",
        )
        .expect("archival UPDATE failed");

        let stale_state: String = Spi::get_one::<String>(&format!(
            "SELECT state FROM semantic.mnemes WHERE id = '{stale_id}'::uuid"
        ))
        .expect("stale state query failed")
        .expect("null");
        assert_eq!(stale_state, "archived", "stale low-conf mneme should be archived");

        let recent_state: String = Spi::get_one::<String>(&format!(
            "SELECT state FROM semantic.mnemes WHERE id = '{recent_id}'::uuid"
        ))
        .expect("recent state query failed")
        .expect("null");
        assert_eq!(recent_state, "active", "recent mneme should stay active despite low conf");

        let confident_state: String = Spi::get_one::<String>(&format!(
            "SELECT state FROM semantic.mnemes WHERE id = '{confident_id}'::uuid"
        ))
        .expect("confident state query failed")
        .expect("null");
        assert_eq!(
            confident_state, "active",
            "stale but confident mneme should stay active"
        );
    }
}
