package repository_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"
)

func repeatCsv(s string, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = s
	}
	return strings.Join(parts, ",")
}

func TestWorkspaceSessionL1s_ReturnsOnlyPopulated(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "1024")
	require.NoError(t, repository.ApplyMigrations(context.Background(), pg.Pool))
	repo := repository.New(pg.Pool)
	ctx := context.Background()
	ws := uuid.New()

	// One session WITH l1_embedding, one WITHOUT.
	with := uuid.New()
	lit := "[" + repeatCsv("0.1", 1024) + "]"
	_, err := pg.Pool.Exec(ctx, `
		INSERT INTO episodic.sessions (id, user_id, started_at, ended_at, event_count, l1_embedding)
		VALUES ($1, $2, now(), now(), 0, ($3::text)::vector)`, with, ws, lit)
	require.NoError(t, err)
	_, err = pg.Pool.Exec(ctx, `
		INSERT INTO episodic.sessions (id, user_id, started_at, ended_at, event_count)
		VALUES ($1, $2, now(), now(), 0)`, uuid.New(), ws)
	require.NoError(t, err)

	got, err := repo.WorkspaceSessionL1s(ctx, ws)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, with, got[0].SessionID)
	require.Len(t, got[0].Embedding, 1024)
}

// TestSessionEnrichmentEvents_ReturnsEmbeddedTextEvents mirrors the seed
// pattern above: a session with 2 embedded, tagged/entitied events (plus
// one embedding-null event that must be excluded — no centrality
// signal). Asserts ordering (created_at ASC, id ASC) and that
// tags/entities round-trip.
func TestSessionEnrichmentEvents_ReturnsEmbeddedTextEvents(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "1024")
	require.NoError(t, repository.ApplyMigrations(context.Background(), pg.Pool))
	repo := repository.New(pg.Pool)
	ctx := context.Background()

	ws := uuid.New()
	sessionID := uuid.New()
	_, err := pg.Pool.Exec(ctx, `
		INSERT INTO episodic.sessions (id, user_id, started_at, ended_at, event_count)
		VALUES ($1, $2, now(), now(), 0)`, sessionID, ws)
	require.NoError(t, err)

	lit := "[" + repeatCsv("0.1", 1024) + "]"
	first := uuid.New()
	second := uuid.New()
	noEmbedding := uuid.New()

	_, err = pg.Pool.Exec(ctx, `
		INSERT INTO episodic.events (id, session_id, user_id, type, text, raw_event, embedding, tags, entities, created_at)
		VALUES ($1, $2, $3, 'user', 'first message', '{}'::jsonb, ($4::text)::vector, $5, $6, now())`,
		first, sessionID, ws, lit, []string{"go", "db"}, []string{"pgvector"})
	require.NoError(t, err)
	_, err = pg.Pool.Exec(ctx, `
		INSERT INTO episodic.events (id, session_id, user_id, type, text, raw_event, embedding, tags, entities, created_at)
		VALUES ($1, $2, $3, 'assistant', 'second message', '{}'::jsonb, ($4::text)::vector, $5, $6, now() + interval '1 second')`,
		second, sessionID, ws, lit, []string{"go", "test"}, []string{"hdbscan"})
	require.NoError(t, err)
	// Embedding-null event must be excluded — no centrality signal.
	_, err = pg.Pool.Exec(ctx, `
		INSERT INTO episodic.events (id, session_id, user_id, type, text, raw_event, created_at)
		VALUES ($1, $2, $3, 'user', 'no embedding', '{}'::jsonb, now() + interval '2 seconds')`,
		noEmbedding, sessionID, ws)
	require.NoError(t, err)

	got, err := repo.SessionEnrichmentEvents(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, got, 2, "embedding-null event excluded")
	require.Equal(t, first, got[0].ID, "ordered created_at ASC, id ASC")
	require.Equal(t, second, got[1].ID)
	require.Equal(t, "first message", got[0].Text)
	require.ElementsMatch(t, []string{"go", "db"}, got[0].Tags)
	require.ElementsMatch(t, []string{"pgvector"}, got[0].Entities)
	require.ElementsMatch(t, []string{"go", "test"}, got[1].Tags)
	require.ElementsMatch(t, []string{"hdbscan"}, got[1].Entities)
	require.Len(t, got[0].Embedding, 1024)
	require.False(t, got[0].CreatedAt.IsZero())
}
