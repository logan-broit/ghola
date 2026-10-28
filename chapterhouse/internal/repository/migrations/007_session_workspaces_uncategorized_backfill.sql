-- Backfill: every existing session that lacks any session_workspaces
-- row gets one for the deterministic "uncategorized" workspace.
--
-- The UUID below is uuid5(NS_workspace, "uncategorized") where
-- NS_workspace = '8e3a4c2d-1b5f-4d7a-9c8e-0f1a2b3c4d5e' (defined in
-- internal/core/workspace.go on the ghola side). Computed once and
-- inlined here — recomputation must produce the same value, so this
-- migration stays idempotent on re-run.
--
-- The NOT EXISTS guard skips sessions that already have at least one
-- workspace assignment. The bench's 23,854 backfilled sessions are
-- skipped by this guard; only sessions ingested before the
-- production-ingest path was wired pick up the "uncategorized" row.

INSERT INTO episodic.session_workspaces (session_id, workspace_id)
SELECT s.id, 'bf99072d-0db6-52f5-9b1c-2b76dcbd41f1'::uuid
FROM episodic.sessions s
WHERE NOT EXISTS (
    SELECT 1
    FROM episodic.session_workspaces sw
    WHERE sw.session_id = s.id
);
