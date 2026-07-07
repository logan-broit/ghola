package repository_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"
)

// TestMigration012_AddsContentColumns asserts migration 012 added the
// selection-first content columns (nullable, so the migration is
// zero-downtime additive) and the GIN indexes on tags/entities.
func TestMigration012_AddsContentColumns(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "1024")
	require.NoError(t, repository.ApplyMigrations(context.Background(), pg.Pool))

	cols := map[string]string{
		"label":           "text",
		"representatives": "jsonb",
		"tags":            "ARRAY",
		"entities":        "ARRAY",
		"span_start":      "timestamp with time zone",
		"span_end":        "timestamp with time zone",
		"meta":            "jsonb",
	}
	for col, wantType := range cols {
		var dataType string
		err := pg.Pool.QueryRow(context.Background(), `
			SELECT data_type FROM information_schema.columns
			WHERE table_schema = 'semantic' AND table_name = 'mnemes' AND column_name = $1`,
			col).Scan(&dataType)
		require.NoError(t, err, "column %s must exist", col)
		require.Equal(t, wantType, dataType, "column %s type", col)

		var nullable string
		require.NoError(t, pg.Pool.QueryRow(context.Background(), `
			SELECT is_nullable FROM information_schema.columns
			WHERE table_schema = 'semantic' AND table_name = 'mnemes' AND column_name = $1`,
			col).Scan(&nullable))
		require.Equal(t, "YES", nullable, "column %s must be nullable (additive)", col)
	}

	for _, idx := range []string{"mnemes_tags_gin", "mnemes_entities_gin"} {
		var exists bool
		require.NoError(t, pg.Pool.QueryRow(context.Background(), `
			SELECT EXISTS (SELECT 1 FROM pg_indexes
			WHERE schemaname = 'semantic' AND indexname = $1)`, idx).Scan(&exists))
		require.True(t, exists, "index %s must exist", idx)
	}
}
