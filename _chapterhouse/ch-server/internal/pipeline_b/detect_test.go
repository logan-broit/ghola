package pipeline_b_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/pipeline_b"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"
)

// seedEvent drops one episodic.events row with the given entity set.
// created_at / ingested_at default to now() unless overridden.
func seedEvent(t *testing.T, pool *pgxpool.Pool, sessionID uuid.UUID, entities []string, ingestedAt time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := pool.Exec(ctx, `
		INSERT INTO episodic.events
		  (id, session_id, user_id, type, raw_event, entities, created_at, ingested_at)
		VALUES
		  ($1, $2, $3, 'user', '{}'::jsonb, $4, $5, $5)
	`, uuid.New(), sessionID, uuid.New(), entities, ingestedAt)
	require.NoError(t, err)
}

func seedSession(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	id := uuid.New()
	_, err := pool.Exec(ctx, `
		INSERT INTO episodic.sessions (id, user_id, started_at)
		VALUES ($1, $2, now())
	`, id, uuid.New())
	require.NoError(t, err)
	return id
}

func applyMigrations(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if os.Getenv("EMBEDDING_DIM") == "" {
		t.Setenv("EMBEDDING_DIM", "8")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	require.NoError(t, repository.ApplyMigrations(ctx, pool))
}

// TestDetectPairs_MeetsThreshold: 5 sessions all tagging (CNPG,
// Postgres) → detector returns support=5 with all 5 session IDs.
func TestDetectPairs_MeetsThreshold(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	applyMigrations(t, pg.Pool)

	now := time.Now().UTC()
	var wantSessions []uuid.UUID
	for i := 0; i < 5; i++ {
		s := seedSession(t, pg.Pool)
		seedEvent(t, pg.Pool, s, []string{"CNPG", "Postgres"}, now)
		wantSessions = append(wantSessions, s)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pairs, err := pipeline_b.DetectPairs(ctx, pg.Pool, 24*time.Hour, 3)
	require.NoError(t, err)
	require.Len(t, pairs, 1)
	assert.Equal(t, "CNPG", pairs[0].E1)
	assert.Equal(t, "Postgres", pairs[0].E2)
	assert.Equal(t, 5, pairs[0].Support)
	assert.Len(t, pairs[0].SessionIDs, 5)
}

// TestDetectPairs_BelowThreshold: pair mentioned in only 2 sessions
// is excluded (threshold is 3).
func TestDetectPairs_BelowThreshold(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	applyMigrations(t, pg.Pool)

	now := time.Now().UTC()
	for i := 0; i < 2; i++ {
		s := seedSession(t, pg.Pool)
		seedEvent(t, pg.Pool, s, []string{"RustLang", "pgrx"}, now)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pairs, err := pipeline_b.DetectPairs(ctx, pg.Pool, 24*time.Hour, 3)
	require.NoError(t, err)
	assert.Empty(t, pairs)
}

// TestDetectPairs_WindowExcludesOld: events older than the window are
// skipped even if they would otherwise meet support.
func TestDetectPairs_WindowExcludesOld(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	applyMigrations(t, pg.Pool)

	old := time.Now().UTC().Add(-48 * time.Hour)
	for i := 0; i < 5; i++ {
		s := seedSession(t, pg.Pool)
		seedEvent(t, pg.Pool, s, []string{"oldA", "oldB"}, old)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pairs, err := pipeline_b.DetectPairs(ctx, pg.Pool, 24*time.Hour, 3)
	require.NoError(t, err)
	assert.Empty(t, pairs)
}

// TestDetectPairs_CanonicalOrdering: detector returns pair with
// e1 < e2 regardless of how entities were stored.
func TestDetectPairs_CanonicalOrdering(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	applyMigrations(t, pg.Pool)

	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		s := seedSession(t, pg.Pool)
		// Stored as (Zebra, Ant) but should come out (Ant, Zebra).
		seedEvent(t, pg.Pool, s, []string{"Zebra", "Ant"}, now)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pairs, err := pipeline_b.DetectPairs(ctx, pg.Pool, 24*time.Hour, 3)
	require.NoError(t, err)
	require.Len(t, pairs, 1)
	assert.Equal(t, "Ant", pairs[0].E1)
	assert.Equal(t, "Zebra", pairs[0].E2)
}
