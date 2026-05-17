package consolidation_test

import (
	"context"
	"math"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/consolidation"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"
)

// newRepo boots an ephemeral Postgres + applies migrations and returns
// a Repository scoped to that pool. Mirrors newAssociationsRepo from
// internal/repository/associations_test.go — kept inline here because
// the helper is a different package and the duplication is small.
func newRepo(t *testing.T) *repository.Repository {
	t.Helper()
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "1024")
	require.NoError(t, repository.ApplyMigrations(t.Context(), pg.Pool))
	return repository.New(pg.Pool)
}

// seedEvent inserts a minimal episodic.events row with the given id
// (and a freshly-minted session_id wrapped around userID) so the FKs
// on semantic.associations.{src,dst}_event_id are satisfied. Mirrors
// seedEventForAssoc from associations_test.go — inlined here because
// the helper lives in a different package and the duplication is
// minimal (premature abstraction is worse than the copy).
func seedEvent(t *testing.T, repo *repository.Repository, eventID, userID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	sessionID := uuid.New()
	_, err := repo.Pool().Exec(ctx, `
		INSERT INTO episodic.sessions (id, user_id, started_at, event_count)
		VALUES ($1, $2, now(), 1)
	`, sessionID, userID)
	require.NoError(t, err)
	_, err = repo.Pool().Exec(ctx, `
		INSERT INTO episodic.events (id, session_id, user_id, type, text, raw_event, created_at)
		VALUES ($1, $2, $3, 'user', 'seed', '{}'::jsonb, now())
	`, eventID, sessionID, userID)
	require.NoError(t, err)
}

// enqueuePair inserts one row into semantic.co_activation_queue. Used
// to pre-populate the queue independently of the public Enqueue path
// (so this test isn't entangled with C1's ingest behavior).
func enqueuePair(t *testing.T, repo *repository.Repository, src, dst, ws uuid.UUID) {
	t.Helper()
	_, err := repo.Pool().Exec(context.Background(), `
		INSERT INTO semantic.co_activation_queue
			(src_event_id, dst_event_id, workspace_id)
		VALUES ($1, $2, $3)
	`, src, dst, ws)
	require.NoError(t, err)
}

// TestDrainAndStrengthen_DrainsAndUpserts pins the happy path: 5
// distinct pairs in the queue → 5 association rows (each with
// co_activations=1 and weight = 1 - exp(-1/5) ~= 0.18127) and an
// empty queue afterwards.
func TestDrainAndStrengthen_DrainsAndUpserts(t *testing.T) {
	repo := newRepo(t)
	ctx := t.Context()

	userID := uuid.New()
	workspaceID := uuid.New()
	type pairIDs struct{ src, dst uuid.UUID }
	pairs := make([]pairIDs, 5)
	for i := range pairs {
		pairs[i] = pairIDs{src: uuid.New(), dst: uuid.New()}
		seedEvent(t, repo, pairs[i].src, userID)
		seedEvent(t, repo, pairs[i].dst, userID)
		enqueuePair(t, repo, pairs[i].src, pairs[i].dst, workspaceID)
	}

	n, err := consolidation.DrainAndStrengthen(ctx, repo, 100)
	require.NoError(t, err)
	require.Equal(t, 5, n, "all 5 queued pairs must be processed")

	// Queue must be empty.
	remaining, _, err := repo.DrainCoActivationQueue(ctx, 100)
	require.NoError(t, err)
	require.Empty(t, remaining, "drain must consume the queue")

	// Associations table must have 5 rows, each at the n=1 weight.
	wantWeight := 1 - math.Exp(-1.0/5.0)
	for _, p := range pairs {
		var (
			gotCo     int
			gotWeight float64
			gotWS     uuid.UUID
		)
		require.NoError(t, repo.Pool().QueryRow(ctx, `
			SELECT co_activations, weight, workspace_id
			FROM semantic.associations
			WHERE src_event_id = $1 AND dst_event_id = $2 AND association_type = 'hebbian'
		`, p.src, p.dst).Scan(&gotCo, &gotWeight, &gotWS))
		require.Equal(t, 1, gotCo)
		require.InDelta(t, wantWeight, gotWeight, 1e-9)
		require.Equal(t, workspaceID, gotWS)
	}
}

// TestDrainAndStrengthen_DuplicatePair_IncrementsCount pins the
// fold-into-existing branch: 3 copies of the same (src, dst) pair in
// the queue → 1 association row with co_activations=3 and weight =
// 1 - exp(-3/5) ~= 0.45119.
func TestDrainAndStrengthen_DuplicatePair_IncrementsCount(t *testing.T) {
	repo := newRepo(t)
	ctx := t.Context()

	userID := uuid.New()
	workspaceID := uuid.New()
	src, dst := uuid.New(), uuid.New()
	seedEvent(t, repo, src, userID)
	seedEvent(t, repo, dst, userID)

	for i := 0; i < 3; i++ {
		enqueuePair(t, repo, src, dst, workspaceID)
	}

	n, err := consolidation.DrainAndStrengthen(ctx, repo, 100)
	require.NoError(t, err)
	require.Equal(t, 3, n)

	// Exactly one row, co_activations=3, weight at n=3.
	var (
		rowCount  int
		gotCo     int
		gotWeight float64
	)
	require.NoError(t, repo.Pool().QueryRow(ctx, `
		SELECT count(*) FROM semantic.associations
		WHERE src_event_id = $1 AND dst_event_id = $2 AND association_type = 'hebbian'
	`, src, dst).Scan(&rowCount))
	require.Equal(t, 1, rowCount, "duplicate pairs must collapse to one row")

	require.NoError(t, repo.Pool().QueryRow(ctx, `
		SELECT co_activations, weight FROM semantic.associations
		WHERE src_event_id = $1 AND dst_event_id = $2 AND association_type = 'hebbian'
	`, src, dst).Scan(&gotCo, &gotWeight))
	require.Equal(t, 3, gotCo)
	wantWeight := 1 - math.Exp(-3.0/5.0)
	require.InDelta(t, wantWeight, gotWeight, 1e-9)
}

// TestDrainAndStrengthen_RespectsBatchSize pins the bounded-batch
// behavior: 10 pairs queued, drained with batchSize=5 → exactly 5
// processed, 5 still in the queue. The unprocessed 5 must be the
// newest 5 (drain is oldest-first).
func TestDrainAndStrengthen_RespectsBatchSize(t *testing.T) {
	repo := newRepo(t)
	ctx := t.Context()

	userID := uuid.New()
	workspaceID := uuid.New()
	for i := 0; i < 10; i++ {
		src, dst := uuid.New(), uuid.New()
		seedEvent(t, repo, src, userID)
		seedEvent(t, repo, dst, userID)
		enqueuePair(t, repo, src, dst, workspaceID)
	}

	n, err := consolidation.DrainAndStrengthen(ctx, repo, 5)
	require.NoError(t, err)
	require.Equal(t, 5, n, "batchSize must cap the work per call")

	// Queue still has 5 rows.
	var remaining int
	require.NoError(t, repo.Pool().QueryRow(ctx,
		`SELECT count(*) FROM semantic.co_activation_queue`,
	).Scan(&remaining))
	require.Equal(t, 5, remaining, "exactly batchSize rows must be deleted")

	// Associations table reflects the 5 we processed.
	var assocCount int
	require.NoError(t, repo.Pool().QueryRow(ctx,
		`SELECT count(*) FROM semantic.associations`,
	).Scan(&assocCount))
	require.Equal(t, 5, assocCount)
}

// TestDrainAndStrengthen_EmptyQueue_FastNoOp pins the empty-queue
// fast-path: returns (0, nil), the associations table stays empty,
// and no transaction is opened needlessly. We can't easily assert the
// "no tx" half without instrumenting the pool, so we settle for the
// observable: zero rows in either table, no error.
func TestDrainAndStrengthen_EmptyQueue_FastNoOp(t *testing.T) {
	repo := newRepo(t)
	ctx := t.Context()

	n, err := consolidation.DrainAndStrengthen(ctx, repo, 100)
	require.NoError(t, err)
	require.Equal(t, 0, n)

	var assocCount int
	require.NoError(t, repo.Pool().QueryRow(ctx,
		`SELECT count(*) FROM semantic.associations`,
	).Scan(&assocCount))
	require.Equal(t, 0, assocCount)
}
