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

-- ${EMBEDDING_DIM} is substituted by docker-compose via envsubst-style
-- init hooks? No — the Postgres entrypoint doesn't run envsubst. We
-- bake a fixed dim for dev; override via compose build-arg if needed.
CREATE TABLE IF NOT EXISTS semantic.mnemes (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id          uuid NOT NULL,
    concept               text NOT NULL,
    content               text NOT NULL,
    embedding             vector(1024),
    confidence            double precision NOT NULL DEFAULT 0.5,
    access_count          integer NOT NULL DEFAULT 0,
    last_access           timestamptz NOT NULL DEFAULT now(),
    created_at            timestamptz NOT NULL DEFAULT now(),
    state                 text NOT NULL DEFAULT 'active',
    memory_type           text NOT NULL DEFAULT 'factual',
    tags                  text[] NOT NULL DEFAULT '{}',
    entities              text[] NOT NULL DEFAULT '{}',
    source_episodic_ids   uuid[] NOT NULL DEFAULT '{}',
    contributor_user_ids  uuid[] NOT NULL DEFAULT '{}'
);

CREATE INDEX IF NOT EXISTS mnemes_workspace      ON semantic.mnemes (workspace_id);
CREATE INDEX IF NOT EXISTS mnemes_embedding_hnsw ON semantic.mnemes USING hnsw (embedding vector_cosine_ops);

CREATE OR REPLACE FUNCTION semantic.bayesian_update(prior double precision, evidence double precision)
RETURNS double precision
LANGUAGE SQL IMMUTABLE AS $$
    SELECT 0.95 * (prior * evidence /
             GREATEST(prior * evidence + (1-prior)*(1-evidence), 1e-9))
           + 0.025;
$$;

-- A stub recall function matching the pg_ghola signature so the
-- /v1/semantic/query endpoint returns something sensible in dev.
-- Postgres has no "CREATE TYPE IF NOT EXISTS"; guard with DO block.
DO $$
BEGIN
    CREATE TYPE semantic.recall_result AS (
        mneme_id       uuid,
        score          double precision,
        content_match  double precision,
        activation     double precision,
        hebbian_boost  double precision,
        confidence     double precision,
        concept        text,
        content        text
    );
EXCEPTION
    WHEN duplicate_object THEN NULL;
END$$;

CREATE OR REPLACE FUNCTION semantic.recall(
    ws        uuid,
    qtext     text,
    qembed    vector,
    limit_n   int,
    min_conf  double precision
) RETURNS SETOF semantic.recall_result
LANGUAGE SQL AS $$
    SELECT id, 0.5::double precision, 0.5::double precision,
           0.0::double precision, 0.0::double precision,
           confidence, concept, content
      FROM semantic.mnemes
     WHERE workspace_id = ws
       AND confidence >= min_conf
     ORDER BY 1 - (embedding <=> qembed) DESC
     LIMIT limit_n;
$$;

CREATE OR REPLACE FUNCTION semantic.update_confidence(
    mid uuid, evidence double precision
) RETURNS double precision LANGUAGE SQL AS $$
    UPDATE semantic.mnemes
    SET confidence = GREATEST(0.025, evidence)
    WHERE id = mid
    RETURNING confidence;
$$;
