// pg_recall::types — Composite types for recall_result and score_weights
//
// These types are defined as pgrx composite types visible in the pg_recall schema.
// Downstream modules import these for function signatures.
// The control file's schema directive places all objects in pg_recall automatically.
//
// Owned by: define_composite_types task

use pgrx::prelude::*;

/// Composite type representing a single recall result with all scoring components.
///
/// Fields (from DATA: recall_result):
///   mneme_id:      uuid
///   score:         float, final composite score
///   content_match: float, vector + FTS fusion component
///   activation:    float, ACT-R base-level activation
///   hebbian_boost: float, association weight sum from co-candidates
///   confidence:    float, current Bayesian confidence
///   concept:       text
///   content:       text
#[pg_extern(immutable)]
fn recall_result_stub() -> &'static str {
    // Stub: will be replaced by define_composite_types task
    "recall_result type stub"
}

/// Composite type representing scoring weights for the recall function.
///
/// Fields (from DATA: score_weights):
///   semantic:      float, default 0.6
///   fts:           float, default 0.4
///   actr_decay:    float, default 0.5
///   hebbian_scale: float, default 4.0
#[pg_extern(immutable)]
fn score_weights_stub() -> &'static str {
    // Stub: will be replaced by define_composite_types task
    "score_weights type stub"
}
