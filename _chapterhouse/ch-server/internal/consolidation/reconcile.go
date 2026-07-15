package consolidation

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
)

// SessionPooler is the seam the reconcile step depends on. *semantic.Writer
// satisfies it via PoolSessionToL1 (workspaceID, sessionID). Tests inject a
// fake so reconcile runs without a live mentat.
type SessionPooler interface {
	PoolSessionToL1(ctx context.Context, workspaceID, sessionID uuid.UUID) error
}

// Reconcile pools every closed session missing an L1 embedding (up to
// limit) so the cluster step sees fresh session vectors. Idempotent: the
// underlying UpdateSessionL1 overwrites. One failing session is logged
// and skipped — it must not starve the rest. Returns the count pooled OK.
func Reconcile(ctx context.Context, repo *repository.Repository, pooler SessionPooler, limit int) (int, error) {
	sessions, err := repo.ClosedSessionsMissingL1(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("closed sessions missing l1: %w", err)
	}
	ok := 0
	for _, s := range sessions {
		if err := pooler.PoolSessionToL1(ctx, s.WorkspaceID, s.ID); err != nil {
			slog.Warn("consolidation.reconcile: pool failed",
				"session_id", s.ID, "workspace_id", s.WorkspaceID, "error", err.Error())
			continue
		}
		ok++
	}
	return ok, nil
}
