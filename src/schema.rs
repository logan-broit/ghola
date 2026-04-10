// pg_ghola::schema — Table, index, and constraint definitions
//
// Defines mnemes, associations, and co_activation_queue tables via
// pgrx extension_sql! macros. All objects live in the pg_ghola schema
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
    embedding       vector(1024) NOT NULL,
    search_vector   tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('english', concept), 'A') ||
        setweight(to_tsvector('english', content), 'B')
    ) STORED,
    confidence      double precision NOT NULL DEFAULT 1.0,
    access_count    integer NOT NULL DEFAULT 0,
    last_access     timestamptz NOT NULL DEFAULT now(),
    created_at      timestamptz NOT NULL DEFAULT now(),
    state           text NOT NULL DEFAULT 'active'
        CHECK (state IN ('active', 'archived', 'dormant')),
    memory_type     text NOT NULL DEFAULT 'factual'
        CHECK (memory_type IN ('factual', 'experiential', 'working')),
    scope           text NOT NULL DEFAULT 'personal'
        CHECK (scope IN ('personal', 'org')),
    tier            text NOT NULL DEFAULT 'index'
        CHECK (tier IN ('core', 'index', 'state')),
    tags            text[] NOT NULL DEFAULT '{}',
    session_id      uuid,
    expires_at      timestamptz,
    -- Thalamic gating columns (nullable; populated by async gating worker)
    entities        text[] DEFAULT NULL,
    content_dates   timestamptz[] DEFAULT NULL,
    cluster_id      integer DEFAULT NULL,
    intent          text DEFAULT NULL
        CHECK (intent IN ('decision', 'preference', 'fact', 'question', 'plan', 'experience'))
);
"#,
    name = "create_mnemes_table",
);

extension_sql!(
    r#"
-- associations: typed links between mnemes
-- Undirected types (hebbian, session): canonical ordering maintained by convention (src < dst)
-- Directed types (contradicts, supersedes, supports): src is subject, dst is object
CREATE TABLE associations (
    src_id              uuid NOT NULL REFERENCES mnemes(id) ON DELETE CASCADE,
    dst_id              uuid NOT NULL REFERENCES mnemes(id) ON DELETE CASCADE,
    association_type    text NOT NULL DEFAULT 'hebbian'
        CHECK (association_type IN ('hebbian', 'contradicts', 'supersedes', 'supports', 'session')),
    weight              double precision NOT NULL DEFAULT 0.01,
    co_activations      integer NOT NULL DEFAULT 0,
    updated_at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (src_id, dst_id, association_type)
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
// Contradiction queue: pending contradiction scans for async processing
// ---------------------------------------------------------------------------

extension_sql!(
    r#"
CREATE TABLE contradiction_queue (
    id           bigserial PRIMARY KEY,
    workspace_id uuid NOT NULL,
    mneme_id     uuid NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
"#,
    name = "create_contradiction_queue_table",
);

extension_sql!(
    r#"
CREATE TABLE contradiction_worker_stats (
    id                integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    state             text NOT NULL DEFAULT 'stopped',
    queue_depth       bigint NOT NULL DEFAULT 0,
    scans_completed   bigint NOT NULL DEFAULT 0,
    candidates_found  bigint NOT NULL DEFAULT 0,
    last_scan_at      timestamptz,
    poll_interval_ms  integer NOT NULL DEFAULT 5000,
    started_at        timestamptz,
    updated_at        timestamptz DEFAULT now()
);

INSERT INTO @extschema@.contradiction_worker_stats (id) VALUES (1) ON CONFLICT DO NOTHING;
"#,
    name = "create_contradiction_worker_stats_table",
);

// ---------------------------------------------------------------------------
// Gating queue: pending mnemes for async attribute extraction
// ---------------------------------------------------------------------------

extension_sql!(
    r#"
CREATE TABLE gating_queue (
    id           bigserial PRIMARY KEY,
    workspace_id uuid NOT NULL,
    mneme_id     uuid NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);
"#,
    name = "create_gating_queue_table",
);

extension_sql!(
    r#"
CREATE TABLE gating_worker_stats (
    id                integer PRIMARY KEY DEFAULT 1 CHECK (id = 1),
    state             text NOT NULL DEFAULT 'stopped',
    queue_depth       bigint NOT NULL DEFAULT 0,
    items_processed   bigint NOT NULL DEFAULT 0,
    last_process_at   timestamptz,
    poll_interval_ms  integer NOT NULL DEFAULT 5000,
    started_at        timestamptz,
    updated_at        timestamptz DEFAULT now()
);

INSERT INTO @extschema@.gating_worker_stats (id) VALUES (1) ON CONFLICT DO NOTHING;
"#,
    name = "create_gating_worker_stats_table",
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

-- B-tree index for memory_type filtering within a workspace
CREATE INDEX mnemes_memory_type_idx
    ON mnemes (workspace_id, memory_type);

-- B-tree index for session-based lookups
CREATE INDEX mnemes_session_id_idx
    ON mnemes (session_id) WHERE session_id IS NOT NULL;

-- GIN index for tag-based filtering
CREATE INDEX mnemes_tags_idx
    ON mnemes USING gin (tags);

-- B-tree index for working memory expiration
CREATE INDEX mnemes_expires_at_idx
    ON mnemes (expires_at) WHERE expires_at IS NOT NULL;

-- Gating indexes (partial -- only index populated rows)
CREATE INDEX mnemes_entities_gin_idx
    ON mnemes USING gin (entities) WHERE entities IS NOT NULL;

CREATE INDEX mnemes_content_dates_gin_idx
    ON mnemes USING gin (content_dates) WHERE content_dates IS NOT NULL;

CREATE INDEX mnemes_cluster_id_idx
    ON mnemes (cluster_id) WHERE cluster_id IS NOT NULL;

CREATE INDEX mnemes_intent_idx
    ON mnemes (intent) WHERE intent IS NOT NULL;
"#,
    name = "create_indexes",
    requires = ["create_mnemes_table", "create_associations_table", "create_contradiction_candidates_table"],
);

// ---------------------------------------------------------------------------
// configure_dimensions: reconfigure embedding dimensions for non-default setups
// ---------------------------------------------------------------------------

/// Reconfigure the embedding dimension for this pg_ghola installation.
///
/// Must be called on an empty mnemes table (errors if rows exist).
/// Drops and recreates the HNSW index with the new dimension.
///
/// Example: `SELECT ghola.configure_dimensions(3072)` for OpenAI text-embedding-3-large
#[pg_extern]
fn configure_dimensions(dims: i32) -> &'static str {
    if dims <= 0 || dims > 4096 {
        pgrx::error!("embedding dimensions must be between 1 and 4096, got {dims}");
    }

    Spi::connect_mut(|client| {
        // Verify mnemes table is empty
        let count = client
            .select(
                "SELECT count(*) FROM ghola.mnemes",
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
                "DROP INDEX IF EXISTS ghola.mnemes_embedding_hnsw_idx",
                None,
                &[],
            )
            .expect("failed to drop HNSW index");

        // Alter the column type
        client
            .update(
                &format!(
                    "ALTER TABLE ghola.mnemes \
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
                 ON ghola.mnemes USING hnsw (embedding vector_cosine_ops)",
                None,
                &[],
            )
            .expect("failed to recreate HNSW index");

        // Update config
        client
            .update(
                &format!(
                    "UPDATE ghola.config SET value = '{dims}' \
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
// Session association trigger: link mnemes sharing the same session_id
// ---------------------------------------------------------------------------

extension_sql!(
    r#"
CREATE OR REPLACE FUNCTION session_association_trigger()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- Only fire when the new mneme has a session_id
    IF NEW.session_id IS NOT NULL THEN
        INSERT INTO @extschema@.associations (src_id, dst_id, association_type, weight, co_activations, updated_at)
        SELECT NEW.id, m.id, 'session', 0.5, 1, now()
        FROM @extschema@.mnemes m
        WHERE m.workspace_id = NEW.workspace_id
          AND m.session_id = NEW.session_id
          AND m.id != NEW.id
        ON CONFLICT (src_id, dst_id, association_type) DO NOTHING;
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER mneme_session_association
    AFTER INSERT ON mnemes
    FOR EACH ROW
    EXECUTE FUNCTION session_association_trigger();
"#,
    name = "create_session_association_trigger",
    requires = [
        "create_mnemes_table",
        "create_associations_table",
    ],
);

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
        // Verify the mnemes table was created in the pg_ghola schema
        let count = Spi::get_one::<i64>(
            "SELECT count(*) FROM information_schema.tables
             WHERE table_schema = 'pg_ghola' AND table_name = 'mnemes'",
        )
        .expect("query failed")
        .expect("null result");
        assert_eq!(count, 1, "mnemes table should exist in pg_ghola schema");
    }

    #[pg_test]
    fn test_associations_table_exists() {
        let count = Spi::get_one::<i64>(
            "SELECT count(*) FROM information_schema.tables
             WHERE table_schema = 'pg_ghola' AND table_name = 'associations'",
        )
        .expect("query failed")
        .expect("null result");
        assert_eq!(count, 1, "associations table should exist in pg_ghola schema");
    }

    #[pg_test]
    fn test_co_activation_queue_table_exists() {
        let count = Spi::get_one::<i64>(
            "SELECT count(*) FROM information_schema.tables
             WHERE table_schema = 'pg_ghola' AND table_name = 'co_activation_queue'",
        )
        .expect("query failed")
        .expect("null result");
        assert_eq!(
            count, 1,
            "co_activation_queue table should exist in pg_ghola schema"
        );
    }

    #[pg_test]
    fn test_mnemes_insert_with_search_vector() {
        // Insert a row and verify search_vector is auto-populated
        let emb = zero_embedding_literal();
        Spi::run(&format!(
            "INSERT INTO ghola.mnemes (id, workspace_id, concept, content, embedding)
             VALUES (gen_random_uuid(), gen_random_uuid(), 'k8s', 'pod scheduling', '{emb}'::vector)"
        ))
        .expect("insert into mnemes should succeed");

        let sv = Spi::get_one::<String>(
            "SELECT search_vector::text FROM ghola.mnemes WHERE concept = 'k8s'",
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
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding, state)
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
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding, state)
             VALUES (gen_random_uuid(), 'test', 'content', '{emb}'::vector, 'invalid')"
        ))
        .expect("should have failed");
    }

    #[pg_test]
    fn test_associations_typed_insert() {
        let emb = zero_embedding_literal();
        let id_a = "00000000-0000-0000-0000-000000000001";
        let id_b = "ffffffff-ffff-ffff-ffff-ffffffffffff";
        Spi::run(&format!(
            "INSERT INTO ghola.mnemes (id, workspace_id, concept, content, embedding)
             VALUES ('{id_a}'::uuid, gen_random_uuid(), 'a', 'a content', '{emb}'::vector),
                    ('{id_b}'::uuid, gen_random_uuid(), 'b', 'b content', '{emb}'::vector)"
        ))
        .expect("inserting mnemes should succeed");

        // Same pair can have multiple association types
        Spi::run(&format!(
            "INSERT INTO ghola.associations (src_id, dst_id, association_type)
             VALUES ('{id_a}'::uuid, '{id_b}'::uuid, 'hebbian')"
        ))
        .expect("hebbian association should succeed");

        Spi::run(&format!(
            "INSERT INTO ghola.associations (src_id, dst_id, association_type)
             VALUES ('{id_a}'::uuid, '{id_b}'::uuid, 'supports')"
        ))
        .expect("supports association for same pair should succeed");

        let count = Spi::get_one::<i64>(
            "SELECT count(*) FROM ghola.associations"
        )
        .expect("query failed")
        .expect("null");
        assert_eq!(count, 2, "same pair should have 2 typed associations");
    }

    #[pg_test]
    fn test_associations_directional_insert() {
        let emb = zero_embedding_literal();
        let id_a = "00000000-0000-0000-0000-000000000001";
        let id_b = "ffffffff-ffff-ffff-ffff-ffffffffffff";
        Spi::run(&format!(
            "INSERT INTO ghola.mnemes (id, workspace_id, concept, content, embedding)
             VALUES ('{id_a}'::uuid, gen_random_uuid(), 'a', 'a content', '{emb}'::vector),
                    ('{id_b}'::uuid, gen_random_uuid(), 'b', 'b content', '{emb}'::vector)"
        ))
        .expect("inserting mnemes should succeed");

        // Directed: A contradicts B (src > dst is allowed for directed types)
        Spi::run(&format!(
            "INSERT INTO ghola.associations (src_id, dst_id, association_type)
             VALUES ('{id_b}'::uuid, '{id_a}'::uuid, 'contradicts')"
        ))
        .expect("directed association with src > dst should succeed");
    }

    #[pg_test]
    #[should_panic(expected = "violates check constraint")]
    fn test_associations_type_check_invalid() {
        let emb = zero_embedding_literal();
        let id_a = "00000000-0000-0000-0000-000000000001";
        let id_b = "ffffffff-ffff-ffff-ffff-ffffffffffff";
        Spi::run(&format!(
            "INSERT INTO ghola.mnemes (id, workspace_id, concept, content, embedding)
             VALUES ('{id_a}'::uuid, gen_random_uuid(), 'a', 'a content', '{emb}'::vector),
                    ('{id_b}'::uuid, gen_random_uuid(), 'b', 'b content', '{emb}'::vector)"
        ))
        .expect("inserting mnemes should succeed");

        Spi::run(&format!(
            "INSERT INTO ghola.associations (src_id, dst_id, association_type)
             VALUES ('{id_a}'::uuid, '{id_b}'::uuid, 'invalid_type')"
        ))
        .expect("should have failed");
    }

    #[pg_test]
    fn test_indexes_exist() {
        // Verify all indexes are present
        let indexes = vec![
            "mnemes_embedding_hnsw_idx",
            "mnemes_search_vector_gin_idx",
            "mnemes_workspace_last_access_idx",
            "associations_dst_src_idx",
            "mnemes_memory_type_idx",
            "mnemes_session_id_idx",
            "mnemes_tags_idx",
            "mnemes_expires_at_idx",
        ];
        for idx_name in indexes {
            let count = Spi::get_one::<i64>(&format!(
                "SELECT count(*) FROM pg_indexes
                 WHERE schemaname = 'pg_ghola' AND indexname = '{idx_name}'"
            ))
            .expect("query failed")
            .expect("null result");
            assert_eq!(count, 1, "index {idx_name} should exist in pg_ghola schema");
        }
    }

    #[pg_test]
    fn test_co_activation_queue_insert() {
        Spi::run(
            "INSERT INTO ghola.co_activation_queue (workspace_id, mneme_ids, scores)
             VALUES (gen_random_uuid(), ARRAY[gen_random_uuid(), gen_random_uuid()]::uuid[], ARRAY[0.9, 0.7]::float8[])"
        )
        .expect("inserting into co_activation_queue should succeed");
    }

    #[pg_test]
    fn test_mnemes_default_values() {
        let emb = zero_embedding_literal();
        Spi::run(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding)
             VALUES (gen_random_uuid(), 'test', 'content', '{emb}'::vector)"
        ))
        .expect("insert should succeed");

        // Check defaults including v0.4 typed columns
        let row = Spi::get_one::<String>(
            "SELECT concat(confidence::text, '|', access_count::text, '|', state, \
                           '|', memory_type, '|', scope, '|', tier)
             FROM ghola.mnemes WHERE concept = 'test'",
        )
        .expect("query failed")
        .expect("null result");

        assert_eq!(row, "0.5|0|active|factual|personal|index", "defaults should match");
    }

    #[pg_test]
    #[should_panic(expected = "expected 768 dimensions")]
    fn test_wrong_vector_dimensions_rejected() {
        // A 3-dim vector should be rejected by the vector(1024) column type
        Spi::run(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding) \
             VALUES (gen_random_uuid(), 'test', 'content', '[0.1, 0.2, 0.3]'::vector(1024))"
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
            "INSERT INTO ghola.associations (src_id, dst_id, weight) \
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
            "INSERT INTO ghola.mnemes (id, workspace_id, concept, content, embedding) \
             VALUES ('{id_a}'::uuid, gen_random_uuid(), 'a', 'content', '{emb}'::vector),
                    ('{id_b}'::uuid, gen_random_uuid(), 'b', 'content', '{emb}'::vector)"
        ))
        .expect("insert mnemes");

        Spi::run(&format!(
            "INSERT INTO ghola.associations (src_id, dst_id, weight) \
             VALUES ('{id_a}'::uuid, '{id_b}'::uuid, 0.5)"
        ))
        .expect("insert assoc");

        // Delete one mneme — association should cascade-delete
        Spi::run(&format!(
            "DELETE FROM ghola.mnemes WHERE id = '{id_a}'::uuid"
        ))
        .expect("delete mneme");

        let count = Spi::get_one::<i64>(
            "SELECT count(*) FROM ghola.associations"
        )
        .expect("query failed")
        .expect("null");

        assert_eq!(count, 0, "association should be cascade-deleted when mneme is deleted");
    }

    // ── v0.4 typed column tests ──

    #[pg_test]
    #[should_panic(expected = "violates check constraint")]
    fn test_memory_type_check_invalid() {
        let emb = zero_embedding_literal();
        Spi::run(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding, memory_type)
             VALUES (gen_random_uuid(), 'test', 'content', '{emb}'::vector, 'invalid')"
        ))
        .expect("should have failed");
    }

    #[pg_test]
    #[should_panic(expected = "violates check constraint")]
    fn test_scope_check_invalid() {
        let emb = zero_embedding_literal();
        Spi::run(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding, scope)
             VALUES (gen_random_uuid(), 'test', 'content', '{emb}'::vector, 'invalid')"
        ))
        .expect("should have failed");
    }

    #[pg_test]
    #[should_panic(expected = "violates check constraint")]
    fn test_tier_check_invalid() {
        let emb = zero_embedding_literal();
        Spi::run(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding, tier)
             VALUES (gen_random_uuid(), 'test', 'content', '{emb}'::vector, 'invalid')"
        ))
        .expect("should have failed");
    }

    #[pg_test]
    fn test_typed_columns_accept_valid_values() {
        let emb = zero_embedding_literal();
        // All valid combinations
        for (mtype, scope, tier) in &[
            ("factual", "personal", "core"),
            ("experiential", "org", "index"),
            ("working", "personal", "state"),
        ] {
            Spi::run(&format!(
                "INSERT INTO ghola.mnemes \
                 (workspace_id, concept, content, embedding, memory_type, scope, tier) \
                 VALUES (gen_random_uuid(), 'test', 'content', '{emb}'::vector, \
                         '{mtype}', '{scope}', '{tier}')"
            ))
            .unwrap_or_else(|_| panic!("should accept {mtype}/{scope}/{tier}"));
        }
    }

    #[pg_test]
    fn test_tags_and_session_id() {
        let emb = zero_embedding_literal();
        let sid = "00000000-0000-0000-0000-000000000099";
        Spi::run(&format!(
            "INSERT INTO ghola.mnemes \
             (workspace_id, concept, content, embedding, tags, session_id) \
             VALUES (gen_random_uuid(), 'tagged', 'content', '{emb}'::vector, \
                     ARRAY['rust', 'async']::text[], '{sid}'::uuid)"
        ))
        .expect("insert with tags and session_id should succeed");

        let tag_count = Spi::get_one::<i32>(
            "SELECT array_length(tags, 1) FROM ghola.mnemes WHERE concept = 'tagged'",
        )
        .expect("query failed")
        .expect("null");
        assert_eq!(tag_count, 2, "should have 2 tags");
    }

    #[pg_test]
    fn test_expires_at_column() {
        let emb = zero_embedding_literal();
        Spi::run(&format!(
            "INSERT INTO ghola.mnemes \
             (workspace_id, concept, content, embedding, memory_type, expires_at) \
             VALUES (gen_random_uuid(), 'ephemeral', 'temp content', '{emb}'::vector, \
                     'working', now() + interval '1 hour')"
        ))
        .expect("insert with expires_at should succeed");

        let has_expiry = Spi::get_one::<bool>(
            "SELECT expires_at IS NOT NULL FROM ghola.mnemes WHERE concept = 'ephemeral'",
        )
        .expect("query failed")
        .expect("null");
        assert!(has_expiry, "expires_at should be set");
    }

    #[pg_test]
    fn test_typed_indexes_exist() {
        let indexes = vec![
            "mnemes_memory_type_idx",
            "mnemes_session_id_idx",
            "mnemes_tags_idx",
            "mnemes_expires_at_idx",
        ];
        for idx_name in indexes {
            let count = Spi::get_one::<i64>(&format!(
                "SELECT count(*) FROM pg_indexes
                 WHERE schemaname = 'pg_ghola' AND indexname = '{idx_name}'"
            ))
            .expect("query failed")
            .expect("null result");
            assert_eq!(count, 1, "index {idx_name} should exist");
        }
    }

    // ── Thalamic gating schema tests ──

    #[pg_test]
    fn test_gating_columns_exist() {
        let emb = zero_embedding_literal();
        // Insert with gating columns set to non-null values
        Spi::run(&format!(
            "INSERT INTO ghola.mnemes \
             (workspace_id, concept, content, embedding, entities, content_dates, cluster_id, intent) \
             VALUES (gen_random_uuid(), 'gating test', 'content', '{emb}'::vector, \
                     ARRAY['sarah chen', 'new york']::text[], \
                     ARRAY['2026-04-09'::timestamptz]::timestamptz[], \
                     42, 'decision')"
        ))
        .expect("insert with gating columns should succeed");

        // Verify values round-trip
        let entity_count = Spi::get_one::<i32>(
            "SELECT array_length(entities, 1) FROM ghola.mnemes WHERE concept = 'gating test'",
        )
        .expect("query failed")
        .expect("null");
        assert_eq!(entity_count, 2, "should have 2 entities");

        let intent = Spi::get_one::<String>(
            "SELECT intent FROM ghola.mnemes WHERE concept = 'gating test'",
        )
        .expect("query failed")
        .expect("null");
        assert_eq!(intent, "decision");

        let cluster = Spi::get_one::<i32>(
            "SELECT cluster_id FROM ghola.mnemes WHERE concept = 'gating test'",
        )
        .expect("query failed")
        .expect("null");
        assert_eq!(cluster, 42);
    }

    #[pg_test]
    fn test_gating_columns_nullable() {
        let emb = zero_embedding_literal();
        // Gating columns should default to NULL
        Spi::run(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding) \
             VALUES (gen_random_uuid(), 'null gating', 'content', '{emb}'::vector)"
        ))
        .expect("insert without gating columns should succeed");

        let is_null = Spi::get_one::<bool>(
            "SELECT entities IS NULL AND content_dates IS NULL \
                    AND cluster_id IS NULL AND intent IS NULL \
             FROM ghola.mnemes WHERE concept = 'null gating'",
        )
        .expect("query failed")
        .expect("null");
        assert!(is_null, "gating columns should default to NULL");
    }

    #[pg_test]
    #[should_panic(expected = "violates check constraint")]
    fn test_intent_check_invalid() {
        let emb = zero_embedding_literal();
        Spi::run(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding, intent) \
             VALUES (gen_random_uuid(), 'test', 'content', '{emb}'::vector, 'invalid_intent')"
        ))
        .expect("should have failed");
    }

    #[pg_test]
    fn test_intent_check_valid_values() {
        let emb = zero_embedding_literal();
        for intent in &["decision", "preference", "fact", "question", "plan", "experience"] {
            Spi::run(&format!(
                "INSERT INTO ghola.mnemes \
                 (workspace_id, concept, content, embedding, intent) \
                 VALUES (gen_random_uuid(), 'intent test {intent}', 'content', '{emb}'::vector, '{intent}')"
            ))
            .unwrap_or_else(|_| panic!("should accept intent='{intent}'"));
        }
    }

    #[pg_test]
    fn test_gating_queue_table_exists() {
        let count = Spi::get_one::<i64>(
            "SELECT count(*) FROM information_schema.tables \
             WHERE table_schema = 'pg_ghola' AND table_name = 'gating_queue'",
        )
        .expect("query failed")
        .expect("null");
        assert_eq!(count, 1, "gating_queue table should exist in pg_ghola schema");
    }

    #[pg_test]
    fn test_gating_queue_insert() {
        Spi::run(
            "INSERT INTO ghola.gating_queue (workspace_id, mneme_id) \
             VALUES (gen_random_uuid(), gen_random_uuid())"
        )
        .expect("inserting into gating_queue should succeed");
    }

    #[pg_test]
    fn test_gating_worker_stats_table_exists() {
        let count = Spi::get_one::<i64>(
            "SELECT count(*) FROM information_schema.tables \
             WHERE table_schema = 'pg_ghola' AND table_name = 'gating_worker_stats'",
        )
        .expect("query failed")
        .expect("null");
        assert_eq!(count, 1, "gating_worker_stats table should exist in pg_ghola schema");
    }

    #[pg_test]
    fn test_gating_worker_stats_has_initial_row() {
        let state = Spi::get_one::<String>(
            "SELECT state FROM ghola.gating_worker_stats WHERE id = 1",
        )
        .expect("query failed")
        .expect("null");
        assert_eq!(state, "stopped", "initial gating worker state should be 'stopped'");
    }

    #[pg_test]
    fn test_gating_enqueue_trigger_fires() {
        // Verify mneme insert enqueues to BOTH contradiction_queue and gating_queue
        Spi::run("DELETE FROM ghola.contradiction_queue").expect("clear");
        Spi::run("DELETE FROM ghola.gating_queue").expect("clear");

        let emb = zero_embedding_literal();
        Spi::run(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding) \
             VALUES (gen_random_uuid(), 'trigger test', 'content', '{emb}'::vector)"
        ))
        .expect("insert should succeed");

        let cq_count = Spi::get_one::<i64>(
            "SELECT count(*) FROM ghola.contradiction_queue",
        )
        .expect("query failed")
        .expect("null");
        assert_eq!(cq_count, 1, "contradiction_queue should have 1 entry");

        let gq_count = Spi::get_one::<i64>(
            "SELECT count(*) FROM ghola.gating_queue",
        )
        .expect("query failed")
        .expect("null");
        assert_eq!(gq_count, 1, "gating_queue should have 1 entry");
    }

    #[pg_test]
    fn test_gating_indexes_exist() {
        let indexes = vec![
            "mnemes_entities_gin_idx",
            "mnemes_content_dates_gin_idx",
            "mnemes_cluster_id_idx",
            "mnemes_intent_idx",
        ];
        for idx_name in indexes {
            let count = Spi::get_one::<i64>(&format!(
                "SELECT count(*) FROM pg_indexes \
                 WHERE schemaname = 'pg_ghola' AND indexname = '{idx_name}'"
            ))
            .expect("query failed")
            .expect("null");
            assert_eq!(count, 1, "index {idx_name} should exist in pg_ghola schema");
        }
    }

    #[pg_test]
    fn test_config_table_has_default_dims() {
        let dims = Spi::get_one::<String>(
            "SELECT value FROM ghola.config WHERE key = 'embedding_dims'",
        )
        .expect("query failed")
        .expect("null");
        assert_eq!(dims, "768", "default embedding_dims should be 768");
    }

    // ── session association trigger ──

    #[pg_test]
    fn test_session_trigger_creates_associations() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup");

        let ws = "00000000-0000-0000-0000-aaaaaaaaaaaa";
        let session = "11111111-1111-1111-1111-111111111111";
        let emb = zero_embedding_literal();

        // Disable contradiction trigger to avoid interference
        Spi::run(
            "ALTER TABLE ghola.mnemes DISABLE TRIGGER mneme_insert_enqueue"
        ).expect("disable trigger");

        // Insert two mnemes with the same session_id
        let m1 = Spi::get_one::<String>(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding, session_id) \
             VALUES ('{ws}', 'topic', 'first note', '{emb}'::vector({DIMS}), '{session}'::uuid) \
             RETURNING id::text"
        ))
        .expect("insert failed")
        .expect("null");

        let m2 = Spi::get_one::<String>(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding, session_id) \
             VALUES ('{ws}', 'topic', 'second note', '{emb}'::vector({DIMS}), '{session}'::uuid) \
             RETURNING id::text"
        ))
        .expect("insert failed")
        .expect("null");

        // Should have created a session association m2 → m1
        let count = Spi::get_one::<i64>(&format!(
            "SELECT count(*) FROM ghola.associations \
             WHERE src_id = '{m2}'::uuid AND dst_id = '{m1}'::uuid \
               AND association_type = 'session'"
        ))
        .expect("query failed")
        .expect("null");

        assert_eq!(count, 1, "session trigger should create association between session peers");

        Spi::run(
            "ALTER TABLE ghola.mnemes ENABLE TRIGGER mneme_insert_enqueue"
        ).expect("enable trigger");
    }

    #[pg_test]
    fn test_session_trigger_no_association_without_session_id() {
        Spi::run("CREATE EXTENSION IF NOT EXISTS vector").expect("vector setup");

        let ws = "00000000-0000-0000-0000-bbbbbbbbbbbb";
        let emb = zero_embedding_literal();

        Spi::run(
            "ALTER TABLE ghola.mnemes DISABLE TRIGGER mneme_insert_enqueue"
        ).expect("disable trigger");

        // Insert two mnemes without session_id
        Spi::run(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding) \
             VALUES ('{ws}', 'topic', 'no session 1', '{emb}'::vector({DIMS}))"
        ))
        .expect("insert failed");

        Spi::run(&format!(
            "INSERT INTO ghola.mnemes (workspace_id, concept, content, embedding) \
             VALUES ('{ws}', 'topic', 'no session 2', '{emb}'::vector({DIMS}))"
        ))
        .expect("insert failed");

        let count = Spi::get_one::<i64>(
            "SELECT count(*) FROM ghola.associations WHERE association_type = 'session'",
        )
        .expect("query failed")
        .expect("null");

        assert_eq!(count, 0, "no session associations without session_id");

        Spi::run(
            "ALTER TABLE ghola.mnemes ENABLE TRIGGER mneme_insert_enqueue"
        ).expect("enable trigger");
    }
}
