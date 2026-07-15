package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"
)

// newAdvisoryLockRepo boots a fresh ephemeral Postgres + applies
// migrations, mirroring the pattern in associations_test.go /
// mnemes_write_test.go. Each test gets its own DB.
func newAdvisoryLockRepo(t *testing.T) *repository.Repository {
	t.Helper()
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "1024")
	require.NoError(t, repository.ApplyMigrations(context.Background(), pg.Pool))
	return repository.New(pg.Pool)
}

// TestTryWorkspaceConsolidationLock_AcquireIsBounded pins the pool-Acquire
// hardening: the manual-trigger caller hands TryWorkspaceConsolidationLock
// a context.WithoutCancel'd, deadline-free context (handler/semantic.go's
// Consolidate), so pool exhaustion must fail fast with a bounded, retryable
// error rather than blocking the acquire forever. A dedicated MaxConns=1
// pool against the same DB, with its only connection held externally,
// deterministically exhausts the pool.
func TestTryWorkspaceConsolidationLock_AcquireIsBounded(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "1024")
	require.NoError(t, repository.ApplyMigrations(context.Background(), pg.Pool))

	poolConfig, err := pgxpool.ParseConfig(pg.DSN)
	require.NoError(t, err)
	poolConfig.MaxConns = 1
	tinyPool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)
	require.NoError(t, err)
	defer tinyPool.Close()

	held, err := tinyPool.Acquire(context.Background())
	require.NoError(t, err)
	defer held.Release()

	repo := repository.New(tinyPool)
	ws := uuid.New()

	start := time.Now()
	// Background, no deadline — mirrors the manual-trigger caller's
	// context.WithoutCancel(r.Context()). Without the bound this would
	// block until the test timeout; with it, it must return quickly.
	_, acquired, err := repo.TryWorkspaceConsolidationLock(context.Background(), ws)
	elapsed := time.Since(start)

	require.False(t, acquired)
	require.Error(t, err, "pool exhaustion must surface as an error, not a hang")
	require.Less(t, elapsed, 10*time.Second, "acquire must be bounded, not unbounded")
}

// TestTryWorkspaceConsolidationLock_ReleaseSurvivesDeadConnection extends
// the release-on-completion coverage (consolidation_lock_test.go's
// TestRunWorkspace_ReleasesLockOnCompletion) to the failure path: if the
// connection holding the lock dies out from under release() (forcing
// pg_advisory_unlock to error), release() must not panic or hang, and the
// workspace's lock must be immediately acquirable again — no stuck lock,
// no waiting on the pool's health-check/lifetime recycling.
func TestTryWorkspaceConsolidationLock_ReleaseSurvivesDeadConnection(t *testing.T) {
	repo := newAdvisoryLockRepo(t)
	ctx := context.Background()
	ws := uuid.New()

	release, acquired, err := repo.TryWorkspaceConsolidationLock(ctx, ws)
	require.NoError(t, err)
	require.True(t, acquired)

	// Kill the backend holding the lock's connection out from under
	// release(), so the upcoming pg_advisory_unlock exec fails.
	var pid int
	require.NoError(t, repo.Pool().QueryRow(ctx,
		`SELECT pid FROM pg_locks WHERE locktype = 'advisory' LIMIT 1`).Scan(&pid))
	_, err = repo.Pool().Exec(ctx, `SELECT pg_terminate_backend($1)`, pid)
	require.NoError(t, err)

	require.NotPanics(t, func() { release() })

	release2, acquired2, err := repo.TryWorkspaceConsolidationLock(ctx, ws)
	require.NoError(t, err)
	require.True(t, acquired2, "workspace lock must be acquirable immediately after a dead-connection release")
	release2()
}
