// pg_ghola::types — Composite types for recall_result and score_weights
//
// These types are defined as SQL composite types via extension_sql! and are
// visible in the pg_ghola schema (placed there by the control file's
// schema = 'pg_ghola' directive).
//
// Downstream Rust modules import the struct definitions for internal use.
// The SQL composite types support standard tuple casting syntax, e.g.:
//   SELECT (gen_random_uuid(), 0.9, 0.8, 2.1, 0.3, 0.7, 'k8s', 'pod scheduling')::ghola.recall_result
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

    // One test per composite type that verifies all fields are accessible with
    // correct types and values. This replaces 13 separate catalog/cast/count tests.

    #[pg_test]
    fn test_recall_result_field_access() {
        // Verify every field of recall_result is accessible with correct types
        let row = Spi::connect(|client| {
            let rows = client
                .select(
                    "SELECT (r).mneme_id, (r).score, (r).content_match, \
                            (r).activation, (r).hebbian_boost, (r).confidence, \
                            (r).concept, (r).content \
                     FROM (SELECT (gen_random_uuid(), 0.9, 0.8, 2.1, 0.3, 0.7, 'k8s', 'pod scheduling')::ghola.recall_result AS r) sub",
                    None,
                    &[],
                )
                .expect("query failed");

            let row = rows.into_iter().next().expect("no rows");
            let _id: pgrx::Uuid = row.get::<pgrx::Uuid>(1).expect("err").expect("null mneme_id");
            let score: f64 = row.get::<f64>(2).expect("err").expect("null score");
            let cm: f64 = row.get::<f64>(3).expect("err").expect("null content_match");
            let act: f64 = row.get::<f64>(4).expect("err").expect("null activation");
            let heb: f64 = row.get::<f64>(5).expect("err").expect("null hebbian_boost");
            let conf: f64 = row.get::<f64>(6).expect("err").expect("null confidence");
            let concept: String = row.get::<String>(7).expect("err").expect("null concept");
            let content: String = row.get::<String>(8).expect("err").expect("null content");
            (score, cm, act, heb, conf, concept, content)
        });

        assert!((row.0 - 0.9).abs() < 1e-9, "score field");
        assert!((row.1 - 0.8).abs() < 1e-9, "content_match field");
        assert!((row.2 - 2.1).abs() < 1e-9, "activation field");
        assert!((row.3 - 0.3).abs() < 1e-9, "hebbian_boost field");
        assert!((row.4 - 0.7).abs() < 1e-9, "confidence field");
        assert_eq!(row.5, "k8s", "concept field");
        assert_eq!(row.6, "pod scheduling", "content field");
    }

    #[pg_test]
    fn test_score_weights_field_access() {
        // Verify all 4 fields of score_weights are accessible with correct values
        let row = Spi::connect(|client| {
            let rows = client
                .select(
                    "SELECT (w).semantic, (w).fts, (w).actr_decay, (w).hebbian_scale \
                     FROM (SELECT (0.7, 0.3, 0.5, 4.0)::ghola.score_weights AS w) sub",
                    None,
                    &[],
                )
                .expect("query failed");

            let row = rows.into_iter().next().expect("no rows");
            let semantic: f64 = row.get::<f64>(1).expect("err").expect("null");
            let fts: f64 = row.get::<f64>(2).expect("err").expect("null");
            let actr: f64 = row.get::<f64>(3).expect("err").expect("null");
            let heb: f64 = row.get::<f64>(4).expect("err").expect("null");
            (semantic, fts, actr, heb)
        });

        assert!((row.0 - 0.7).abs() < 1e-9, "semantic field");
        assert!((row.1 - 0.3).abs() < 1e-9, "fts field");
        assert!((row.2 - 0.5).abs() < 1e-9, "actr_decay field");
        assert!((row.3 - 4.0).abs() < 1e-9, "hebbian_scale field");
    }
}
