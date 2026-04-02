// pg_ghola::worker_stats — Worker statistics table and query function
//
// Provides a singleton table for the background worker to report its state,
// and a SQL-callable function for users to query worker status.
//
// The worker_stats table is a single-row table (enforced by CHECK id = 1)
// that the background worker upserts after every processing cycle.

use pgrx::prelude::*;

// ---------------------------------------------------------------------------
// Table: worker_stats (singleton row for bgworker state)
// ---------------------------------------------------------------------------

extension_sql!(
    r#"
CREATE TABLE worker_stats (
    id                 integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    state              text NOT NULL DEFAULT 'stopped',
    queue_depth        bigint NOT NULL DEFAULT 0,
    batches_processed  bigint NOT NULL DEFAULT 0,
    rows_processed     bigint NOT NULL DEFAULT 0,
    pairs_updated      bigint NOT NULL DEFAULT 0,
    last_batch_at      timestamptz,
    last_decay_at      timestamptz,
    poll_interval_ms   integer NOT NULL DEFAULT 100,
    started_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);

-- Seed the singleton row so get_worker_stats() never returns empty
INSERT INTO @extschema@.worker_stats (id) VALUES (1);
"#,
    name = "create_worker_stats_table",
);

// ---------------------------------------------------------------------------
// Composite type: worker_status (returned by get_worker_stats)
// ---------------------------------------------------------------------------

extension_sql!(
    r#"
CREATE TYPE worker_status AS (
    state              text,
    queue_depth        bigint,
    batches_processed  bigint,
    rows_processed     bigint,
    pairs_updated      bigint,
    last_batch_at      timestamptz,
    last_decay_at      timestamptz,
    poll_interval_ms   integer,
    started_at         timestamptz,
    uptime_seconds     float8
);
"#,
    name = "create_type_worker_status",
);

// ---------------------------------------------------------------------------
// Function: get_worker_stats() -> worker_status
// ---------------------------------------------------------------------------

extension_sql!(
    r#"
CREATE OR REPLACE FUNCTION get_worker_stats()
RETURNS @extschema@.worker_status
LANGUAGE sql STABLE
AS $$
    SELECT
        state,
        queue_depth,
        batches_processed,
        rows_processed,
        pairs_updated,
        last_batch_at,
        last_decay_at,
        poll_interval_ms,
        started_at,
        EXTRACT(EPOCH FROM (now() - started_at))::float8 AS uptime_seconds
    FROM @extschema@.worker_stats
    WHERE id = 1;
$$;
"#,
    name = "create_fn_get_worker_stats",
    requires = ["create_worker_stats_table", "create_type_worker_status"],
);

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(any(test, feature = "pg_test"))]
#[pg_schema]
mod tests {
    use pgrx::prelude::*;

    #[pg_test]
    fn test_worker_stats_table_exists() {
        let count = Spi::get_one::<i64>(
            "SELECT count(*) FROM information_schema.tables
             WHERE table_schema = 'pg_ghola' AND table_name = 'worker_stats'",
        )
        .expect("query failed")
        .expect("null result");
        assert_eq!(count, 1, "worker_stats table should exist in pg_ghola schema");
    }

    #[pg_test]
    fn test_worker_stats_singleton_seeded() {
        // The singleton row should be pre-seeded by the extension install
        let count = Spi::get_one::<i64>(
            "SELECT count(*) FROM ghola.worker_stats",
        )
        .expect("query failed")
        .expect("null result");
        assert_eq!(count, 1, "worker_stats should have exactly one row");
    }

    #[pg_test]
    #[should_panic(expected = "violates check constraint")]
    fn test_worker_stats_singleton_enforced() {
        // Attempting to insert a second row should fail
        Spi::run(
            "INSERT INTO ghola.worker_stats (id) VALUES (2)",
        )
        .expect("should have failed");
    }

    #[pg_test]
    fn test_worker_stats_default_state() {
        let state = Spi::get_one::<String>(
            "SELECT state FROM ghola.worker_stats WHERE id = 1",
        )
        .expect("query failed")
        .expect("null result");
        assert_eq!(state, "stopped", "initial state should be 'stopped'");
    }

    #[pg_test]
    fn test_worker_status_type_exists() {
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
    fn test_get_worker_stats_callable() {
        let state = Spi::get_one::<String>(
            "SELECT (s).state FROM ghola.get_worker_stats() AS s",
        )
        .expect("query failed")
        .expect("null result");
        assert_eq!(state, "stopped", "get_worker_stats should return initial state");
    }

    #[pg_test]
    fn test_get_worker_stats_uptime() {
        let uptime = Spi::get_one::<f64>(
            "SELECT (s).uptime_seconds FROM ghola.get_worker_stats() AS s",
        )
        .expect("query failed")
        .expect("null result");
        assert!(uptime >= 0.0, "uptime_seconds should be non-negative");
    }

    #[pg_test]
    fn test_worker_stats_upsert() {
        // Simulate the worker updating stats
        Spi::run(
            "UPDATE ghola.worker_stats SET
                state = 'active',
                queue_depth = 42,
                batches_processed = 10,
                rows_processed = 350,
                pairs_updated = 1200,
                last_batch_at = now(),
                poll_interval_ms = 100,
                updated_at = now()
             WHERE id = 1",
        )
        .expect("update should succeed");

        let state = Spi::get_one::<String>(
            "SELECT (s).state FROM ghola.get_worker_stats() AS s",
        )
        .expect("query failed")
        .expect("null result");
        assert_eq!(state, "active");

        let depth = Spi::get_one::<i64>(
            "SELECT (s).queue_depth FROM ghola.get_worker_stats() AS s",
        )
        .expect("query failed")
        .expect("null result");
        assert_eq!(depth, 42);
    }
}
