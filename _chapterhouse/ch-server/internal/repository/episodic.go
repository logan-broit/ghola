package repository

import (
	"context"
	"encoding/json"
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
