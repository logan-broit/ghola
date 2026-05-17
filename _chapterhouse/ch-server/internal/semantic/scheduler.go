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

// tick clusters each configured workspace once. Errors on individual
// workspaces are logged but don't abort the batch — one slow or
// failing workspace shouldn't block the others. Use a per-call
// context with a generous timeout: clustering 20k sessions takes
// tens of seconds, but no individual call should exceed a few minutes.
func (s *Scheduler) tick(ctx context.Context) {
	for _, ws := range s.workspaces {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		resp, err := s.client.Cluster(callCtx, mentat.ClusterRequest{
			WorkspaceID:    ws,
			MinClusterSize: 3,
		})
		cancel()
		if err != nil {
			s.logger.Warn("semantic.scheduler: cluster failed",
				"workspace_id", ws.String(),
				"err", err.Error())
			continue
		}
		s.logger.Info("semantic.scheduler: clustered",
			"workspace_id", ws.String(),
			"n_sessions", resp.NSessions,
			"n_clusters", resp.NClusters,
			"n_outliers", resp.NOutliers,
			"upserted_mnemes", resp.UpsertedMnemes)
	}
}
