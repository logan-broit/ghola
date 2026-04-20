// pg_ghola::schema — v2 semantic-tier DDL
//
// Five tables under the `semantic` schema:
//   - mnemes                    (primary store; distilled facts)
//   - associations              (hebbian / contradicts / supersedes / supports)
//   - co_activation_queue       (drained by the Hebbian worker)
//   - contradiction_queue       (drained by the contradiction worker)
//   - contradiction_candidates  (flagged pairs awaiting review)
//
// Default embedding dimension is 1024 (Qwen3-Embedding).
// `configure_dimensions(dims)` reconfigures the column + HNSW index on
// an empty table for teams running a different embedding model.
//
// Per the greenfield design doc, v2 drops sub_mnemes (that role moved
// to `episodic.turns` in chapterhouse), drops the cluster pathway, and
// drops gating columns. The Rust cognitive-primitive workers
// (consolidation, contradiction, hebbian) are preserved and retargeted
// to `semantic.*`.

use pgrx::prelude::*;

// ---------------------------------------------------------------------------
// Schema + mnemes
// ---------------------------------------------------------------------------

extension_sql!(
    r#"
CREATE TABLE semantic.mnemes (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id          uuid NOT NULL,
    concept               text NOT NULL,
    content               text NOT NULL,
    embedding             vector(1024) NOT NULL,
    search_vector         tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('english', concept), 'A') ||
        setweight(to_tsvector('english', content), 'B')
    ) STORED,
    confidence            double precision NOT NULL DEFAULT 0.5,
    access_count          integer NOT NULL DEFAULT 0,
    last_access           timestamptz NOT NULL DEFAULT now(),
    created_at            timestamptz NOT NULL DEFAULT now(),
    state                 text NOT NULL DEFAULT 'active'
        CHECK (state IN ('active','archived')),
    memory_type           text NOT NULL DEFAULT 'factual'
        CHECK (memory_type IN ('factual','experiential','procedural')),
    tags                  text[] NOT NULL DEFAULT '{}',
    entities              text[] NOT NULL DEFAULT '{}',
    source_episodic_ids   uuid[] NOT NULL DEFAULT '{}',
    contributor_user_ids  uuid[] NOT NULL DEFAULT '{}'
);

CREATE INDEX mnemes_workspace      ON semantic.mnemes (workspace_id);
CREATE INDEX mnemes_embedding_hnsw ON semantic.mnemes USING hnsw (embedding vector_cosine_ops);
CREATE INDEX mnemes_search_gin     ON semantic.mnemes USING gin (search_vector);
CREATE INDEX mnemes_entities_gin   ON semantic.mnemes USING gin (entities);
CREATE INDEX mnemes_tags_gin       ON semantic.mnemes USING gin (tags);
CREATE INDEX mnemes_last_access    ON semantic.mnemes (last_access DESC)
    WHERE state = 'active';
"#,
    name = "create_semantic_schema_and_mnemes",
);

// ---------------------------------------------------------------------------
// Associations (hebbian / contradicts / supersedes / supports)
// ---------------------------------------------------------------------------

extension_sql!(
    r#"
CREATE TABLE semantic.associations (
    src_id            uuid NOT NULL REFERENCES semantic.mnemes(id) ON DELETE CASCADE,
    dst_id            uuid NOT NULL REFERENCES semantic.mnemes(id) ON DELETE CASCADE,
    association_type  text NOT NULL
        CHECK (association_type IN ('hebbian','contradicts','supersedes','supports')),
    weight            double precision NOT NULL DEFAULT 0.01,
    co_activations    integer NOT NULL DEFAULT 0,
    updated_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (src_id, dst_id, association_type)
);
"#,
    name = "create_associations_table",
    requires = ["create_semantic_schema_and_mnemes"],
);

// ---------------------------------------------------------------------------
// Worker queues + contradiction candidates
// ---------------------------------------------------------------------------

extension_sql!(
    r#"
CREATE TABLE semantic.co_activation_queue (
    id          bigserial PRIMARY KEY,
    src_id      uuid NOT NULL,
    dst_id      uuid NOT NULL,
    enqueued_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE semantic.contradiction_queue (
    id          bigserial PRIMARY KEY,
    mneme_id    uuid NOT NULL,
    enqueued_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE semantic.contradiction_candidates (
    id           bigserial PRIMARY KEY,
    mneme_a      uuid NOT NULL REFERENCES semantic.mnemes(id) ON DELETE CASCADE,
    mneme_b      uuid NOT NULL REFERENCES semantic.mnemes(id) ON DELETE CASCADE,
    similarity   double precision NOT NULL,
    detected_at  timestamptz NOT NULL DEFAULT now(),
    status       text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending','resolved','dismissed'))
);
"#,
    name = "create_queue_tables",
    requires = ["create_semantic_schema_and_mnemes"],
);

// ---------------------------------------------------------------------------
// configure_dimensions: reconfigure embedding dimensions on an empty table
// ---------------------------------------------------------------------------

/// Reconfigure the embedding dimension on `semantic.mnemes`. Must be
/// called on an empty table; drops and recreates the HNSW index.
///
/// Example: `SELECT semantic.configure_dimensions(3072)` for
/// OpenAI text-embedding-3-large.
#[pg_extern]
fn configure_dimensions(dims: i32) -> &'static str {
    if dims <= 0 || dims > 4096 {
        pgrx::error!("embedding dimensions must be between 1 and 4096, got {dims}");
    }

    Spi::connect_mut(|client| {
        let count = client
            .select("SELECT count(*) FROM semantic.mnemes", None, &[])
            .expect("failed to count mnemes")
            .into_iter()
            .next()
            .and_then(|r| r.get::<i64>(1).ok().flatten())
            .unwrap_or(0);

        if count > 0 {
            pgrx::error!(
                "cannot reconfigure dimensions: semantic.mnemes has {count} rows. \
                 Drop all data first or recreate the extension."
            );
        }

        client
            .update(
                "DROP INDEX IF EXISTS semantic.mnemes_embedding_hnsw",
                None,
                &[],
            )
            .expect("failed to drop HNSW index");

        client
            .update(
                &format!(
                    "ALTER TABLE semantic.mnemes \
                     ALTER COLUMN embedding TYPE vector({dims})"
                ),
                None,
                &[],
            )
            .expect("failed to alter embedding column");

        client
            .update(
                "CREATE INDEX mnemes_embedding_hnsw \
                 ON semantic.mnemes USING hnsw (embedding vector_cosine_ops)",
                None,
                &[],
            )
            .expect("failed to recreate HNSW index");
    });

    "ok"
}
