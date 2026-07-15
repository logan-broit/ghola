package repository_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"
)

// TestQueryMnemesByEmbedding_CarriesLabelAndExcerpt pins the Task 18
// hydration: a recall hit must surface the mneme's label and the first
// representative's excerpt so the semantic tier finally contributes
// readable text. The excerpt is extracted via a jsonb path in SQL, so
// this also guards that the `representatives->0->>'excerpt'` projection
// resolves against the migration-012 jsonb shape.
func TestQueryMnemesByEmbedding_CarriesLabelAndExcerpt(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "8")
	require.NoError(t, repository.ApplyMigrations(context.Background(), pg.Pool))
	repo := repository.New(pg.Pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	workspace := uuid.New()
	var id uuid.UUID
	require.NoError(t, pg.Pool.QueryRow(ctx, `
		INSERT INTO semantic.mnemes (workspace_id, level, embedding, label, representatives)
		VALUES ($1, 1, ($2::text)::vector, $3, $4::jsonb)
		RETURNING id`,
		workspace, "[1,0,0,0,0,0,0,0]", "cluster label",
		`[{"excerpt":"the top excerpt"},{"excerpt":"second"}]`).Scan(&id))

	hits, err := repo.QueryMnemesByEmbedding(ctx, workspace,
		[]float32{1, 0, 0, 0, 0, 0, 0, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, id, hits[0].ID)
	assert.Equal(t, "cluster label", hits[0].Label)
	assert.Equal(t, "the top excerpt", hits[0].TopExcerpt,
		"first representative's excerpt only")
}

// TestQueryMnemesByEmbedding_BoundsOversizedMultibyteExcerpt drives an
// over-cap multibyte excerpt through the real read path (a seeded row,
// not a unit call on boundExcerpt in isolation) and pins that the
// hydrated TopExcerpt is bounded at mnemeExcerptCap (500 bytes) and never
// invalid UTF-8. "世" is 3 bytes, so 400 runes (1200 bytes) both exceeds
// the cap and does not land on a byte boundary at 500 — a naive
// s[:500] would split a multibyte rune.
func TestQueryMnemesByEmbedding_BoundsOversizedMultibyteExcerpt(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "8")
	require.NoError(t, repository.ApplyMigrations(context.Background(), pg.Pool))
	repo := repository.New(pg.Pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	long := strings.Repeat("世", 400)
	reps, err := json.Marshal([]map[string]string{{"excerpt": long}})
	require.NoError(t, err)

	workspace := uuid.New()
	var id uuid.UUID
	require.NoError(t, pg.Pool.QueryRow(ctx, `
		INSERT INTO semantic.mnemes (workspace_id, level, embedding, label, representatives)
		VALUES ($1, 1, ($2::text)::vector, $3, $4::jsonb)
		RETURNING id`,
		workspace, "[1,0,0,0,0,0,0,0]", "cluster label", string(reps)).Scan(&id))

	hits, err := repo.QueryMnemesByEmbedding(ctx, workspace,
		[]float32{1, 0, 0, 0, 0, 0, 0, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.True(t, utf8.ValidString(hits[0].TopExcerpt),
		"bounded excerpt must be valid UTF-8, got %q", hits[0].TopExcerpt)
	assert.LessOrEqual(t, len(hits[0].TopExcerpt), 500)
	assert.NotEmpty(t, hits[0].TopExcerpt)
}
