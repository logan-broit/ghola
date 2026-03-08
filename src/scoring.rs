// pg_recall::scoring — Pure cognitive scoring functions
//
// Implements softplus, actr_activation, ebbinghaus_decay, bayesian_update.
// All functions are stateless, immutable, and parallel-safe.
// The control file's schema directive places all objects in pg_recall automatically.
//
// Owned by: implement_scoring_primitives task

use pgrx::prelude::*;
use pgrx::datum::TimestampWithTimeZone;

/// softplus(x) = ln(1 + exp(x)), with overflow guard for x > 20.
#[pg_extern(immutable, parallel_safe)]
fn softplus(x: f64) -> f64 {
    // Stub: returns placeholder value
    if x > 20.0 {
        x
    } else {
        (1.0 + x.exp()).ln()
    }
}

/// ACT-R base-level activation.
/// B = ln(n+1) - d * ln(max(age_days, 1/1440) / (n+1))
#[pg_extern(immutable, parallel_safe)]
fn actr_activation(access_count: i32, last_access: TimestampWithTimeZone) -> f64 {
    // Stub: returns placeholder
    let _ = (access_count, last_access);
    0.0
}

/// Ebbinghaus retention with spacing-aware stability.
#[pg_extern(immutable, parallel_safe)]
fn ebbinghaus_decay(
    last_access: TimestampWithTimeZone,
    access_count: i32,
    created_at: TimestampWithTimeZone,
) -> f64 {
    // Stub: returns placeholder
    let _ = (last_access, access_count, created_at);
    1.0
}

/// Bayesian update with Laplace smoothing. Result in [0.025, 0.975].
#[pg_extern(immutable, parallel_safe)]
fn bayesian_update(prior: f64, evidence: f64) -> f64 {
    // Stub: returns placeholder
    let _ = (prior, evidence);
    0.5
}

#[cfg(any(test, feature = "pg_test"))]
#[pg_schema]
mod tests {
    use pgrx::prelude::*;

    #[pg_test]
    fn test_softplus_stub() {
        let result = Spi::get_one::<f64>("SELECT pg_recall.softplus(0.0::float8)").unwrap().unwrap();
        assert!((result - 0.6931).abs() < 0.01);
    }

    #[pg_test]
    fn test_softplus_overflow_guard() {
        let result = Spi::get_one::<f64>("SELECT pg_recall.softplus(25.0::float8)").unwrap().unwrap();
        assert!((result - 25.0).abs() < 0.001);
    }

    #[pg_test]
    fn test_extension_creates_schema() {
        // Verify the pg_recall schema exists after extension creation
        let schema_exists = Spi::get_one::<bool>(
            "SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname = 'pg_recall')"
        ).unwrap().unwrap();
        assert!(schema_exists, "pg_recall schema should exist");
    }

    #[pg_test]
    fn test_stub_functions_exist() {
        // Verify stub functions are callable via schema-qualified names
        let result = Spi::get_one::<&str>("SELECT pg_recall.recall_result_stub()").unwrap().unwrap();
        assert_eq!(result, "recall_result type stub");

        let result = Spi::get_one::<&str>("SELECT pg_recall.score_weights_stub()").unwrap().unwrap();
        assert_eq!(result, "score_weights type stub");
    }

    #[pg_test]
    fn test_scoring_stubs_callable() {
        // actr_activation stub returns 0.0
        let result = Spi::get_one::<f64>(
            "SELECT pg_recall.actr_activation(5, now() - interval '10 days')"
        ).unwrap().unwrap();
        assert!((result - 0.0).abs() < 0.001);

        // bayesian_update stub returns 0.5
        let result = Spi::get_one::<f64>(
            "SELECT pg_recall.bayesian_update(0.5, 0.95)"
        ).unwrap().unwrap();
        assert!((result - 0.5).abs() < 0.001);

        // ebbinghaus_decay stub returns 1.0
        let result = Spi::get_one::<f64>(
            "SELECT pg_recall.ebbinghaus_decay(now() - interval '30 days', 50, now() - interval '180 days')"
        ).unwrap().unwrap();
        assert!((result - 1.0).abs() < 0.001);
    }

    #[pg_test]
    fn test_hebbian_stubs_callable() {
        // process_co_activation_batch_stub returns 0
        let result = Spi::get_one::<i64>(
            "SELECT pg_recall.process_co_activation_batch_stub()"
        ).unwrap().unwrap();
        assert_eq!(result, 0);

        // process_all_pending_co_activations_stub returns 0
        let result = Spi::get_one::<i64>(
            "SELECT pg_recall.process_all_pending_co_activations_stub()"
        ).unwrap().unwrap();
        assert_eq!(result, 0);
    }

    #[pg_test]
    fn test_recall_stub_callable() {
        let result = Spi::get_one::<&str>("SELECT pg_recall.recall_stub()").unwrap().unwrap();
        assert_eq!(result, "recall function stub");
    }
}
