-- pg_ghola Scoring Primitives Test Suite
-- Run against a database with pg_recall/pg_ghola installed
-- Tests the mathematical foundations independently

\set ON_ERROR_STOP on
\timing on

-- ============================================================
-- 1. ACT-R Activation
-- ============================================================

\echo '=== ACT-R Activation Tests ==='

-- High frequency, recent access → high activation
SELECT 'actr_high_freq_recent' AS test,
       pg_recall.actr_activation(100, now() - interval '1 hour') AS result,
       CASE WHEN pg_recall.actr_activation(100, now() - interval '1 hour') > 3.0
            THEN 'PASS' ELSE 'FAIL' END AS status;

-- Low frequency, old access → low activation
SELECT 'actr_low_freq_old' AS test,
       pg_recall.actr_activation(1, now() - interval '365 days') AS result,
       CASE WHEN pg_recall.actr_activation(1, now() - interval '365 days') < 0.0
            THEN 'PASS' ELSE 'FAIL' END AS status;

-- Zero access, just created → moderate (should not crash)
SELECT 'actr_zero_access' AS test,
       pg_recall.actr_activation(0, now()) AS result,
       CASE WHEN pg_recall.actr_activation(0, now()) IS NOT NULL
            THEN 'PASS' ELSE 'FAIL' END AS status;

-- Monotonicity: more accesses = higher activation (same recency)
SELECT 'actr_monotonic_frequency' AS test,
       CASE WHEN pg_recall.actr_activation(50, now() - interval '1 day')
               > pg_recall.actr_activation(5, now() - interval '1 day')
            THEN 'PASS' ELSE 'FAIL' END AS status;

-- Monotonicity: more recent = higher activation (same frequency)
SELECT 'actr_monotonic_recency' AS test,
       CASE WHEN pg_recall.actr_activation(10, now() - interval '1 hour')
               > pg_recall.actr_activation(10, now() - interval '30 days')
            THEN 'PASS' ELSE 'FAIL' END AS status;

-- ============================================================
-- 2. Ebbinghaus Decay
-- ============================================================

\echo '=== Ebbinghaus Decay Tests ==='

-- Spaced access should produce higher retention than massed access
SELECT 'ebbinghaus_spacing_effect' AS test,
       pg_recall.ebbinghaus_decay(now() - interval '30 days', 50, now() - interval '180 days') AS spaced,
       pg_recall.ebbinghaus_decay(now() - interval '30 days', 50, now() - interval '1 day') AS massed,
       CASE WHEN pg_recall.ebbinghaus_decay(now() - interval '30 days', 50, now() - interval '180 days')
               > pg_recall.ebbinghaus_decay(now() - interval '30 days', 50, now() - interval '1 day')
            THEN 'PASS' ELSE 'FAIL' END AS status;

-- Floor: never goes below 0.05
SELECT 'ebbinghaus_floor' AS test,
       pg_recall.ebbinghaus_decay(now() - interval '1000 days', 0, now() - interval '1001 days') AS result,
       CASE WHEN pg_recall.ebbinghaus_decay(now() - interval '1000 days', 0, now() - interval '1001 days') >= 0.05
            THEN 'PASS' ELSE 'FAIL' END AS status;

-- Recently accessed = high retention
SELECT 'ebbinghaus_recent' AS test,
       pg_recall.ebbinghaus_decay(now() - interval '1 minute', 10, now() - interval '30 days') AS result,
       CASE WHEN pg_recall.ebbinghaus_decay(now() - interval '1 minute', 10, now() - interval '30 days') > 0.95
            THEN 'PASS' ELSE 'FAIL' END AS status;

-- ============================================================
-- 3. Bayesian Confidence
-- ============================================================

\echo '=== Bayesian Confidence Tests ==='

-- Confirmation raises confidence
SELECT 'bayes_confirm' AS test,
       pg_recall.bayesian_update(0.5, 0.95) AS result,
       CASE WHEN pg_recall.bayesian_update(0.5, 0.95) > 0.9
            THEN 'PASS' ELSE 'FAIL' END AS status;

-- Contradiction lowers confidence
SELECT 'bayes_contradict' AS test,
       pg_recall.bayesian_update(0.8, 0.10) AS result,
       CASE WHEN pg_recall.bayesian_update(0.8, 0.10) < 0.4
            THEN 'PASS' ELSE 'FAIL' END AS status;

-- Never reaches 0 or 1
SELECT 'bayes_bounded_low' AS test,
       pg_recall.bayesian_update(
         pg_recall.bayesian_update(
           pg_recall.bayesian_update(0.5, 0.01), 0.01), 0.01) AS result,
       CASE WHEN pg_recall.bayesian_update(
              pg_recall.bayesian_update(
                pg_recall.bayesian_update(0.5, 0.01), 0.01), 0.01) > 0.02
            THEN 'PASS' ELSE 'FAIL' END AS status;

SELECT 'bayes_bounded_high' AS test,
       pg_recall.bayesian_update(
         pg_recall.bayesian_update(
           pg_recall.bayesian_update(0.5, 0.99), 0.99), 0.99) AS result,
       CASE WHEN pg_recall.bayesian_update(
              pg_recall.bayesian_update(
                pg_recall.bayesian_update(0.5, 0.99), 0.99), 0.99) < 0.98
            THEN 'PASS' ELSE 'FAIL' END AS status;

-- Neutral evidence (0.5) should barely change confidence
SELECT 'bayes_neutral' AS test,
       pg_recall.bayesian_update(0.7, 0.5) AS result,
       CASE WHEN abs(pg_recall.bayesian_update(0.7, 0.5) - 0.7) < 0.05
            THEN 'PASS' ELSE 'FAIL' END AS status;

-- ============================================================
-- 4. Softplus
-- ============================================================

\echo '=== Softplus Tests ==='

SELECT 'softplus_zero' AS test,
       pg_recall.softplus(0.0) AS result,
       CASE WHEN abs(pg_recall.softplus(0.0) - 0.6931) < 0.001
            THEN 'PASS' ELSE 'FAIL' END AS status;

-- Overflow guard: large x returns x
SELECT 'softplus_overflow' AS test,
       pg_recall.softplus(25.0) AS result,
       CASE WHEN abs(pg_recall.softplus(25.0) - 25.0) < 0.001
            THEN 'PASS' ELSE 'FAIL' END AS status;

-- Always positive
SELECT 'softplus_positive' AS test,
       CASE WHEN pg_recall.softplus(-100.0) > 0
            THEN 'PASS' ELSE 'FAIL' END AS status;

-- ============================================================
-- 5. Composite Recall (integration)
-- ============================================================

\echo '=== Composite Recall Integration Tests ==='

-- Count existing memories
SELECT 'recall_data_check' AS test,
       count(*) AS mneme_count,
       CASE WHEN count(*) > 0 THEN 'PASS' ELSE 'SKIP (no data)' END AS status
FROM pg_recall.mnemes WHERE state = 'active';

-- Recall should return results for a relevant query
-- (only if we have data)
DO $$
DECLARE
  result_count int;
  workspace uuid;
BEGIN
  SELECT workspace_id INTO workspace FROM pg_recall.mnemes LIMIT 1;
  IF workspace IS NULL THEN
    RAISE NOTICE 'SKIP: no mnemes in database';
    RETURN;
  END IF;

  SELECT count(*) INTO result_count
  FROM pg_recall.recall(
    workspace,
    'kubernetes cluster infrastructure',
    (SELECT embedding FROM pg_recall.mnemes WHERE workspace_id = workspace LIMIT 1),
    5, 0.0, NULL
  );

  IF result_count > 0 THEN
    RAISE NOTICE 'recall_returns_results: PASS (% results)', result_count;
  ELSE
    RAISE NOTICE 'recall_returns_results: FAIL (0 results)';
  END IF;
END $$;

-- Verify scoring components are populated in recall results
DO $$
DECLARE
  rec record;
  workspace uuid;
BEGIN
  SELECT workspace_id INTO workspace FROM pg_recall.mnemes LIMIT 1;
  IF workspace IS NULL THEN
    RAISE NOTICE 'SKIP: no mnemes in database';
    RETURN;
  END IF;

  SELECT * INTO rec
  FROM pg_recall.recall(
    workspace,
    'DNS failure',
    (SELECT embedding FROM pg_recall.mnemes WHERE workspace_id = workspace LIMIT 1),
    1, 0.0, NULL
  ) LIMIT 1;

  IF rec IS NULL THEN
    RAISE NOTICE 'recall_scoring_components: SKIP (no results)';
    RETURN;
  END IF;

  IF rec.score > 0 AND rec.content_match > 0 AND rec.confidence > 0 THEN
    RAISE NOTICE 'recall_scoring_components: PASS (score=%, content_match=%, activation=%, hebbian=%, confidence=%)',
      round(rec.score::numeric, 3), round(rec.content_match::numeric, 3),
      round(rec.activation::numeric, 3), round(rec.hebbian_boost::numeric, 3),
      round(rec.confidence::numeric, 3);
  ELSE
    RAISE NOTICE 'recall_scoring_components: FAIL (score=%, content_match=%, confidence=%)',
      rec.score, rec.content_match, rec.confidence;
  END IF;
END $$;

-- ============================================================
-- 6. Association & Co-activation
-- ============================================================

\echo '=== Association Tests ==='

SELECT 'associations_exist' AS test,
       count(*) AS assoc_count,
       CASE WHEN count(*) > 0 THEN 'PASS' ELSE 'SKIP (no associations)' END AS status
FROM pg_recall.associations;

-- Associations should have weights in valid range
SELECT 'association_weights_valid' AS test,
       min(weight) AS min_weight, max(weight) AS max_weight,
       CASE WHEN min(weight) >= 0 AND max(weight) <= 1.0
            THEN 'PASS' ELSE 'FAIL' END AS status
FROM pg_recall.associations;

-- ============================================================
-- 7. Worker Stats
-- ============================================================

\echo '=== Worker Stats ==='

SELECT * FROM pg_recall.get_worker_stats();

-- ============================================================
-- Summary
-- ============================================================

\echo '=== Done ==='
\echo 'Review PASS/FAIL results above. SKIP means insufficient test data.'
