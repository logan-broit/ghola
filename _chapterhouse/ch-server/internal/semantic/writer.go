// Package semantic is the v0.3 replacement for internal/replay. It owns
// the session -> L1 write path (PR1.7), the recall read path (PR1.8),
// and (later PRs) clustering + training orchestration.
//
// The package deliberately stays small: Writer takes a concrete
// *repository.Repository and *mentat.Client directly rather than
// behind interfaces, because the e2e test in PR1.9 exercises the real
// path end-to-end and we don't want a "Pooler interface" to grow just
// to support unit-level mocking.
package semantic

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/thinkwright/chapterhouse/ch-server/internal/mentat"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
)

// Writer pools a session's events into a single L1 embedding via
// mentat and writes the result to episodic.sessions.l1_embedding.
type Writer struct {
	repo *repository.Repository
	m    *mentat.Client
}

// NewWriter constructs a Writer. Both arguments are required; nil
// values are caller errors and will surface as nil-deref at first use.
func NewWriter(repo *repository.Repository, m *mentat.Client) *Writer {
	return &Writer{repo: repo, m: m}
}

// PoolSessionToL1 fetches a session's embedded events, pools them via
// mentat, and writes the pooled vector to episodic.sessions.l1_embedding.
//
// Idempotent by construction: re-running overwrites with the current
// pooler's output. Sessions with zero embedded events are skipped
// silently — there is nothing to pool, and leaving l1_embedding NULL
// keeps the partial HNSW index honest.
func (w *Writer) PoolSessionToL1(ctx context.Context, workspaceID, sessionID uuid.UUID) error {
	events, err := w.repo.SessionEvents(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session events: %w", err)
	}
	if len(events) == 0 {
		return nil
	}

	req := mentat.PoolRequest{
		WorkspaceID: workspaceID,
		Events:      make([]mentat.Event, len(events)),
	}
	for i, e := range events {
		req.Events[i] = mentat.Event{Type: e.Type, Embedding: e.Embedding}
	}

	resp, err := w.m.Pool(ctx, req)
	if err != nil {
		return fmt.Errorf("mentat pool: %w", err)
	}
	if err := w.repo.UpdateSessionL1Embedding(ctx, sessionID, resp.Embedding); err != nil {
		return fmt.Errorf("update l1: %w", err)
	}
	return nil
}
