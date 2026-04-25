-- pg_ghola v0.3: per-session L1 embedding on episodic.sessions.
--
-- The mentat service pools session-close events into a single
-- semantic-tier ("L1") vector that the reconciler later clusters into
-- semantic.mnemes. Storing the L1 vector on the session row gives the
-- replay path a cheap HNSW seed for clustering and recall without a
-- side table.
--
-- Most rows will not have an L1 vector until the reconciler (PR1.7)
-- fills them in, so the HNSW index is partial: empty rows stay out of
-- the index entirely.
--
-- ${EMBEDDING_DIM} is substituted at migration-apply time by the
-- runner from the EMBEDDING_DIM environment variable, the same
-- convention used by 001_episodic.sql and 002_semantic_v03.sql.

ALTER TABLE episodic.sessions
    ADD COLUMN IF NOT EXISTS l1_embedding vector(${EMBEDDING_DIM});

CREATE INDEX IF NOT EXISTS episodic_sessions_l1_hnsw
    ON episodic.sessions USING hnsw (l1_embedding vector_cosine_ops)
    WHERE l1_embedding IS NOT NULL;
