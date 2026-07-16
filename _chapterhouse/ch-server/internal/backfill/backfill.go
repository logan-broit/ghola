// Package backfill segments an over-long episodic session into the
// natural conversation episodes it should have been. A single session
// row that accreted weeks of turns (because nothing on the MCP path
// ever closed it) is split on idle gaps so consolidation sees real
// episode boundaries. One-time, operator-run; see cmd/backfill-sessions.
package backfill

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// evt is the minimal event projection the segmenter needs. IDs are kept
// as strings and cast to uuid in SQL to avoid pgx uuid-type wiring.
type evt struct {
	ID        string
	CreatedAt time.Time
}

// Segment is a contiguous run of events with no internal gap larger than
// the threshold. IsOriginal marks the final segment, which keeps the
// original session id; every earlier segment gets a fresh NewID.
type Segment struct {
	NewID      string
	IsOriginal bool
	EventIDs   []string
	StartedAt  time.Time
	EndedAt    time.Time
}

// Plan is the full segmentation of one session plus the restore backup.
type Plan struct {
	SessionID string
	Segments  []Segment
	Backup    map[string]string // event_id -> old session_id
}

// segment splits events (assumed sorted by created_at, id) on gaps
// strictly greater than gap. Pure: no DB, no clock, no IO.
func segment(events []evt, gap time.Duration) [][]evt {
	if len(events) == 0 {
		return nil
	}
	var out [][]evt
	cur := []evt{events[0]}
	for i := 1; i < len(events); i++ {
		if events[i].CreatedAt.Sub(events[i-1].CreatedAt) > gap {
			out = append(out, cur)
			cur = nil
		}
		cur = append(cur, events[i])
	}
	return append(out, cur)
}

// BuildPlan reads the session's events and computes its segments.
func BuildPlan(ctx context.Context, pool *pgxpool.Pool, sessionID string, gap time.Duration) (Plan, error) {
	if _, err := uuid.Parse(sessionID); err != nil {
		return Plan{}, fmt.Errorf("session id must be a uuid: %w", err)
	}
	rows, err := pool.Query(ctx,
		`SELECT id::text, created_at FROM episodic.events
		  WHERE session_id = $1::uuid ORDER BY created_at, id`, sessionID)
	if err != nil {
		return Plan{}, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	var events []evt
	backup := map[string]string{}
	for rows.Next() {
		var e evt
		if err := rows.Scan(&e.ID, &e.CreatedAt); err != nil {
			return Plan{}, fmt.Errorf("scan event: %w", err)
		}
		events = append(events, e)
		backup[e.ID] = sessionID
	}
	if err := rows.Err(); err != nil {
		return Plan{}, fmt.Errorf("iterate events: %w", err)
	}

	groups := segment(events, gap)
	plan := Plan{SessionID: sessionID, Backup: backup}
	for i, g := range groups {
		seg := Segment{
			IsOriginal: i == len(groups)-1,
			StartedAt:  g[0].CreatedAt,
			EndedAt:    g[len(g)-1].CreatedAt,
		}
		for _, e := range g {
			seg.EventIDs = append(seg.EventIDs, e.ID)
		}
		if seg.IsOriginal {
			seg.NewID = sessionID
		} else {
			seg.NewID = uuid.NewString()
		}
		plan.Segments = append(plan.Segments, seg)
	}
	return plan, nil
}

// String renders the dry-run view: segment count, per-segment event
// count and time span.
func (p Plan) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "session %s -> %d segment(s):\n", p.SessionID, len(p.Segments))
	for i, s := range p.Segments {
		tag := "new     "
		if s.IsOriginal {
			tag = "original"
		}
		fmt.Fprintf(&b, "  [%d] %s id=%s events=%d span=%s..%s\n",
			i, tag, s.NewID, len(s.EventIDs),
			s.StartedAt.Format(time.RFC3339), s.EndedAt.Format(time.RFC3339))
	}
	return b.String()
}

// Execute writes the backup, then applies the plan in one transaction:
// new session rows for non-final segments, events re-keyed,
// session_workspaces mirrored, every segment closed, l1_embedding
// cleared on the original, and (when non-empty) the orphan session
// closed.
func Execute(ctx context.Context, pool *pgxpool.Pool, p Plan, backupPath, orphanSessionID string) error {
	if backupPath == "" {
		return errors.New("backup path required")
	}
	// Write the restore backup BEFORE mutating anything. fsync explicitly
	// (rather than os.WriteFile) so "backup before mutate" is actually
	// crash-durable: a bare write can sit in the page cache and vanish on
	// a crash between this call and the transaction below, leaving no
	// restore key for a mutation that DID land.
	raw, err := json.MarshalIndent(p.Backup, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal backup: %w", err)
	}
	f, err := os.OpenFile(backupPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create backup %s: %w", backupPath, err)
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return fmt.Errorf("write backup %s: %w", backupPath, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("fsync backup %s: %w", backupPath, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close backup %s: %w", backupPath, err)
	}

	// Source the original session's metadata + its workspaces to copy
	// onto the new segment rows.
	var userID string
	var cwd, gitBranch, agentKind, sourceDevice *string
	if err := pool.QueryRow(ctx,
		`SELECT user_id::text, cwd, git_branch, agent_kind, source_device
		   FROM episodic.sessions WHERE id = $1::uuid`, p.SessionID).
		Scan(&userID, &cwd, &gitBranch, &agentKind, &sourceDevice); err != nil {
		return fmt.Errorf("read original session %s: %w", p.SessionID, err)
	}
	wsRows, err := pool.Query(ctx,
		`SELECT workspace_id::text FROM episodic.session_workspaces WHERE session_id = $1::uuid`, p.SessionID)
	if err != nil {
		return fmt.Errorf("read workspaces: %w", err)
	}
	var workspaces []string
	for wsRows.Next() {
		var w string
		if err := wsRows.Scan(&w); err != nil {
			wsRows.Close()
			return fmt.Errorf("scan workspace: %w", err)
		}
		workspaces = append(workspaces, w)
	}
	wsRows.Close()
	if err := wsRows.Err(); err != nil {
		return fmt.Errorf("iterate workspaces: %w", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	for _, seg := range p.Segments {
		if seg.IsOriginal {
			// Keep the id; restate the span + count and clear
			// l1_embedding so the reconciler re-pools the (now smaller)
			// final segment. Its events already carry this id.
			if _, err := tx.Exec(ctx, `
				UPDATE episodic.sessions
				   SET started_at = $2, ended_at = $3, event_count = $4, l1_embedding = NULL
				 WHERE id = $1::uuid`,
				seg.NewID, seg.StartedAt, seg.EndedAt, len(seg.EventIDs)); err != nil {
				return fmt.Errorf("update original session: %w", err)
			}
			continue
		}
		// New closed session row copying the original's metadata.
		if _, err := tx.Exec(ctx, `
			INSERT INTO episodic.sessions
			  (id, user_id, started_at, ended_at, event_count, cwd, git_branch, agent_kind, source_device)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9)`,
			seg.NewID, userID, seg.StartedAt, seg.EndedAt, len(seg.EventIDs),
			cwd, gitBranch, agentKind, sourceDevice); err != nil {
			return fmt.Errorf("insert segment session: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`UPDATE episodic.events SET session_id = $1::uuid WHERE id = ANY($2::uuid[])`,
			seg.NewID, seg.EventIDs); err != nil {
			return fmt.Errorf("re-key events: %w", err)
		}
		for _, w := range workspaces {
			if _, err := tx.Exec(ctx,
				`INSERT INTO episodic.session_workspaces (session_id, workspace_id)
				 VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING`,
				seg.NewID, w); err != nil {
				return fmt.Errorf("mirror workspace: %w", err)
			}
		}
	}

	// Close the orphaned second open session, if one was supplied.
	if orphanSessionID != "" {
		if _, err := tx.Exec(ctx, `
			UPDATE episodic.sessions
			   SET ended_at = COALESCE(
			       (SELECT max(created_at) FROM episodic.events WHERE session_id = $1::uuid), now())
			 WHERE id = $1::uuid AND ended_at IS NULL`, orphanSessionID); err != nil {
			return fmt.Errorf("close orphan session: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
