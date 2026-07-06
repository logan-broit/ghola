-- 011_associations_workspace_pk.sql
--
-- Bug fix: workspace_id was missing from the associations primary key.
--
-- The original PK (src_event_id, dst_event_id, association_type) from
-- migration 008 did not include workspace_id. When two workspaces ingest
-- overlapping content that produces identical event IDs, the second
-- workspace's UpsertAssociation ON CONFLICT would match the first
-- workspace's row and increment its co_activations counter rather than
-- inserting a new workspace-scoped row. The second workspace therefore
-- never accumulated its own association graph — its edges were silently
-- captured by whichever workspace wrote first.
--
-- Observed impact: the P4 eval workspace was missing 66% of its edges
-- because a prior workspace had already claimed the row slots under the
-- old PK. See seeding-eval/data/P42-FORENSICS.md.
--
-- Fix: drop the old three-column PK and replace it with a four-column PK
-- that includes workspace_id. Existing rows are unique under the new key
-- because no two rows with identical (src, dst, type) exist for two
-- different workspace_ids in a correctly-operating system prior to this fix
-- (each workspace ingests its own content). The ALTER is therefore safe
-- with no pre-dedup step required.
--
-- Index implications: migration 008 created associations_src on
-- (src_event_id, association_type) and associations_dst on
-- (dst_event_id, association_type). These remain useful. The new PK index
-- leads with (src_event_id, dst_event_id, association_type, workspace_id),
-- so src-lookup queries that also filter on workspace_id are covered by the
-- PK index directly. The associations_src partial index still serves range
-- lookups that don't need workspace_id in the predicate, but
-- LookupAssociations — which always adds AND workspace_id = $3 — will
-- benefit from the PK leading src column without requiring a separate
-- covering index. The associations_dst_workspace index added in migration
-- 010 continues to cover LookupAssociationsByDst.

ALTER TABLE semantic.associations
    DROP CONSTRAINT associations_pkey,
    ADD PRIMARY KEY (src_event_id, dst_event_id, association_type, workspace_id);
