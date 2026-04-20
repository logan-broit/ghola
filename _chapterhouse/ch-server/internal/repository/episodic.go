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
type EpisodicSession struct {
	ID            uuid.UUID  `json:"id"`
	UserID        uuid.UUID  `json:"user_id"`
	StartedAt     time.Time  `json:"started_at"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	EventCount    int        `json:"event_count,omitempty"`
	Summary       *string    `json:"summary,omitempty"`
	Cwd           *string    `json:"cwd,omitempty"`
	GitBranch     *string    `json:"git_branch,omitempty"`
	AgentKind     *string    `json:"agent_kind,omitempty"`
	SourceDevice  *string    `json:"source_device,omitempty"`
}

// EpisodicEvent mirrors episodic.events + the OpenAPI Event DTO. The
// JSON tags match the wire format used by the ghola local service.
type EpisodicEvent struct {
	ID             uuid.UUID       `json:"id"`
	ParentID       *uuid.UUID      `json:"parent_id,omitempty"`
	SessionID      uuid.UUID       `json:"session_id"`
	UserID         uuid.UUID       `json:"user_id"`
	RequestID      *string         `json:"request_id,omitempty"`
	Type           string          `json:"type"`
	Role           *string         `json:"role,omitempty"`
	Text           *string         `json:"text,omitempty"`
	ToolName       *string         `json:"tool_name,omitempty"`
	ToolUseID      *string         `json:"tool_use_id,omitempty"`
	ToolInput      json.RawMessage `json:"tool_input,omitempty"`
	ToolOutput     json.RawMessage `json:"tool_output,omitempty"`
	BookmarkLabel  *string         `json:"bookmark_label,omitempty"`
	Cwd            *string         `json:"cwd,omitempty"`
	GitBranch      *string         `json:"git_branch,omitempty"`
	AgentID        *string         `json:"agent_id,omitempty"`
	IsSidechain    bool            `json:"is_sidechain"`
	Model          *string         `json:"model,omitempty"`
	RawEvent       json.RawMessage `json:"raw_event"`
	Embedding      []float64       `json:"embedding,omitempty"`
	Entities       []string        `json:"entities,omitempty"`
	Tags           []string        `json:"tags,omitempty"`
	SourceDevice   *string         `json:"source_device,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
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
// Query
// ---------------------------------------------------------------------

// QueryFilters narrows the event pool by optional structured fields.
// All fields are zero-value-means-not-filtered.
type QueryFilters struct {
	SessionID   *uuid.UUID `json:"session_id,omitempty"`
	Entities    []string   `json:"entities,omitempty"`
	Tags        []string   `json:"tags,omitempty"`
	ToolName    *string    `json:"tool_name,omitempty"`
	IsSidechain *bool      `json:"is_sidechain,omitempty"`
	Since       *time.Time `json:"since,omitempty"`
	Until       *time.Time `json:"until,omitempty"`
}

// EpisodicQueryParams is the full input to QueryEpisodicEvents.
type EpisodicQueryParams struct {
	UserID         uuid.UUID
	QueryText      string
	QueryEmbedding []float64
	Limit          int
	IncludeShared  bool
	WSemantic      float64
	WFTS           float64
	Filters        QueryFilters
}

// EpisodicEventHit is a scored event from QueryEpisodicEvents.
type EpisodicEventHit struct {
	Event    EpisodicEvent
	Semantic float64
	FTS      float64
	Merged   float64
}

// QueryEpisodicEvents runs the hybrid vector+FTS search scoped to the
// caller's visibility (own events + shares). Top-level SQL:
//
//   WITH accessible    AS (...)
//        vector_hits   AS (top N by cosine over accessible)
//        lexical_hits  AS (top N by ts_rank over accessible)
//        merged        AS (max-merge per event id)
//   SELECT ... FROM merged JOIN events WHERE <filters>
//   ORDER BY w_sem*semantic + w_fts*fts DESC LIMIT N
//
// Branch-scope shares are out of scope for v1a — only session and
// event scopes are honored by the accessible CTE here. Adding
// branch support is a recursive-CTE extension of that block.
func (r *Repository) QueryEpisodicEvents(
	ctx context.Context, p EpisodicQueryParams,
) ([]EpisodicEventHit, error) {
	if p.Limit <= 0 {
		p.Limit = 10
	}
	pool := 3 * p.Limit

	// Concrete string (not any-wrapping) so pgx passes it as text and
	// the ::vector cast in SQL works uniformly. Empty string
	// short-circuits the vector pathway via the `$2 <> ''` guard.
	var embLit string
	if len(p.QueryEmbedding) > 0 {
		parts := make([]string, len(p.QueryEmbedding))
		for i, x := range p.QueryEmbedding {
			parts[i] = strconv.FormatFloat(x, 'f', -1, 64)
		}
		embLit = "[" + strings.Join(parts, ",") + "]"
	}

	// Accessible CTE: own events, optionally UNION'd with shares.
	accessible := `SELECT id FROM episodic.events WHERE user_id = $1`
	if p.IncludeShared {
		accessible += `
			UNION
			SELECT e.id FROM episodic.events e
			JOIN episodic.shares s ON
				(s.scope_type = 'event'   AND s.scope_id = e.id)
				OR (s.scope_type = 'session' AND s.scope_id = e.session_id)
			WHERE s.target = 'team' OR (s.target = 'user' AND s.target_id = $1)`
	}

	// Filters: pushed into the final SELECT so one set of predicates
	// covers both pathways without duplication.
	filterSQL, filterArgs := buildFilterSQL(p.Filters, /*baseIdx=*/7)

	sql := `
WITH accessible AS (
	` + accessible + `
), vector_hits AS (
	SELECT id,
	       1 - (embedding <=> ($2::text)::vector) AS semantic_score,
	       0::double precision            AS fts_score
	FROM episodic.events
	WHERE id IN (SELECT id FROM accessible)
	  AND embedding IS NOT NULL
	  AND $2 <> ''
	ORDER BY embedding <=> ($2::text)::vector
	LIMIT $3
), lexical_hits AS (
	SELECT id,
	       0::double precision AS semantic_score,
	       ts_rank(search_vector, plainto_tsquery('english', $4)) AS fts_score
	FROM episodic.events
	WHERE id IN (SELECT id FROM accessible)
	  AND $4 <> ''
	  AND search_vector @@ plainto_tsquery('english', $4)
	ORDER BY fts_score DESC
	LIMIT $3
), merged AS (
	SELECT id, max(semantic_score) AS semantic_score, max(fts_score) AS fts_score
	FROM (SELECT * FROM vector_hits UNION ALL SELECT * FROM lexical_hits) u
	GROUP BY id
)
SELECT e.id, e.parent_id, e.session_id, e.user_id, e.request_id, e.type,
       e.role, e.text, e.tool_name, e.tool_use_id, e.tool_input,
       e.tool_output, e.bookmark_label, e.cwd, e.git_branch, e.agent_id,
       e.is_sidechain, e.model, e.raw_event, e.entities, e.tags,
       e.source_device, e.created_at,
       m.semantic_score, m.fts_score,
       ($5 * m.semantic_score + $6 * m.fts_score) AS merged_score
FROM merged m
JOIN episodic.events e ON e.id = m.id
WHERE TRUE ` + filterSQL + `
ORDER BY merged_score DESC
LIMIT $7
`

	args := []any{p.UserID, embLit, pool, p.QueryText, p.WSemantic, p.WFTS, p.Limit}
	args = append(args, filterArgs...)

	rows, err := r.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
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
			&h.Semantic, &h.FTS, &h.Merged,
		); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		ev.ToolInput = toolIn
		ev.ToolOutput = toolOut
		ev.RawEvent = rawEvent
		h.Event = ev
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// buildFilterSQL folds the optional filters into AND clauses using
// placeholder indices starting at baseIdx. Returns the SQL fragment
// (leading space, each clause prefixed with AND) and the appended
// args in order.
func buildFilterSQL(f QueryFilters, baseIdx int) (string, []any) {
	var b strings.Builder
	var args []any
	next := baseIdx

	if f.SessionID != nil {
		b.WriteString(fmt.Sprintf(" AND e.session_id = $%d", next))
		args = append(args, *f.SessionID)
		next++
	}
	if len(f.Entities) > 0 {
		b.WriteString(fmt.Sprintf(" AND e.entities && $%d::text[]", next))
		args = append(args, f.Entities)
		next++
	}
	if len(f.Tags) > 0 {
		b.WriteString(fmt.Sprintf(" AND e.tags @> $%d::text[]", next))
		args = append(args, f.Tags)
		next++
	}
	if f.ToolName != nil {
		b.WriteString(fmt.Sprintf(" AND e.tool_name = $%d", next))
		args = append(args, *f.ToolName)
		next++
	}
	if f.IsSidechain != nil {
		b.WriteString(fmt.Sprintf(" AND e.is_sidechain = $%d", next))
		args = append(args, *f.IsSidechain)
		next++
	}
	if f.Since != nil {
		b.WriteString(fmt.Sprintf(" AND e.created_at >= $%d", next))
		args = append(args, *f.Since)
		next++
	}
	if f.Until != nil {
		b.WriteString(fmt.Sprintf(" AND e.created_at <= $%d", next))
		args = append(args, *f.Until)
		next++
	}
	return b.String(), args
}

// vectorLiteralOrNil returns a pgvector text literal like "[1,2,3]"
// or nil (maps to SQL NULL) when the slice is empty.
func vectorLiteralOrNil(v []float64) any {
	if len(v) == 0 {
		return nil
	}
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = strconv.FormatFloat(x, 'f', -1, 64)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// nullableJSONB returns the raw bytes unchanged when non-empty, and
// nil otherwise so SQL sees NULL.
func nullableJSONB(r json.RawMessage) any {
	if len(r) == 0 {
		return nil
	}
	return []byte(r)
}
