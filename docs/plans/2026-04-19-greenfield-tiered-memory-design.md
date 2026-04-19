# Ghola — Greenfield Tiered Memory Design

A shared brain for teams of agents, built on neuroscience-grounded memory
tiers rather than a single monolithic store.

## Purpose

Reset the memory system from first principles. Iters 0-16 on pg_ghola
converged on a single-substrate, single-schema design that could not
simultaneously satisfy (a) shared team knowledge, (b) fast local writes, and
(c) the cognitive primitives that pg_ghola's whole value proposition rests
on. This document specifies a tiered architecture that decomposes memory by
role (working / episodic / semantic) rather than by storage, borrows top-of-
leaderboard retrieval patterns for each tier, and integrates the cognitive
primitives as the layer that moves information between tiers as it matures.

v1a ships the minimum viable cut: episodic + semantic + the consolidation
pipelines between them, plus a working-memory store that makes the whole
architecture feel coherent from the agent's perspective. Procedural,
prospective, spatial, flashbulb, and priming memory types are explicitly
deferred to later iterations.

## The naming (Dune cast)

Each role in the system gets a name that describes what it does, not what
it is made of, so the specific substrate can change without the name going
stale:

- **Sietch** — the per-session ephemeral store (embedded SQLite, one file
  per session). Named for the Fremen hidden refuges: private, yours alone,
  temporary by design.
- **Chapterhouse** — the shared team archive (Go service over Postgres).
  The library that preserves what becomes common knowledge.
- **Melange** — the embedding service. "The substance that makes text
  navigable." Swappable across models (Qwen3, BGE, Nomic, any
  sentence-transformer).
- **Mentat** — the LLM called only during Pipeline B distillation. The
  pattern-finder. Swappable across models (Gemma, Qwen, Llama, Haiku).

See the accompanying [`GholaArchitecture.tsx`](assets/GholaArchitecture.tsx)
artifact for an interactive visualization of the topology and end-to-end
sequence.

---

## Design decisions (the five that matter)

### 1. Three tiers, two substrates, one canonical interface

Three storage tiers with different roles:

- **Working** — ephemeral per-session tree of turns. Lives on the user's
  device as SQLite. Owned by the Ghola local service.
- **Episodic** — raw preservation of all sessions across time, per-user
  partitioned, centralized on the team's Postgres. Verbatim storage
  (MemPalace-style — the top of the leaderboard without LLM).
- **Semantic** — distilled facts, shared across team, centralized on the
  same Postgres in a separate schema. Cognitive primitives (ACT-R, Hebbian,
  Bayesian confidence, contradiction detection) apply here via the
  pg_ghola v2 Rust extension.

Two substrates total: SQLite (working) and Postgres (episodic + semantic).

One canonical set of operations, exposed as both MCP (Claude Code path) and
HTTP/JSON (pi-mono path) from the same Go service. Same operations, two
wire protocols.

### 2. Local-first for working, centralized for episodic + semantic

Working is truly local — per-session SQLite file, fast writes, no network
dependency on the hot path.

Episodic centralizes because the semantic-consolidation worker (Pipeline B)
needs analytical scans across many users' episodic history. Distributed
episodic would either force replication back to a center anyway, or make
semantic consolidation infeasible.

Privacy for episodic is ACL-based (per-user partitioning + explicit
`episodic.shares` entries), not physical isolation. Chapterhouse holds the
Postgres credentials; user devices have only per-user API keys. No
Postgres credentials distributed to laptops.

### 3. Two consolidation pipelines with different characters

- **Pipeline A (Working → Episodic)**: continuous, per-user, **no LLM
  required**. Runs inside the Go local service. Copies raw turn nodes into
  episodic with lightweight entity extraction and embedding. Incremental
  by watermark. Lossless.
- **Pipeline B (Episodic → Semantic)**: nightly, cross-user,
  **LLM-assisted** (local LLM only — Gemma on the team's vLLM, or
  equivalent). Runs inside Chapterhouse. Scans last 24h of episodic,
  groups by entity co-occurrences across sessions, distills patterns into
  semantic mnemes. Lossy by design.

Pipeline A is the hot path from the user's perspective; Pipeline B is
settled team knowledge accumulating overnight.

### 4. Consolidation triggers are hybrid

Working → episodic fires on a **Hybrid F** of triggers:
- Continuous background (every ~10 turns or ~5 min)
- Size-threshold forced flush (>50MB or >5000 turns)
- Explicit agent-trigger (e.g. `consolidate` tool, or session_end)

Each session's working tree yields consolidatable units via **Hybrid C+D**:
- Continuous incremental deltas (D) — the background tick writes small
  episodic rows as turns accrue.
- Terminal-branch coherence pass (C) — when a branch goes terminal (no
  activity for >N minutes or on session_end), rewrite the fine-grained
  deltas of that branch into a cohesive episodic memory with metadata.
- Optional explicit bookmark-bounded slice (B subset) — the agent marks a
  branch as a self-contained unit of work via `bookmark`, triggering a
  coherence pass immediately.

### 5. Episodic stores RAW

Episodic is not summarized. Raw turn content is preserved verbatim, indexed
by vector + FTS + entities. This matches MemPalace's 96.6% LongMemEval
retrieval pattern and separates concerns cleanly:

- Working → episodic: cheap, lossless, no LLM
- Episodic → semantic: expensive, lossy, LLM-assisted

Semantic is the tier where distillation happens and where the cognitive
primitives operate on stable facts.

---

## Topology

```
╔════════════════════════════ USER DEVICE ═══════════════════════════╗
║                                                                    ║
║  ┌─────────────────────────────────────────────────────────────┐   ║
║  │ Agent process (Claude Code / pi-mono / etc.)                │   ║
║  └─────────┬─────────────────────────────────────┬─────────────┘   ║
║            │ MCP (stdio or HTTP/localhost)       │ HTTP/JSON       ║
║            ▼                                     ▼                 ║
║  ┌─────────────────────────────────────────────────────────────┐   ║
║  │ Ghola local service (Go, localhost:7421)                    │   ║
║  │   • Core library (single source of truth)                   │   ║
║  │   • HTTP/JSON server + MCP wrapper (protocol-only)          │   ║
║  │   • Pipeline A worker (continuous, lossless)                │   ║
║  │   • Sietch — SQLite per-session working DB                  │   ║
║  └──────────────────────────┬──────────────────────────────────┘   ║
║                             │ HTTPS + per-user API key             ║
╚═════════════════════════════│══════════════════════════════════════╝
            (dev: localhost:8080  ·  prod: chapterhouse FQDN)
                              ▼
╔══════════════════════ TEAM INFRA (NUC in prod) ════════════════════╗
║                                                                    ║
║  ┌──────────────────────────────────────────────────────────────┐  ║
║  │ Chapterhouse (Go, REST API, per-user auth)                   │  ║
║  │   • /v1/episodic/{ingest, query, share, forget}              │  ║
║  │   • /v1/semantic/{query, feedback, list}                     │  ║
║  │   • Pipeline B worker (nightly, LLM-assisted distillation)   │  ║
║  └──────────────────────────┬───────────────────────────────────┘  ║
║                             │ pgx                                  ║
║                             ▼                                      ║
║  ┌──────────────────────────────────────────────────────────────┐  ║
║  │ Postgres (CNPG)                                              │  ║
║  │   • SCHEMA episodic — raw, per-user partition                │  ║
║  │   • SCHEMA semantic — distilled, shared, pg_ghola v2 ext     │  ║
║  │   • pg_ghola v2 workers (Rust, in-process):                  │  ║
║  │       contradiction · Hebbian · consolidation                │  ║
║  └──────────────────────────────────────────────────────────────┘  ║
║                                                                    ║
║  ┌──────────────────────────────────────────────────────────────┐  ║
║  │ Melange — embedding service (D-dim, hot path, D at deploy)   │  ║
║  └──────────────────────────────────────────────────────────────┘  ║
║                                                                    ║
║  ┌──────────────────────────────────────────────────────────────┐  ║
║  │ Mentat — LLM inference (Pipeline B only, cold path)          │  ║
║  └──────────────────────────────────────────────────────────────┘  ║
╚════════════════════════════════════════════════════════════════════╝
```

**Dev vs prod differs by one env var.** In development, everything collapses
onto the developer's laptop via `docker-compose` — Postgres, Chapterhouse,
embedding service, LLM, and Ghola local service all on `localhost`. In
production, `CHAPTERHOUSE_URL` points to the team FQDN and the infra tier
lives on the NUC. Same binaries, same code paths, one config toggle.

---

## End-to-end event sequence

A single user story walking the system from session start through a
coworker recalling a fact that promoted from the first user's session.

| # | T | Tier | Event |
|---|---|---|---|
| 1 | T=0 | Device | User opens agent → `session_start` provisions a new SQLite sietch. |
| 2 | +10s | Device | User's first turn → `record` embeds via Melange, inserts into sietch. |
| 3 | +15s | Infra | Agent calls `recall` → local service fans out to working + episodic + semantic, merges by score, returns tier-attributed results. |
| 4 | +18s | Device | Agent responds → `record` again, linked by parent_id to the user turn. |
| 5 | +5 min | Worker | Pipeline A wakes, reads turns past watermark, POSTs to `/v1/episodic/ingest`. Watermark advances on ACK. |
| 6 | ~35 min | Device | User closes agent → `session_end` runs final Pipeline A flush + per-branch coherence pass; sietch retained 24h then GC'd. |
| 7 | +8h (02:00) | Worker | Pipeline B runs on Chapterhouse: scans last 24h of episodic cross-user (ACL-respecting), finds recurring entity co-occurrences, calls Mentat to distill each into `{concept, content, memory_type, entities}`, dedups against semantic via HNSW cosine > 0.9 (strengthens or inserts), enqueues contradiction + co-activation jobs. |
| 8 | +8h + ε | Worker | pg_ghola v2 Rust workers drain queues: contradiction flagging, Hebbian weight updates, hourly decay + 6-hourly archival. |
| 9 | next day | Infra | Coworker's agent calls `recall`. Semantic tier returns the mneme promoted in step 7, confidence elevated by Bayesian update, `contributor_user_ids` preserves attribution. Shared brain working. |

---

## Data models

### Sietch (working, SQLite, per-session, ephemeral)

```sql
CREATE TABLE session (
    id                          TEXT PRIMARY KEY,
    user_id                     TEXT NOT NULL,
    started_at                  INTEGER NOT NULL,
    last_turn_at                INTEGER,
    consolidated_to_episodic_at INTEGER  -- NULL until promoted
);

CREATE TABLE turns (
    id              INTEGER PRIMARY KEY,
    parent_id       INTEGER REFERENCES turns(id),  -- tree structure
    role            TEXT CHECK (role IN ('user','assistant','system','tool')),
    content         TEXT NOT NULL,
    tool_name       TEXT,
    tool_input      TEXT,   -- JSON
    tool_output     TEXT,   -- JSON
    bookmark_label  TEXT,
    created_at      INTEGER NOT NULL
);
CREATE INDEX turns_parent    ON turns(parent_id);
CREATE INDEX turns_bookmarks ON turns(bookmark_label)
    WHERE bookmark_label IS NOT NULL;

CREATE VIRTUAL TABLE turn_embeddings USING vec0(
    turn_id   INTEGER PRIMARY KEY,
    embedding FLOAT[${EMBEDDING_DIM}]   -- substituted at schema init
);

CREATE VIRTUAL TABLE turns_fts USING fts5(
    content, content='turns', content_rowid='id'
);
```

One file per session. Tree-structured via `parent_id`. Embedded vector
search (sqlite-vec) and FTS5. Writes are SQLite-fast; never blocks on
network.

### Episodic (Postgres, per-user partitioned, RAW)

```sql
CREATE SCHEMA episodic;

CREATE TABLE episodic.sessions (
    id              uuid PRIMARY KEY,
    user_id         uuid NOT NULL,
    started_at      timestamptz NOT NULL,
    ended_at        timestamptz,
    turn_count      integer NOT NULL DEFAULT 0,
    summary         text,                              -- optional cached summary
    source_device   text,
    promoted_to_semantic_count integer DEFAULT 0
);
CREATE INDEX episodic_sessions_user ON episodic.sessions(user_id, started_at DESC);

CREATE TABLE episodic.turns (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         uuid NOT NULL,
    session_id      uuid NOT NULL,
    parent_id       uuid REFERENCES episodic.turns(id) ON DELETE CASCADE,
    role            text NOT NULL
        CHECK (role IN ('user','assistant','system','tool')),
    content         text NOT NULL,
    tool_name       text,
    tool_input      jsonb,
    tool_output     jsonb,
    bookmark_label  text,
    embedding       vector(${EMBEDDING_DIM}),  -- dim set at deploy
    search_vector   tsvector GENERATED ALWAYS AS (to_tsvector('english', content)) STORED,
    entities        text[],
    tags            text[],
    created_at      timestamptz NOT NULL,
    ingested_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX episodic_turns_user       ON episodic.turns(user_id);
CREATE INDEX episodic_turns_session    ON episodic.turns(user_id, session_id);
CREATE INDEX episodic_turns_parent     ON episodic.turns(parent_id);
CREATE INDEX episodic_turns_embedding  ON episodic.turns USING hnsw (embedding vector_cosine_ops);
CREATE INDEX episodic_turns_fts        ON episodic.turns USING gin (search_vector);
CREATE INDEX episodic_turns_entities   ON episodic.turns USING gin (entities);
CREATE INDEX episodic_turns_created    ON episodic.turns(user_id, created_at DESC);
CREATE INDEX episodic_turns_bookmark   ON episodic.turns(bookmark_label)
    WHERE bookmark_label IS NOT NULL;

CREATE TABLE episodic.shares (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id   uuid NOT NULL,
    target          text NOT NULL CHECK (target IN ('team', 'user')),
    target_id       uuid,                              -- NULL if target='team'
    scope_type      text NOT NULL CHECK (scope_type IN ('session','branch','turn')),
    scope_id        uuid NOT NULL,
    created_at      timestamptz NOT NULL DEFAULT now()
);
```

Mirrors the sietch's structure (verbatim preservation) at cluster scale.
Per-user `user_id` column is the partition key; RLS or app-layer filters
enforce privacy. Sharing is a separate ACL table — mark a session/branch/
turn as shareable, and other users' `recall` queries include it.

### Semantic (Postgres, shared, DISTILLED — pg_ghola v2)

Simplified from the pre-v2 schema. No more `sub_mnemes` (that was
episodic's job). No cluster pathway (iter 14 showed it was harmful). No
dedicated gating columns per mneme. Five tables total.

```sql
CREATE SCHEMA semantic;

CREATE TABLE semantic.mnemes (
    id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id          uuid NOT NULL,
    concept               text NOT NULL,
    content               text NOT NULL,
    embedding             vector(${EMBEDDING_DIM}) NOT NULL,  -- dim at deploy
    search_vector         tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('english', concept), 'A') ||
        setweight(to_tsvector('english', content), 'B')
    ) STORED,
    confidence            double precision NOT NULL DEFAULT 0.5,
    access_count          integer NOT NULL DEFAULT 0,
    last_access           timestamptz NOT NULL DEFAULT now(),
    created_at            timestamptz NOT NULL DEFAULT now(),
    state                 text NOT NULL DEFAULT 'active'
        CHECK (state IN ('active', 'archived')),
    memory_type           text NOT NULL DEFAULT 'factual'
        CHECK (memory_type IN ('factual', 'experiential', 'procedural')),
    tags                  text[] NOT NULL DEFAULT '{}',
    entities              text[] NOT NULL DEFAULT '{}',
    source_episodic_ids   uuid[] NOT NULL DEFAULT '{}',
    contributor_user_ids  uuid[] NOT NULL DEFAULT '{}'
);

CREATE TABLE semantic.associations (
    src_id            uuid NOT NULL REFERENCES semantic.mnemes(id) ON DELETE CASCADE,
    dst_id            uuid NOT NULL REFERENCES semantic.mnemes(id) ON DELETE CASCADE,
    association_type  text NOT NULL
        CHECK (association_type IN ('hebbian','contradicts','supersedes','supports')),
    weight            double precision NOT NULL DEFAULT 0.01,
    co_activations    integer NOT NULL DEFAULT 0,
    updated_at        timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (src_id, dst_id, association_type)
);

CREATE TABLE semantic.co_activation_queue ( /* drained by Hebbian worker */ );
CREATE TABLE semantic.contradiction_queue ( /* drained by contradiction worker */ );
CREATE TABLE semantic.contradiction_candidates ( /* flagged pairs for review */ );
```

Cognitive primitives (pg_ghola v2 Rust extension): ACT-R activation,
Bayesian confidence update, Hebbian weight multiplicative update, softplus
compositing, hourly decay, 6-hourly archival, contradiction scanning via
HNSW similarity.

---

## Agent interface — canonical operations

One set of operations. Two protocol wrappings (MCP, HTTP/JSON). Same
semantics.

| Operation | Purpose |
|---|---|
| `record` | Append a turn to the current session's working tree |
| `branch` | Create a new branch from an existing node |
| `bookmark` | Label a node (explicit "remember this point") |
| `navigate` | Move "current" pointer to a different node |
| `recall` | Query; returns ranked merged results across working + episodic + semantic with tier attribution |
| `forget` | Mark a memory for deletion (any tier) |
| `share` | Grant visibility of a session/branch/turn to team or specific users |
| `consolidate` | Force-run Pipeline A for the current session |
| `session_start` / `session_end` | Lifecycle |
| `list_sessions` | Enumerate user's episodic sessions |
| `feedback` | Adjust confidence on a semantic mneme |

Pi-mono ships a TypeScript extension that calls the HTTP/JSON surface.
Claude Code uses the MCP surface directly.

Chapterhouse's internal API (not agent-facing, called only by the local
service on the agent's behalf):

| Endpoint | Purpose |
|---|---|
| `POST /v1/episodic/ingest` | Batch-insert consolidated turns |
| `POST /v1/episodic/query` | Vector + FTS + filter search over episodic |
| `POST /v1/episodic/share` | Create ACL entry in `episodic.shares` |
| `POST /v1/episodic/forget` | Mark episodic rows for deletion |
| `POST /v1/semantic/query` | Query semantic with cognitive scoring |
| `POST /v1/semantic/feedback` | Apply Bayesian confidence update |
| `POST /v1/semantic/list` | List a user's promoted facts |

---

## What survives the rebuild, what doesn't

### Keep (unchanged)

- CNPG Postgres cluster, deployment infrastructure, ArgoCD pipeline
- Melange (embedding service on :8082 with Qwen3-Embedding, or any
  sentence-transformer replacement)
- Mentat (vLLM + Gemma on NUC, or any replacement)
- Docker image conventions, `k3s ctr images import`, homelab-k3s git repo

### Keep, scope down

- **Chapterhouse ch-server (Go)** — code survives, MCP tool surface is
  replaced with the internal HTTP API above. Auth, workspace scoping,
  deploy all keep working.
- **pg_ghola Rust extension** — simplified to v2: 5 tables instead of 12,
  3 workers, single-pathway recall. Cognitive scoring preserved.

### Replace

- Agent-facing memory API (the old chapterhouse MCP tools `remember`,
  `recall`, `forget`, etc.) — removed. Agents talk to the local service now.
- Sub_mnemes table and all related infrastructure added in iter 15-16 —
  removed. That concept lives in episodic now as per-turn rows.
- `matched_position` field on `recall_result` — removed (no sub_mnemes to
  attribute to).

### New

- **Ghola local service (Go)** — greenfield. Core library + HTTP/JSON +
  MCP + Pipeline A + sietch management. Runs on user devices.
- **Sietch** (SQLite schema with sqlite-vec + FTS5)
- **Episodic** (Postgres schema) — raw, per-user
- **Pipeline A worker** — inside Ghola local service
- **Pipeline B worker** — inside Chapterhouse
- **Pi-mono extension** (TypeScript) — HTTP client against the local service
- **docker-compose stack** — for local dev everything-on-one-laptop

### Drop the existing database

No migration. The current production Postgres with 42 mnemes + historical
iter artifacts gets dropped. The v1a deploy is a fresh `CREATE EXTENSION
pg_ghola` on an empty database. The lost 42 mnemes are acceptable; they're
test/scratch content, not durable user data.

---

## v1a success criteria

1. A pi-mono agent and a Claude Code agent both call `record` / `recall`
   against the same local service and get consistent results.
2. Pipeline A runs continuously without errors over a 1-hour session;
   episodic grows as expected; watermark advances idempotently.
3. Pipeline B runs on a cron; semantic mnemes get created from recurring
   episodic patterns; cognitive primitives fire correctly.
4. `share` exposes a session/branch to team; another user's `recall` picks
   it up from episodic.
5. Fresh `CREATE EXTENSION pg_ghola` installs cleanly on an empty DB with
   the simplified v2 schema.
6. Local service runs on workstation + laptop without Postgres credentials
   on either device — only a per-user chapterhouse API key.
7. `docker-compose up` in the ghola repo produces a working end-to-end
   stack on a developer laptop in under 30 seconds, with no external
   dependencies beyond the embedding model download.

Explicit non-goals for v1a: beating any retrieval benchmark, matching or
exceeding iter 9's 27.5% R@5 on LongMemEval. Those are future-iter scope.

---

## Explicit deferrals (not v1a)

- Procedural, prospective, spatial, flashbulb memory tiers
- Priming (recency-weighted semantic promotion into working context)
- Cross-device working-DB sync (my-laptop ↔ my-desktop)
- LongMemEval benchmarking of the new architecture
- Encoding-strategy iteration (late chunking variants, multi-scale, etc.)
- Multi-tenant chapterhouse (multiple independent teams on one instance)
- Federated cross-team semantic sharing
- An LLM-free entity-extraction upgrade (regex/lightweight NER is enough
  for v1a)

---

## Rough sequencing

(This becomes the writing-plans input.)

1. Fresh pg_ghola v2 extension (Rust, simplified 5-table schema)
2. Episodic schema (Postgres, new `episodic.*`)
3. New chapterhouse tool surface (Go — `/v1/episodic/*`, `/v1/semantic/*`)
4. Ghola local service skeleton (Go — HTTP/JSON + SQLite + sqlite-vec + FTS5)
5. Pipeline A worker (Go, inside local service)
6. MCP wrapper (Go, inside local service)
7. Pi-mono extension (TypeScript, against HTTP/JSON)
8. Pipeline B worker (Go, inside chapterhouse — LLM-assisted distillation)
9. docker-compose for local dev
10. Production deploy: new Docker images, ArgoCD manifest updates,
    drop-and-recreate prod DB, fresh `CREATE EXTENSION` install
11. Integration tests: cross-agent smoke tests, pipeline end-to-end,
    success-criteria checks

Repo layout decisions (keep existing repos vs new monorepo vs split)
are for the writing-plans step to resolve concretely.

---

## References

- Interactive architecture visualization: [`assets/GholaArchitecture.tsx`](assets/GholaArchitecture.tsx)
- Predecessor design that this supersedes: [`2026-04-16-multi-granularity-encoding-design.md`](2026-04-16-multi-granularity-encoding-design.md)
- Iter 16 result documenting why the prior direction was rejected: [`../../.samsara/iterations/016.md`](../../.samsara/iterations/016.md)
- Longitudinal eval design (applies once v1a is running): [`2026-04-18-longitudinal-eval-design.md`](2026-04-18-longitudinal-eval-design.md)
- Leaderboard context (LongMemEval, MemPalace, Mastra, Hindsight, Stella,
  Contriever, BM25) used as the reasoning floor for substrate choices:
  documented in-conversation on 2026-04-19.
- Neuroscience grounding: Complementary Learning Systems (McClelland,
  McNaughton, O'Reilly, 1995); ACT-R activation (Anderson 1993);
  Ebbinghaus decay (1885); Hebbian learning (Hebb 1949); hippocampal
  indexing theory (Teyler & DiScenna, 1986); contradiction detection
  (Kumaran & Maguire 2007); sleep consolidation (Tononi & Cirelli 2006,
  Diekelmann & Born 2010); tree-structured session memory pattern
  (pi-mono, badlogic/pi-mono).
