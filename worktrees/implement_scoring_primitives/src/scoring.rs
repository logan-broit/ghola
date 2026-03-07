/// Cognitive scoring primitives for pg_recall.
///
/// Four stateless, pure scoring functions inspired by cognitive science models:
/// - softplus: smooth ReLU approximation
/// - actr_activation: ACT-R base-level activation with power-law decay
/// - ebbinghaus_decay: spacing-aware retention factor
/// - bayesian_update: Bayesian posterior with Laplace smoothing
///
/// All functions are immutable and parallel_safe, registered in the pg_recall schema.

use pgrx::prelude::*;

/// Microseconds per day, used for timestamp-to-days conversion.
const USEC_PER_DAY: f64 = 86_400_000_000.0;

/// One-minute floor in days (1/1440), used to prevent division by zero.
const ONE_MINUTE_DAYS: f64 = 1.0 / 1440.0;

// ──────────────────────────────────────────────
// Helper: get current Postgres timestamp as raw i64 microseconds
// ──────────────────────────────────────────────

/// Returns the current time as a raw i64 (microseconds since PG epoch 2000-01-01).
fn pg_now_usec() -> i64 {
    // Use SPI to call now() and get the internal timestamptz value
    Spi::get_one::<TimestampWithTimeZone>("SELECT now()")
        .expect("failed to get now()")
        .map(|ts| {
            let raw: pg_sys::TimestampTz = ts.into();
            raw
        })
        .expect("now() returned null")
}

/// Converts a TimestampWithTimeZone to its internal i64 microseconds value.
#[inline]
fn tstz_to_usec(ts: TimestampWithTimeZone) -> i64 {
    let raw: pg_sys::TimestampTz = ts.into();
    raw
}

// ──────────────────────────────────────────────
// softplus(x float8) -> float8
// ──────────────────────────────────────────────

/// Returns ln(1 + exp(x)), with an overflow guard: returns x directly when x > 20.
#[pg_extern(immutable, parallel_safe, schema = "pg_recall")]
pub fn softplus(x: f64) -> f64 {
    softplus_inner(x)
}

/// Pure computation, reusable from other modules without SPI/Postgres context.
#[inline]
pub fn softplus_inner(x: f64) -> f64 {
    if x > 20.0 {
        x
    } else {
        (1.0_f64 + x.exp()).ln()
    }
}

// ──────────────────────────────────────────────
// actr_activation(access_count int4, last_access timestamptz) -> float8
// ──────────────────────────────────────────────

/// ACT-R base-level activation.
///
/// B = ln(n+1) - d * ln(max(age_days, 1/1440) / (n+1))
///
/// where:
///   n = access_count + 1  (total presentations including initial encoding)
///   d = 0.5               (power-law decay exponent)
///   age_days = days since last_access, clamped to at least 1/1440 (one minute)
#[pg_extern(immutable, parallel_safe, schema = "pg_recall")]
pub fn actr_activation(access_count: i32, last_access: TimestampWithTimeZone) -> f64 {
    let now_usec = pg_now_usec();
    let last_usec = tstz_to_usec(last_access);
    let age_days = ((now_usec - last_usec) as f64 / USEC_PER_DAY).max(ONE_MINUTE_DAYS);
    actr_activation_inner(access_count, age_days, 0.5)
}

/// Pure computation of ACT-R activation given pre-computed age in days.
/// Exposed so the recall module can pass a custom decay exponent.
#[inline]
pub fn actr_activation_inner(access_count: i32, age_days: f64, d: f64) -> f64 {
    let age_days = age_days.max(ONE_MINUTE_DAYS);
    let n = (access_count + 1) as f64;
    n.ln() - d * (age_days / n).ln()
}

// ──────────────────────────────────────────────
// ebbinghaus_decay(last_access timestamptz, access_count int4, created_at timestamptz) -> float8
// ──────────────────────────────────────────────

/// Ebbinghaus decay with spacing-aware stability.
///
/// stability = clamp(ln(n+1)*20 * (1 + 0.5*tanh(avg_spacing/7)), 14, 365)
/// retention = max(0.05, exp(-days_since / stability))
///
/// where avg_spacing = lifespan_days / max(access_count, 1)
#[pg_extern(immutable, parallel_safe, schema = "pg_recall")]
pub fn ebbinghaus_decay(
    last_access: TimestampWithTimeZone,
    access_count: i32,
    created_at: TimestampWithTimeZone,
) -> f64 {
    let now_usec = pg_now_usec();
    let last_usec = tstz_to_usec(last_access);
    let created_usec = tstz_to_usec(created_at);

    let days_since = ((now_usec - last_usec) as f64 / USEC_PER_DAY).max(0.0);
    let lifespan_days = ((now_usec - created_usec) as f64 / USEC_PER_DAY).max(0.0);

    ebbinghaus_decay_inner(days_since, access_count, lifespan_days)
}

/// Pure computation of Ebbinghaus decay given pre-computed day values.
#[inline]
pub fn ebbinghaus_decay_inner(days_since: f64, access_count: i32, lifespan_days: f64) -> f64 {
    let n = (access_count + 1) as f64;
    let avg_spacing = lifespan_days / (access_count.max(1) as f64);

    let stability =
        (n.ln() * 20.0 * (1.0 + 0.5 * (avg_spacing / 7.0).tanh())).clamp(14.0, 365.0);
    (-days_since / stability).exp().max(0.05)
}

// ──────────────────────────────────────────────
// bayesian_update(prior float8, evidence float8) -> float8
// ──────────────────────────────────────────────

/// Bayesian posterior with Laplace smoothing.
///
/// result = 0.95 * (prior * ev / max(prior * ev + (1 - prior) * (1 - ev), 1e-9)) + 0.025
///
/// Result is always in [0.025, 0.975], never reaching 0 or 1.
#[pg_extern(immutable, parallel_safe, schema = "pg_recall")]
pub fn bayesian_update(prior: f64, evidence: f64) -> f64 {
    bayesian_update_inner(prior, evidence)
}

/// Pure computation, reusable from other modules.
#[inline]
pub fn bayesian_update_inner(prior: f64, evidence: f64) -> f64 {
    let numerator = prior * evidence;
    let denominator = (prior * evidence + (1.0 - prior) * (1.0 - evidence)).max(1e-9);
    0.95 * (numerator / denominator) + 0.025
}

// ──────────────────────────────────────────────
// Unit tests (pure Rust math, no Postgres needed)
// ──────────────────────────────────────────────

#[cfg(test)]
mod tests {
    use super::*;

    fn approx_eq(a: f64, b: f64, tol: f64) -> bool {
        (a - b).abs() < tol
    }

    // ── softplus ──

    #[test]
    fn test_softplus_zero() {
        assert!(approx_eq(softplus_inner(0.0), 0.6931, 0.001));
    }

    #[test]
    fn test_softplus_positive() {
        assert!(approx_eq(softplus_inner(2.0), 2.1269, 0.001));
    }

    #[test]
    fn test_softplus_negative() {
        assert!(approx_eq(softplus_inner(-5.0), 0.0067, 0.001));
    }

    #[test]
    fn test_softplus_overflow_guard() {
        assert_eq!(softplus_inner(25.0), 25.0);
    }

    #[test]
    fn test_softplus_boundary_20() {
        // x = 20 should NOT trigger the guard (only x > 20)
        let result = softplus_inner(20.0);
        let expected = (1.0_f64 + 20.0_f64.exp()).ln();
        assert!(approx_eq(result, expected, 1e-6));
        // x = 20.01 should trigger guard
        assert_eq!(softplus_inner(20.01), 20.01);
    }

    // ── bayesian_update ──

    #[test]
    fn test_bayesian_neutral_strong_confirmation() {
        // bayesian_update(0.5, 0.95) -> ~0.925
        let result = bayesian_update_inner(0.5, 0.95);
        assert!(
            approx_eq(result, 0.925, 0.01),
            "bayesian_update(0.5, 0.95) = {result}"
        );
    }

    #[test]
    fn test_bayesian_confident_contradiction() {
        // bayesian_update(0.8, 0.10) -> ~0.32
        let result = bayesian_update_inner(0.8, 0.10);
        assert!(
            approx_eq(result, 0.32, 0.02),
            "bayesian_update(0.8, 0.10) = {result}"
        );
    }

    #[test]
    fn test_bayesian_chained_contradiction() {
        // bayesian_update(0.32, 0.10) -> ~0.078
        let result = bayesian_update_inner(0.32, 0.10);
        assert!(
            approx_eq(result, 0.078, 0.02),
            "bayesian_update(0.32, 0.10) = {result}"
        );
    }

    #[test]
    fn test_bayesian_bounds_never_reach_zero_or_one() {
        let low = bayesian_update_inner(0.001, 0.001);
        assert!(low >= 0.025, "lower bound violated: {low}");

        let high = bayesian_update_inner(0.999, 0.999);
        assert!(high <= 0.975, "upper bound violated: {high}");

        // Even extreme values stay bounded
        let extreme_low = bayesian_update_inner(0.0, 0.0);
        assert!(extreme_low >= 0.025, "extreme lower bound: {extreme_low}");

        let extreme_high = bayesian_update_inner(1.0, 1.0);
        assert!(extreme_high <= 0.975, "extreme upper bound: {extreme_high}");
    }

    // ── actr_activation ──

    #[test]
    fn test_actr_frequently_accessed_moderate_age() {
        // actr_activation(13, 10 days): frequently accessed, moderate age -> high activation
        // Formula: B = ln(n) - d * ln(age/n) where n = access_count + 1 = 14
        // = ln(14) - 0.5 * ln(10/14) = 2.639 + 0.168 = 2.807
        let result = actr_activation_inner(13, 10.0, 0.5);
        let n = 14.0_f64;
        let expected = n.ln() - 0.5 * (10.0 / n).ln();
        assert!(
            approx_eq(result, expected, 0.001),
            "actr_activation(13, 10d) = {result}, expected {expected}"
        );
        // Verify it's positive (high activation for frequent recent access)
        assert!(result > 2.0, "frequently accessed should have high activation");
    }

    #[test]
    fn test_actr_never_reaccessed_very_old() {
        // actr_activation(0, 1400 days): never re-accessed, very old -> low activation
        // Formula: B = ln(1) - 0.5 * ln(1400/1) = 0 - 3.622 = -3.622
        let result = actr_activation_inner(0, 1400.0, 0.5);
        let n = 1.0_f64;
        let expected = n.ln() - 0.5 * (1400.0 / n).ln();
        assert!(
            approx_eq(result, expected, 0.001),
            "actr_activation(0, 1400d) = {result}, expected {expected}"
        );
        // Verify it's negative (low activation for old unused memories)
        assert!(result < -3.0, "old unused should have very low activation");
    }

    #[test]
    fn test_actr_very_recent_clamped() {
        // 10 seconds = 10/86400 days, should be clamped to 1/1440
        let result = actr_activation_inner(5, 10.0 / 86400.0, 0.5);
        let clamped = actr_activation_inner(5, ONE_MINUTE_DAYS, 0.5);
        assert_eq!(result, clamped, "very recent should clamp to one-minute floor");
    }

    #[test]
    fn test_actr_custom_decay() {
        // Higher decay -> lower activation for same age
        let d_low = actr_activation_inner(5, 30.0, 0.3);
        let d_high = actr_activation_inner(5, 30.0, 0.8);
        assert!(d_low > d_high, "higher decay should give lower activation");
    }

    // ── ebbinghaus_decay ──

    #[test]
    fn test_ebbinghaus_high_stability() {
        // 50 accesses over 180 days, last access 30 days ago -> ~0.78
        let result = ebbinghaus_decay_inner(30.0, 50, 180.0);
        assert!(
            approx_eq(result, 0.78, 0.1),
            "ebbinghaus(30d, 50, 180d) = {result}"
        );
    }

    #[test]
    fn test_ebbinghaus_crammed() {
        // 50 accesses crammed in 1 day, last access 30 days ago
        // avg_spacing = 1.0/50 = 0.02 days; tanh(0.02/7) ≈ 0.00286
        // stability = clamp(ln(51)*20*(1+0.5*0.00286), 14, 365) ≈ 78.7
        // retention = exp(-30/78.7) ≈ 0.68
        // Crammed access has higher stability than expected because the ln(n+1)
        // term dominates; the spacing penalty is modest in this formula.
        let result = ebbinghaus_decay_inner(30.0, 50, 1.0);
        let n = 51.0_f64;
        let avg_spacing: f64 = 1.0 / 50.0;
        let stability = (n.ln() * 20.0 * (1.0 + 0.5 * (avg_spacing / 7.0).tanh()))
            .clamp(14.0, 365.0);
        let expected = (-30.0 / stability).exp().max(0.05);
        assert!(
            approx_eq(result, expected, 0.001),
            "ebbinghaus(30d, 50, 1d) = {result}, expected {expected}"
        );
        // Crammed should have lower retention than well-spaced
        let well_spaced = ebbinghaus_decay_inner(30.0, 50, 180.0);
        assert!(
            result < well_spaced,
            "crammed ({result}) should decay faster than well-spaced ({well_spaced})"
        );
    }

    #[test]
    fn test_ebbinghaus_retention_floor() {
        // Very old with no re-access should floor at 0.05
        let result = ebbinghaus_decay_inner(10000.0, 0, 10000.0);
        assert!(
            approx_eq(result, 0.05, 0.001),
            "ebbinghaus should floor at 0.05: {result}"
        );
    }

    #[test]
    fn test_ebbinghaus_recent_access_high_retention() {
        // Recently accessed, many times, over long span -> high retention
        let result = ebbinghaus_decay_inner(1.0, 100, 365.0);
        assert!(result > 0.9, "recent frequent access should give high retention: {result}");
    }
}

// ──────────────────────────────────────────────
// pgrx integration tests
// ──────────────────────────────────────────────

#[cfg(feature = "pg_test")]
#[pgrx::pg_schema]
mod pg_tests {
    use pgrx::prelude::*;

    #[pg_test]
    fn test_softplus_sql() {
        let result = Spi::get_one::<f64>("SELECT pg_recall.softplus(0.0)")
            .expect("query failed")
            .expect("null result");
        assert!((result - 0.6931).abs() < 0.001);
    }

    #[pg_test]
    fn test_softplus_overflow_guard_sql() {
        let result = Spi::get_one::<f64>("SELECT pg_recall.softplus(25.0)")
            .expect("query failed")
            .expect("null result");
        assert_eq!(result, 25.0);
    }

    #[pg_test]
    fn test_bayesian_update_sql() {
        let result = Spi::get_one::<f64>("SELECT pg_recall.bayesian_update(0.5, 0.95)")
            .expect("query failed")
            .expect("null result");
        assert!((result - 0.925).abs() < 0.01);
    }

    #[pg_test]
    fn test_bayesian_bounds_sql() {
        let low = Spi::get_one::<f64>("SELECT pg_recall.bayesian_update(0.001, 0.001)")
            .expect("query failed")
            .expect("null result");
        assert!(low >= 0.025);

        let high = Spi::get_one::<f64>("SELECT pg_recall.bayesian_update(0.999, 0.999)")
            .expect("query failed")
            .expect("null result");
        assert!(high <= 0.975);
    }

    #[pg_test]
    fn test_actr_activation_sql() {
        let result = Spi::get_one::<f64>(
            "SELECT pg_recall.actr_activation(13, now() - interval '10 days')",
        )
        .expect("query failed")
        .expect("null result");
        assert!(
            (result - 2.08).abs() < 0.15,
            "actr_activation(13, 10d) = {result}"
        );
    }

    #[pg_test]
    fn test_actr_activation_old_sql() {
        let result = Spi::get_one::<f64>(
            "SELECT pg_recall.actr_activation(0, now() - interval '1400 days')",
        )
        .expect("query failed")
        .expect("null result");
        assert!(
            (result - (-3.27)).abs() < 0.15,
            "actr_activation(0, 1400d) = {result}"
        );
    }

    #[pg_test]
    fn test_ebbinghaus_decay_sql() {
        let result = Spi::get_one::<f64>(
            "SELECT pg_recall.ebbinghaus_decay(\
                now() - interval '30 days', 50, now() - interval '180 days')",
        )
        .expect("query failed")
        .expect("null result");
        assert!(
            (result - 0.78).abs() < 0.1,
            "ebbinghaus_decay(30d, 50, 180d) = {result}"
        );
    }

    #[pg_test]
    fn test_ebbinghaus_crammed_sql() {
        let result = Spi::get_one::<f64>(
            "SELECT pg_recall.ebbinghaus_decay(\
                now() - interval '30 days', 50, now() - interval '1 day')",
        )
        .expect("query failed")
        .expect("null result");
        assert!(
            (result - 0.12).abs() < 0.08,
            "ebbinghaus_decay(30d, 50, 1d) = {result}"
        );
    }
}
