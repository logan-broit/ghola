package sietch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/logan-broit/ghola/internal/core"
)

// rowQuery is shared by *sql.DB and *sql.Tx; receipt checks inside an append
// must use the transaction rather than acquiring another pooled connection.
type rowQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// existingSessionConn requires s.mu. A rejected record must not create an
// orphan database through conn's normal create-on-open behavior.
func (s *Store) existingSessionConn(sessionID string) (*sql.DB, error) {
	if _, err := os.Stat(s.pathFor(sessionID)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, core.ErrSessionNotFound
		}
		return nil, err
	}
	return s.conn(sessionID)
}

func checkOwner(ctx context.Context, q rowQuery, sessionID, userID string) error {
	var owner string
	err := q.QueryRowContext(ctx, `SELECT user_id FROM session WHERE id = ?`, sessionID).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ErrSessionNotFound
	}
	if err != nil {
		return err
	}
	if owner != userID {
		return core.ErrReplayConflict
	}
	return nil
}

func replayEvent(ctx context.Context, q rowQuery, sourceKey, fingerprint string) (core.Event, bool, error) {
	var previous, eventID string
	err := q.QueryRowContext(ctx, `SELECT fingerprint, event_id FROM ingest_receipts
		WHERE operation = 'record' AND source_key = ?`, sourceKey).Scan(&previous, &eventID)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Event{}, false, nil
	}
	if err != nil {
		return core.Event{}, false, err
	}
	if previous != fingerprint {
		return core.Event{}, false, core.ErrReplayConflict
	}
	ev, err := scanEvent(q.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM events WHERE id = ?`, eventID))
	return ev, true, err
}

// ReplayEvent resolves retries before the caller processes the original text.
func (s *Store) ReplayEvent(ctx context.Context, sessionID, userID, sourceKey, fingerprint string) (core.Event, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.existingSessionConn(sessionID)
	if err != nil {
		return core.Event{}, false, err
	}
	if err := checkOwner(ctx, db, sessionID, userID); err != nil {
		return core.Event{}, false, err
	}
	return replayEvent(ctx, db, sourceKey, fingerprint)
}

// RecordEventOnce commits a new event and its side effects with the receipt,
// or reads the existing event without repeating those effects.
func (s *Store) RecordEventOnce(ctx context.Context, ev core.Event, sourceKey, fingerprint string) (core.Event, error) {
	// ponytail: serialize local writes against file GC; use per-session locks
	// if measured throughput requires parallel writes to unrelated sessions.
	s.mu.Lock()
	defer s.mu.Unlock()
	db, err := s.existingSessionConn(ev.SessionID)
	if err != nil {
		return core.Event{}, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return core.Event{}, err
	}
	defer tx.Rollback()
	// Acquire SQLite's writer lock before reading. The reservation and all
	// append side effects roll back together on any validation/insert failure.
	result, err := tx.ExecContext(ctx, `INSERT INTO ingest_receipts (operation, source_key, fingerprint)
		VALUES ('record', ?, ?) ON CONFLICT(operation, source_key) DO NOTHING`, sourceKey, fingerprint)
	if err != nil {
		return core.Event{}, err
	}
	if err := checkOwner(ctx, tx, ev.SessionID, ev.UserID); err != nil {
		return core.Event{}, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return core.Event{}, err
	}
	if n == 0 {
		stored, _, err := replayEvent(ctx, tx, sourceKey, fingerprint)
		if err != nil {
			return core.Event{}, err
		}
		if err := tx.Commit(); err != nil {
			return core.Event{}, err
		}
		return stored, nil
	}
	// Allocating before embedding can leave a slow event behind an already
	// advanced watermark. Assign only while holding the append transaction.
	ev.ID = core.NewID()
	if _, err := insertEvent(ctx, tx, ev); err != nil {
		return core.Event{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE ingest_receipts SET event_id = ?
		WHERE operation = 'record' AND source_key = ?`, ev.ID, sourceKey); err != nil {
		return core.Event{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE session SET current_event_id = ? WHERE id = ?`, ev.ID, ev.SessionID); err != nil {
		return core.Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return core.Event{}, err
	}
	return scanEvent(db.QueryRowContext(ctx, `SELECT `+eventColumns+` FROM events WHERE id = ?`, ev.ID))
}

const eventColumns = `id, parent_id, session_id, user_id, request_id,
	type, role, text, tool_name, tool_use_id, tool_input, tool_output,
	bookmark_label, cwd, git_branch, agent_id, is_sidechain, model,
	raw_event, embedding, entities, tags, source_device, created_at, state`

// Both *sql.Row and *sql.Rows implement Scan.
func scanEvent(row interface{ Scan(...any) error }) (core.Event, error) {
	var ev core.Event
	var (
		parentID, requestID, role, text, toolName, toolUseID,
		bookmarkLabel, cwd, gitBranch, agentID, model, sourceDevice sql.NullString
		toolInput, toolOutput, rawEvent sql.NullString
		entities, tags, state           string
		emb                             []byte
		isSidechain                     int
		createdAtMs                     int64
	)
	if err := row.Scan(
		&ev.ID, &parentID, &ev.SessionID, &ev.UserID, &requestID,
		&ev.Type, &role, &text, &toolName, &toolUseID,
		&toolInput, &toolOutput, &bookmarkLabel,
		&cwd, &gitBranch, &agentID, &isSidechain, &model,
		&rawEvent, &emb, &entities, &tags, &sourceDevice, &createdAtMs, &state,
	); err != nil {
		return core.Event{}, err
	}
	ev.CreatedAt = time.UnixMilli(createdAtMs).UTC()
	if state == "forgotten" {
		// SoftForget retains raw/tool metadata in storage. Replaying a receipt
		// must not expose it as a new response or resurrect original content.
		forgotten := "[forgotten]"
		ev.Text = &forgotten
		ev.RawEvent = json.RawMessage(`{}`)
		return ev, nil
	}
	ev.ParentID = stringPtr(parentID)
	ev.RequestID = stringPtr(requestID)
	ev.Role = stringPtr(role)
	ev.Text = stringPtr(text)
	ev.ToolName = stringPtr(toolName)
	ev.ToolUseID = stringPtr(toolUseID)
	if toolInput.Valid {
		ev.ToolInput = json.RawMessage(toolInput.String)
	}
	if toolOutput.Valid {
		ev.ToolOutput = json.RawMessage(toolOutput.String)
	}
	ev.BookmarkLabel = stringPtr(bookmarkLabel)
	ev.Cwd = stringPtr(cwd)
	ev.GitBranch = stringPtr(gitBranch)
	ev.AgentID = stringPtr(agentID)
	ev.IsSidechain = isSidechain != 0
	ev.Model = stringPtr(model)
	if rawEvent.Valid {
		ev.RawEvent = json.RawMessage(rawEvent.String)
	}
	ev.Embedding = unpackFloat32(emb)
	_ = json.Unmarshal([]byte(entities), &ev.Entities)
	_ = json.Unmarshal([]byte(tags), &ev.Tags)
	ev.SourceDevice = stringPtr(sourceDevice)
	return ev, nil
}
