// ghola: Cognitive Memory Primitives for Postgres (v2)
//
// A pgrx extension. All extension objects install into the `semantic`
// schema via the control file's `schema = 'semantic'` directive.
//
// v2 background workers (consolidation + contradiction) target
// semantic.* and require shared_preload_libraries = 'ghola' in
// postgresql.conf. Target database is configured via the
// ghola.database GUC (defaults to 'memories').

::pgrx::pg_module_magic!();

pub mod types;
pub mod scoring;
pub mod schema;
pub mod hebbian;
pub mod recall;
pub mod consolidation_worker;
pub mod contradiction;
pub mod contradiction_worker;
pub mod associations;

#[cfg(any(test, feature = "pg_test"))]
pub mod integration_tests;

#[cfg(any(test, feature = "pg_test"))]
pub mod tests;

// ---------------------------------------------------------------------------
// GUC: ghola.database
// ---------------------------------------------------------------------------

use pgrx::prelude::*;
use pgrx::guc::*;
use std::ffi::CString;

pub static GHOLA_DATABASE: GucSetting<Option<CString>> =
    GucSetting::<Option<CString>>::new(None);

// ---------------------------------------------------------------------------
// _PG_init: GUC registration + background worker
// ---------------------------------------------------------------------------

#[allow(non_snake_case)]
#[pg_guard]
pub extern "C-unwind" fn _PG_init() {
    GucRegistry::define_string_guc(
        c"ghola.database",
        c"Target database for the ghola background workers.",
        c"Workers connect to this database for consolidation and contradiction scanning.",
        &GHOLA_DATABASE,
        GucContext::Sighup,
        GucFlags::default(),
    );

    register_background_workers();
}

// Workers are only registered outside the pg_test feature. Under
// pg_test, pgrx spawns an ephemeral database whose name is not
// 'memories' (the GUC default), so the workers' connect_worker_to_spi
// call fails and triggers a 10-second restart loop that hangs the
// test harness. Task 1.8 rewires the integration tests to drive the
// workers explicitly (via process_co_activation_batch and direct
// SQL on the decay/archival queries) rather than relying on the
// BGWorker lifecycle.
#[cfg(not(feature = "pg_test"))]
fn register_background_workers() {
    use pgrx::bgworkers::*;
    use std::time::Duration;

    BackgroundWorkerBuilder::new("ghola Consolidation Worker")
        .set_function("consolidation_worker_main")
        .set_library("ghola")
        .set_argument(0i32.into_datum())
        .enable_spi_access()
        .set_start_time(BgWorkerStartTime::RecoveryFinished)
        .set_restart_time(Some(Duration::from_secs(10)))
        .load();

    BackgroundWorkerBuilder::new("ghola Contradiction Worker")
        .set_function("contradiction_worker_main")
        .set_library("ghola")
        .set_argument(0i32.into_datum())
        .enable_spi_access()
        .set_start_time(BgWorkerStartTime::RecoveryFinished)
        .set_restart_time(Some(Duration::from_secs(10)))
        .load();
}

#[cfg(feature = "pg_test")]
fn register_background_workers() {
    // See comment above: tests drive workers directly.
}

#[cfg(test)]
pub mod pg_test {
    pub fn setup(_options: Vec<&str>) {
        // perform one-off initialization when the pg_test framework starts
    }

    pub fn postgresql_conf_options() -> Vec<&'static str> {
        // v2 schema is `semantic`, not `pg_*`, so we no longer need
        // allow_system_table_mods. Leave the hook in place for future
        // per-test GUCs without polluting production configuration.
        vec![]
    }
}
