package repository_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"
)

// TestSemanticMnemesV03Shape pins the v0.3 column set on
// semantic.mnemes. The v0.2 LLM-distillation columns
// (concept/content/memory_type/source_episodic_ids) must be gone;
// the predictive-replay columns (level, member_ids, last_reinforced_at)
// must be present; and the migration-012 content columns
// (label, representatives, tags, entities, span_start, span_end, meta)
// must also be present.
func TestSemanticMnemesV03Shape(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "1024")
	require.NoError(t, repository.ApplyMigrations(t.Context(), pg.Pool))

	cols := map[string]string{}
	rows, err := pg.Pool.Query(t.Context(), `
        SELECT column_name, data_type
        FROM information_schema.columns
        WHERE table_schema='semantic' AND table_name='mnemes'`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var name, typ string
		require.NoError(t, rows.Scan(&name, &typ))
		cols[name] = typ
	}
	require.NoError(t, rows.Err())

	// v0.3 predictive-replay columns
	for _, required := range []string{"level", "member_ids", "last_reinforced_at"} {
		require.Contains(t, cols, required)
	}
	// migration-012 content columns (selection-first, additive)
	for _, required := range []string{"label", "representatives", "tags", "entities", "span_start", "span_end", "meta"} {
		require.Contains(t, cols, required)
	}
	// v0.2 LLM-distillation columns must be gone
	for _, removed := range []string{"concept", "content", "memory_type", "source_episodic_ids"} {
		require.NotContains(t, cols, removed)
	}
}
