package repository_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"
)

func seedSession(t *testing.T, pool *pgxpool.Pool, userID, sessionID uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `
		INSERT INTO episodic.sessions (id, user_id, started_at, event_count)
		VALUES ($1, $2, now(), 0)`, sessionID, userID)
	require.NoError(t, err)
}

func TestAddSessionWorkspace_Happy(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "8")
	require.NoError(t, repository.ApplyMigrations(context.Background(), pg.Pool))
	repo := repository.New(pg.Pool)
	userID, sessionID, workspaceID := uuid.New(), uuid.New(), uuid.New()
	seedSession(t, pg.Pool, userID, sessionID)

	added, err := repo.AddSessionWorkspace(context.Background(), repository.AddSessionWorkspaceParams{
		UserID: userID, SessionID: sessionID, WorkspaceID: workspaceID,
	})
	require.NoError(t, err)
	assert.True(t, added)
}

func TestAddSessionWorkspace_Idempotent(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "8")
	require.NoError(t, repository.ApplyMigrations(context.Background(), pg.Pool))
	repo := repository.New(pg.Pool)
	userID, sessionID, workspaceID := uuid.New(), uuid.New(), uuid.New()
	seedSession(t, pg.Pool, userID, sessionID)
	params := repository.AddSessionWorkspaceParams{
		UserID: userID, SessionID: sessionID, WorkspaceID: workspaceID,
	}

	added1, err := repo.AddSessionWorkspace(context.Background(), params)
	require.NoError(t, err)
	added2, err := repo.AddSessionWorkspace(context.Background(), params)
	require.NoError(t, err)
	assert.True(t, added1)
	assert.False(t, added2, "second insert is a no-op via ON CONFLICT")
}

func TestAddSessionWorkspace_RejectsOtherUser(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "8")
	require.NoError(t, repository.ApplyMigrations(context.Background(), pg.Pool))
	repo := repository.New(pg.Pool)
	owner, stranger, sessionID, workspaceID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	seedSession(t, pg.Pool, owner, sessionID)

	_, err := repo.AddSessionWorkspace(context.Background(), repository.AddSessionWorkspaceParams{
		UserID: stranger, SessionID: sessionID, WorkspaceID: workspaceID,
	})
	require.ErrorIs(t, err, repository.ErrSessionNotOwned)
}

func TestAddSessionWorkspace_RejectsMissingSession(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "8")
	require.NoError(t, repository.ApplyMigrations(context.Background(), pg.Pool))
	repo := repository.New(pg.Pool)

	_, err := repo.AddSessionWorkspace(context.Background(), repository.AddSessionWorkspaceParams{
		UserID:      uuid.New(),
		SessionID:   uuid.New(), // doesn't exist
		WorkspaceID: uuid.New(),
	})
	require.ErrorIs(t, err, repository.ErrSessionNotFound)
}
