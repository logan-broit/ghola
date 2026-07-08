package semantic

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/thinkwright/chapterhouse/ch-server/internal/mentat"
)

// Scheduler periodically calls mentat's /v1/cluster for each
// configured workspace and lets mentat upsert mnemes. Production
// default is daily at 02:00; dev runs can dial Interval down to 1m
// to iterate.
//
// Workspaces are explicit because chapterhouse doesn't track them on
// session rows today (the workspace_id is implicit per deployment in
// v0.4). Operators configure the list via env or wire-up — the
// scheduler keeps no opinion on how the list got built.
type Scheduler struct {
	client     *mentat.Client
	workspaces []uuid.UUID
	interval   time.Duration
	logger     *slog.Logger
}

// NewScheduler constructs a Scheduler. interval <= 0 falls back to
// 24h; nil logger falls back to slog.Default(). Empty workspaces
// list is allowed (the loop will do nothing every tick) so deployments
// without clustering needs can leave it unset.
func NewScheduler(c *mentat.Client, workspaces []uuid.UUID, interval time.Duration, logger *slog.Logger) *Scheduler {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{
		client:     c,
		workspaces: workspaces,
		interval:   interval,
		logger:     logger,
	}
}

// Run blocks until ctx is cancelled. Returns ctx.Err() on shutdown.
// First tick fires immediately so dev loops don't have to wait the
// full interval to see clustering happen.
func (s *Scheduler) Run(ctx context.Context) error {
	if len(s.workspaces) == 0 {
		s.logger.Info("semantic.scheduler: no workspaces configured; idle")
	}
	s.tick(ctx)
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			s.tick(ctx)
		}
	}
}

// tick is retired: mentat no longer clusters-by-workspace (it is now a
// pure math kernel taking caller-supplied embeddings, not a DSN). The
// consolidation worker owns the reconcile -> cluster -> upsert pipeline.
// This body is neutered so the tree stays buildable between here and the
// compose-cleanup task, which removes the Scheduler type entirely.
func (s *Scheduler) tick(_ context.Context) {
	s.logger.Info("semantic.scheduler: retired — consolidation worker owns clustering")
}
