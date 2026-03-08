// pg_recall: Cognitive Memory Primitives for Postgres
// A pgrx extension implementing neuroscience-inspired memory primitives.
//
// All extension objects are installed into the pg_recall schema via the
// control file's `schema = 'pg_recall'` directive. We do NOT use pgrx's
// #[pg_schema] or schema = "pg_recall" in #[pg_extern] because:
// 1. PG18 reserves pg_ prefix for system schemas (requires allow_system_table_mods)
// 2. The control file handles schema placement automatically

::pgrx::pg_module_magic!();

pub mod types;
pub mod scoring;
pub mod schema;
pub mod hebbian;
pub mod recall;

#[cfg(any(test, feature = "pg_test"))]
pub mod integration_tests;

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
