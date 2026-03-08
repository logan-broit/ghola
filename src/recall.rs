// pg_recall::recall — Composite recall function
//
// The primary retrieval entry point that fuses vector similarity, full-text search,
// ACT-R temporal activation, Hebbian association strength, and Bayesian confidence
// into a single ranked result set.
// The control file's schema directive places all objects in pg_recall automatically.
//
// Owned by: implement_composite_recall task

use pgrx::prelude::*;

/// Stub for the composite recall function.
/// The real implementation will be provided by the implement_composite_recall task.
///
/// SQL signature:
///   pg_recall.recall(workspace_id uuid, query_text text,
///     query_embedding vector(384), limit_n int DEFAULT 10,
///     min_confidence float DEFAULT 0.0,
///     weights pg_recall.score_weights DEFAULT NULL)
///   RETURNS SETOF pg_recall.recall_result
#[pg_extern]
fn recall_stub() -> &'static str {
    // Stub: will be replaced by implement_composite_recall task
    "recall function stub"
}
