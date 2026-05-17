package repository_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"
)

// TestWorkspaceScoping_IsolatesAcrossWorkspaces is the load-bearing
// invariant the entire workspace_id design exists for: a recall
// query scoped to workspace A must never return events from a
// session that lives only in workspace B, even when:
//   - both workspaces have matching content
//   - both workspaces belong to the same user (i.e. the user_id
//     boundary is *not* what's keeping them apart)
//
// If this test ever fails, every prior test pinning "workspace
// scoping works" was checking incidental properties.
func TestWorkspaceScoping_IsolatesAcrossWorkspaces(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "8")
	require.NoError(t, repository.ApplyMigrations(context.Background(), pg.Pool))
	repo := repository.New(pg.Pool)

	userID := uuid.New()
	sessionA, sessionB := uuid.New(), uuid.New()
	workspaceA, workspaceB := uuid.New(), uuid.New()

	// Seed two sessions, each in a different workspace, both same user.
	for _, s := range []struct {
		id uuid.UUID
		ws uuid.UUID
	}{
		{sessionA, workspaceA}, {sessionB, workspaceB},
	} {
		_, err := pg.Pool.Exec(context.Background(), `
			INSERT INTO episodic.sessions (id, user_id, started_at, event_count)
			VALUES ($1, $2, now(), 1)`, s.id, userID)
		require.NoError(t, err)
		_, err = pg.Pool.Exec(context.Background(), `
			INSERT INTO episodic.session_workspaces (session_id, workspace_id)
			VALUES ($1, $2)`, s.id, s.ws)
		require.NoError(t, err)
	}

	// Each session has one event with the same matching text.
	insertEvent := func(sess uuid.UUID, text string) {
		_, err := pg.Pool.Exec(context.Background(), `
			INSERT INTO episodic.events (id, session_id, user_id, type, text, raw_event, created_at)
			VALUES ($1, $2, $3, 'user', $4, '{}'::jsonb, now())`,
			uuid.New(), sess, userID, text)
		require.NoError(t, err)
	}
	insertEvent(sessionA, "kubernetes pods")
	insertEvent(sessionB, "kubernetes pods")

	// Recall scoped to workspaceA must return only sessionA's event.
	hitsA, err := repo.QueryEpisodicKeyword(context.Background(), repository.EpisodicKeywordParams{
		UserID: userID, WorkspaceID: workspaceA, QueryText: "kubernetes", Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, hitsA, 1, "workspaceA recall must return exactly 1 hit")
	assert.Equal(t, sessionA, hitsA[0].Event.SessionID)

	// And vice versa.
	hitsB, err := repo.QueryEpisodicKeyword(context.Background(), repository.EpisodicKeywordParams{
		UserID: userID, WorkspaceID: workspaceB, QueryText: "kubernetes", Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, hitsB, 1, "workspaceB recall must return exactly 1 hit")
	assert.Equal(t, sessionB, hitsB[0].Event.SessionID)
}
