-- pg_ghola v0.4: per-session L1 chunk text on episodic.sessions.
--
-- Mirrors the l1_embedding artifact (003_sessions_l1.sql) on the text
-- side. mentat already pools event embeddings into l1_embedding at
-- session close; the same write site now also persists the role-
-- prefixed concatenation of the session's events as l1_chunk_text.
--
-- Why we need it: the recall path's cross-encoder reranker wants
-- session-level text, not single-event text. Building it on demand at
-- read time means N+1 queries against episodic.events for the top-K
-- candidates per recall — expensive in the hot path. Persisting it on
-- the session row turns recall into one JOIN. (See ghola PR-C for the
-- consumer.)
--
-- l1_chunk_text is NULL for sessions whose l1_embedding hasn't been
-- written yet (open sessions, or pre-migration closed sessions until
-- the backfill runs). Readers must handle NULL by falling back to
-- whatever per-event content they already have.

ALTER TABLE episodic.sessions
    ADD COLUMN IF NOT EXISTS l1_chunk_text TEXT;

-- Backfill l1_chunk_text for sessions that already have events. Format
-- matches semantic.Writer.buildChunkText so the rerank input shape is
-- identical for backfilled sessions and freshly-closed ones:
--
--   user: hello
--   assistant: hi
--   user: ...
--
-- Idempotent via the WHERE l1_chunk_text IS NULL guard — re-applying
-- the migration is a no-op once the column is populated. Open sessions
-- get backfilled too (no l1_embedding gate): chunk text is independent
-- of the embedding and useful even for in-progress sessions.
UPDATE episodic.sessions s
   SET l1_chunk_text = sub.chunk
  FROM (
       SELECT session_id,
              string_agg(type || ': ' || text, E'\n' ORDER BY created_at, id) AS chunk
         FROM episodic.events
        WHERE text IS NOT NULL AND text <> ''
        GROUP BY session_id
       ) sub
 WHERE s.id = sub.session_id
   AND s.l1_chunk_text IS NULL;
