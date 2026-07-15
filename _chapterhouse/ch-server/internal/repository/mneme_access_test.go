package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"
)

// TestTouchMnemes_BumpsAccess pins the HOLA weak-label write: TouchMnemes
// increments access_count and advances last_access. last_access defaults
// to now() at insert, so the row is pre-aged to a fixed past timestamp to
// make the advance unambiguous. Empty ids must be a clean no-op.
func TestTouchMnemes_BumpsAccess(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "8")
	require.NoError(t, repository.ApplyMigrations(context.Background(), pg.Pool))
	repo := repository.New(pg.Pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	workspace := uuid.New()
	var id uuid.UUID
	require.NoError(t, pg.Pool.QueryRow(ctx, `
		INSERT INTO semantic.mnemes (workspace_id, level, embedding)
		VALUES ($1, 1, ($2::text)::vector)
		RETURNING id`, workspace, "[1,0,0,0,0,0,0,0]").Scan(&id))

	// Pre-age last_access so the post-touch advance is unmistakable.
	_, err := pg.Pool.Exec(ctx,
		`UPDATE semantic.mnemes SET last_access = '2000-01-01T00:00:00Z' WHERE id = $1`, id)
	require.NoError(t, err)

	var beforeCount int
	var beforeAccess time.Time
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT access_count, last_access FROM semantic.mnemes WHERE id = $1`, id).
		Scan(&beforeCount, &beforeAccess))
	assert.Equal(t, 0, beforeCount)

	require.NoError(t, repo.TouchMnemes(ctx, []uuid.UUID{id}))

	var afterCount int
	var afterAccess time.Time
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT access_count, last_access FROM semantic.mnemes WHERE id = $1`, id).
		Scan(&afterCount, &afterAccess))
	assert.Equal(t, 1, afterCount, "access_count bumped by one")
	assert.True(t, afterAccess.After(beforeAccess), "last_access advanced from the pre-aged timestamp")

	// Empty ids is a no-op: no error, no phantom bump.
	require.NoError(t, repo.TouchMnemes(ctx, nil))
	var stillOne int
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT access_count FROM semantic.mnemes WHERE id = $1`, id).Scan(&stillOne))
	assert.Equal(t, 1, stillOne, "empty ids must not bump anything")
}
