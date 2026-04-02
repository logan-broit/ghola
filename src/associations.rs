// pg_ghola::associations — Typed association management
//
// Provides functions for creating and querying typed associations:
// - mark_supersedes: creates 'supersedes' link and archives the older mneme
// - mark_supports: creates 'supports' link and boosts supported mneme's confidence
// - get_typed_associations: queries associations with optional type filter

use pgrx::prelude::*;

use crate::scoring::bayesian_update_inner;

/// Create a directed `supersedes` association from `newer_id` to `older_id`.
/// Sets the older mneme's state to `archived`.
#[pg_extern]
fn mark_supersedes(newer_id: pgrx::Uuid, older_id: pgrx::Uuid) -> &'static str {
    if newer_id == older_id {
        pgrx::error!("cannot supersede self: {newer_id}");
    }

    Spi::connect_mut(|client| {
        // Verify both mnemes exist
        let count = client
            .select(
                &format!(
                    "SELECT count(*) FROM ghola.mnemes \
                     WHERE id IN ('{newer_id}'::uuid, '{older_id}'::uuid)"
                ),
                None,
                &[],
            )
            .expect("failed to check mnemes")
            .into_iter()
            .next()
            .and_then(|r| r.get::<i64>(1).ok().flatten())
            .unwrap_or(0);

        if count < 2 {
            pgrx::error!("one or both mneme IDs not found");
        }

        // Create the supersedes association
        client
            .update(
                &format!(
                    "INSERT INTO ghola.associations \
                     (src_id, dst_id, association_type, weight, co_activations, updated_at) \
                     VALUES ('{newer_id}'::uuid, '{older_id}'::uuid, 'supersedes', 1.0, 1, now()) \
                     ON CONFLICT (src_id, dst_id, association_type) DO UPDATE SET \
                         weight = 1.0, updated_at = now()"
                ),
                None,
                &[],
            )
            .expect("failed to create supersedes association");

        // Archive the older mneme
        client
            .update(
                &format!(
                    "UPDATE ghola.mnemes SET state = 'archived' \
                     WHERE id = '{older_id}'::uuid"
                ),
                None,
                &[],
            )
            .expect("failed to archive older mneme");
    });

    "ok"
}

/// Create a directed `supports` association from `supporting_id` to `supported_id`.
/// Boosts the supported mneme's confidence via bayesian_update(confidence, 0.65).
#[pg_extern]
fn mark_supports(supporting_id: pgrx::Uuid, supported_id: pgrx::Uuid) -> &'static str {
    if supporting_id == supported_id {
        pgrx::error!("cannot support self: {supporting_id}");
    }

    Spi::connect_mut(|client| {
        // Verify both mnemes exist
        let count = client
            .select(
                &format!(
                    "SELECT count(*) FROM ghola.mnemes \
                     WHERE id IN ('{supporting_id}'::uuid, '{supported_id}'::uuid)"
                ),
                None,
                &[],
            )
            .expect("failed to check mnemes")
            .into_iter()
            .next()
            .and_then(|r| r.get::<i64>(1).ok().flatten())
            .unwrap_or(0);

        if count < 2 {
            pgrx::error!("one or both mneme IDs not found");
        }

        // Create the supports association
        client
            .update(
                &format!(
                    "INSERT INTO ghola.associations \
                     (src_id, dst_id, association_type, weight, co_activations, updated_at) \
                     VALUES ('{supporting_id}'::uuid, '{supported_id}'::uuid, 'supports', 1.0, 1, now()) \
                     ON CONFLICT (src_id, dst_id, association_type) DO UPDATE SET \
                         weight = 1.0, updated_at = now()"
                ),
                None,
                &[],
            )
            .expect("failed to create supports association");

        // Boost the supported mneme's confidence
        let prior_row = client
            .select(
                &format!(
                    "SELECT confidence, tier FROM ghola.mnemes \
                     WHERE id = '{supported_id}'::uuid FOR UPDATE"
                ),
                None,
                &[],
            )
            .expect("failed to read supported mneme");

        if let Some(r) = prior_row.into_iter().next() {
            let prior: f64 = r.get::<f64>(1).expect("err").expect("null confidence");
            let tier: String = r.get::<String>(2).expect("err").expect("null tier");
            let posterior = bayesian_update_inner(prior, 0.65);
            let floor = match tier.as_str() {
                "core" => 0.30,
                _ => 0.025,
            };
            let clamped = posterior.max(floor);

            client
                .update(
                    &format!(
                        "UPDATE ghola.mnemes SET confidence = {clamped} \
                         WHERE id = '{supported_id}'::uuid"
                    ),
                    None,
                    &[],
                )
                .expect("failed to update supported mneme confidence");
        }
    });

    "ok"
}

/// Query associations for a mneme, optionally filtered by type.
/// Returns associations in both directions (src and dst), ordered by weight descending.
#[pg_extern]
fn get_typed_associations(
    mneme_id: pgrx::Uuid,
    association_type: default!(Option<String>, "NULL"),
    min_weight: default!(f64, 0.01),
) -> TableIterator<
    'static,
    (
        name!(related_id, pgrx::Uuid),
        name!(association_type, String),
        name!(weight, f64),
        name!(direction, String),
    ),
> {
    let type_filter = match &association_type {
        Some(t) => format!(" AND association_type = '{}'", t.replace('\'', "''")),
        None => String::new(),
    };

    let results = Spi::connect(|client| {
        let query = format!(
            "SELECT related_id, association_type, weight, direction FROM ( \
                SELECT dst_id AS related_id, association_type, weight, 'outgoing' AS direction \
                FROM ghola.associations \
                WHERE src_id = '{mneme_id}' AND weight >= {min_weight}{type_filter} \
                UNION ALL \
                SELECT src_id AS related_id, association_type, weight, 'incoming' AS direction \
                FROM ghola.associations \
                WHERE dst_id = '{mneme_id}' AND weight >= {min_weight}{type_filter} \
            ) sub \
            ORDER BY weight DESC"
        );

        let tup_table = client
            .select(&query, None, &[])
            .expect("failed to query typed associations");

        let mut results = Vec::new();
        for row in tup_table {
            let related_id: pgrx::Uuid = row.get(1).expect("err").expect("null related_id");
            let assoc_type: String = row.get(2).expect("err").expect("null type");
            let weight: f64 = row.get(3).expect("err").expect("null weight");
            let direction: String = row.get(4).expect("err").expect("null direction");
            results.push((related_id, assoc_type, weight, direction));
        }
        results
    });

    TableIterator::new(results)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(any(test, feature = "pg_test"))]
#[pg_schema]
mod tests {
    use pgrx::prelude::*;

    const DIMS: usize = 768;

    fn zero_embedding() -> String {
        let zeros: Vec<String> = (0..DIMS).map(|_| "0".to_string()).collect();
        format!("[{}]", zeros.join(","))
    }

    fn insert_mneme(ws: &str, concept: &str, content: &str) -> String {
        let emb = zero_embedding();
        // Disable triggers to avoid interference
        Spi::run(
            "ALTER TABLE ghola.mnemes DISABLE TRIGGER mneme_contradiction_check"
        ).expect("disable");
        Spi::run(
            "ALTER TABLE ghola.mnemes DISABLE TRIGGER mneme_session_association"
        ).expect("disable");

        let id = Spi::get_one::<String>(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding) \
             VALUES ('{ws}', '{concept}', '{content}', '{emb}'::vector({DIMS})) \
             RETURNING id::text"
        ))
        .expect("insert failed")
        .expect("null");

        Spi::run(
            "ALTER TABLE ghola.mnemes ENABLE TRIGGER mneme_contradiction_check"
        ).expect("enable");
        Spi::run(
            "ALTER TABLE ghola.mnemes ENABLE TRIGGER mneme_session_association"
        ).expect("enable");

        id
    }

    // ── mark_supersedes ──

    #[pg_test]
    fn test_mark_supersedes_archives_older() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup");

        let ws = "00000000-0000-0000-0000-dddddddddddd";
        let m1 = insert_mneme(ws, "fact", "old version");
        let m2 = insert_mneme(ws, "fact", "new version");

        Spi::run(&format!(
            "SELECT ghola.mark_supersedes('{m2}'::uuid, '{m1}'::uuid)"
        ))
        .expect("mark_supersedes failed");

        // Older mneme should be archived
        let state = Spi::get_one::<String>(&format!(
            "SELECT state FROM ghola.mnemes WHERE id = '{m1}'::uuid"
        ))
        .expect("query failed")
        .expect("null");

        assert_eq!(state, "archived", "older mneme should be archived");

        // Supersedes association should exist
        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.associations \
             WHERE src_id = '{m2}'::uuid AND dst_id = '{m1}'::uuid \
               AND association_type = 'supersedes'"
        ))
        .expect("query failed")
        .expect("null");

        assert_eq!(count, 1, "supersedes association should exist");
    }

    #[pg_test]
    #[should_panic(expected = "cannot supersede self")]
    fn test_mark_supersedes_self_panics() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup");

        let ws = "00000000-0000-0000-0000-eeeeeeeeeeee";
        let m1 = insert_mneme(ws, "fact", "content");

        Spi::run(&format!(
            "SELECT ghola.mark_supersedes('{m1}'::uuid, '{m1}'::uuid)"
        ))
        .expect("should panic");
    }

    // ── mark_supports ──

    #[pg_test]
    fn test_mark_supports_boosts_confidence() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup");

        let ws = "00000000-0000-0000-0000-ffffffffffff";
        let m1 = insert_mneme(ws, "evidence", "supporting evidence");
        let m2 = insert_mneme(ws, "claim", "the claim");

        let before = Spi::get_one::<f64>(&format!(
            "SELECT confidence FROM ghola.mnemes WHERE id = '{m2}'::uuid"
        ))
        .expect("query failed")
        .expect("null");

        Spi::run(&format!(
            "SELECT ghola.mark_supports('{m1}'::uuid, '{m2}'::uuid)"
        ))
        .expect("mark_supports failed");

        let after = Spi::get_one::<f64>(&format!(
            "SELECT confidence FROM ghola.mnemes WHERE id = '{m2}'::uuid"
        ))
        .expect("query failed")
        .expect("null");

        assert!(
            after > before,
            "supported mneme confidence should increase: {before} -> {after}"
        );

        // Supports association should exist
        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.associations \
             WHERE src_id = '{m1}'::uuid AND dst_id = '{m2}'::uuid \
               AND association_type = 'supports'"
        ))
        .expect("query failed")
        .expect("null");

        assert_eq!(count, 1, "supports association should exist");
    }

    // ── get_typed_associations ──

    #[pg_test]
    fn test_get_typed_associations_all() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup");

        let ws = "00000000-0000-0000-0000-111111111111";
        let m1 = insert_mneme(ws, "a", "content a");
        let m2 = insert_mneme(ws, "b", "content b");
        let m3 = insert_mneme(ws, "c", "content c");

        // Create different typed associations
        Spi::run(&format!(
            "SELECT ghola.mark_supports('{m1}'::uuid, '{m2}'::uuid)"
        )).expect("supports failed");
        Spi::run(&format!(
            "SELECT ghola.mark_supersedes('{m3}'::uuid, '{m2}'::uuid)"
        )).expect("supersedes failed");

        // Get all associations for m2
        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.get_typed_associations('{m2}'::uuid)"
        ))
        .expect("query failed")
        .expect("null");

        assert!(count >= 2, "m2 should have at least 2 associations, got {count}");
    }

    #[pg_test]
    fn test_get_typed_associations_filtered() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup");

        let ws = "00000000-0000-0000-0000-222222222222";
        let m1 = insert_mneme(ws, "a", "content a");
        let m2 = insert_mneme(ws, "b", "content b");
        let m3 = insert_mneme(ws, "c", "content c");

        Spi::run(&format!(
            "SELECT ghola.mark_supports('{m1}'::uuid, '{m2}'::uuid)"
        )).expect("supports failed");
        Spi::run(&format!(
            "INSERT INTO ghola.associations \
             (src_id, dst_id, association_type, weight) \
             VALUES ('{m2}'::uuid, '{m3}'::uuid, 'hebbian', 0.5)"
        )).expect("insert failed");

        // Filter to supports only
        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.get_typed_associations('{m2}'::uuid, 'supports')"
        ))
        .expect("query failed")
        .expect("null");

        assert_eq!(count, 1, "filtered to supports should return 1, got {count}");
    }
}
