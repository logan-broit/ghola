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

// TestIngestEpisodicBatch_WritesSessionWorkspace pins that ingest
// produces a (session_id, workspace_id) row in episodic.session_workspaces
// in the same transaction as the session UPSERT — without it the
// recall-side workspace filter (shipped in 326ae27) would never find
// the session.
func TestIngestEpisodicBatch_WritesSessionWorkspace(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "8")
	require.NoError(t, repository.ApplyMigrations(context.Background(), pg.Pool))
	repo := repository.New(pg.Pool)

	userID := uuid.New()
	sessionID := uuid.New()
	workspaceID := uuid.New()

	_, _, err := repo.IngestEpisodicBatch(context.Background(),
		&repository.EpisodicSession{
			ID:          sessionID,
			UserID:      userID,
			StartedAt:   time.Now(),
			WorkspaceID: workspaceID,
		},
		nil, // empty batch is valid; session row is what we're checking
	)
	require.NoError(t, err)

	var count int
	require.NoError(t, pg.Pool.QueryRow(context.Background(), `
		SELECT count(*) FROM episodic.session_workspaces
		WHERE session_id = $1 AND workspace_id = $2`,
		sessionID, workspaceID).Scan(&count))
	assert.Equal(t, 1, count, "Ingest must write the session_workspaces row in the same tx")
}

// TestIngestEpisodicBatch_SessionWorkspaceIsIdempotent pins that
// re-ingest of the same batch (Pipeline A's at-least-once retry
// contract) does not error or duplicate the join row.
func TestIngestEpisodicBatch_SessionWorkspaceIsIdempotent(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "8")
	require.NoError(t, repository.ApplyMigrations(context.Background(), pg.Pool))
	repo := repository.New(pg.Pool)

	userID := uuid.New()
	sessionID := uuid.New()
	workspaceID := uuid.New()
	session := &repository.EpisodicSession{
		ID:          sessionID,
		UserID:      userID,
		StartedAt:   time.Now(),
		WorkspaceID: workspaceID,
	}

	_, _, err := repo.IngestEpisodicBatch(context.Background(), session, nil)
	require.NoError(t, err)
	_, _, err = repo.IngestEpisodicBatch(context.Background(), session, nil) // re-ingest
	require.NoError(t, err)

	var count int
	require.NoError(t, pg.Pool.QueryRow(context.Background(), `
		SELECT count(*) FROM episodic.session_workspaces
		WHERE session_id = $1`, sessionID).Scan(&count))
	assert.Equal(t, 1, count, "duplicate session_workspaces rows must be deduped")
}
