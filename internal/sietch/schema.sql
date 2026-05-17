-- Per-session sietch schema. One SQLite file per session lives at
-- <root>/<session_id>.sqlite. Schema mirrors the projected columns of
-- docs/2026-04-20-jsonl-native-event-shape.md with SQLite types.
--
-- Vector search for v1a uses in-Go cosine similarity over the
-- embedding BLOBs — no sqlite-vec dependency yet. The BLOB format is
-- the raw bytes of a []float32 written little-endian (4 bytes per dim).
-- Future: swap to sqlite-vec's vec0 virtual table for sub-linear
-- search once session event counts warrant it.

CREATE TABLE IF NOT EXISTS session (
    id                  TEXT PRIMARY KEY,
    user_id             TEXT NOT NULL,
    started_at          INTEGER NOT NULL,   -- unix ms
    ended_at            INTEGER,
    last_event_at       INTEGER,
    event_count         INTEGER NOT NULL DEFAULT 0,
    current_event_id    TEXT,
    watermark_event_id  TEXT,
    summary             TEXT,
    workspace_id        TEXT,
    cwd                 TEXT,
    git_branch          TEXT,
    agent_kind          TEXT,
    source_device       TEXT
);

CREATE TABLE IF NOT EXISTS events (
    id              TEXT PRIMARY KEY,
    parent_id       TEXT REFERENCES events(id),
    session_id      TEXT NOT NULL,
    user_id         TEXT NOT NULL,
    request_id      TEXT,
    type            TEXT NOT NULL CHECK (type IN ('user','assistant','tool_result','system')),
    role            TEXT,
    text            TEXT,
    tool_name       TEXT,
    tool_use_id     TEXT,
    tool_input      TEXT,          -- JSON
    tool_output     TEXT,          -- JSON
    bookmark_label  TEXT,
    cwd             TEXT,
    git_branch      TEXT,
    agent_id        TEXT,
    is_sidechain    INTEGER NOT NULL DEFAULT 0,
    model           TEXT,
    raw_event       TEXT NOT NULL,
    embedding       BLOB,          -- little-endian packed []float32
    entities        TEXT NOT NULL DEFAULT '[]',
    tags            TEXT NOT NULL DEFAULT '[]',
    source_device   TEXT,
    created_at      INTEGER NOT NULL,
    state           TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active','forgotten'))
);

CREATE INDEX IF NOT EXISTS events_parent    ON events(parent_id);
CREATE INDEX IF NOT EXISTS events_bookmark  ON events(bookmark_label) WHERE bookmark_label IS NOT NULL;
CREATE INDEX IF NOT EXISTS events_created   ON events(created_at);
CREATE INDEX IF NOT EXISTS events_tool_name ON events(tool_name) WHERE tool_name IS NOT NULL;
CREATE INDEX IF NOT EXISTS events_state     ON events(state);

-- FTS5 index over events.text, content-linked so triggers keep it in
-- sync automatically.
CREATE VIRTUAL TABLE IF NOT EXISTS events_fts USING fts5(
    text,
    content='events',
    content_rowid='rowid'
);

CREATE TRIGGER IF NOT EXISTS events_fts_insert AFTER INSERT ON events BEGIN
    INSERT INTO events_fts(rowid, text) VALUES (new.rowid, COALESCE(new.text, ''));
END;

CREATE TRIGGER IF NOT EXISTS events_fts_delete AFTER DELETE ON events BEGIN
    INSERT INTO events_fts(events_fts, rowid, text) VALUES ('delete', old.rowid, COALESCE(old.text, ''));
END;

CREATE TRIGGER IF NOT EXISTS events_fts_update AFTER UPDATE ON events BEGIN
    INSERT INTO events_fts(events_fts, rowid, text) VALUES ('delete', old.rowid, COALESCE(old.text, ''));
    INSERT INTO events_fts(rowid, text) VALUES (new.rowid, COALESCE(new.text, ''));
END;
