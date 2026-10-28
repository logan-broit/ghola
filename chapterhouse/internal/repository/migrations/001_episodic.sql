-- Episodic raw-preservation tier. Shape follows
-- docs/2026-04-20-jsonl-native-event-shape.md.
--
-- Three tables:
--   episodic.sessions  - per-session metadata
--   episodic.events    - one row per JSONL line (content block)
--   episodic.shares    - ACL for cross-user visibility
--
-- ${EMBEDDING_DIM} is substituted at migration-apply time by the
-- runner from the EMBEDDING_DIM environment variable. That's how the
-- substrate stays dimension-agnostic: same file works for any vector
-- size without editing.

-- ---------------------------------------------------------------
-- sessions
-- ---------------------------------------------------------------

CREATE TABLE episodic.sessions (
    id                          uuid PRIMARY KEY,
    user_id                     uuid NOT NULL,
    started_at                  timestamptz NOT NULL,
    ended_at                    timestamptz,
    event_count                 integer NOT NULL DEFAULT 0,
    summary                     text,
    cwd                         text,
    git_branch                  text,
    agent_kind                  text,
    source_device               text,
    promoted_to_semantic_count  integer NOT NULL DEFAULT 0
);

CREATE INDEX episodic_sessions_user       ON episodic.sessions (user_id, started_at DESC);
CREATE INDEX episodic_sessions_agent_kind ON episodic.sessions (agent_kind)
    WHERE agent_kind IS NOT NULL;

-- ---------------------------------------------------------------
-- events (one row per JSONL line)
-- ---------------------------------------------------------------

CREATE TABLE episodic.events (
    id              uuid PRIMARY KEY,
    parent_id       uuid REFERENCES episodic.events(id) ON DELETE CASCADE,
    session_id      uuid NOT NULL,
    user_id         uuid NOT NULL,
    request_id      text,
    type            text NOT NULL
        CHECK (type IN ('user','assistant','tool_result','system')),
    role            text,
    text            text,
    tool_name       text,
    tool_use_id     text,
    tool_input      jsonb,
    tool_output     jsonb,
    bookmark_label  text,
    cwd             text,
    git_branch      text,
    agent_id        text,
    is_sidechain    boolean NOT NULL DEFAULT false,
    model           text,
    raw_event       jsonb NOT NULL,
    embedding       vector(${EMBEDDING_DIM}),
    search_vector   tsvector GENERATED ALWAYS AS (to_tsvector('english', coalesce(text, ''))) STORED,
    entities        text[] NOT NULL DEFAULT '{}',
    tags            text[] NOT NULL DEFAULT '{}',
    source_device   text,
    created_at      timestamptz NOT NULL,
    ingested_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX episodic_events_parent         ON episodic.events (parent_id);
CREATE INDEX episodic_events_session        ON episodic.events (session_id, created_at);
CREATE INDEX episodic_events_user           ON episodic.events (user_id, created_at DESC);
CREATE INDEX episodic_events_request        ON episodic.events (session_id, request_id)
    WHERE request_id IS NOT NULL;
CREATE INDEX episodic_events_tool_name      ON episodic.events (tool_name)
    WHERE tool_name IS NOT NULL;
CREATE INDEX episodic_events_tool_use_id    ON episodic.events (tool_use_id)
    WHERE tool_use_id IS NOT NULL;
CREATE INDEX episodic_events_bookmark       ON episodic.events (bookmark_label)
    WHERE bookmark_label IS NOT NULL;
CREATE INDEX episodic_events_sidechain      ON episodic.events (is_sidechain, created_at DESC);
CREATE INDEX episodic_events_embedding_hnsw ON episodic.events
    USING hnsw (embedding vector_cosine_ops);
CREATE INDEX episodic_events_search_gin     ON episodic.events USING gin (search_vector);
CREATE INDEX episodic_events_entities_gin   ON episodic.events USING gin (entities);
CREATE INDEX episodic_events_tags_gin       ON episodic.events USING gin (tags);

-- ---------------------------------------------------------------
-- shares (ACL for cross-user visibility)
-- ---------------------------------------------------------------

CREATE TABLE episodic.shares (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id   uuid NOT NULL,
    target          text NOT NULL
        CHECK (target IN ('team','user')),
    target_id       uuid,
    scope_type      text NOT NULL
        CHECK (scope_type IN ('session','branch','event')),
    scope_id        uuid NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX episodic_shares_owner  ON episodic.shares (owner_user_id);
CREATE INDEX episodic_shares_target ON episodic.shares (target, target_id);
CREATE INDEX episodic_shares_scope  ON episodic.shares (scope_type, scope_id);
