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

	// user_id MUST differ from the workspace id: the workspace<->session
	// mapping lives in episodic.session_workspaces (migration 006), NOT on
	// episodic.sessions.user_id. Seeding user_id == ws would make the old
	// `WHERE user_id = $1` SQL pass by coincidence; a distinct user_id forces
	// the query to scope through the join table (the real prod shape, where a
	// workspace pools many users' sessions).
	userID := uuid.New()
	require.NotEqual(t, ws, userID)

	// One session WITH l1_embedding, one WITHOUT — both mapped to ws via
	// session_workspaces so the IS NOT NULL filter (not the mapping) is what
	// excludes the second.
	with := uuid.New()
	without := uuid.New()
	lit := "[" + repeatCsv("0.1", 1024) + "]"
	_, err := pg.Pool.Exec(ctx, `
		INSERT INTO episodic.sessions (id, user_id, started_at, ended_at, event_count, l1_embedding)
		VALUES ($1, $2, now(), now(), 0, ($3::text)::vector)`, with, userID, lit)
	require.NoError(t, err)
	_, err = pg.Pool.Exec(ctx, `
		INSERT INTO episodic.sessions (id, user_id, started_at, ended_at, event_count)
		VALUES ($1, $2, now(), now(), 0)`, without, userID)
	require.NoError(t, err)
	for _, sid := range []uuid.UUID{with, without} {
		_, err = pg.Pool.Exec(ctx, `
			INSERT INTO episodic.session_workspaces (session_id, workspace_id)
			VALUES ($1, $2)`, sid, ws)
		require.NoError(t, err)
	}

	got, err := repo.WorkspaceSessionL1s(ctx, ws)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, with, got[0].SessionID)
	require.Len(t, got[0].Embedding, 1024)
}

// TestSessionEnrichmentEvents_ReturnsEmbeddedTextEvents mirrors the seed
// pattern above: a session with 2 embedded, tagged/entitied 'user'/
// 'assistant' events, plus an embedded 'tool_result' event and an
// embedding-null event that must both be excluded (tool output never
// surfaces as a user-facing excerpt; a null embedding has no centrality
// signal). The two included events share an identical created_at to
// exercise the id ASC tiebreak; asserts ordering and tags/entities
// round-trip.
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
	// Fixed, ascending UUIDs so the shared created_at resolves by id ASC
	// deterministically (first < second).
	first := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	second := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	toolResult := uuid.New()
	noEmbedding := uuid.New()

	// first and second share an identical created_at -> the id ASC
	// tiebreak decides their order.
	_, err = pg.Pool.Exec(ctx, `
		INSERT INTO episodic.events (id, session_id, user_id, type, text, raw_event, embedding, tags, entities, created_at)
		VALUES ($1, $2, $3, 'user', 'first message', '{}'::jsonb, ($4::text)::vector, $5, $6, '2026-07-01 00:00:00+00')`,
		first, sessionID, ws, lit, []string{"go", "db"}, []string{"pgvector"})
	require.NoError(t, err)
	_, err = pg.Pool.Exec(ctx, `
		INSERT INTO episodic.events (id, session_id, user_id, type, text, raw_event, embedding, tags, entities, created_at)
		VALUES ($1, $2, $3, 'assistant', 'second message', '{}'::jsonb, ($4::text)::vector, $5, $6, '2026-07-01 00:00:00+00')`,
		second, sessionID, ws, lit, []string{"go", "test"}, []string{"hdbscan"})
	require.NoError(t, err)
	// Embedded tool_result event must be excluded — a mneme excerpt is
	// never raw tool output, even when embedded + text-bearing.
	_, err = pg.Pool.Exec(ctx, `
		INSERT INTO episodic.events (id, session_id, user_id, type, text, raw_event, embedding, tags, entities, created_at)
		VALUES ($1, $2, $3, 'tool_result', 'tool output', '{}'::jsonb, ($4::text)::vector, $5, $6, now() + interval '1 second')`,
		toolResult, sessionID, ws, lit, []string{"go"}, []string{"pgvector"})
	require.NoError(t, err)
	// Embedding-null event must be excluded — no centrality signal.
	_, err = pg.Pool.Exec(ctx, `
		INSERT INTO episodic.events (id, session_id, user_id, type, text, raw_event, created_at)
		VALUES ($1, $2, $3, 'user', 'no embedding', '{}'::jsonb, now() + interval '2 seconds')`,
		noEmbedding, sessionID, ws)
	require.NoError(t, err)

	got, err := repo.SessionEnrichmentEvents(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, got, 2, "embedding-null and tool_result events excluded")
	require.Equal(t, first, got[0].ID, "identical created_at resolves by id ASC")
	require.Equal(t, second, got[1].ID)
	require.Equal(t, "first message", got[0].Text)
	require.ElementsMatch(t, []string{"go", "db"}, got[0].Tags)
	require.ElementsMatch(t, []string{"pgvector"}, got[0].Entities)
	require.ElementsMatch(t, []string{"go", "test"}, got[1].Tags)
	require.ElementsMatch(t, []string{"hdbscan"}, got[1].Entities)
	require.Len(t, got[0].Embedding, 1024)
	require.False(t, got[0].CreatedAt.IsZero())
}
