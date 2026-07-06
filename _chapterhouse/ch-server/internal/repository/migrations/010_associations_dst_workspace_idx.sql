-- LookupAssociationsByDst is a hot-path read in the P4 recurrent-settle
-- pipeline: for each BFS hop it fetches all associations whose dst_event_id
-- is in the current frontier, filtered by association_type and workspace_id.
--
-- Migration 008 added associations_dst on (dst_event_id, association_type)
-- but without workspace_id, so every lookup requires a recheck filter on
-- workspace_id after the index scan.  This covering index matches the exact
-- WHERE clause of LookupAssociationsByDst:
--
--   WHERE dst_event_id = ANY(...)
--     AND association_type = $2
--     AND workspace_id = $3
--
-- The (dst_event_id, association_type, workspace_id) column order puts the
-- selective equality filters first, matching the src-side associations_src
-- index shape: (src_event_id, association_type).  workspace_id is added as
-- a third column rather than a separate index so the planner can use it as
-- a covering filter without a bitmap heap fetch when workspace isolation
-- is tight (common in multi-tenant workloads).

CREATE INDEX IF NOT EXISTS associations_dst_workspace
    ON semantic.associations (dst_event_id, association_type, workspace_id);
