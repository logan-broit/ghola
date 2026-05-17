package repository_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"
)

// TestEpisodicSchemaHasExpectedTables verifies that a fresh Postgres,
// after applying the episodic migrations, exposes exactly the tables
// the JSONL-native addendum specifies plus the workspace-scoping
// join table (migration 006): events, sessions, session_workspaces,
// shares.
func TestEpisodicSchemaHasExpectedTables(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)

	t.Setenv("EMBEDDING_DIM", "1024")
	require.NoError(t, repository.ApplyMigrations(t.Context(), pg.Pool))

	got := testutil.QueryTables(t, pg.Pool, "episodic")
	want := []string{"events", "session_workspaces", "sessions", "shares"}
	require.Equal(t, want, got,
		"episodic schema must hold events/sessions/session_workspaces/shares")
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

// TestEpisodicSessionsHasL1Embedding pins the v0.3 addition: the
// mentat-pooled per-session embedding column and its partial HNSW
// index. Dimensions are exercised elsewhere; this test asserts only
// presence so it stays orthogonal to EMBEDDING_DIM regression checks.
func TestEpisodicSessionsHasL1Embedding(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)

	t.Setenv("EMBEDDING_DIM", "1024")
	require.NoError(t, repository.ApplyMigrations(t.Context(), pg.Pool))

	var hasColumn bool
	require.NoError(t, pg.Pool.QueryRow(t.Context(), `
        SELECT EXISTS (
            SELECT 1
            FROM information_schema.columns
            WHERE table_schema = 'episodic'
              AND table_name   = 'sessions'
              AND column_name  = 'l1_embedding'
        )`).Scan(&hasColumn))
	require.True(t, hasColumn,
		"episodic.sessions must expose an l1_embedding column after migrations")

	// Pin the load-bearing properties of the index, not just its name:
	// a partial HNSW index over l1_embedding using cosine ops. This
	// catches refactors that drop the partial-ness, swap HNSW for
	// IVFFlat/btree, or change cosine ops to L2 ops.
	var indexDef string
	require.NoError(t, pg.Pool.QueryRow(t.Context(), `
        SELECT indexdef
        FROM pg_indexes
        WHERE schemaname = 'episodic'
          AND tablename  = 'sessions'
          AND indexname  = 'episodic_sessions_l1_hnsw'
        `).Scan(&indexDef))

	def := strings.ToLower(indexDef)
	require.Contains(t, def, strings.ToLower("USING hnsw"),
		"episodic_sessions_l1_hnsw must use HNSW, got: %s", indexDef)
	require.Contains(t, def, strings.ToLower("vector_cosine_ops"),
		"episodic_sessions_l1_hnsw must use cosine ops, got: %s", indexDef)
	require.Contains(t, def, strings.ToLower("l1_embedding IS NOT NULL"),
		"episodic_sessions_l1_hnsw must be partial on l1_embedding IS NOT NULL, got: %s", indexDef)
}
