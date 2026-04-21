package pipeline_b_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/pipeline_b"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"
)

// installSemanticStub creates a minimal semantic.mnemes table + a
// bayesian_update function that mirrors the pg_ghola math. Matches
// the stub used by handler/semantic_test.go so Pipeline B tests can
// run without the real extension image.
func installSemanticStub(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx, `
		CREATE EXTENSION IF NOT EXISTS vector;
		CREATE EXTENSION IF NOT EXISTS pgcrypto;
		CREATE SCHEMA IF NOT EXISTS semantic;

		CREATE TABLE semantic.mnemes (
			id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id          uuid NOT NULL,
			concept               text NOT NULL,
			content               text NOT NULL,
			embedding             vector(4),
			confidence            double precision NOT NULL DEFAULT 0.5,
			access_count          integer NOT NULL DEFAULT 0,
			last_access           timestamptz NOT NULL DEFAULT now(),
			created_at            timestamptz NOT NULL DEFAULT now(),
			state                 text NOT NULL DEFAULT 'active',
			memory_type           text NOT NULL DEFAULT 'factual',
			tags                  text[] NOT NULL DEFAULT '{}',
			entities              text[] NOT NULL DEFAULT '{}',
			source_episodic_ids   uuid[] NOT NULL DEFAULT '{}',
			contributor_user_ids  uuid[] NOT NULL DEFAULT '{}'
		);

		CREATE OR REPLACE FUNCTION semantic.bayesian_update(prior double precision, evidence double precision)
		RETURNS double precision
		LANGUAGE SQL IMMUTABLE AS $$
			SELECT 0.95 * (prior * evidence /
			         GREATEST(prior * evidence + (1-prior)*(1-evidence), 1e-9))
			       + 0.025;
		$$;
	`)
	require.NoError(t, err)
}

func TestUpsert_InsertsWhenEmpty(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	installSemanticStub(t, pg.Pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	workspace := uuid.New()
	res, err := pipeline_b.Upsert(ctx, pg.Pool, pipeline_b.DedupConfig{}, pipeline_b.UpsertInput{
		WorkspaceID: workspace,
		Mneme: pipeline_b.Mneme{
			Concept: "CNPG provisions Postgres",
			Content: "Chapterhouse uses CloudNativePG.",
			MemoryType: "factual",
			Entities: []string{"CNPG", "Postgres"},
		},
		Embedding:          []float32{1, 0, 0, 0},
		SourceEpisodicIDs:  []uuid.UUID{uuid.New()},
		ContributorUserIDs: []uuid.UUID{uuid.New()},
	})
	require.NoError(t, err)
	assert.True(t, res.Inserted)
	assert.NotEqual(t, uuid.Nil, res.MnemeID)
}

func TestUpsert_StrengthensWhenSimilar(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	installSemanticStub(t, pg.Pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	workspace := uuid.New()
	firstUser := uuid.New()
	firstSrc := uuid.New()
	first, err := pipeline_b.Upsert(ctx, pg.Pool, pipeline_b.DedupConfig{}, pipeline_b.UpsertInput{
		WorkspaceID: workspace,
		Mneme: pipeline_b.Mneme{Concept: "A", Content: "a", MemoryType: "factual"},
		Embedding: []float32{1, 0, 0, 0},
		SourceEpisodicIDs: []uuid.UUID{firstSrc},
		ContributorUserIDs: []uuid.UUID{firstUser},
	})
	require.NoError(t, err)
	require.True(t, first.Inserted)

	// Identical embedding → similarity = 1 → must strengthen, not insert.
	secondUser := uuid.New()
	secondSrc := uuid.New()
	second, err := pipeline_b.Upsert(ctx, pg.Pool, pipeline_b.DedupConfig{}, pipeline_b.UpsertInput{
		WorkspaceID: workspace,
		Mneme: pipeline_b.Mneme{Concept: "A duplicate", Content: "a", MemoryType: "factual"},
		Embedding: []float32{1, 0, 0, 0},
		SourceEpisodicIDs: []uuid.UUID{secondSrc},
		ContributorUserIDs: []uuid.UUID{secondUser},
	})
	require.NoError(t, err)
	assert.False(t, second.Inserted, "should have strengthened existing row")
	assert.Equal(t, first.MnemeID, second.MnemeID)

	var confidence float64
	var sources, contribs []uuid.UUID
	require.NoError(t, pg.Pool.QueryRow(ctx, `
		SELECT confidence, source_episodic_ids, contributor_user_ids
		  FROM semantic.mnemes WHERE id = $1
	`, first.MnemeID).Scan(&confidence, &sources, &contribs))
	assert.Greater(t, confidence, 0.5, "confidence should increase from default 0.5")
	assert.ElementsMatch(t, []uuid.UUID{firstSrc, secondSrc}, sources)
	assert.ElementsMatch(t, []uuid.UUID{firstUser, secondUser}, contribs)

	var count int
	require.NoError(t, pg.Pool.QueryRow(ctx, `SELECT count(*) FROM semantic.mnemes`).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestUpsert_InsertsWhenDissimilar(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	installSemanticStub(t, pg.Pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	workspace := uuid.New()
	_, err := pipeline_b.Upsert(ctx, pg.Pool, pipeline_b.DedupConfig{}, pipeline_b.UpsertInput{
		WorkspaceID: workspace,
		Mneme: pipeline_b.Mneme{Concept: "A", Content: "a", MemoryType: "factual"},
		Embedding: []float32{1, 0, 0, 0},
	})
	require.NoError(t, err)

	// Orthogonal embedding → similarity = 0 → insert new row.
	res, err := pipeline_b.Upsert(ctx, pg.Pool, pipeline_b.DedupConfig{}, pipeline_b.UpsertInput{
		WorkspaceID: workspace,
		Mneme: pipeline_b.Mneme{Concept: "B", Content: "b", MemoryType: "factual"},
		Embedding: []float32{0, 1, 0, 0},
	})
	require.NoError(t, err)
	assert.True(t, res.Inserted)

	var count int
	require.NoError(t, pg.Pool.QueryRow(ctx, `SELECT count(*) FROM semantic.mnemes`).Scan(&count))
	assert.Equal(t, 2, count)
}

func TestUpsert_DifferentWorkspaceNeverDedupes(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	installSemanticStub(t, pg.Pool)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ws1, ws2 := uuid.New(), uuid.New()
	_, err := pipeline_b.Upsert(ctx, pg.Pool, pipeline_b.DedupConfig{}, pipeline_b.UpsertInput{
		WorkspaceID: ws1,
		Mneme: pipeline_b.Mneme{Concept: "A", Content: "a", MemoryType: "factual"},
		Embedding: []float32{1, 0, 0, 0},
	})
	require.NoError(t, err)

	// Same embedding, different workspace → insert.
	res, err := pipeline_b.Upsert(ctx, pg.Pool, pipeline_b.DedupConfig{}, pipeline_b.UpsertInput{
		WorkspaceID: ws2,
		Mneme: pipeline_b.Mneme{Concept: "A", Content: "a", MemoryType: "factual"},
		Embedding: []float32{1, 0, 0, 0},
	})
	require.NoError(t, err)
	assert.True(t, res.Inserted, "workspace isolation: must insert new row")
}
