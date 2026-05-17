-- pg_ghola v0.6: session_workspaces join table.
--
-- A session belongs to >=1 workspace. Workspaces are the scoping
-- primitive recall queries filter by — without one, we'd have to
-- search the user's entire history per query, which doesn't scale
-- and doesn't reflect how agents actually think about memory.
--
-- Cardinality is N:N because a single conversation can be relevant
-- to multiple projects/topics (a debugging session might span
-- ProjectA's bug and ProjectB's fix, and both projects want to
-- recall it). The join table keeps session content single-source-
-- of-truth in episodic.sessions while scoping is denormalized into
-- this table.
--
-- Why not just a column on sessions: 1:1 forces choosing one
-- workspace at session-close, which loses cross-project conversations.
-- The bench's overlapping haystacks make this concrete: ~25k
-- session-instances across 500 questions, with substantial overlap.
-- Duplicating session content per workspace would require
-- re-embedding everything (~hours); the join table is O(rows) where
-- rows = (session × workspace) pairs.

CREATE TABLE episodic.session_workspaces (
    session_id   uuid NOT NULL REFERENCES episodic.sessions(id) ON DELETE CASCADE,
    workspace_id uuid NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (session_id, workspace_id)
);

-- Lookups go workspace_id -> sessions, so this index is the hot path.
CREATE INDEX session_workspaces_by_workspace
    ON episodic.session_workspaces (workspace_id, session_id);
