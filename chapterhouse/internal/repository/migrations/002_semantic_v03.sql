-- pg_ghola v0.3: predictive-replay shape for semantic.mnemes.
--
-- The v0.2 LLM-distillation pipeline never ran in production, so the
-- text-summary columns (concept/content/memory_type/tags/entities/
-- source_episodic_ids) are dropped destructively. The new shape is
-- centred on a single embedding + level + member_ids, which the JEPA
-- replay path uses for cosine recall and clustering.
--
-- ${EMBEDDING_DIM} is substituted at migration-apply time by the
-- runner from the EMBEDDING_DIM environment variable, exactly the
-- same convention used by 001_episodic.sql.

CREATE SCHEMA IF NOT EXISTS semantic;

-- Drop any prior v0.2 table outright. Truncate-then-alter would not
-- survive the column-set diff, and there is no production data to
-- preserve. Idempotent for fresh installs via IF EXISTS.
DROP TABLE IF EXISTS semantic.mnemes CASCADE;

CREATE TABLE semantic.mnemes (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id          uuid NOT NULL,
    level                 integer NOT NULL DEFAULT 1,
    embedding             vector(${EMBEDDING_DIM}) NOT NULL,
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

CREATE INDEX mnemes_by_level
    ON semantic.mnemes (workspace_id, level);
CREATE INDEX mnemes_embedding_hnsw
    ON semantic.mnemes USING hnsw (embedding vector_cosine_ops);
CREATE INDEX mnemes_member_ids_gin
    ON semantic.mnemes USING gin (member_ids);
CREATE INDEX mnemes_last_reinforced
    ON semantic.mnemes (last_reinforced_at DESC) WHERE state = 'active';
