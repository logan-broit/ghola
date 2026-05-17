-- 009_co_activation_queue.sql
-- Work queue: ingest writes pairs that should be Hebbian-linked.
-- The consolidation worker (cmd/worker/) drains this queue periodically,
-- upserts the association weights, then deletes the rows.

CREATE TABLE semantic.co_activation_queue (
    id           bigserial PRIMARY KEY,
    src_event_id uuid NOT NULL,
    dst_event_id uuid NOT NULL,
    workspace_id uuid NOT NULL,
    enqueued_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX coactivation_queue_enqueued
    ON semantic.co_activation_queue (enqueued_at);
