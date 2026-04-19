// pg_ghola: Cognitive Memory Primitives for Postgres
// A pgrx extension implementing neuroscience-inspired memory primitives.
//
// All extension objects are installed into the pg_ghola schema via the
// control file's `schema = 'pg_ghola'` directive. We do NOT use pgrx's
// #[pg_schema] or schema = "pg_ghola" in #[pg_extern] because:
// 1. PG18 reserves pg_ prefix for system schemas (requires allow_system_table_mods)
// 2. The control file handles schema placement automatically
//
// v0.2: Background workers for autonomous memory consolidation.
// Requires shared_preload_libraries = 'pg_ghola' for workers to start.
// Configure target database via postgresql.conf: pg_ghola.database = 'memories'
// (defaults to 'postgres' if not set)

::pgrx::pg_module_magic!();

pub mod types;
pub mod scoring;
pub mod schema;
pub mod hebbian;
pub mod recall;
pub mod worker_stats;
pub mod consolidation_worker;
pub mod contradiction;
pub mod contradiction_worker;
pub mod gating_worker;
pub mod associations;

#[cfg(any(test, feature = "pg_test"))]
pub mod integration_tests;

#[cfg(any(test, feature = "pg_test"))]
pub mod tests;

// ---------------------------------------------------------------------------
// GUC: pg_ghola.database
// ---------------------------------------------------------------------------

use pgrx::prelude::*;
use pgrx::guc::*;
use std::ffi::CString;

pub static PG_GHOLA_DATABASE: GucSetting<Option<CString>> =
    GucSetting::<Option<CString>>::new(None);

// ---------------------------------------------------------------------------
// _PG_init: GUC registration + background worker
// ---------------------------------------------------------------------------

#[allow(non_snake_case)]
#[pg_guard]
pub extern "C-unwind" fn _PG_init() {
    use pgrx::bgworkers::*;
    use std::time::Duration;

    GucRegistry::define_string_guc(
        c"ghola.database",
        c"Target database for the pg_ghola background worker.",
        c"The background worker will connect to this database for memory consolidation.",
        &PG_GHOLA_DATABASE,
        GucContext::Sighup,
        GucFlags::default(),
    );

    BackgroundWorkerBuilder::new("pg_ghola Consolidation Worker")
        .set_function("consolidation_worker_main")
        .set_library("pg_ghola")
        .set_argument(0i32.into_datum())
        .enable_spi_access()
        .set_start_time(BgWorkerStartTime::RecoveryFinished)
        .set_restart_time(Some(Duration::from_secs(10)))
        .load();

    BackgroundWorkerBuilder::new("pg_ghola Contradiction Worker")
        .set_function("contradiction_worker_main")
        .set_library("pg_ghola")
        .set_argument(0i32.into_datum())
        .enable_spi_access()
        .set_start_time(BgWorkerStartTime::RecoveryFinished)
        .set_restart_time(Some(Duration::from_secs(10)))
        .load();

    // Gating extraction worker
    BackgroundWorkerBuilder::new("pg_ghola Gating Worker")
        .set_function("gating_worker_main")
        .set_library("pg_ghola")
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
