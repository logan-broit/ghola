# pg_recall Integration Plan

## Context

Chapterhouse currently stores memories in two places: PostgreSQL `memory_blocks` table (metadata + content) and Qdrant vector DB (embeddings + duplicated metadata). Every remember/forget/share operation must sync both. pg_recall is a purpose-built PostgreSQL extension (already deployed to the CNPG cluster) that unifies everything — content, embeddings, scoring, associations, lifecycle — in a single `mnemes` table. This integration eliminates Qdrant and `memory_blocks`, consolidating onto pg_recall as the single source of truth.

## Key Design Decisions

### 1. workspace_id Mapping
pg_recall uses `workspace_id` to scope data. Chapterhouse has `user_id` + `org_id`.

**Approach:** Use `user_id` as `workspace_id` for personal memories, `org_id` for org-scoped memories. On recall, make two `pg_recall.recall()` calls (one for personal workspace, one for org workspace with scope='org' filter) and merge results by score. On `share_memory` scope change, UPDATE the `workspace_id` column (personal→org moves from user workspace to org workspace).

### 2. IDs: Integer → UUID
`memory_blocks.id` is BIGSERIAL; `mnemes.id` is UUID. MCP tool responses currently show `[42]`, will show `[a1b2c3d4-...]`. Tool input schemas change `fact_id` from `integer` to `string`. This is a breaking change but IDs are opaque to clients (used only as arguments to forget/share_memory).

### 3. Versioning → Supersession
`memory_blocks` uses version numbers + `is_current` view. pg_recall uses `mark_supersedes()` which creates a directed association and archives the older mneme. On remember, if a mneme with the same concept exists in the workspace, the new one supersedes the old.

### 4. Search Consolidation
Currently: semantic search via Qdrant + keyword search via SQL + RRF fusion in Go.
With pg_recall: `recall()` function handles semantic + FTS + temporal + Hebbian scoring in one SQL call. The Go-side RRF fusion, 4 keyword search query variants, and separate search paths are eliminated.

### 5. Embedding Timing
Currently: embedding + Qdrant upsert happen async in background after DB insert.
With pg_recall: embedding must happen **before** INSERT (the `embedding` column is NOT NULL). The `remember` tool becomes synchronous for the embed+insert, which adds ~50-100ms latency but simplifies the flow. Near-duplicate detection also becomes synchronous (cosine distance query on mnemes table).

### 6. Recall Confirmation
Currently: `IncrementRecallCount` bumps a counter.
With pg_recall: `confirm_recall(mneme_ids)` applies Bayesian confidence updates. Called in background after recall, same pattern as current.

## Files to Modify

### New Files
- `ch-server/internal/mneme/store.go` — pg_recall adapter (replaces vector.Client + memory sqlc queries)
- `ch-server/internal/mneme/queries.go` — raw SQL for pg_recall operations

### Modified Files
- `ch-server/internal/mcp/server.go` — replace `MemoryQuerier + VectorDB` with `mneme.Store`
- `ch-server/internal/mcp/tools.go` — rewrite all 9 handlers against `mneme.Store`
- `ch-server/cmd/api/main.go` — remove Qdrant init, create `mneme.Store`
- `ch-server/internal/config/config.go` — remove/deprecate QdrantConfig

### Deleted Files
- `ch-server/internal/vector/qdrant.go` — entire Qdrant client

### Unchanged
- `ch-server/internal/embedding/` — stays (still need Together.ai for embeddings)
- `ch-server/internal/auth/` — stays
- `ch-server/internal/secrets/` — stays
- `ch-server/internal/repository/` — stays for audit_log, users, api_keys (non-memory queries)

## Implementation Steps

### Step 1: Create `internal/mneme/` Package

New package with two files providing the pg_recall storage layer.

**`store.go`** — types and methods:
```go
type Store struct {
    pool     *pgxpool.Pool
    embedder embedding.Provider
    logger   *slog.Logger
}

// Core operations called by MCP tools
func (s *Store) Remember(ctx, userID, orgID, fact, memType, scope, tier, tags, sessionID) (Mneme, *NearDuplicate, error)
func (s *Store) Recall(ctx, userID, orgID, query, limit, mode, memType, tags, sessionID) ([]RecallResult, error)
func (s *Store) Forget(ctx, userID, mnemeID) error
func (s *Store) ChangeScope(ctx, userID, orgID, mnemeID, newScope) error
func (s *Store) List(ctx, userID, orgID, memType, tags, sessionID, limit) ([]Mneme, error)
func (s *Store) Export(ctx, userID, orgID, filters) ([]Mneme, error)
func (s *Store) ListSessions(ctx, userID, limit) ([]Session, error)
func (s *Store) GetSessionMemories(ctx, userID, sessionID) ([]Mneme, error)
func (s *Store) ConfirmRecall(ctx, mnemeIDs) error
```

**`queries.go`** — raw SQL (no sqlc, since pg_recall types aren't in our schema):
- INSERT into `pg_recall.mnemes` with embedded vector
- SELECT for near-duplicate check (cosine distance `<=>` operator)
- `SELECT * FROM pg_recall.recall(...)` with score_weights mapping
- DELETE from `pg_recall.mnemes` with ownership check
- UPDATE workspace_id + scope for share_memory
- List/export queries against `pg_recall.mnemes` directly
- Session aggregation queries (GROUP BY session_id)
- Supersession check: SELECT existing mneme by (workspace_id, concept), call `pg_recall.mark_supersedes()`

**Remember flow:**
1. Embed fact text (synchronous, ~50-100ms)
2. Check for existing mneme with same concept in workspace → if found, will supersede
3. INSERT into pg_recall.mnemes (content, embedding, metadata)
4. If superseding, call `pg_recall.mark_supersedes(new_id, old_id)`
5. Near-duplicate check: cosine distance query, threshold 0.92
6. Return mneme + optional near-duplicate notice

**Recall flow:**
1. Embed query text
2. Call `pg_recall.recall(user_id, query, embedding, ...)` for personal results
3. Call `pg_recall.recall(org_id, query, embedding, ..., scope='org')` for org results
4. Merge by score, deduplicate, truncate to limit
5. Background: `pg_recall.confirm_recall(mneme_ids)` for top results

**Mode mapping to score_weights:**
- `semantic`: (semantic=1.0, fts=0.0)
- `keyword`: (semantic=0.0, fts=1.0) — still needs embedding for the recall() call but FTS dominates
- `hybrid`: (semantic=0.6, fts=0.4) — pg_recall defaults

### Step 2: Rewire MCP Server

Update `server.go`:
- Replace `MemoryQuerier` + `VectorDB` with `*mneme.Store`
- Keep a minimal `AuditQuerier` interface for `CreateAuditLog` only
- Update `NewServer` signature

Update each handler in `tools.go`:
- `handleRemember` → `s.store.Remember()`
- `handleRecall` → `s.store.Recall()` (eliminates RRF fusion, keyword SQL variants)
- `handleForget` → `s.store.Forget()`
- `handleShareMemory` → `s.store.ChangeScope()`
- `handleListMemories` → `s.store.List()`
- `handleExportMemories` → `s.store.Export()`
- `handleListSessions` → `s.store.ListSessions()`
- `handleSessionSummary/Context` → `s.store.GetSessionMemories()`

Update tool schemas: `fact_id` type changes from `integer` to `string` (UUID).

### Step 3: Rewire Main + Config

Update `cmd/api/main.go`:
- Remove `vector.NewClient()` and Qdrant connection
- Create `mneme.NewStore(pool, embedder, logger)`
- Pass store to `mcp.NewServer()`

Update `config/config.go`:
- Remove QdrantConfig (or keep as dead code with deprecation comment)

### Step 4: Delete Dead Code

- Delete `internal/vector/` package
- Remove memory-block sqlc queries from `db/queries/memory_blocks.sql` (keep users, audit, etc.)
- Remove `MemoryQuerier` interface from server.go
- Clean up go.mod: remove `github.com/qdrant/go-client` dependency

### Step 5: Verify

- Build: `go build ./...`
- Deploy to homelab with updated Helm values (remove Qdrant env vars)
- Test each MCP tool via Claude Code connected to homelab
- Verify pg_recall worker stats show activity after remember+recall cycles
- Verify near-duplicate detection, scope changes, session tracking
