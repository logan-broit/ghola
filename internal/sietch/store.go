// Package sietch is the on-device working-memory substrate: one
// SQLite file per session under a configurable root directory
// (default ~/.ghola/sessions/). Implements core.SietchStore.
package sietch

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"github.com/logan-broit/ghola/internal/core"
)

//go:embed schema.sql
var schemaSQL string

// Store is a thread-safe registry of open per-session SQLite handles.
type Store struct {
	root string

	mu    sync.Mutex
	conns map[string]*sql.DB
}

// Open creates a Store rooted at `root` (created if missing). Each
// session id will have its own <id>.sqlite file in this directory.
func Open(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", root, err)
	}
	return &Store{root: root, conns: map[string]*sql.DB{}}, nil
}

// DefaultRoot returns ~/.ghola/sessions, creating the home-dir
// suffix via os.UserHomeDir. Callers pass this to Open in production.
func DefaultRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".ghola", "sessions"), nil
}

// Close releases all cached DB handles. Safe to call from shutdown.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var firstErr error
	for id, db := range s.conns {
		if err := db.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("close %s: %w", id, err)
		}
	}
	s.conns = map[string]*sql.DB{}
	return firstErr
}

func (s *Store) pathFor(sessionID string) string {
	return filepath.Join(s.root, sessionID+".sqlite")
}

// conn returns the cached sql.DB for a session, opening the file if
// we haven't seen it yet. The caller must hold s.mu.
func (s *Store) conn(sessionID string) (*sql.DB, error) {
	if db, ok := s.conns[sessionID]; ok {
		return db, nil
	}
	// modernc.org/sqlite uses the URL-style DSN. WAL + FK + busy
	// timeout are the reasonable defaults for a per-session store.
	dsn := "file:" + s.pathFor(sessionID) +
		"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", sessionID, err)
	}
	// SQLite is happy with a single connection per file for our
	// workload; keeping the pool narrow avoids WAL contention.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema %s: %w", sessionID, err)
	}
	s.conns[sessionID] = db
	return db, nil
}

// ---------------------------------------------------------------------
// OpenSession / CloseSession
// ---------------------------------------------------------------------

func (s *Store) OpenSession(ctx context.Context, sess core.Session) error {
	if sess.ID == "" {
		return errors.New("session id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	db, err := s.conn(sess.ID)
	if err != nil {
		return err
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO session (
			id, user_id, started_at, ended_at, workspace_id, cwd,
			git_branch, agent_kind, source_device
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			ended_at      = excluded.ended_at,
			workspace_id  = excluded.workspace_id,
			cwd           = excluded.cwd,
			git_branch    = excluded.git_branch,
			agent_kind    = excluded.agent_kind,
			source_device = excluded.source_device
	`,
		sess.ID, sess.UserID, sess.StartedAt.UnixMilli(),
		nullInt64Ms(sess.EndedAt),
		nullStringFromValue(sess.WorkspaceID),
		nullString(sess.Cwd), nullString(sess.GitBranch),
		nullString(sess.AgentKind), nullString(sess.SourceDevice),
	)
	return err
}

// MarkEnded stamps the session row's ended_at without closing the
// SQLite handle. Split out from CloseSession so SessionEnd can record
// the end timestamp before Consolidate runs — Consolidate ships the
// session row to chapterhouse and needs the ended_at to make the
// reconciler's `ended_at IS NOT NULL AND l1_embedding IS NULL`
// predicate fire. Idempotent: calling twice just overwrites with the
// later timestamp.
func (s *Store) MarkEnded(ctx context.Context, sessionID string, t time.Time) error {
	if sessionID == "" {
		return errors.New("session id required")
	}
	db, err := s.sessionConn(sessionID)
	if err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE session SET ended_at = ? WHERE id = ?`, t.UnixMilli(), sessionID,
	); err != nil {
		return fmt.Errorf("mark ended: %w", err)
	}
	return nil
}

// GetSession returns the session metadata row, including ended_at if
// MarkEnded has fired. Used by Consolidate so the chapterhouse upsert
// carries the same ended_at sietch already knows.
func (s *Store) GetSession(ctx context.Context, sessionID string) (core.Session, error) {
	if sessionID == "" {
		return core.Session{}, errors.New("session id required")
	}
	return s.readSessionRow(ctx, sessionID)
}

func (s *Store) CloseSession(ctx context.Context, sessionID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	db, ok := s.conns[sessionID]
	if !ok {
		// Nothing to close; session may have never been opened this
		// process or was already closed.
		return nil
	}
	// Belt-and-suspenders: ensure ended_at is populated even when the
	// caller skipped MarkEnded. Real call sites (SessionEnd) stamp via
	// MarkEnded first; this preserves the invariant for any other path
	// that closes a session directly.
	if _, err := db.ExecContext(ctx,
		`UPDATE session SET ended_at = COALESCE(ended_at, ?) WHERE id = ?`,
		time.Now().UnixMilli(), sessionID,
	); err != nil {
		return fmt.Errorf("mark ended: %w", err)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("close %s: %w", sessionID, err)
	}
	delete(s.conns, sessionID)
	return nil
}

// RemoveSession closes the cached handle and deletes the session's
// SQLite file plus its WAL/SHM siblings. Callers (Core.GCSession)
// guarantee the session is fully consolidated first — after that the
// file is a redundant local cache of what chapterhouse already holds.
// Idempotent: removing an absent session is a no-op.
func (s *Store) RemoveSession(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return errors.New("session id required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if db, ok := s.conns[sessionID]; ok {
		if err := db.Close(); err != nil {
			return fmt.Errorf("close %s: %w", sessionID, err)
		}
		delete(s.conns, sessionID)
	}
	base := s.pathFor(sessionID)
	for _, p := range []string{base, base + "-wal", base + "-shm"} {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", p, err)
		}
	}
	return nil
}

// ---------------------------------------------------------------------
// RecordEvent
// ---------------------------------------------------------------------

func (s *Store) RecordEvent(ctx context.Context, ev core.Event) (core.Event, error) {
	if ev.SessionID == "" {
		return core.Event{}, errors.New("event.session_id required")
	}
	if ev.ID == "" {
		return core.Event{}, errors.New("event.id required")
	}

	s.mu.Lock()
	db, err := s.conn(ev.SessionID)
	s.mu.Unlock()
	if err != nil {
		return core.Event{}, err
	}

	raw := []byte(ev.RawEvent)
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	entities, _ := json.Marshal(defaultStringSlice(ev.Entities))
	tags, _ := json.Marshal(defaultStringSlice(ev.Tags))
	embBlob := packFloat32(ev.Embedding)

	_, err = db.ExecContext(ctx, `
		INSERT INTO events (
			id, parent_id, session_id, user_id, request_id,
			type, role, text, tool_name, tool_use_id,
			tool_input, tool_output, bookmark_label,
			cwd, git_branch, agent_id, is_sidechain, model,
			raw_event, embedding, entities, tags, source_device, created_at
		) VALUES (?, ?, ?, ?, ?,
			?, ?, ?, ?, ?,
			?, ?, ?,
			?, ?, ?, ?, ?,
			?, ?, ?, ?, ?, ?)
	`,
		ev.ID, nullString(ev.ParentID), ev.SessionID, ev.UserID, nullString(ev.RequestID),
		ev.Type, nullString(ev.Role), nullString(ev.Text),
		nullString(ev.ToolName), nullString(ev.ToolUseID),
		nullJSON(ev.ToolInput), nullJSON(ev.ToolOutput),
		nullString(ev.BookmarkLabel),
		nullString(ev.Cwd), nullString(ev.GitBranch), nullString(ev.AgentID),
		boolToInt(ev.IsSidechain), nullString(ev.Model),
		string(raw), nullBlob(embBlob), string(entities), string(tags),
		nullString(ev.SourceDevice), ev.CreatedAt.UnixMilli(),
	)
	if err != nil {
		return core.Event{}, fmt.Errorf("insert event: %w", err)
	}

	if _, err := db.ExecContext(ctx,
		`UPDATE session SET event_count = event_count + 1, last_event_at = ? WHERE id = ?`,
		ev.CreatedAt.UnixMilli(), ev.SessionID,
	); err != nil {
		return core.Event{}, fmt.Errorf("bump event_count: %w", err)
	}

	return ev, nil
}

// ---------------------------------------------------------------------
// Bookmark / Navigate
// ---------------------------------------------------------------------

func (s *Store) SetBookmark(ctx context.Context, sessionID, eventID, label string) error {
	db, err := s.sessionConn(sessionID)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx,
		`UPDATE events SET bookmark_label = ? WHERE id = ?`, label, eventID)
	return err
}

func (s *Store) SetCurrent(ctx context.Context, sessionID, eventID string) error {
	db, err := s.sessionConn(sessionID)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx,
		`UPDATE session SET current_event_id = ? WHERE id = ?`, eventID, sessionID)
	return err
}

func (s *Store) CurrentEvent(ctx context.Context, sessionID string) (string, error) {
	db, err := s.sessionConn(sessionID)
	if err != nil {
		return "", err
	}
	var cur sql.NullString
	if err := db.QueryRowContext(ctx,
		`SELECT current_event_id FROM session WHERE id = ?`, sessionID,
	).Scan(&cur); err != nil {
		return "", err
	}
	return cur.String, nil
}

// ---------------------------------------------------------------------
// Search: vector + FTS
// ---------------------------------------------------------------------

func (s *Store) SearchVector(ctx context.Context, sessionID string, emb []float32, limit int) ([]core.RecallHit, error) {
	if limit <= 0 {
		limit = 10
	}
	if len(emb) == 0 {
		return nil, nil
	}

	db, err := s.sessionConn(sessionID)
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx, `
		SELECT id, COALESCE(text, ''), embedding
		FROM events
		WHERE embedding IS NOT NULL AND state = 'active'
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type candidate struct {
		id, text string
		sim      float64
	}
	var cands []candidate
	for rows.Next() {
		var id, text string
		var blob []byte
		if err := rows.Scan(&id, &text, &blob); err != nil {
			return nil, err
		}
		other := unpackFloat32(blob)
		if len(other) != len(emb) {
			continue
		}
		cands = append(cands, candidate{id, text, cosine(emb, other)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.Slice(cands, func(i, j int) bool { return cands[i].sim > cands[j].sim })
	if len(cands) > limit {
		cands = cands[:limit]
	}

	sid := sessionID
	out := make([]core.RecallHit, 0, len(cands))
	for _, c := range cands {
		out = append(out, core.RecallHit{
			Tier:      "working",
			ID:        c.id,
			Score:     c.sim,
			Content:   c.text,
			SessionID: &sid,
		})
	}
	return out, nil
}

func (s *Store) SearchFTS(ctx context.Context, sessionID, text string, limit int) ([]core.RecallHit, error) {
	if limit <= 0 {
		limit = 10
	}
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	db, err := s.sessionConn(sessionID)
	if err != nil {
		return nil, err
	}

	// bm25(events_fts) returns a lower-is-better score; flip sign so
	// downstream merge logic (higher = better) aligns with the vector
	// pathway.
	rows, err := db.QueryContext(ctx, `
		SELECT e.id, COALESCE(e.text, ''), -bm25(events_fts)
		FROM events_fts
		JOIN events e ON e.rowid = events_fts.rowid
		WHERE events_fts MATCH ? AND e.state = 'active'
		ORDER BY bm25(events_fts)
		LIMIT ?
	`, ftsQuery(text), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	sid := sessionID
	var out []core.RecallHit
	for rows.Next() {
		var id, t string
		var score float64
		if err := rows.Scan(&id, &t, &score); err != nil {
			return nil, err
		}
		out = append(out, core.RecallHit{
			Tier: "working", ID: id, Score: score, Content: t, SessionID: &sid,
		})
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------
// Sessions list / soft forget / watermark / pending
// ---------------------------------------------------------------------

// ActiveSessionIDs returns the ids of every session whose file is
// present under the store root, regardless of user. Pipeline A uses
// this to know which sessions to tick through on each consolidation
// pass.
func (s *Store) ActiveSessionIDs(_ context.Context) ([]string, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read sessions dir: %w", err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sqlite") {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".sqlite"))
	}
	sort.Strings(out)
	return out, nil
}

func (s *Store) ListSessions(ctx context.Context, userID string) ([]core.Session, error) {
	entries, err := os.ReadDir(s.root)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read sessions dir: %w", err)
	}

	var sessions []core.Session
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sqlite") {
			continue
		}
		sessionID := strings.TrimSuffix(e.Name(), ".sqlite")
		sess, err := s.readSessionRow(ctx, sessionID)
		if err != nil {
			continue // skip unreadable sessions; don't fail the list
		}
		if sess.UserID != userID {
			continue
		}
		sessions = append(sessions, sess)
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].StartedAt.After(sessions[j].StartedAt)
	})
	return sessions, nil
}

func (s *Store) readSessionRow(ctx context.Context, sessionID string) (core.Session, error) {
	db, err := s.sessionConn(sessionID)
	if err != nil {
		return core.Session{}, err
	}
	var (
		id, uid string
		started, ended, lastEvent sql.NullInt64
		eventCount                int
		summary, workspaceID, cwd, gitBranch, agentKind, sourceDevice sql.NullString
	)
	err = db.QueryRowContext(ctx, `
		SELECT id, user_id, started_at, ended_at, last_event_at, event_count,
		       summary, workspace_id, cwd, git_branch, agent_kind, source_device
		FROM session WHERE id = ?
	`, sessionID).Scan(&id, &uid, &started, &ended, &lastEvent, &eventCount,
		&summary, &workspaceID, &cwd, &gitBranch, &agentKind, &sourceDevice)
	if errors.Is(err, sql.ErrNoRows) {
		// File exists (conn() create-on-open) but no session row — an
		// orphan. Surface a typed sentinel so GCSession can recognize it.
		return core.Session{}, fmt.Errorf("%w (session=%q)", core.ErrSessionNotFound, sessionID)
	}
	if err != nil {
		return core.Session{}, err
	}
	sess := core.Session{
		ID:           id,
		UserID:       uid,
		EventCount:   eventCount,
		Summary:      stringPtr(summary),
		WorkspaceID:  workspaceID.String,
		Cwd:          stringPtr(cwd),
		GitBranch:    stringPtr(gitBranch),
		AgentKind:    stringPtr(agentKind),
		SourceDevice: stringPtr(sourceDevice),
	}
	if started.Valid {
		sess.StartedAt = time.UnixMilli(started.Int64).UTC()
	}
	if ended.Valid {
		t := time.UnixMilli(ended.Int64).UTC()
		sess.EndedAt = &t
	}
	if lastEvent.Valid {
		t := time.UnixMilli(lastEvent.Int64).UTC()
		sess.LastEventAt = &t
	}
	return sess, nil
}

func (s *Store) SoftForget(ctx context.Context, sessionID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	db, err := s.sessionConn(sessionID)
	if err != nil {
		return err
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	_, err = db.ExecContext(ctx, fmt.Sprintf(`
		UPDATE events
		SET state = 'forgotten', text = '[forgotten]', embedding = NULL
		WHERE id IN (%s)
	`, strings.Join(placeholders, ",")), args...)
	return err
}

func (s *Store) Watermark(ctx context.Context, sessionID string) (string, error) {
	db, err := s.sessionConn(sessionID)
	if err != nil {
		return "", err
	}
	var w sql.NullString
	err = db.QueryRowContext(ctx,
		`SELECT watermark_event_id FROM session WHERE id = ?`, sessionID,
	).Scan(&w)
	if errors.Is(err, sql.ErrNoRows) {
		// Same orphan case as readSessionRow: schema-only file, no row.
		return "", fmt.Errorf("%w (session=%q)", core.ErrSessionNotFound, sessionID)
	}
	if err != nil {
		return "", err
	}
	return w.String, nil
}

func (s *Store) SetWatermark(ctx context.Context, sessionID, eventID string) error {
	db, err := s.sessionConn(sessionID)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx,
		`UPDATE session SET watermark_event_id = ? WHERE id = ?`, eventID, sessionID)
	return err
}

// PendingEvents returns events created strictly after `afterEventID`
// (or every active event when afterEventID is empty) in id order.
//
// Event ids are ULIDs packed into the UUID string format (see
// core.NewID), so id comparison and id ordering are equivalent to
// chronological comparison and chronological ordering — no rowid /
// timestamp trickery needed. Empty afterEventID compares less than
// any real id, so the "no watermark yet" path returns everything.
func (s *Store) PendingEvents(ctx context.Context, sessionID, afterEventID string) ([]core.Event, error) {
	db, err := s.sessionConn(sessionID)
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx, `
		SELECT id, parent_id, session_id, user_id, request_id,
		       type, role, text, tool_name, tool_use_id,
		       tool_input, tool_output, bookmark_label,
		       cwd, git_branch, agent_id, is_sidechain, model,
		       raw_event, embedding, entities, tags, source_device, created_at
		FROM events
		WHERE id > ? AND state = 'active'
		ORDER BY id ASC
	`, afterEventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []core.Event
	for rows.Next() {
		var ev core.Event
		var (
			parentID, requestID, role, text, toolName, toolUseID,
			bookmarkLabel, cwd, gitBranch, agentID, model, sourceDevice sql.NullString
			toolInput, toolOutput, rawEvent                             sql.NullString
			entities, tags                                              string
			emb                                                         []byte
			isSidechain                                                 int
			createdAtMs                                                 int64
		)
		if err := rows.Scan(
			&ev.ID, &parentID, &ev.SessionID, &ev.UserID, &requestID,
			&ev.Type, &role, &text, &toolName, &toolUseID,
			&toolInput, &toolOutput, &bookmarkLabel,
			&cwd, &gitBranch, &agentID, &isSidechain, &model,
			&rawEvent, &emb, &entities, &tags, &sourceDevice, &createdAtMs,
		); err != nil {
			return nil, err
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
		ev.CreatedAt = time.UnixMilli(createdAtMs).UTC()
		out = append(out, ev)
	}
	return out, rows.Err()
}

// EventsNeedingEmbedding returns active events that have text but no
// embedding yet — i.e. events recorded while the embedder was
// unreachable — in id (chronological) order. Only ID, SessionID,
// UserID and Text are populated; that is all the backfill pass needs.
func (s *Store) EventsNeedingEmbedding(ctx context.Context, sessionID string, limit int) ([]core.Event, error) {
	if limit <= 0 {
		limit = 64
	}
	db, err := s.sessionConn(sessionID)
	if err != nil {
		return nil, err
	}

	rows, err := db.QueryContext(ctx, `
		SELECT id, session_id, user_id, text
		FROM events
		WHERE state = 'active' AND embedding IS NULL
		  AND text IS NOT NULL AND text != ''
		ORDER BY id ASC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []core.Event
	for rows.Next() {
		var ev core.Event
		var text sql.NullString
		if err := rows.Scan(&ev.ID, &ev.SessionID, &ev.UserID, &text); err != nil {
			return nil, err
		}
		ev.Text = stringPtr(text)
		out = append(out, ev)
	}
	return out, rows.Err()
}

// SetEmbedding backfills the embedding for one event. The state guard
// keeps a concurrent forget from being resurrected by a late backfill.
func (s *Store) SetEmbedding(ctx context.Context, sessionID, eventID string, emb []float32) error {
	db, err := s.sessionConn(sessionID)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx,
		`UPDATE events SET embedding = ? WHERE id = ? AND state = 'active'`,
		nullBlob(packFloat32(emb)), eventID)
	return err
}

// ---------------------------------------------------------------------
// Internals
// ---------------------------------------------------------------------

func (s *Store) sessionConn(sessionID string) (*sql.DB, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conn(sessionID)
}

// packFloat32 serializes a []float32 as little-endian 4-byte floats
// back-to-back. Round-trips via unpackFloat32.
func packFloat32(v []float32) []byte {
	if len(v) == 0 {
		return nil
	}
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

func unpackFloat32(b []byte) []float32 {
	if len(b) == 0 || len(b)%4 != 0 {
		return nil
	}
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}

// cosine returns the cosine similarity of two same-length vectors.
func cosine(a, b []float32) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		da, db := float64(a[i]), float64(b[i])
		dot += da * db
		na += da * da
		nb += db * db
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// ftsQuery sanitizes a plain-language query into a safe FTS5 query
// string by tokenizing on whitespace and wrapping each token in
// double quotes. Avoids accidental operator interpretation.
func ftsQuery(q string) string {
	fields := strings.Fields(q)
	for i, f := range fields {
		fields[i] = `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
	}
	return strings.Join(fields, " ")
}

func nullString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// nullStringFromValue maps a value-typed string to SQL NULL when empty.
// Used for workspace_id where a zero value must round-trip as NULL
// rather than the literal empty string, so `WHERE workspace_id IS NULL`
// queries read consistently across callers.
func nullStringFromValue(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt64Ms(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}

func nullJSON(r json.RawMessage) any {
	if len(r) == 0 {
		return nil
	}
	return string(r)
}

func nullBlob(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func defaultStringSlice(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func stringPtr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	out := s.String
	return &out
}
