// pg_recall: Cognitive Memory Primitives for Postgres
// A pgrx extension implementing neuroscience-inspired memory primitives.
//
// All extension objects are installed into the pg_recall schema via the
// control file's `schema = 'pg_recall'` directive. We do NOT use pgrx's
// #[pg_schema] or schema = "pg_recall" in #[pg_extern] because:
// 1. PG18 reserves pg_ prefix for system schemas (requires allow_system_table_mods)
// 2. The control file handles schema placement automatically
//
// v0.2: Background worker for autonomous Hebbian processing.
// Requires shared_preload_libraries = 'pg_recall' for the worker to start.
// Configure target database via postgresql.conf: pg_recall.database = 'memories'
// (defaults to 'postgres' if not set)

::pgrx::pg_module_magic!();

pub mod types;
pub mod scoring;
pub mod schema;
pub mod hebbian;
pub mod recall;
pub mod worker_stats;
pub mod worker;
pub mod contradiction;

#[cfg(any(test, feature = "pg_test"))]
pub mod integration_tests;

// ---------------------------------------------------------------------------
// _PG_init: background worker registration
// ---------------------------------------------------------------------------

use pgrx::prelude::*;

#[allow(non_snake_case)]
#[pg_guard]
pub extern "C-unwind" fn _PG_init() {
    use pgrx::bgworkers::*;
    use std::time::Duration;

    BackgroundWorkerBuilder::new("pg_recall Hebbian Worker")
        .set_function("worker_main")
        .set_library("pg_recall")
        .set_argument(0i32.into_datum())
        .enable_spi_access()
        .set_start_time(BgWorkerStartTime::RecoveryFinished)
        .set_restart_time(Some(Duration::from_secs(10)))
        .load();
}

#[cfg(test)]
pub mod pg_test {
    pub fn setup(_options: Vec<&str>) {
        // perform one-off initialization when the pg_test framework starts
    }

    pub fn postgresql_conf_options() -> Vec<&'static str> {
        // PG18 reserves pg_ prefix for system schemas; allow it for our extension
        vec!["allow_system_table_mods = on"]
    }
}
