// pg_recall::schema — Table, index, and constraint definitions
//
// Defines mnemes, associations, and co_activation_queue tables via
// pgrx extension_sql! macros. All objects live in the pg_recall schema
// (placed there by the control file's schema directive).
//
// Owned by: create_extension_schema task

use pgrx::prelude::*;

// ---------------------------------------------------------------------------
// Core tables
// ---------------------------------------------------------------------------

extension_sql!(
    r#"
-- mnemes: the primary memory store
CREATE TABLE mnemes (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    uuid NOT NULL,
    concept         text NOT NULL,
    content         text NOT NULL,
    embedding       vector(768) NOT NULL,
    search_vector   tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('english', concept), 'A') ||
        setweight(to_tsvector('english', content), 'B')
    ) STORED,
    confidence      double precision NOT NULL DEFAULT 0.5,
    access_count    integer NOT NULL DEFAULT 0,
    last_access     timestamptz NOT NULL DEFAULT now(),
    created_at      timestamptz NOT NULL DEFAULT now(),
    state           text NOT NULL DEFAULT 'active'
        CHECK (state IN ('active', 'archived', 'dormant'))
);
"#,
    name = "create_mnemes_table",
);

extension_sql!(
    r#"
-- associations: Hebbian links between mnemes (canonical ordering src < dst)
CREATE TABLE associations (
    src_id          uuid NOT NULL REFERENCES mnemes(id) ON DELETE CASCADE,
    dst_id          uuid NOT NULL REFERENCES mnemes(id) ON DELETE CASCADE,
    weight          double precision NOT NULL DEFAULT 0.01,
    co_activations  integer NOT NULL DEFAULT 0,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (src_id, dst_id),
    CHECK (src_id < dst_id)
);
"#,
    name = "create_associations_table",
    requires = ["create_mnemes_table"],
);

extension_sql!(
    r#"
-- co_activation_queue: pending co-activation events for batch Hebbian processing
CREATE TABLE co_activation_queue (
    id              bigserial PRIMARY KEY,
    workspace_id    uuid NOT NULL,
    mneme_ids       uuid[] NOT NULL,
    scores          double precision[] NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);
"#,
    name = "create_co_activation_queue_table",
);

// ---------------------------------------------------------------------------
// Contradiction detection
// ---------------------------------------------------------------------------

extension_sql!(
    r#"
-- contradiction_candidates: flagged pairs of potentially contradicting mnemes
CREATE TABLE contradiction_candidates (
    id              bigserial PRIMARY KEY,
    workspace_id    uuid NOT NULL,
    mneme_a         uuid NOT NULL REFERENCES mnemes(id) ON DELETE CASCADE,
    mneme_b         uuid NOT NULL REFERENCES mnemes(id) ON DELETE CASCADE,
    similarity      double precision NOT NULL,
    concept_overlap boolean NOT NULL DEFAULT false,
    status          text NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'confirmed', 'dismissed')),
    created_at      timestamptz NOT NULL DEFAULT now(),
    resolved_at     timestamptz,
    UNIQUE (mneme_a, mneme_b)
);
"#,
    name = "create_contradiction_candidates_table",
    requires = ["create_mnemes_table"],
);

extension_sql!(
    r#"
CREATE TYPE contradiction_candidate_result AS (
    candidate_id    bigint,
    mneme_a         uuid,
    mneme_b         uuid,
    similarity      float8,
    concept_overlap boolean
);
"#,
    name = "create_type_contradiction_candidate_result",
);

extension_sql!(
    r#"
CREATE TYPE contradiction_detail AS (
    candidate_id    bigint,
    similarity      float8,
    concept_overlap boolean,
    concept_a       text,
    content_a       text,
    confidence_a    float8,
    concept_b       text,
    content_b       text,
    confidence_b    float8,
    created_at      timestamptz
);
"#,
    name = "create_type_contradiction_detail",
);

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

extension_sql!(
    r#"
-- config: extension configuration (embedding dimensions, etc.)
CREATE TABLE config (
    key   text PRIMARY KEY,
    value text NOT NULL
);

-- Default embedding dimensions: 768 (BAAI/bge-base-en-v1.5, gte-modernbert-base)
INSERT INTO config (key, value) VALUES ('embedding_dims', '768');
"#,
    name = "create_config_table",
);

// ---------------------------------------------------------------------------
// Indexes
// ---------------------------------------------------------------------------

extension_sql!(
    r#"
-- HNSW index for fast approximate nearest-neighbor search on embeddings
CREATE INDEX mnemes_embedding_hnsw_idx
    ON mnemes USING hnsw (embedding vector_cosine_ops);

-- GIN index for full-text search on the generated tsvector column
CREATE INDEX mnemes_search_vector_gin_idx
    ON mnemes USING gin (search_vector);

-- B-tree index for workspace-scoped temporal queries
CREATE INDEX mnemes_workspace_last_access_idx
    ON mnemes (workspace_id, last_access DESC);

-- B-tree index for reverse association lookups (dst -> src)
CREATE INDEX associations_dst_src_idx
    ON associations (dst_id, src_id);

-- B-tree index for pending contradiction lookups by workspace
CREATE INDEX contradiction_candidates_workspace_idx
    ON contradiction_candidates (workspace_id, status);
"#,
    name = "create_indexes",
    requires = ["create_mnemes_table", "create_associations_table", "create_contradiction_candidates_table"],
);

// ---------------------------------------------------------------------------
// configure_dimensions: reconfigure embedding dimensions for non-default setups
// ---------------------------------------------------------------------------

/// Reconfigure the embedding dimension for this pg_recall installation.
///
/// Must be called on an empty mnemes table (errors if rows exist).
/// Drops and recreates the HNSW index with the new dimension.
///
/// Example: `SELECT pg_recall.configure_dimensions(3072)` for OpenAI text-embedding-3-large
#[pg_extern]
fn configure_dimensions(dims: i32) -> &'static str {
    if dims <= 0 || dims > 4096 {
        pgrx::error!("embedding dimensions must be between 1 and 4096, got {dims}");
    }

    Spi::connect_mut(|client| {
        // Verify mnemes table is empty
        let count = client
            .select(
                "SELECT count(*) FROM pg_recall.mnemes",
                None,
                &[],
            )
            .expect("failed to count mnemes")
            .into_iter()
            .next()
            .and_then(|r| r.get::<i64>(1).ok().flatten())
            .unwrap_or(0);

        if count > 0 {
            pgrx::error!(
                "cannot reconfigure dimensions: mnemes table has {count} rows. \
                 Drop all data first or recreate the extension."
            );
        }

        // Drop the HNSW index
        client
            .update(
                "DROP INDEX IF EXISTS pg_recall.mnemes_embedding_hnsw_idx",
                None,
                &[],
            )
            .expect("failed to drop HNSW index");

        // Alter the column type
        client
            .update(
                &format!(
                    "ALTER TABLE pg_recall.mnemes \
                     ALTER COLUMN embedding TYPE vector({dims})"
                ),
                None,
                &[],
            )
            .expect("failed to alter embedding column type");

        // Recreate the HNSW index
        client
            .update(
                "CREATE INDEX mnemes_embedding_hnsw_idx \
                 ON pg_recall.mnemes USING hnsw (embedding vector_cosine_ops)",
                None,
                &[],
            )
            .expect("failed to recreate HNSW index");

        // Update config
        client
            .update(
                &format!(
                    "UPDATE pg_recall.config SET value = '{dims}' \
                     WHERE key = 'embedding_dims'"
                ),
                None,
                &[],
            )
            .expect("failed to update config");
    });

    "ok"
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

#[cfg(any(test, feature = "pg_test"))]
#[pgrx::pg_schema]
mod tests {
    use pgrx::prelude::*;

    const DIMS: usize = 768;

    /// Helper: generate a zero-vector literal for DIMS dimensions.
    fn zero_embedding_literal() -> String {
        let zeros: Vec<String> = (0..DIMS).map(|_| "0".to_string()).collect();
        format!("[{}]", zeros.join(","))
    }

    #[pg_test]
    fn test_mnemes_table_exists() {
        // Verify the mnemes table was created in the pg_recall schema
        let count = Spi::get_one::<i64>(
            "SELECT count(*) FROM information_schema.tables
             WHERE table_schema = 'pg_recall' AND table_name = 'mnemes'",
        )
        .expect("query failed")
        .expect("null result");
        assert_eq!(count, 1, "mnemes table should exist in pg_recall schema");
    }

    #[pg_test]
    fn test_associations_table_exists() {
        let count = Spi::get_one::<i64>(
            "SELECT count(*) FROM information_schema.tables
             WHERE table_schema = 'pg_recall' AND table_name = 'associations'",
        )
        .expect("query failed")
        .expect("null result");
        assert_eq!(count, 1, "associations table should exist in pg_recall schema");
    }

    #[pg_test]
    fn test_co_activation_queue_table_exists() {
        let count = Spi::get_one::<i64>(
            "SELECT count(*) FROM information_schema.tables
             WHERE table_schema = 'pg_recall' AND table_name = 'co_activation_queue'",
        )
        .expect("query failed")
        .expect("null result");
        assert_eq!(
            count, 1,
            "co_activation_queue table should exist in pg_recall schema"
        );
    }

    #[pg_test]
    fn test_mnemes_insert_with_search_vector() {
        // Insert a row and verify search_vector is auto-populated
        let emb = zero_embedding_literal();
        Spi::run(&format!(
            "INSERT INTO pg_recall.mnemes (id, workspace_id, concept, content, embedding)
             VALUES (gen_random_uuid(), gen_random_uuid(), 'k8s', 'pod scheduling', '{emb}'::vector)"
        ))
        .expect("insert into mnemes should succeed");

        let sv = Spi::get_one::<String>(
            "SELECT search_vector::text FROM pg_recall.mnemes WHERE concept = 'k8s'",
        )
        .expect("query failed")
        .expect("search_vector should not be null");

        // The generated tsvector should contain terms from concept and content
        assert!(
            sv.contains("k8s") || sv.contains("pod") || sv.contains("schedul"),
            "search_vector should contain terms from concept/content, got: {sv}"
        );
    }

    #[pg_test]
    fn test_mnemes_state_check_valid() {
        let emb = zero_embedding_literal();
        // 'dormant' is a valid state
        Spi::run(&format!(
            "INSERT INTO pg_recall.mnemes (workspace_id, concept, content, embedding, state)
             VALUES (gen_random_uuid(), 'test', 'content', '{emb}'::vector, 'dormant')"
        ))
        .expect("inserting with state='dormant' should succeed");
    }

    #[pg_test]
    #[should_panic(expected = "violates check constraint")]
    fn test_mnemes_state_check_invalid() {
        let emb = zero_embedding_literal();
        // 'invalid' is not a valid state — should trigger CHECK violation
        Spi::run(&format!(
            "INSERT INTO pg_recall.mnemes (workspace_id, concept, content, embedding, state)
             VALUES (gen_random_uuid(), 'test', 'content', '{emb}'::vector, 'invalid')"
        ))
        .expect("should have failed");
    }

    #[pg_test]
    #[should_panic(expected = "violates check constraint")]
    fn test_associations_check_src_lt_dst() {
        let emb = zero_embedding_literal();
        // Insert two mnemes with known UUIDs where id_a > id_b
        let id_a = "ffffffff-ffff-ffff-ffff-ffffffffffff";
        let id_b = "00000000-0000-0000-0000-000000000001";
        Spi::run(&format!(
            "INSERT INTO pg_recall.mnemes (id, workspace_id, concept, content, embedding)
             VALUES ('{id_a}'::uuid, gen_random_uuid(), 'a', 'a content', '{emb}'::vector),
                    ('{id_b}'::uuid, gen_random_uuid(), 'b', 'b content', '{emb}'::vector)"
        ))
        .expect("inserting mnemes should succeed");

        // Try to insert association with src_id > dst_id — should fail
        Spi::run(&format!(
            "INSERT INTO pg_recall.associations (src_id, dst_id)
             VALUES ('{id_a}'::uuid, '{id_b}'::uuid)"
        ))
        .expect("should have failed with CHECK violation");
    }

    #[pg_test]
    fn test_associations_valid_insert() {
        let emb = zero_embedding_literal();
        let id_a = "00000000-0000-0000-0000-000000000001";
        let id_b = "ffffffff-ffff-ffff-ffff-ffffffffffff";
        Spi::run(&format!(
            "INSERT INTO pg_recall.mnemes (id, workspace_id, concept, content, embedding)
             VALUES ('{id_a}'::uuid, gen_random_uuid(), 'a', 'a content', '{emb}'::vector),
                    ('{id_b}'::uuid, gen_random_uuid(), 'b', 'b content', '{emb}'::vector)"
        ))
        .expect("inserting mnemes should succeed");

        // src_id < dst_id — should succeed
        Spi::run(&format!(
            "INSERT INTO pg_recall.associations (src_id, dst_id)
             VALUES ('{id_a}'::uuid, '{id_b}'::uuid)"
        ))
        .expect("inserting association with src < dst should succeed");
    }

    #[pg_test]
    fn test_indexes_exist() {
        // Verify all four indexes are present
        let indexes = vec![
            "mnemes_embedding_hnsw_idx",
            "mnemes_search_vector_gin_idx",
            "mnemes_workspace_last_access_idx",
            "associations_dst_src_idx",
        ];
        for idx_name in indexes {
            let count = Spi::get_one::<i64>(&format!(
                "SELECT count(*) FROM pg_indexes
                 WHERE schemaname = 'pg_recall' AND indexname = '{idx_name}'"
            ))
            .expect("query failed")
            .expect("null result");
            assert_eq!(count, 1, "index {idx_name} should exist in pg_recall schema");
        }
    }

    #[pg_test]
    fn test_co_activation_queue_insert() {
        Spi::run(
            "INSERT INTO pg_recall.co_activation_queue (workspace_id, mneme_ids, scores)
             VALUES (gen_random_uuid(), ARRAY[gen_random_uuid(), gen_random_uuid()]::uuid[], ARRAY[0.9, 0.7]::float8[])"
        )
        .expect("inserting into co_activation_queue should succeed");
    }

    #[pg_test]
    fn test_mnemes_default_values() {
        let emb = zero_embedding_literal();
        Spi::run(&format!(
            "INSERT INTO pg_recall.mnemes (workspace_id, concept, content, embedding)
             VALUES (gen_random_uuid(), 'test', 'content', '{emb}'::vector)"
        ))
        .expect("insert should succeed");

        // Check defaults
        let row = Spi::get_one::<String>(
            "SELECT concat(confidence::text, '|', access_count::text, '|', state)
             FROM pg_recall.mnemes WHERE concept = 'test'",
        )
        .expect("query failed")
        .expect("null result");

        assert_eq!(row, "0.5|0|active", "defaults should be confidence=0.5, access_count=0, state='active'");
    }

    #[pg_test]
    #[should_panic(expected = "expected 768 dimensions")]
    fn test_wrong_vector_dimensions_rejected() {
        // A 3-dim vector should be rejected by the vector(768) column type
        Spi::run(
            "INSERT INTO pg_recall.mnemes (workspace_id, concept, content, embedding) \
             VALUES (gen_random_uuid(), 'test', 'content', '[0.1, 0.2, 0.3]'::vector(768))"
        )
        .expect("should have failed");
    }

    #[pg_test]
    #[should_panic(expected = "violates foreign key constraint")]
    fn test_association_fk_rejects_nonexistent_mneme() {
        // Associations referencing non-existent mneme IDs should fail
        let fake_a = "00000000-0000-0000-0000-000000000001";
        let fake_b = "ffffffff-ffff-ffff-ffff-ffffffffffff";
        Spi::run(&format!(
            "INSERT INTO pg_recall.associations (src_id, dst_id, weight) \
             VALUES ('{fake_a}'::uuid, '{fake_b}'::uuid, 0.5)"
        ))
        .expect("should have failed with FK violation");
    }

    #[pg_test]
    fn test_cascade_delete_cleans_associations() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup");
        let emb = zero_embedding_literal();

        let id_a = "00000000-0000-0000-0000-00000000aa01";
        let id_b = "ffffffff-ffff-ffff-ffff-ffffffaa0002";
        Spi::run(&format!(
            "INSERT INTO pg_recall.mnemes (id, workspace_id, concept, content, embedding) \
             VALUES ('{id_a}'::uuid, gen_random_uuid(), 'a', 'content', '{emb}'::vector),
                    ('{id_b}'::uuid, gen_random_uuid(), 'b', 'content', '{emb}'::vector)"
        ))
        .expect("insert mnemes");

        Spi::run(&format!(
            "INSERT INTO pg_recall.associations (src_id, dst_id, weight) \
             VALUES ('{id_a}'::uuid, '{id_b}'::uuid, 0.5)"
        ))
        .expect("insert assoc");

        // Delete one mneme — association should cascade-delete
        Spi::run(&format!(
            "DELETE FROM pg_recall.mnemes WHERE id = '{id_a}'::uuid"
        ))
        .expect("delete mneme");

        let count = Spi::get_one::<i64>(
            "SELECT count(*) FROM pg_recall.associations"
        )
        .expect("query failed")
        .expect("null");

        assert_eq!(count, 0, "association should be cascade-deleted when mneme is deleted");
    }
}
