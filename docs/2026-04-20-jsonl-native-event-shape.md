# JSONL-Native Event Shape

**Status:** Ground-0 spec for the working + episodic schemas. Not a frozen contract — we will adapt if it fights us in practice.

**Supersedes:** the "Sietch" and "Episodic" table definitions in
[`2026-04-19-greenfield-tiered-memory-design.md`](2026-04-19-greenfield-tiered-memory-design.md) (§ Data models). Everything else in the greenfield doc (tiers, pipelines, sequence, operations) still holds.

---

## What changed

| Concept | Before | Now |
|---|---|---|
| Row unit | one per "turn" (agent message) | one per **JSONL line** (content block) |
| Table name | `turns` | **`events`** |
| Raw data | not stored — only projected fields | `raw_event` (full JSONL line) is source of truth |
| `tool_input` / `tool_output` | opaque TEXT / jsonb | **indexable** (SQLite JSON1, Postgres jsonb) |
| Agent context | dropped | `cwd`, `git_branch`, `agent_id`, `is_sidechain`, `model`, `version` carried per event |
| Turn concept | = row | = grouping of events with same `request_id` (query-time) |

## Why

Agents (Claude Code, pi-mono, future) emit **content blocks**, not flat turns. One assistant response can be: text → tool_use A → tool_use B → tool_result A → tool_result B, each as a separate JSONL entry linked by `parentUuid`. The v0 schema collapsed this into a single `(content, tool_name, tool_input, tool_output)` row that can't represent multi-block responses without data loss.

Real-world agent workflows query working memory all the time:
- "find all `Bash` calls that ran `git log`"
- "find `Read` calls that touched `src/schema.rs`"
- "find the last `tool_result` with an error"
- "find my last bookmark and its immediate parent"

The v0 schema indexed none of those dimensions. This revision indexes the ones that matter and keeps the raw JSONL so nothing is lost.

## Evidence

A Claude Code session line (condensed):

```json
{
  "parentUuid": "11452ebb-...",
  "isSidechain": true,
  "userType": "external",
  "cwd": "/home/loganb/ai",
  "sessionId": "a5c63339-...",
  "version": "2.1.56",
  "gitBranch": "HEAD",
  "agentId": "a58b9e89a1f2e7d8c",
  "type": "assistant",
  "message": {
    "role": "assistant",
    "model": "claude-haiku-4-5-20251001",
    "id": "msg_01USy...",
    "content": [
      {"type": "tool_use", "id": "toolu_01Jx...", "name": "Bash",
       "input": {"command": "find /home/loganb/ai/muninndb -type f -name '*.go' | head -30"}}
    ],
    "usage": {"input_tokens": 3, "cache_creation_input_tokens": 14802, ...}
  },
  "requestId": "req_011CYhQgEJwsQmju42RKPEv2",
  "uuid": "3a25ad2d-...",
  "timestamp": "2026-03-04T04:28:28.137Z"
}
```

The previous line in the same session was the `"type": "text"` block of the same `requestId` — two rows for one assistant response.

## Common shape

Both tiers carry the same 17 projected columns + raw_event. Episodic adds 5 more for partitioning / ingestion metadata.

### Core columns (present in both tiers)

| Column | SQLite type | Postgres type | Source in JSONL | Notes |
|---|---|---|---|---|
| `id` | `TEXT PRIMARY KEY` | `uuid PRIMARY KEY` | `.uuid` | global unique |
| `parent_id` | `TEXT` | `uuid` | `.parentUuid` | tree structure |
| `session_id` | `TEXT` | `uuid` | `.sessionId` | partition key |
| `request_id` | `TEXT` | `text` | `.requestId` | groups content blocks of one API response |
| `type` | `TEXT` | `text` | `.type` | `user` / `assistant` / `tool_result` / `system` |
| `role` | `TEXT` | `text` | `.message.role` | usually mirrors `type` but keeps API parity |
| `text` | `TEXT` | `text` | projected (see below) | the text view; FTS + embedding target |
| `tool_name` | `TEXT` | `text` | `.message.content[].name` | NULL unless `content[i].type = 'tool_use'` |
| `tool_use_id` | `TEXT` | `text` | `.message.content[].id` | pairs tool_use ↔ tool_result |
| `tool_input` | `TEXT` (JSON1) | `jsonb` | `.message.content[].input` | queryable |
| `tool_output` | `TEXT` (JSON1) | `jsonb` | `tool_result` content | queryable |
| `bookmark_label` | `TEXT` | `text` | agent-set | partial index |
| `cwd` | `TEXT` | `text` | `.cwd` | forensic |
| `git_branch` | `TEXT` | `text` | `.gitBranch` | forensic |
| `agent_id` | `TEXT` | `text` | `.agentId` | subagent / sidechain tracking |
| `is_sidechain` | `INTEGER NOT NULL DEFAULT 0` | `boolean NOT NULL DEFAULT false` | `.isSidechain` | |
| `model` | `TEXT` | `text` | `.message.model` | which Claude variant |
| `raw_event` | `TEXT NOT NULL` | `jsonb NOT NULL` | the full JSONL line | source of truth |
| `created_at` | `INTEGER NOT NULL` (unix ms) | `timestamptz NOT NULL` | `.timestamp` parsed | |

### `text` projection rule

One deterministic rule so downstream FTS/embedding is predictable:

- `type = 'user'` with string content → `content` as-is.
- `type = 'assistant'` + a `content[]` block of type `text` → that block's `.text`.
- `type = 'assistant'` + a `content[]` block of type `tool_use` → `"<tool_name>(<stringified first-arg or full input JSON>)"`.
- `type = 'tool_result'` → first 2 KiB of the result body (truncated with `…` when longer).
- `type = 'system'` → the system text.

The goal: a short, human-readable string suitable for vector encoding and FTS. The `raw_event` blob retains the full fidelity for anything the projection drops.

### Indexes

Same set on both tiers (syntax differs per engine):

- `parent_id`
- `session_id`
- `tool_name WHERE tool_name IS NOT NULL`
- `tool_use_id WHERE tool_use_id IS NOT NULL`
- `bookmark_label WHERE bookmark_label IS NOT NULL`
- `is_sidechain`
- `created_at DESC`
- Vector: sqlite-vec `vec0` virtual table on `text`-embedding (Sietch) / HNSW on `embedding vector(N)` (Episodic)
- FTS: sqlite FTS5 virtual table content-linked to `text` (Sietch) / `tsvector` + GIN on `text` (Episodic)

### Episodic-only columns

| Column | Postgres type | Purpose |
|---|---|---|
| `user_id` | `uuid NOT NULL` | ACL + partition key |
| `entities` | `text[] NOT NULL DEFAULT '{}'` | Pipeline A extraction; GIN-indexed |
| `tags` | `text[] NOT NULL DEFAULT '{}'` | agent-set or derived; GIN-indexed |
| `ingested_at` | `timestamptz NOT NULL DEFAULT now()` | watermark for Pipeline A idempotency |
| `source_device` | `text` | forensic: which laptop produced it |

### Sietch-only structural notes

- Each session is a separate `.sqlite` file under `~/.ghola/sessions/<session_id>.sqlite`. `session_id` is therefore 1:1 with file; queries that span sessions happen via episodic, not sietch.
- `events_fts` is content-linked via `content='events' content_rowid='rowid'` so triggers keep FTS in sync automatically.
- `event_embeddings` is a separate `vec0` virtual table keyed by `id` — not a column on `events`, because sqlite-vec requires its own virtual table.

## Sessions table (both tiers)

Both tiers still track per-session metadata, unchanged in intent but lightly renamed for consistency:

```sql
-- Sietch (SQLite)
CREATE TABLE session (
    id                           TEXT PRIMARY KEY,
    user_id                      TEXT NOT NULL,
    started_at                   INTEGER NOT NULL,
    last_event_at                INTEGER,
    consolidated_to_episodic_at  INTEGER,
    cwd                          TEXT,
    git_branch                   TEXT,
    agent_kind                   TEXT,           -- 'claude-code', 'pi-mono', etc.
    source_device                TEXT
);
```

```sql
-- Episodic (Postgres)
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
```

## Pipeline A mapping

Sietch row → episodic row is nearly a 1:1 copy with translations:

| Sietch (SQLite) | → | Episodic (Postgres) | Translation |
|---|---|---|---|
| `id TEXT` | → | `id uuid` | string → uuid cast |
| `created_at INTEGER` (ms) | → | `created_at timestamptz` | `to_timestamp(ms/1000)` |
| `raw_event TEXT` | → | `raw_event jsonb` | `raw_event::jsonb` |
| `tool_input / tool_output TEXT` | → | `jsonb` | cast |
| — | → | `user_id`, `ingested_at` | from session + now() |
| — | → | `entities`, `tags` | Pipeline A extraction (currently regex; LLM-assisted later) |

Pipeline A never transforms `raw_event`. If the agent recorded it, we preserve it byte-for-byte (minus encoding normalization).

## Open questions (reserved for adaptation)

1. **`usage` metrics.** Where do input_tokens / cache_creation / output_tokens live? Current plan: inside `raw_event.message.usage` only — no projected column. Revisit if we need cost queries.
2. **Multi-modal content.** Today's projection rule handles text + tool_use + tool_result. Image blocks and file attachments will need rules added — TBD when we encounter them.
3. **Sub-agent session modeling.** Sidechain events have their own `agentId` and may appear in the parent session's stream. Right now: same `session_id`, `is_sidechain = true`, `agent_id` set. Might need a separate episodic projection per sub-agent if cross-user sharing of subagent outputs becomes desired.
4. **Claude Code session import.** Once the sietch schema lands, a one-shot importer for `~/.claude/projects/**/*.jsonl` would seed sietch + kick Pipeline A to promote historical sessions into episodic. Not v1a scope, but the shape is compatible.
5. **Bookmark semantics under JSONL.** Original design had `bookmark_label` on a turn; here it's on an event. An agent marking "this point" probably means "this request_id" — might be cleaner as a separate `bookmarks(session_id, request_id, label, created_at)` table. Deferred until we have an agent actually setting bookmarks.

## Rollout

- **Phase 2 (episodic Postgres):** implement with this shape from the start. No transitional v0 → v1 schema.
- **Phase 4 (ghola local service + sietch):** mirror this shape in SQLite. The core `internal/core/` Go types can be shared between the sietch store and the chapterhouse HTTP client because the projected columns are identical.
- **Phase 11 integration tests:** dimension-agnostic suite still runs against both tiers; just targets `events` now instead of `turns`.
