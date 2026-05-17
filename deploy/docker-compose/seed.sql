-- Dev-only bootstrap. Production uses the real pg_ghola extension
-- (installed in the CNPG image) which creates semantic.* itself.
-- Here we stub enough of the semantic schema for the replay worker
-- to run end-to-end without a pgrx build.
--
-- Episodic migrations are applied by ch-init at service start, so
-- we do NOT duplicate them here.

CREATE EXTENSION IF NOT EXISTS pgcrypto;
CREATE EXTENSION IF NOT EXISTS vector;

CREATE SCHEMA IF NOT EXISTS semantic;

-- The Postgres entrypoint does not run envsubst, so we bake a fixed
-- dim for dev. Production substitutes ${EMBEDDING_DIM} via the
-- migration runner; the seed file mirrors the v0.3 column set with a
-- literal vector(1024).
CREATE TABLE IF NOT EXISTS semantic.mnemes (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id          uuid NOT NULL,
    level                 integer NOT NULL DEFAULT 1,
    embedding             vector(1024) NOT NULL,
    confidence            double precision NOT NULL DEFAULT 0.5,
    access_count          integer NOT NULL DEFAULT 0,
    member_ids            uuid[] NOT NULL DEFAULT '{}',
    contributor_user_ids  uuid[] NOT NULL DEFAULT '{}',
    created_at            timestamptz NOT NULL DEFAULT now(),
    last_reinforced_at    timestamptz NOT NULL DEFAULT now(),
    last_access           timestamptz NOT NULL DEFAULT now(),
    state                 text NOT NULL DEFAULT 'active'
        CHECK (state IN ('active','archived'))
);

CREATE INDEX IF NOT EXISTS mnemes_by_level
    ON semantic.mnemes (workspace_id, level);
CREATE INDEX IF NOT EXISTS mnemes_embedding_hnsw
    ON semantic.mnemes USING hnsw (embedding vector_cosine_ops);
CREATE INDEX IF NOT EXISTS mnemes_member_ids_gin
    ON semantic.mnemes USING gin (member_ids);
CREATE INDEX IF NOT EXISTS mnemes_last_reinforced
    ON semantic.mnemes (last_reinforced_at DESC) WHERE state = 'active';

CREATE OR REPLACE FUNCTION semantic.bayesian_update(prior double precision, evidence double precision)
RETURNS double precision
LANGUAGE SQL IMMUTABLE AS $$
    SELECT 0.95 * (prior * evidence /
             GREATEST(prior * evidence + (1-prior)*(1-evidence), 1e-9))
           + 0.025;
$$;

CREATE OR REPLACE FUNCTION semantic.update_confidence(
    mid uuid, evidence double precision
) RETURNS double precision LANGUAGE SQL AS $$
    UPDATE semantic.mnemes
    SET confidence = GREATEST(0.025, evidence)
    WHERE id = mid
    RETURNING confidence;
$$;
