-- pg_ghola 0.0.5 -> 0.0.6 migration: multi-granularity encoding (sub_mnemes)
--
-- Run as postgres in the `memories` database AFTER the 0.0.6 image has been
-- deployed to the CNPG cluster. The ch-server deployment MUST be scaled to 0
-- before running this: the recall_inner function's signature changes (new
-- matched_position column), so any in-flight recall queries would fail
-- between the binary swap and this catalog migration.
--
-- Extension is named pg_recall in this cluster (ch-system/memory-db), not
-- pg_ghola -- the Dockerfile symlinks pg_ghola.so to pg_recall.so and this
-- install was named pg_recall at CREATE EXTENSION time. All ALTER EXTENSION
-- commands use pg_recall; the function AS '$libdir/pg_recall' matches the
-- symlinked .so name.
--
-- Design: docs/plans/2026-04-16-multi-granularity-encoding-design.md
-- Impl:   docs/plans/2026-04-16-multi-granularity-encoding-implementation.md
--
-- This migration is TRANSACTION-ATOMIC. A failure mid-way rolls back to
-- pre-migration state. Can be re-run if rolled back.

BEGIN;

-- =============================================================================
-- 1. Detach existing recall surface from the extension so we can DROP it.
-- =============================================================================

ALTER EXTENSION pg_recall DROP FUNCTION ghola.recall(
    uuid, text, vector, int, float8, ghola.score_weights,
    text, text, text[], uuid, text[], text
);

ALTER EXTENSION pg_recall DROP FUNCTION ghola.recall_inner(
    uuid, text, text, int, float8, float8, float8, float8, float8,
    text, text, text[], uuid, text[], text
);

ALTER EXTENSION pg_recall DROP TYPE ghola.recall_result;

-- =============================================================================
-- 2. DROP the 8-column catalog entries. The new .so (now running in the pod)
-- expects the 9-column shape; the catalog must match before any recall call.
-- =============================================================================

DROP FUNCTION ghola.recall(
    uuid, text, vector, int, float8, ghola.score_weights,
    text, text, text[], uuid, text[], text
);

DROP FUNCTION ghola.recall_inner(
    uuid, text, text, int, float8, float8, float8, float8, float8,
    text, text, text[], uuid, text[], text
);

DROP TYPE ghola.recall_result;

-- =============================================================================
-- 3. CREATE the 9-column type with matched_position. Mirrors the
-- `extension_sql!` block in src/types.rs.
-- =============================================================================

CREATE TYPE ghola.recall_result AS (
    mneme_id         uuid,
    score            float8,
    content_match    float8,
    activation       float8,
    hebbian_boost    float8,
    confidence       float8,
    concept          text,
    content          text,
    matched_position smallint
);

-- =============================================================================
-- 4. CREATE the 9-column recall_inner pointing at the new .so.
-- Module path is pg_recall (the symlinked name, not pg_ghola).
-- Entry point is recall_inner_wrapper (pgrx convention: {function}_wrapper).
-- =============================================================================

CREATE FUNCTION ghola.recall_inner(
    "workspace_id"         uuid,
    "query_text"           text,
    "query_embedding_text" text,
    "limit_n"              int               DEFAULT 10,
    "min_confidence"       double precision  DEFAULT 0.0,
    "w_semantic"           double precision  DEFAULT 0.6,
    "w_fts"                double precision  DEFAULT 0.4,
    "w_actr_decay"         double precision  DEFAULT 0.5,
    "w_hebbian_scale"      double precision  DEFAULT 4.0,
    "filter_memory_type"   text              DEFAULT NULL,
    "filter_scope"         text              DEFAULT NULL,
    "filter_tags"          text[]            DEFAULT NULL,
    "filter_session_id"    uuid              DEFAULT NULL,
    "filter_entities"      text[]            DEFAULT NULL,
    "filter_intent"        text              DEFAULT NULL
) RETURNS TABLE (
    "mneme_id"         uuid,
    "score"            double precision,
    "content_match"    double precision,
    "activation"       double precision,
    "hebbian_boost"    double precision,
    "confidence"       double precision,
    "concept"          text,
    "content"          text,
    "matched_position" smallint
)
STABLE
LANGUAGE c
AS '$libdir/pg_recall', 'recall_inner_wrapper';

-- =============================================================================
-- 5. Re-create the SQL wrapper function. Mirrors src/recall.rs lines 26-67
-- with the additional matched_position field.
-- =============================================================================

CREATE FUNCTION ghola.recall(
    workspace_id      uuid,
    query_text        text,
    query_embedding   vector,
    limit_n           int                    DEFAULT 10,
    min_confidence    float8                 DEFAULT 0.0,
    weights           ghola.score_weights    DEFAULT NULL,
    memory_type       text                   DEFAULT NULL,
    scope             text                   DEFAULT NULL,
    tags              text[]                 DEFAULT NULL,
    session_id        uuid                   DEFAULT NULL,
    filter_entities   text[]                 DEFAULT NULL,
    filter_intent     text                   DEFAULT NULL
) RETURNS SETOF ghola.recall_result
LANGUAGE SQL
STABLE
AS $FN$
    SELECT (mneme_id, score, content_match, activation, hebbian_boost,
            confidence, concept, content, matched_position)::ghola.recall_result
    FROM ghola.recall_inner(
        workspace_id, query_text, query_embedding::text, limit_n, min_confidence,
        COALESCE((weights).semantic, 0.6),
        COALESCE((weights).fts, 0.4),
        COALESCE((weights).actr_decay, 0.5),
        COALESCE((weights).hebbian_scale, 4.0),
        memory_type, scope, tags, session_id,
        filter_entities, filter_intent
    );
$FN$;

-- =============================================================================
-- 6. Re-attach the new type and functions to the extension so DROP EXTENSION
-- cleans them up properly in the future.
-- =============================================================================

ALTER EXTENSION pg_recall ADD TYPE ghola.recall_result;

ALTER EXTENSION pg_recall ADD FUNCTION ghola.recall_inner(
    uuid, text, text, int, float8, float8, float8, float8, float8,
    text, text, text[], uuid, text[], text
);

ALTER EXTENSION pg_recall ADD FUNCTION ghola.recall(
    uuid, text, vector, int, float8, ghola.score_weights,
    text, text, text[], uuid, text[], text
);

-- =============================================================================
-- 7. Create sub_mnemes table and indexes. Mirrors src/schema.rs
-- create_sub_mnemes_table. Additive -- no data touched in existing tables.
-- =============================================================================

CREATE TABLE ghola.sub_mnemes (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    mneme_id        uuid NOT NULL REFERENCES ghola.mnemes(id) ON DELETE CASCADE,
    position        smallint NOT NULL,
    role            text NOT NULL
        CHECK (role IN ('user', 'assistant', 'system', 'tool')),
    content         text NOT NULL,
    embedding       vector(1024) NOT NULL,
    search_vector   tsvector GENERATED ALWAYS AS (
        to_tsvector('english', content)
    ) STORED,
    token_start     integer NOT NULL,
    token_end       integer NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now(),
    UNIQUE (mneme_id, position),
    CHECK (token_end >= token_start),
    CHECK (position >= 0)
);

CREATE INDEX sub_mnemes_embedding_hnsw_idx
    ON ghola.sub_mnemes USING hnsw (embedding vector_cosine_ops);

CREATE INDEX sub_mnemes_search_vector_gin_idx
    ON ghola.sub_mnemes USING gin (search_vector);

CREATE INDEX sub_mnemes_mneme_id_position_idx
    ON ghola.sub_mnemes (mneme_id, position);

-- =============================================================================
-- 8. Attach the new table to the extension.
-- =============================================================================

ALTER EXTENSION pg_recall ADD TABLE ghola.sub_mnemes;

-- =============================================================================
-- 9. Grant access to the application role (memory_api). The mnemes table had
-- these grants as part of its initial bootstrap; sub_mnemes needs them too
-- since ch-server connects as memory_api and calls RememberWithTurns which
-- does INSERT/SELECT against ghola.sub_mnemes.
-- =============================================================================

GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER
    ON TABLE ghola.sub_mnemes TO memory_api;

COMMIT;

-- =============================================================================
-- Verify (run after COMMIT):
--   SELECT extname, extversion FROM pg_extension WHERE extname = 'pg_recall';
--   SELECT column_name FROM information_schema.columns
--     WHERE table_schema = 'ghola' AND table_name = 'sub_mnemes';
--   \df+ ghola.recall_inner    -- should show TABLE(..., matched_position smallint)
--   SELECT count(*) FROM ghola.mnemes;       -- existing mneme data preserved
-- =============================================================================
