package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// EpisodicSession mirrors episodic.sessions + the OpenAPI Session DTO.
// Optional fields use pointers so "absent in JSON" and "null" map
// cleanly to SQL NULL.
//
// WorkspaceID is the scoping primitive recall queries filter by. It is
// not a column on episodic.sessions — it lives in the session_workspaces
// join table (N:N) and IngestEpisodicBatch writes it in the same tx as
// the session UPSERT. Required at the wire boundary (handler rejects
// uuid.Nil with 400) so a session never lands without a scope.
type EpisodicSession struct {
	ID           uuid.UUID  `json:"id"`
	UserID       uuid.UUID  `json:"user_id"`
	WorkspaceID  uuid.UUID  `json:"workspace_id"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	EventCount   int        `json:"event_count,omitempty"`
	Summary      *string    `json:"summary,omitempty"`
	Cwd          *string    `json:"cwd,omitempty"`
	GitBranch    *string    `json:"git_branch,omitempty"`
	AgentKind    *string    `json:"agent_kind,omitempty"`
	SourceDevice *string    `json:"source_device,omitempty"`
}

// EpisodicEvent mirrors episodic.events + the OpenAPI Event DTO. The
// JSON tags match the wire format used by the ghola local service.
type EpisodicEvent struct {
	ID            uuid.UUID       `json:"id"`
	ParentID      *uuid.UUID      `json:"parent_id,omitempty"`
	SessionID     uuid.UUID       `json:"session_id"`
	UserID        uuid.UUID       `json:"user_id"`
	RequestID     *string         `json:"request_id,omitempty"`
	Type          string          `json:"type"`
	Role          *string         `json:"role,omitempty"`
	Text          *string         `json:"text,omitempty"`
	ToolName      *string         `json:"tool_name,omitempty"`
	ToolUseID     *string         `json:"tool_use_id,omitempty"`
	ToolInput     json.RawMessage `json:"tool_input,omitempty"`
	ToolOutput    json.RawMessage `json:"tool_output,omitempty"`
	BookmarkLabel *string         `json:"bookmark_label,omitempty"`
	Cwd           *string         `json:"cwd,omitempty"`
	GitBranch     *string         `json:"git_branch,omitempty"`
	AgentID       *string         `json:"agent_id,omitempty"`
	IsSidechain   bool            `json:"is_sidechain"`
	Model         *string         `json:"model,omitempty"`
	RawEvent      json.RawMessage `json:"raw_event"`
	Embedding     []float64       `json:"embedding,omitempty"`
	Entities      []string        `json:"entities,omitempty"`
	Tags          []string        `json:"tags,omitempty"`
	SourceDevice  *string         `json:"source_device,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

// IngestEpisodicBatch upserts one session + its events in a single
// transaction. Returns (inserted, updated) counts across the events
// (the session itself is not counted — it's a side effect).
//
// Idempotency: events use ON CONFLICT (id) DO UPDATE, session uses
// ON CONFLICT (id) DO UPDATE. Re-POSTing the same batch is safe and
// is the retry contract Pipeline A depends on.
//
// xmax trick: for each event we RETURNING (xmax = 0) — true means
// "this row was just inserted", false means "this row already existed
// and was updated in place".
func (r *Repository) IngestEpisodicBatch(
	ctx context.Context,
	session *EpisodicSession,
	events []EpisodicEvent,
) (inserted, updated int, err error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := upsertSession(ctx, tx, session); err != nil {
		return 0, 0, fmt.Errorf("upsert session: %w", err)
	}

	// Workspace scoping: write the join row in the same tx so the
	// recall-side filter (sw.workspace_id = $...) actually finds the
	// session. ON CONFLICT DO NOTHING preserves the at-least-once
	// retry contract Pipeline A relies on — a re-POST of the same
	// batch must be a no-op, not a unique-key error that rolls back.
	if _, err := tx.Exec(ctx, `
		INSERT INTO episodic.session_workspaces (session_id, workspace_id)
		VALUES ($1, $2)
		ON CONFLICT (session_id, workspace_id) DO NOTHING
	`, session.ID, session.WorkspaceID); err != nil {
		return 0, 0, fmt.Errorf("session_workspaces upsert: %w", err)
	}

	if len(events) > 0 {
		i, u, err := upsertEvents(ctx, tx, events)
		if err != nil {
			return 0, 0, fmt.Errorf("upsert events: %w", err)
		}
		inserted, updated = i, u
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, 0, fmt.Errorf("commit: %w", err)
	}
	return inserted, updated, nil
}

const upsertSessionSQL = `
INSERT INTO episodic.sessions (
	id, user_id, started_at, ended_at, event_count, summary,
	cwd, git_branch, agent_kind, source_device
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
ON CONFLICT (id) DO UPDATE SET
	ended_at      = EXCLUDED.ended_at,
	event_count   = EXCLUDED.event_count,
	summary       = EXCLUDED.summary,
	cwd           = EXCLUDED.cwd,
	git_branch    = EXCLUDED.git_branch,
	agent_kind    = EXCLUDED.agent_kind,
	source_device = EXCLUDED.source_device
`

func upsertSession(ctx context.Context, tx pgx.Tx, s *EpisodicSession) error {
	_, err := tx.Exec(ctx, upsertSessionSQL,
		s.ID, s.UserID, s.StartedAt, s.EndedAt, s.EventCount,
		s.Summary, s.Cwd, s.GitBranch, s.AgentKind, s.SourceDevice,
	)
	return err
}

// upsertEvents bulk-upserts via pgx.Batch (one roundtrip, many
// queued INSERT … ON CONFLICT statements). Each row returns
// (xmax = 0) so we can discriminate insert-vs-update.
func upsertEvents(ctx context.Context, tx pgx.Tx, events []EpisodicEvent) (int, int, error) {
	batch := &pgx.Batch{}
	for i := range events {
		ev := &events[i]
		emb := vectorLiteralOrNil(ev.Embedding)
		entities := ev.Entities
		if entities == nil {
			entities = []string{}
		}
		tags := ev.Tags
		if tags == nil {
			tags = []string{}
		}
		rawEvent := []byte(ev.RawEvent)
		if len(rawEvent) == 0 {
			rawEvent = []byte("{}")
		}
		toolInput := nullableJSONB(ev.ToolInput)
		toolOutput := nullableJSONB(ev.ToolOutput)

		batch.Queue(insertEventSQL,
			ev.ID, ev.ParentID, ev.SessionID, ev.UserID, ev.RequestID,
			ev.Type, ev.Role, ev.Text, ev.ToolName, ev.ToolUseID,
			toolInput, toolOutput, ev.BookmarkLabel,
			ev.Cwd, ev.GitBranch, ev.AgentID, ev.IsSidechain, ev.Model,
			rawEvent, emb, entities, tags, ev.SourceDevice, ev.CreatedAt,
		)
	}

	br := tx.SendBatch(ctx, batch)
	defer br.Close()

	var inserted, updated int
	for range events {
		var wasInsert bool
		if err := br.QueryRow().Scan(&wasInsert); err != nil {
			return 0, 0, err
		}
		if wasInsert {
			inserted++
		} else {
			updated++
		}
	}
	return inserted, updated, nil
}

// $1..$24. Note embedding is passed as text (vector literal) and cast
// with ::vector at the DB level — pgx doesn't natively marshal
// []float64 to pgvector's binary format without the pgvector-go
// driver, and the text path is fast enough for Pipeline A's batch
// sizes (<= 1000 events per POST).
const insertEventSQL = `
INSERT INTO episodic.events (
	id, parent_id, session_id, user_id, request_id,
	type, role, text, tool_name, tool_use_id,
	tool_input, tool_output, bookmark_label,
	cwd, git_branch, agent_id, is_sidechain, model,
	raw_event, embedding, entities, tags, source_device, created_at
) VALUES (
	$1, $2, $3, $4, $5,
	$6, $7, $8, $9, $10,
	$11, $12, $13,
	$14, $15, $16, $17, $18,
	$19, $20::vector, $21, $22, $23, $24
)
ON CONFLICT (id) DO UPDATE SET
	parent_id      = EXCLUDED.parent_id,
	session_id     = EXCLUDED.session_id,
	user_id        = EXCLUDED.user_id,
	request_id     = EXCLUDED.request_id,
	type           = EXCLUDED.type,
	role           = EXCLUDED.role,
	text           = EXCLUDED.text,
	tool_name      = EXCLUDED.tool_name,
	tool_use_id    = EXCLUDED.tool_use_id,
	tool_input     = EXCLUDED.tool_input,
	tool_output    = EXCLUDED.tool_output,
	bookmark_label = EXCLUDED.bookmark_label,
	cwd            = EXCLUDED.cwd,
	git_branch     = EXCLUDED.git_branch,
	agent_id       = EXCLUDED.agent_id,
	is_sidechain   = EXCLUDED.is_sidechain,
	model          = EXCLUDED.model,
	raw_event      = EXCLUDED.raw_event,
	embedding      = EXCLUDED.embedding,
	entities       = EXCLUDED.entities,
	tags           = EXCLUDED.tags,
	source_device  = EXCLUDED.source_device,
	created_at     = EXCLUDED.created_at
RETURNING (xmax = 0)
`

// ---------------------------------------------------------------------
// Share / Forget
// ---------------------------------------------------------------------

// ErrShareNotOwned signals a 403: the caller is trying to share a
// scope they don't own.
var ErrShareNotOwned = fmt.Errorf("caller does not own the referenced scope")

// CreateShareParams carries the validated inputs for creating a share.
type CreateShareParams struct {
	Caller    uuid.UUID
	Target    string
	TargetID  *uuid.UUID
	ScopeType string
	ScopeID   uuid.UUID
}

// CreateEpisodicShare inserts a share row after verifying the caller
// owns the referenced scope. Returns ErrShareNotOwned if not.
func (r *Repository) CreateEpisodicShare(ctx context.Context, p CreateShareParams) (uuid.UUID, error) {
	if err := r.verifyScopeOwnership(ctx, p.Caller, p.ScopeType, p.ScopeID); err != nil {
		return uuid.Nil, err
	}

	var id uuid.UUID
	err := r.pool.QueryRow(ctx, `
		INSERT INTO episodic.shares
			(owner_user_id, target, target_id, scope_type, scope_id)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id
	`, p.Caller, p.Target, p.TargetID, p.ScopeType, p.ScopeID).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("insert share: %w", err)
	}
	return id, nil
}

// verifyScopeOwnership checks that `caller` is the user_id on the
// referenced session / event. For branch scopes the check is against
// the root event (scope_id itself).
func (r *Repository) verifyScopeOwnership(
	ctx context.Context, caller uuid.UUID, scopeType string, scopeID uuid.UUID,
) error {
	var table string
	switch scopeType {
	case "session":
		table = "episodic.sessions"
	case "event", "branch":
		table = "episodic.events"
	default:
		return fmt.Errorf("unknown scope_type %q", scopeType)
	}

	var ownerID uuid.UUID
	err := r.pool.QueryRow(ctx,
		`SELECT user_id FROM `+table+` WHERE id = $1`,
		scopeID,
	).Scan(&ownerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrShareNotOwned
		}
		return fmt.Errorf("lookup scope owner: %w", err)
	}
	if ownerID != caller {
		return ErrShareNotOwned
	}
	return nil
}

// SoftDeleteEpisodicEvents flips the target events to the "forgotten"
// state: text='[forgotten]', embedding=NULL. The tree's parent_id
// links stay intact so descendants aren't orphaned. Caller can only
// forget events they own; the WHERE user_id = $caller clause is what
// enforces that.
func (r *Repository) SoftDeleteEpisodicEvents(
	ctx context.Context, caller uuid.UUID, ids []uuid.UUID,
) (int, error) {
	tag, err := r.pool.Exec(ctx, `
		UPDATE episodic.events
		SET text = '[forgotten]',
		    embedding = NULL
		WHERE user_id = $1 AND id = ANY($2::uuid[])
	`, caller, ids)
	if err != nil {
		return 0, fmt.Errorf("soft delete: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// ---------------------------------------------------------------------
// Session workspace tagging (expand-on-demand)
// ---------------------------------------------------------------------

// ErrSessionNotFound signals a 409: the referenced session row does
// not exist. Surfaced ahead of the INSERT so the handler does not
// have to map a Postgres FK violation by hand.
var ErrSessionNotFound = errors.New("session not found")

// ErrSessionNotOwned signals a 403: the caller is not the owner of
// the referenced session. Same shape as the per-user ACL on
// forget/share.
var ErrSessionNotOwned = errors.New("session not owned by caller")

// AddSessionWorkspaceParams carries the validated inputs for tagging
// an existing session into an additional workspace.
type AddSessionWorkspaceParams struct {
	UserID      uuid.UUID
	SessionID   uuid.UUID
	WorkspaceID uuid.UUID
}

// AddSessionWorkspace tags an existing session into an additional
// workspace. Two preconditions:
//  1. The session exists in episodic.sessions (FK requirement;
//     enforced ahead of the INSERT so we return the typed
//     ErrSessionNotFound rather than a Postgres FK error).
//  2. The session belongs to the caller — same per-user ACL the
//     existing /forget and /share endpoints enforce.
//
// Returns added=true when the row was newly written, added=false
// when an identical row already existed (ON CONFLICT DO NOTHING).
func (r *Repository) AddSessionWorkspace(ctx context.Context, p AddSessionWorkspaceParams) (bool, error) {
	var ownerID uuid.UUID
	err := r.pool.QueryRow(ctx, `
		SELECT user_id FROM episodic.sessions WHERE id = $1
	`, p.SessionID).Scan(&ownerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrSessionNotFound
	}
	if err != nil {
		return false, fmt.Errorf("session lookup: %w", err)
	}
	if ownerID != p.UserID {
		return false, ErrSessionNotOwned
	}

	tag, err := r.pool.Exec(ctx, `
		INSERT INTO episodic.session_workspaces (session_id, workspace_id)
		VALUES ($1, $2)
		ON CONFLICT (session_id, workspace_id) DO NOTHING
	`, p.SessionID, p.WorkspaceID)
	if err != nil {
		return false, fmt.Errorf("session_workspaces insert: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ---------------------------------------------------------------------
// Query
// ---------------------------------------------------------------------

// EpisodicEventHit is a scored event hit from any per-tier event-grain
// query (QueryEpisodicKeyword, QueryEpisodicEventsByVector). Per-tier
// callers populate only the score legs they compute (Semantic for
// vector, FTS for keyword); Merged tracks whichever leg is the tier's
// sort key.
//
// SessionChunkText is the role-prefixed concatenation of the event's
// session, persisted to episodic.sessions.l1_chunk_text at session
// close (semantic.Writer). Empty when the session hasn't been
// consolidated yet (open session, mid-tick, or pre-migration). Carried
// here so cross-encoder rerank consumers (ghola.Recall) can score
// against full session text instead of single-event text without an
// extra round-trip per candidate.
type EpisodicEventHit struct {
	Event            EpisodicEvent
	Semantic         float64
	FTS              float64
	Merged           float64
	SessionChunkText string
}

// EpisodicKeywordParams is the input shape for QueryEpisodicKeyword.
// Narrower than EpisodicQueryParams: keyword search is FTS-only, so
// no embedding, no semantic/FTS weighting, and no rich filter set
// (the recall path's RRF fan-out wants a clean ranked list, not
// attribute-filtered subsets).
type EpisodicKeywordParams struct {
	UserID      uuid.UUID
	WorkspaceID uuid.UUID
	QueryText   string
	Limit       int
	// TagsAny is the same overlap-style tag filter as
	// QueryFilters.TagsAny on the dense path: when non-empty,
	// only events whose `tags` column overlaps the supplied list
	// are returned (`tags && $`). Empty/nil → no filter applied.
	TagsAny []string
}

// QueryEpisodicKeyword runs a Postgres FTS-only ranking over the
// caller's accessible events. Used by ghola.Recall as a fourth RRF
// strategy alongside sietch/episodic-vector/semantic — the dense
// vector path can miss literal phrase matches (proper nouns, code
// identifiers, exact quotes) that this path lights up.
//
// Ranking: ts_rank_cd (cover-density variant, weights term proximity
// alongside frequency). Empty query returns zero hits — the caller
// already gates on QueryText != "" before fanning out.
//
// Shares are intentionally not included here; v0.4's RRF fan-out
// scopes shared content via the dense episodic tier only. Adding
// shares on this path is a clean follow-up if a real use case asks.
//
// SessionChunkText comes from the same JOIN as
// QueryEpisodicEventsByVector (episodic.sessions.l1_chunk_text) so the
// cross-encoder rerank input shape is identical across tiers.
func (r *Repository) QueryEpisodicKeyword(
	ctx context.Context, p EpisodicKeywordParams,
) ([]EpisodicEventHit, error) {
	if p.Limit <= 0 {
		p.Limit = 10
	}
	if p.QueryText == "" {
		return nil, nil
	}

	// Args: $1=query_text, $2=user_id, $3=limit, $4=workspace_id,
	// $5=tags_any (only appended when non-empty). Building the SQL
	// dynamically keeps the no-filter path identical to before.
	args := []any{p.QueryText, p.UserID, p.Limit, p.WorkspaceID}
	tagsClause := ""
	if len(p.TagsAny) > 0 {
		args = append(args, p.TagsAny)
		tagsClause = fmt.Sprintf("  AND e.tags && $%d::text[]\n", len(args))
	}
	sql := `
SELECT e.id, e.parent_id, e.session_id, e.user_id, e.request_id, e.type,
       e.role, e.text, e.tool_name, e.tool_use_id, e.tool_input,
       e.tool_output, e.bookmark_label, e.cwd, e.git_branch, e.agent_id,
       e.is_sidechain, e.model, e.raw_event, e.entities, e.tags,
       e.source_device, e.created_at,
       ts_rank_cd(e.search_vector, q) AS fts_score,
       coalesce(s.l1_chunk_text, '') AS session_chunk_text
FROM episodic.events e
CROSS JOIN websearch_to_tsquery('english', $1) q
LEFT JOIN episodic.sessions s ON s.id = e.session_id
JOIN episodic.session_workspaces sw ON sw.session_id = e.session_id
WHERE e.user_id = $2::uuid
  AND sw.workspace_id = $4::uuid
  AND e.search_vector @@ q
` + tagsClause + `ORDER BY fts_score DESC
LIMIT $3
`

	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query keyword: %w", err)
	}
	defer rows.Close()

	var hits []EpisodicEventHit
	for rows.Next() {
		var ev EpisodicEvent
		var h EpisodicEventHit
		var toolIn, toolOut, rawEvent []byte
		if err := rows.Scan(
			&ev.ID, &ev.ParentID, &ev.SessionID, &ev.UserID, &ev.RequestID, &ev.Type,
			&ev.Role, &ev.Text, &ev.ToolName, &ev.ToolUseID, &toolIn,
			&toolOut, &ev.BookmarkLabel, &ev.Cwd, &ev.GitBranch, &ev.AgentID,
			&ev.IsSidechain, &ev.Model, &rawEvent, &ev.Entities, &ev.Tags,
			&ev.SourceDevice, &ev.CreatedAt,
			&h.FTS, &h.SessionChunkText,
		); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		ev.ToolInput = toolIn
		ev.ToolOutput = toolOut
		ev.RawEvent = rawEvent
		h.Event = ev
		// Keyword tier: Merged tracks FTS so callers (and JSON wire
		// shape) see a single sort key. Semantic stays zero so the
		// breakdown remains intelligible to anyone inspecting hits.
		h.Merged = h.FTS
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// EpisodicVectorParams is the input shape for
// QueryEpisodicEventsByVector. Mirrors EpisodicKeywordParams in shape
// but takes a query embedding instead of query text — the tier is
// pure cosine on the per-event grain, no FTS contribution.
//
// TagsAny mirrors QueryFilters.TagsAny on the hybrid path: when
// non-empty, only events whose `tags` column overlaps the supplied
// list are returned. Empty/nil → no filter applied.
type EpisodicVectorParams struct {
	UserID         uuid.UUID
	WorkspaceID    uuid.UUID
	QueryEmbedding []float64
	Limit          int
	TagsAny        []string
}

// QueryEpisodicEventsByVector runs a pure-cosine ranking over the
// caller's accessible events on the per-event grain. Sibling of
// QueryEpisodicKeyword (FTS-only) — the tier exists so callers that
// score events on embedding alone (or want to keep vector and FTS
// paths separable for fan-out) have a dedicated entry point.
//
// Ranking: 1 - (embedding <=> query) — `1 - cosine_distance`, matching
// the convention every tier uses. Order is `embedding <=> query ASC`
// so the HNSW index on (embedding vector_cosine_ops) is the planner's
// natural choice; the SELECT projects `1 - distance` for callers.
//
// Workspace scoping and per-user ACL match QueryEpisodicKeyword.
// Shares are out of scope for this tier — the RRF fan-out scopes
// shared content via downstream layers; adding shares here is a clean
// follow-up if a real use case asks. SessionChunkText is carried from
// episodic.sessions.l1_chunk_text so cross-encoder rerank consumers
// (ghola.Recall) score against full session text without an extra
// round-trip per candidate.
//
// Empty embedding short-circuits to nil hits — the recall caller
// already gates on len(emb) > 0, but defense-in-depth here keeps a
// misbehaving caller from spraying NULL::vector parses at the DB.
func (r *Repository) QueryEpisodicEventsByVector(
	ctx context.Context, p EpisodicVectorParams,
) ([]EpisodicEventHit, error) {
	if p.Limit <= 0 {
		p.Limit = 10
	}
	if len(p.QueryEmbedding) == 0 {
		return nil, nil
	}

	// Text-cast pathway: pgx text + ::vector is the lowest-friction
	// way to bind a slice without depending on pgvector-go's binary
	// codec.
	parts := make([]string, len(p.QueryEmbedding))
	for i, x := range p.QueryEmbedding {
		parts[i] = strconv.FormatFloat(x, 'f', -1, 64)
	}
	embLit := "[" + strings.Join(parts, ",") + "]"

	// Args: $1=user_id, $2=workspace_id, $3=embedding, $4=limit,
	// $5=tags_any (only appended when non-empty). Building the SQL
	// dynamically keeps the no-filter path identical to a static query.
	args := []any{p.UserID, p.WorkspaceID, embLit, p.Limit}
	tagsClause := ""
	if len(p.TagsAny) > 0 {
		args = append(args, p.TagsAny)
		tagsClause = fmt.Sprintf("  AND e.tags && $%d::text[]\n", len(args))
	}

	sql := `
SELECT e.id, e.parent_id, e.session_id, e.user_id, e.request_id, e.type,
       e.role, e.text, e.tool_name, e.tool_use_id, e.tool_input,
       e.tool_output, e.bookmark_label, e.cwd, e.git_branch, e.agent_id,
       e.is_sidechain, e.model, e.raw_event, e.entities, e.tags,
       e.source_device, e.created_at,
       1 - (e.embedding <=> ($3::text)::vector) AS semantic_score,
       coalesce(s.l1_chunk_text, '') AS session_chunk_text
FROM episodic.events e
JOIN episodic.session_workspaces sw ON sw.session_id = e.session_id
LEFT JOIN episodic.sessions s ON s.id = e.session_id
WHERE e.user_id = $1::uuid
  AND sw.workspace_id = $2::uuid
  AND e.embedding IS NOT NULL
` + tagsClause + `ORDER BY e.embedding <=> ($3::text)::vector
LIMIT $4
`

	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query vector: %w", err)
	}
	defer rows.Close()

	var hits []EpisodicEventHit
	for rows.Next() {
		var ev EpisodicEvent
		var h EpisodicEventHit
		var toolIn, toolOut, rawEvent []byte
		if err := rows.Scan(
			&ev.ID, &ev.ParentID, &ev.SessionID, &ev.UserID, &ev.RequestID, &ev.Type,
			&ev.Role, &ev.Text, &ev.ToolName, &ev.ToolUseID, &toolIn,
			&toolOut, &ev.BookmarkLabel, &ev.Cwd, &ev.GitBranch, &ev.AgentID,
			&ev.IsSidechain, &ev.Model, &rawEvent, &ev.Entities, &ev.Tags,
			&ev.SourceDevice, &ev.CreatedAt,
			&h.Semantic, &h.SessionChunkText,
		); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		ev.ToolInput = toolIn
		ev.ToolOutput = toolOut
		ev.RawEvent = rawEvent
		h.Event = ev
		// Pure-cosine tier: Merged tracks Semantic so callers (and
		// JSON wire shape) see a single sort key. FTS stays zero so
		// the breakdown remains intelligible to anyone inspecting hits.
		h.Merged = h.Semantic
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// EpisodicSessionVectorParams is the input shape for
// QueryEpisodicSessionVector. Mirrors EpisodicKeywordParams in shape
// but takes a query embedding instead of query text — the tier exists
// to catch paraphrase-style queries where per-event embeddings miss
// but session-level pooled embeddings hit.
type EpisodicSessionVectorParams struct {
	UserID         uuid.UUID
	WorkspaceID    uuid.UUID
	QueryEmbedding []float64
	Limit          int
}

// EpisodicSessionVectorHit is a scored session from
// QueryEpisodicSessionVector.
//
// Unlike EpisodicEventHit, the unit here is the session itself: the
// id field is the session_id. l1_chunk_text is the role-prefixed
// session text persisted at session-close (mirroring the JOIN that
// the event-level tiers carry); the cross-encoder reranker scores
// against this text directly.
//
// Score is `1 - cosine_distance` (range [-1, 1] for arbitrary vectors,
// [0, 2] for unit vectors via the inverted convention) — higher score
// = more similar, matching the convention used by every other tier.
type EpisodicSessionVectorHit struct {
	SessionID        uuid.UUID
	Score            float64
	SessionChunkText string
}

// QueryEpisodicSessionVector runs a session-level cosine-similarity
// search over episodic.sessions.l1_embedding (the pooled per-session
// vector mentat fills in at session-close). Used by ghola.Recall as a
// fifth RRF strategy alongside sietch / episodic-vector / keyword /
// semantic — paraphrase-style queries that miss the per-event vector
// path can hit on the session-level pooled embedding.
//
// Workspace scoping: the SQL JOINs episodic.session_workspaces and
// filters by workspace_id, same as QueryEpisodicEventsByVector and
// QueryEpisodicKeyword. Without it the tier would silently leak
// across workspaces, defeating both the scoping primitive that
// established recall quality and the security boundary the
// workspace-scoping PR just shipped.
//
// Index path: episodic_sessions_l1_hnsw is a partial HNSW index on
// (l1_embedding vector_cosine_ops) WHERE l1_embedding IS NOT NULL —
// the WHERE clause here mirrors the partial-index predicate so the
// planner picks up the index on the workspace-filtered candidate set.
//
// Empty embedding short-circuits to nil hits (the recall caller gates
// on len(emb) > 0 before fanning out, but defense-in-depth here keeps
// a misbehaving caller from spraying NULL::vector parses at the DB).
func (r *Repository) QueryEpisodicSessionVector(
	ctx context.Context, p EpisodicSessionVectorParams,
) ([]EpisodicSessionVectorHit, error) {
	if p.Limit <= 0 {
		p.Limit = 10
	}
	if len(p.QueryEmbedding) == 0 {
		return nil, nil
	}

	// Text-cast pathway: pgx text + ::vector is the lowest-friction
	// way to bind a slice without depending on pgvector-go's binary
	// codec. Pipeline A's read path is well below the throughput where
	// binary would matter.
	parts := make([]string, len(p.QueryEmbedding))
	for i, x := range p.QueryEmbedding {
		parts[i] = strconv.FormatFloat(x, 'f', -1, 64)
	}
	embLit := "[" + strings.Join(parts, ",") + "]"

	sql := `
WITH accessible AS (
	SELECT s.id, s.l1_embedding, s.l1_chunk_text
	FROM episodic.sessions s
	JOIN episodic.session_workspaces sw ON sw.session_id = s.id
	WHERE s.user_id = $1
	  AND sw.workspace_id = $2::uuid
	  AND s.l1_embedding IS NOT NULL
)
SELECT id,
       coalesce(l1_chunk_text, '') AS chunk_text,
       1 - (l1_embedding <=> ($3::text)::vector) AS score
FROM accessible
ORDER BY l1_embedding <=> ($3::text)::vector
LIMIT $4
`

	rows, err := r.pool.Query(ctx, sql, p.UserID, p.WorkspaceID, embLit, p.Limit)
	if err != nil {
		return nil, fmt.Errorf("query session vector: %w", err)
	}
	defer rows.Close()

	var hits []EpisodicSessionVectorHit
	for rows.Next() {
		var h EpisodicSessionVectorHit
		if err := rows.Scan(&h.SessionID, &h.SessionChunkText, &h.Score); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// GetEventTextByIDs bulk-fetches event text for a set of event IDs, scoped
// to the given user and workspace.  Returns a map from event_id to text
// (only IDs with a non-NULL text column are included).
//
// Workspace ACL is enforced via the session_workspaces join, matching the
// defense-in-depth pattern used by QueryEpisodicEventsByVector and
// QueryEpisodicKeyword — an event that does not belong to the workspace is
// silently omitted (same as a miss).
//
// Empty ids returns a non-nil empty map without hitting the DB.
func (r *Repository) GetEventTextByIDs(
	ctx context.Context,
	ids []uuid.UUID,
	userID, workspaceID uuid.UUID,
) (map[uuid.UUID]string, error) {
	out := make(map[uuid.UUID]string)
	if len(ids) == 0 {
		return out, nil
	}

	const q = `
		SELECT e.id, e.text
		FROM episodic.events e
		JOIN episodic.sessions s ON s.id = e.session_id
		JOIN episodic.session_workspaces sw ON sw.session_id = s.id
		WHERE e.id = ANY($1::uuid[])
		  AND s.user_id = $2
		  AND sw.workspace_id = $3
		  AND e.text IS NOT NULL
	`
	rows, err := r.pool.Query(ctx, q, ids, userID, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("get event text by ids: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			id   uuid.UUID
			text string
		)
		if err := rows.Scan(&id, &text); err != nil {
			return nil, fmt.Errorf("scan event text row: %w", err)
		}
		out[id] = text
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate event text rows: %w", err)
	}
	return out, nil
}

// nullableJSONB returns the raw bytes unchanged when non-empty, and
// nil otherwise so SQL sees NULL.
func nullableJSONB(r json.RawMessage) any {
	if len(r) == 0 {
		return nil
	}
	return []byte(r)
}
