// pg_recall::types — Composite types for recall_result and score_weights
//
// These types are defined as SQL composite types via extension_sql! and are
// visible in the pg_recall schema (placed there by the control file's
// schema = 'pg_recall' directive).
//
// Downstream Rust modules import the struct definitions for internal use.
// The SQL composite types support standard tuple casting syntax, e.g.:
//   SELECT (gen_random_uuid(), 0.9, 0.8, 2.1, 0.3, 0.7, 'k8s', 'pod scheduling')::pg_recall.recall_result
//
// Owned by: define_composite_types task

use pgrx::prelude::*;

// ---------------------------------------------------------------------------
// SQL composite type: recall_result
//
// Represents a single recall result with all scoring components.
// Fields match DATA: recall_result from the spec.
// ---------------------------------------------------------------------------
pgrx::extension_sql!(
    r#"
CREATE TYPE recall_result AS (
    mneme_id      uuid,
    score         float8,
    content_match float8,
    activation    float8,
    hebbian_boost float8,
    confidence    float8,
    concept       text,
    content       text
);
"#,
    name = "create_type_recall_result",
);

// ---------------------------------------------------------------------------
// SQL composite type: score_weights
//
// Scoring weights for the recall function.
// Fields match DATA: score_weights from the spec.
//   semantic:      default 0.6, weight for vector cosine similarity
//   fts:           default 0.4, weight for full-text search rank
//   actr_decay:    default 0.5, ACT-R power-law decay exponent
//   hebbian_scale: default 4.0, multiplier for Hebbian boost
// ---------------------------------------------------------------------------
pgrx::extension_sql!(
    r#"
CREATE TYPE score_weights AS (
    semantic      float8,
    fts           float8,
    actr_decay    float8,
    hebbian_scale float8
);
"#,
    name = "create_type_score_weights",
);

// ---------------------------------------------------------------------------
// Rust-side structs for downstream module use
//
// These mirror the SQL composite types and can be used by other modules
// (scoring, recall, hebbian) for internal data passing.
// ---------------------------------------------------------------------------

/// Rust representation of the recall_result composite type.
/// Used by downstream modules (e.g., recall) to construct result rows.
#[derive(Debug, Clone)]
pub struct RecallResult {
    pub mneme_id: pgrx::Uuid,
    pub score: f64,
    pub content_match: f64,
    pub activation: f64,
    pub hebbian_boost: f64,
    pub confidence: f64,
    pub concept: String,
    pub content: String,
}

/// Rust representation of the score_weights composite type.
/// Used by downstream modules to pass scoring parameters.
#[derive(Debug, Clone, Copy)]
pub struct ScoreWeights {
    pub semantic: f64,
    pub fts: f64,
    pub actr_decay: f64,
    pub hebbian_scale: f64,
}

impl Default for ScoreWeights {
    fn default() -> Self {
        ScoreWeights {
            semantic: 0.6,
            fts: 0.4,
            actr_decay: 0.5,
            hebbian_scale: 4.0,
        }
    }
}

// ---------------------------------------------------------------------------
// Stub functions (kept for backward compatibility with bootstrap tests)
// ---------------------------------------------------------------------------

/// Stub for recall_result type verification.
#[pg_extern(immutable)]
fn recall_result_stub() -> &'static str {
    "recall_result type stub"
}

/// Stub for score_weights type verification.
#[pg_extern(immutable)]
fn score_weights_stub() -> &'static str {
    "score_weights type stub"
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(any(test, feature = "pg_test"))]
#[pg_schema]
mod tests {
    use pgrx::prelude::*;

    #[pg_test]
    fn test_recall_result_type_exists() {
        // Verify the recall_result type exists in pg_recall schema
        let exists = Spi::get_one::<bool>(
            "SELECT EXISTS(
                SELECT 1 FROM pg_type t
                JOIN pg_namespace n ON t.typnamespace = n.oid
                WHERE n.nspname = 'pg_recall' AND t.typname = 'recall_result'
            )"
        ).unwrap().unwrap();
        assert!(exists, "recall_result type should exist in pg_recall schema");
    }

    #[pg_test]
    fn test_score_weights_type_exists() {
        // Verify the score_weights type exists in pg_recall schema
        let exists = Spi::get_one::<bool>(
            "SELECT EXISTS(
                SELECT 1 FROM pg_type t
                JOIN pg_namespace n ON t.typnamespace = n.oid
                WHERE n.nspname = 'pg_recall' AND t.typname = 'score_weights'
            )"
        ).unwrap().unwrap();
        assert!(exists, "score_weights type should exist in pg_recall schema");
    }

    #[pg_test]
    fn test_recall_result_cast_all_fields() {
        // Cast a valid literal to pg_recall.recall_result and verify fields
        let result = Spi::get_one::<bool>(
            "SELECT (r).mneme_id IS NOT NULL
             FROM (SELECT (gen_random_uuid(), 0.9, 0.8, 2.1, 0.3, 0.7, 'k8s', 'pod scheduling')::pg_recall.recall_result AS r) sub"
        ).unwrap().unwrap();
        assert!(result, "mneme_id should be accessible from recall_result");
    }

    #[pg_test]
    fn test_recall_result_field_values() {
        // Verify individual field values are correct after casting
        let score = Spi::get_one::<f64>(
            "SELECT (r).score
             FROM (SELECT (gen_random_uuid(), 0.9, 0.8, 2.1, 0.3, 0.7, 'k8s', 'pod scheduling')::pg_recall.recall_result AS r) sub"
        ).unwrap().unwrap();
        assert!((score - 0.9).abs() < 1e-9, "score should be 0.9");

        let concept = Spi::get_one::<String>(
            "SELECT (r).concept
             FROM (SELECT (gen_random_uuid(), 0.9, 0.8, 2.1, 0.3, 0.7, 'k8s', 'pod scheduling')::pg_recall.recall_result AS r) sub"
        ).unwrap().unwrap();
        assert_eq!(concept, "k8s", "concept should be 'k8s'");

        let content = Spi::get_one::<String>(
            "SELECT (r).content
             FROM (SELECT (gen_random_uuid(), 0.9, 0.8, 2.1, 0.3, 0.7, 'k8s', 'pod scheduling')::pg_recall.recall_result AS r) sub"
        ).unwrap().unwrap();
        assert_eq!(content, "pod scheduling", "content should be 'pod scheduling'");
    }

    #[pg_test]
    fn test_score_weights_cast() {
        // Cast a valid literal to pg_recall.score_weights and verify fields
        let semantic = Spi::get_one::<f64>(
            "SELECT (w).semantic
             FROM (SELECT (0.7, 0.3, 0.5, 4.0)::pg_recall.score_weights AS w) sub"
        ).unwrap().unwrap();
        assert!((semantic - 0.7).abs() < 1e-9, "semantic should be 0.7");

        let fts = Spi::get_one::<f64>(
            "SELECT (w).fts
             FROM (SELECT (0.7, 0.3, 0.5, 4.0)::pg_recall.score_weights AS w) sub"
        ).unwrap().unwrap();
        assert!((fts - 0.3).abs() < 1e-9, "fts should be 0.3");

        let actr_decay = Spi::get_one::<f64>(
            "SELECT (w).actr_decay
             FROM (SELECT (0.7, 0.3, 0.5, 4.0)::pg_recall.score_weights AS w) sub"
        ).unwrap().unwrap();
        assert!((actr_decay - 0.5).abs() < 1e-9, "actr_decay should be 0.5");

        let hebbian_scale = Spi::get_one::<f64>(
            "SELECT (w).hebbian_scale
             FROM (SELECT (0.7, 0.3, 0.5, 4.0)::pg_recall.score_weights AS w) sub"
        ).unwrap().unwrap();
        assert!((hebbian_scale - 4.0).abs() < 1e-9, "hebbian_scale should be 4.0");
    }

    #[pg_test]
    fn test_recall_result_field_count() {
        // Verify that recall_result has exactly 8 fields
        let count = Spi::get_one::<i64>(
            "SELECT count(*)
             FROM pg_attribute a
             JOIN pg_type t ON a.attrelid = t.typrelid
             JOIN pg_namespace n ON t.typnamespace = n.oid
             WHERE n.nspname = 'pg_recall'
               AND t.typname = 'recall_result'
               AND a.attnum > 0
               AND NOT a.attisdropped"
        ).unwrap().unwrap();
        assert_eq!(count, 8, "recall_result should have exactly 8 fields");
    }

    #[pg_test]
    fn test_score_weights_field_count() {
        // Verify that score_weights has exactly 4 fields
        let count = Spi::get_one::<i64>(
            "SELECT count(*)
             FROM pg_attribute a
             JOIN pg_type t ON a.attrelid = t.typrelid
             JOIN pg_namespace n ON t.typnamespace = n.oid
             WHERE n.nspname = 'pg_recall'
               AND t.typname = 'score_weights'
               AND a.attnum > 0
               AND NOT a.attisdropped"
        ).unwrap().unwrap();
        assert_eq!(count, 4, "score_weights should have exactly 4 fields");
    }

    #[pg_test]
    fn test_recall_result_unnest_fields() {
        // Verify fields are individually selectable from unnested composite array
        // (matches spec EXAMPLE: SELECT r.score, r.concept FROM unnest(ARRAY[...]))
        let concept = Spi::get_one::<String>(
            "SELECT (r).concept FROM unnest(
                ARRAY[
                    ROW(gen_random_uuid(), 0.9, 0.8, 2.1, 0.3, 0.7, 'k8s', 'pod scheduling')::pg_recall.recall_result
                ]
             ) AS r LIMIT 1"
        ).unwrap().unwrap();
        assert_eq!(concept, "k8s");
    }

    #[pg_test]
    fn test_score_weights_default_values() {
        // Verify default weights match spec: semantic=0.6, fts=0.4, actr_decay=0.5, hebbian_scale=4.0
        let semantic = Spi::get_one::<f64>(
            "SELECT (w).semantic FROM (SELECT (0.6, 0.4, 0.5, 4.0)::pg_recall.score_weights AS w) sub"
        ).unwrap().unwrap();
        assert!((semantic - 0.6).abs() < 1e-9);
    }

    #[pg_test]
    fn test_recall_result_all_scoring_fields() {
        // Verify all scoring-related fields: content_match, activation, hebbian_boost
        let content_match = Spi::get_one::<f64>(
            "SELECT (r).content_match
             FROM (SELECT (gen_random_uuid(), 0.9, 0.8, 2.1, 0.3, 0.7, 'k8s', 'pod scheduling')::pg_recall.recall_result AS r) sub"
        ).unwrap().unwrap();
        assert!((content_match - 0.8).abs() < 1e-9);

        let activation = Spi::get_one::<f64>(
            "SELECT (r).activation
             FROM (SELECT (gen_random_uuid(), 0.9, 0.8, 2.1, 0.3, 0.7, 'k8s', 'pod scheduling')::pg_recall.recall_result AS r) sub"
        ).unwrap().unwrap();
        assert!((activation - 2.1).abs() < 1e-9);

        let hebbian_boost = Spi::get_one::<f64>(
            "SELECT (r).hebbian_boost
             FROM (SELECT (gen_random_uuid(), 0.9, 0.8, 2.1, 0.3, 0.7, 'k8s', 'pod scheduling')::pg_recall.recall_result AS r) sub"
        ).unwrap().unwrap();
        assert!((hebbian_boost - 0.3).abs() < 1e-9);

        let confidence = Spi::get_one::<f64>(
            "SELECT (r).confidence
             FROM (SELECT (gen_random_uuid(), 0.9, 0.8, 2.1, 0.3, 0.7, 'k8s', 'pod scheduling')::pg_recall.recall_result AS r) sub"
        ).unwrap().unwrap();
        assert!((confidence - 0.7).abs() < 1e-9);
    }

    // Keep backward-compatible stub tests
    #[pg_test]
    fn test_stub_functions_still_work() {
        let result = Spi::get_one::<&str>("SELECT pg_recall.recall_result_stub()").unwrap().unwrap();
        assert_eq!(result, "recall_result type stub");

        let result = Spi::get_one::<&str>("SELECT pg_recall.score_weights_stub()").unwrap().unwrap();
        assert_eq!(result, "score_weights type stub");
    }
}
