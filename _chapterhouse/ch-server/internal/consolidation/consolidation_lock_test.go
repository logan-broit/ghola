package consolidation_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/consolidation"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
)

// TestRunWorkspace_BusyLock_ReturnsErrConsolidationBusy pins Finding 2's
// concurrency guard: when a workspace's advisory lock is already held (a
// concurrent run in flight), RunWorkspace must decline with
// ErrConsolidationBusy rather than proceed and risk duplicate mnemes /
// double reinforcement. The test holds the lock itself on a dedicated
// connection, then asserts the run backs off.
func TestRunWorkspace_BusyLock_ReturnsErrConsolidationBusy(t *testing.T) {
	repo := newSemRepo(t)
	ctx := context.Background()
	ws := uuid.New()

	// Hold the workspace's advisory lock on a dedicated pooled connection
	// so RunWorkspace's pg_try_advisory_lock (on a different connection)
	// fails. Keep the connection out of the pool for the whole test.
	conn, err := repo.Pool().Acquire(ctx)
	require.NoError(t, err)
	defer conn.Release()
	var locked bool
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock($1)`, repository.AdvisoryLockKey(ws)).Scan(&locked))
	require.True(t, locked, "precondition: the test must hold the workspace lock")

	d := consolidation.Deps{Repo: repo, Pooler: &fakePooler{}, Logger: discardLogger()}
	err = consolidation.RunWorkspace(ctx, d, ws)
	require.ErrorIs(t, err, consolidation.ErrConsolidationBusy)
}

// TestRunWorkspace_ReleasesLockOnCompletion pins the release-on-exit half
// of the guard: a completed run must free the advisory lock so a
// subsequent run (nightly tick or manual trigger) can acquire it. An empty
// workspace no-ops after reconcile, exercising the acquire/release seam
// without needing mentat.
func TestRunWorkspace_ReleasesLockOnCompletion(t *testing.T) {
	repo := newSemRepo(t)
	ctx := context.Background()
	ws := uuid.New()
	d := consolidation.Deps{Repo: repo, Pooler: &fakePooler{}, Logger: discardLogger()}

	require.NoError(t, consolidation.RunWorkspace(ctx, d, ws))
	// If the first run leaked the lock, this second run would see it held
	// and return ErrConsolidationBusy. It must acquire cleanly instead.
	err := consolidation.RunWorkspace(ctx, d, ws)
	require.NoError(t, err)
	require.NotErrorIs(t, err, consolidation.ErrConsolidationBusy)
}
