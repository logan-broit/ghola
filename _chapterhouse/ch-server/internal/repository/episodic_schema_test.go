package repository_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"
)

// TestEpisodicSchemaHasExpectedTables verifies that a fresh Postgres,
// after applying the episodic migrations, exposes exactly the three
// tables the JSONL-native addendum specifies: events, sessions,
// shares.
func TestEpisodicSchemaHasExpectedTables(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)

	t.Setenv("EMBEDDING_DIM", "1024")
	require.NoError(t, repository.ApplyMigrations(t.Context(), pg.Pool))

	got := testutil.QueryTables(t, pg.Pool, "episodic")
	want := []string{"events", "sessions", "shares"}
	require.Equal(t, want, got,
		"episodic schema must hold exactly events/sessions/shares")
}

// TestEpisodicEventsHasEmbeddingColumn pins the embedding column to
// whatever EMBEDDING_DIM requested — here, 1024 (the v1a Qwen3
// default).
func TestEpisodicEventsHasEmbeddingColumn(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)

	t.Setenv("EMBEDDING_DIM", "1024")
	require.NoError(t, repository.ApplyMigrations(t.Context(), pg.Pool))

	dim := testutil.ColumnVectorDim(t, pg.Pool, "episodic", "events", "embedding")
	require.Equal(t, 1024, dim,
		"episodic.events.embedding should be vector(1024) with EMBEDDING_DIM=1024")
}
