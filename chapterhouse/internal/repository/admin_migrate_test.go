package repository_test

import (
	"testing"

	"github.com/logan-broit/ghola/chapterhouse/internal/repository"
	"github.com/logan-broit/ghola/chapterhouse/internal/testutil"
	"github.com/stretchr/testify/require"
)

// TestAdminMigrationsFreshAndRerun applies the admin schema next to the
// episodic one, then reruns both: ch-init runs on every compose up.
func TestAdminMigrationsFreshAndRerun(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	ctx := t.Context()
	t.Setenv("EMBEDDING_DIM", "1024")

	for range 2 {
		require.NoError(t, repository.ApplyMigrations(ctx, pg.Pool))
		require.NoError(t, repository.ApplyAdminMigrations(ctx, pg.Pool))
	}

	var n int
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM _migrations WHERE applied LIKE 'admin/%'`).Scan(&n))
	require.Equal(t, 8, n)
	_, err := pg.Pool.Exec(ctx, `SELECT 1 FROM api_keys, admin_sessions, audit_log LIMIT 1`)
	require.NoError(t, err)
}

// TestAdminMigrationsBaselineManual covers a database where the RUNBOOK's
// old psql loop already created the schema: nothing reruns, all recorded.
func TestAdminMigrationsBaselineManual(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	ctx := t.Context()

	_, err := pg.Pool.Exec(ctx, `CREATE TABLE api_keys (id int)`)
	require.NoError(t, err)
	require.NoError(t, repository.ApplyAdminMigrations(ctx, pg.Pool))

	var n int
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM _migrations WHERE applied LIKE 'admin/%'`).Scan(&n))
	require.Equal(t, 8, n)
	var users bool
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT to_regclass('public.users') IS NOT NULL`).Scan(&users))
	require.False(t, users, "baseline must not run the files")
}
