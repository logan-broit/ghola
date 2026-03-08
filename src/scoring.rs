// pg_recall::scoring — Pure cognitive scoring functions
//
// Implements softplus, actr_activation, ebbinghaus_decay, bayesian_update.
// All functions are stateless, immutable, and parallel-safe.
// The control file's schema directive places all objects in pg_recall automatically.
//
// Owned by: implement_scoring_primitives task

use pgrx::prelude::*;
use pgrx::datum::TimestampWithTimeZone;

/// Microseconds per day, used for timestamp-to-days conversion.
const USEC_PER_DAY: f64 = 86_400_000_000.0;

/// One-minute floor in days (1/1440), prevents division by zero for very recent accesses.
const ONE_MINUTE_DAYS: f64 = 1.0 / 1440.0;

// ──────────────────────────────────────────────
// Helpers: timestamp arithmetic
// ──────────────────────────────────────────────

/// Returns the current Postgres time as raw microseconds since PG epoch (2000-01-01).
fn pg_now_usec() -> i64 {
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

/// Returns ln(1 + exp(x)), with overflow guard: returns x directly when x > 20.
#[pg_extern(immutable, parallel_safe)]
fn softplus(x: f64) -> f64 {
    softplus_inner(x)
}

/// Pure computation, reusable from other modules without Postgres context.
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
#[pg_extern(immutable, parallel_safe)]
fn actr_activation(access_count: i32, last_access: TimestampWithTimeZone) -> f64 {
    let now_usec = pg_now_usec();
    let last_usec = tstz_to_usec(last_access);
    let age_days = ((now_usec - last_usec) as f64 / USEC_PER_DAY).max(ONE_MINUTE_DAYS);
    actr_activation_inner(access_count, age_days, 0.5)
}

/// Pure computation of ACT-R activation given pre-computed age in days.
/// Exposed so the recall module can pass a custom decay exponent d.
#[inline]
pub fn actr_activation_inner(access_count: i32, age_days: f64, d: f64) -> f64 {
    let age_days = age_days.max(ONE_MINUTE_DAYS);
    let n = (access_count + 1) as f64;
    // n already includes the +1 from access_count + 1
    // Formula: B = ln(n+1) - d * ln(age_days / (n+1))
    (n + 1.0).ln() - d * (age_days / (n + 1.0)).ln()
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
#[pg_extern(immutable, parallel_safe)]
fn ebbinghaus_decay(
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
#[pg_extern(immutable, parallel_safe)]
fn bayesian_update(prior: f64, evidence: f64) -> f64 {
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
mod unit_tests {
    use super::*;

    fn approx_eq(a: f64, b: f64, tol: f64) -> bool {
        (a - b).abs() < tol
    }

    // ── softplus ──

    #[test]
    fn test_softplus_zero() {
        // softplus(0.0) = ln(2) ≈ 0.6931
        assert!(approx_eq(softplus_inner(0.0), 0.6931, 0.001));
    }

    #[test]
    fn test_softplus_positive() {
        // softplus(2.0) ≈ 2.1269
        assert!(approx_eq(softplus_inner(2.0), 2.1269, 0.001));
    }

    #[test]
    fn test_softplus_negative() {
        // softplus(-5.0) ≈ 0.0067
        assert!(approx_eq(softplus_inner(-5.0), 0.0067, 0.001));
    }

    #[test]
    fn test_softplus_overflow_guard() {
        // x > 20 returns x directly
        assert_eq!(softplus_inner(25.0), 25.0);
    }

    #[test]
    fn test_softplus_boundary_20() {
        // x = 20.0 should NOT trigger the guard (only x > 20)
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
        // actr_activation(13, 10 days ago) with d=0.5
        // n = access_count + 1 = 14
        // B = ln(n+1) - d * ln(age/(n+1)) = ln(15) - 0.5 * ln(10/15)
        let result = actr_activation_inner(13, 10.0, 0.5);
        let expected = 15.0_f64.ln() - 0.5 * (10.0 / 15.0_f64).ln();
        assert!(
            approx_eq(result, expected, 0.001),
            "actr_activation(13, 10d) = {result}, expected {expected}"
        );
        // Should be positive (high activation for frequent recent access)
        assert!(result > 2.0, "frequently accessed should have high activation: {result}");
    }

    #[test]
    fn test_actr_never_reaccessed_very_old() {
        // actr_activation(0, 1400 days ago) with d=0.5
        // n = 1, n+1 = 2
        // B = ln(2) - 0.5 * ln(1400/2)
        let result = actr_activation_inner(0, 1400.0, 0.5);
        let expected = 2.0_f64.ln() - 0.5 * (1400.0_f64 / 2.0).ln();
        assert!(
            approx_eq(result, expected, 0.001),
            "actr_activation(0, 1400d) = {result}, expected {expected}"
        );
        // Should be negative (low activation for old unused memories)
        assert!(result < -2.0, "old unused should have low activation: {result}");
    }

    #[test]
    fn test_actr_very_recent_clamped() {
        // 10 seconds = 10/86400 days < 1/1440 days, should be clamped
        let result = actr_activation_inner(5, 10.0 / 86400.0, 0.5);
        let clamped = actr_activation_inner(5, ONE_MINUTE_DAYS, 0.5);
        assert_eq!(result, clamped, "very recent should clamp to one-minute floor");
    }

    #[test]
    fn test_actr_custom_decay() {
        // Higher decay exponent -> lower activation for same age
        let d_low = actr_activation_inner(5, 30.0, 0.3);
        let d_high = actr_activation_inner(5, 30.0, 0.8);
        assert!(d_low > d_high, "higher decay should give lower activation");
    }

    // ── ebbinghaus_decay ──

    #[test]
    fn test_ebbinghaus_high_stability() {
        // 50 accesses over 180 days, last access 30 days ago
        // Well-spaced repetitions -> high stability -> slow decay
        let result = ebbinghaus_decay_inner(30.0, 50, 180.0);
        assert!(
            result > 0.5 && result < 1.0,
            "ebbinghaus(30d, 50, 180d) = {result}, expected moderate-high retention"
        );
    }

    #[test]
    fn test_ebbinghaus_crammed() {
        // 50 accesses crammed in 1 day, last access 30 days ago
        // Poor spacing -> lower stability -> faster decay
        let result = ebbinghaus_decay_inner(30.0, 50, 1.0);
        // Crammed should have lower retention than well-spaced
        let well_spaced = ebbinghaus_decay_inner(30.0, 50, 180.0);
        assert!(
            result < well_spaced,
            "crammed ({result}) should decay faster than well-spaced ({well_spaced})"
        );
    }

    #[test]
    fn test_ebbinghaus_retention_floor() {
        // Very old with almost no re-access should floor at 0.05
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

    #[test]
    fn test_ebbinghaus_stability_clamped_to_bounds() {
        // Stability should be clamped between 14 and 365
        // With 0 accesses: n=1, ln(1)*20 = 0 -> clamped to 14
        let result = ebbinghaus_decay_inner(14.0, 0, 100.0);
        let expected = (-14.0_f64 / 14.0).exp().max(0.05);
        assert!(
            approx_eq(result, expected, 0.01),
            "stability should be clamped to 14: result={result}, expected={expected}"
        );
    }
}

// ──────────────────────────────────────────────
// pgrx integration tests (require Postgres)
// ──────────────────────────────────────────────

// Integration tests: only test behavior that requires real Postgres timestamps,
// since pure math is fully covered by unit_tests above.

#[cfg(any(test, feature = "pg_test"))]
#[pg_schema]
mod tests {
    use pgrx::prelude::*;

    #[pg_test]
    fn test_actr_timestamp_age_clamping() {
        // Very recent access (10 seconds) should be clamped to one-minute floor,
        // producing very high activation. This tests the timestamptz→days conversion
        // path that unit tests can't exercise.
        let result = Spi::get_one::<f64>(
            "SELECT pg_recall.actr_activation(5, now() - interval '10 seconds')",
        )
        .expect("query failed")
        .expect("null result");
        assert!(
            result > 5.0,
            "very recent access should have high activation (age clamping), got {result}"
        );
    }

    #[pg_test]
    fn test_ebbinghaus_spacing_effect_via_timestamps() {
        // Crammed vs well-spaced via real timestamps — tests the timestamp
        // conversion path: same days_since, different lifespans.
        let crammed = Spi::get_one::<f64>(
            "SELECT pg_recall.ebbinghaus_decay(\
                now() - interval '30 days', 50, now() - interval '1 day')",
        )
        .expect("query failed")
        .expect("null result");
        let spaced = Spi::get_one::<f64>(
            "SELECT pg_recall.ebbinghaus_decay(\
                now() - interval '30 days', 50, now() - interval '180 days')",
        )
        .expect("query failed")
        .expect("null result");
        assert!(
            crammed < spaced,
            "crammed ({crammed}) should retain less than spaced ({spaced})"
        );
    }

    #[pg_test]
    fn test_bayesian_chained_updates_stay_bounded() {
        // Apply 20 rounds of contradicting evidence via SQL and verify bounds hold.
        // Tests that floating-point accumulation doesn't break Laplace bounds.
        let mut conf = 0.5_f64;
        for _ in 0..20 {
            conf = Spi::get_one::<f64>(&format!(
                "SELECT pg_recall.bayesian_update({conf}, 0.05)"
            ))
            .expect("query failed")
            .expect("null");
        }
        assert!(
            conf >= 0.025,
            "20 rounds of contradicting evidence should stay above floor: {conf}"
        );

        // Now 20 rounds of confirming evidence
        conf = 0.5;
        for _ in 0..20 {
            conf = Spi::get_one::<f64>(&format!(
                "SELECT pg_recall.bayesian_update({conf}, 0.99)"
            ))
            .expect("query failed")
            .expect("null");
        }
        assert!(
            conf <= 0.975,
            "20 rounds of confirming evidence should stay below ceiling: {conf}"
        );
    }
}
