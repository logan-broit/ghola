package semantic

import (
	"context"
	"log/slog"
	"time"
)

// Reconciler periodically scans for closed sessions whose l1_embedding
// is still NULL and pools them through the Writer.
//
// We use a reconciler instead of a session-close hook because ghola's
// daemon HTTP API is an invariant of the design — chapterhouse can't
// register a callback there. Chapterhouse already owns episodic.sessions
// and sees writes land, so polling is local and idempotent.
type Reconciler struct {
	writer *Writer
	every  time.Duration
	batch  int
	logger *slog.Logger
}

// NewReconciler constructs a Reconciler with sensible defaults. A
// non-positive `every` falls back to 30s; a nil logger falls back to
// slog.Default(). The batch size is fixed at 32, which keeps each tick
// short while still draining a backlog within a few minutes.
func NewReconciler(w *Writer, every time.Duration, logger *slog.Logger) *Reconciler {
	if every <= 0 {
		every = 30 * time.Second
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Reconciler{writer: w, every: every, batch: 32, logger: logger}
}

// Run blocks until ctx is cancelled, ticking every Reconciler.every.
// Returns ctx.Err() on shutdown so callers can distinguish clean
// cancellation from unexpected exits.
func (r *Reconciler) Run(ctx context.Context) error {
	t := time.NewTicker(r.every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			r.tick(ctx)
		}
	}
}

// tick scans for a batch of closed sessions missing an L1 embedding
// and pools each one. Errors on individual sessions are logged but do
// not abort the batch — one bad session shouldn't starve the rest.
func (r *Reconciler) tick(ctx context.Context) {
	sessions, err := r.writer.repo.ClosedSessionsMissingL1(ctx, r.batch)
	if err != nil {
		r.logger.Warn("semantic: closed-sessions query failed", "err", err.Error())
		return
	}
	for _, s := range sessions {
		if err := r.writer.PoolSessionToL1(ctx, s.WorkspaceID, s.ID); err != nil {
			r.logger.Warn("semantic: pool failed",
				"session_id", s.ID,
				"workspace_id", s.WorkspaceID,
				"err", err.Error())
		}
	}
}
