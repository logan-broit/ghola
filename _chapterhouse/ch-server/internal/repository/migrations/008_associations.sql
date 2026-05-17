-- Hebbian (and future contradicts/supersedes/supports) associations between
-- episodic events. Workspace-scoped. Cascade-deleted with their referenced
-- events.

CREATE TABLE semantic.associations (
    src_event_id     uuid NOT NULL REFERENCES episodic.events(id) ON DELETE CASCADE,
    dst_event_id     uuid NOT NULL REFERENCES episodic.events(id) ON DELETE CASCADE,
    association_type text NOT NULL
        CHECK (association_type IN ('hebbian','contradicts','supersedes','supports')),
    weight           double precision NOT NULL DEFAULT 0.01,
    co_activations   integer NOT NULL DEFAULT 0,
    workspace_id     uuid NOT NULL,
    updated_at       timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (src_event_id, dst_event_id, association_type)
);

CREATE INDEX associations_src
    ON semantic.associations (src_event_id, association_type);
CREATE INDEX associations_dst
    ON semantic.associations (dst_event_id, association_type);
CREATE INDEX associations_workspace
    ON semantic.associations (workspace_id);
