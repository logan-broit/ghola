package repository_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"
)

// vectorLiteral8From builds an 8-dim pgvector text literal from an
// explicit slice. Centralized here so per-axis controlled embeddings
// (parallel / orthogonal / 45°) read clearly in the test body.
func vectorLiteral8From(v []float64) string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = strconv.FormatFloat(x, 'f', -1, 64)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// TestQueryEpisodicEventsByVector_OrdersByCosine pins the load-bearing
// invariant for the new pure-cosine event-grain tier: ranking is by
// cosine similarity alone, no FTS contribution. Three events are
// seeded with controlled embeddings against an 8-dim query vector
// aligned with axis 0:
//
//	A: same direction as the query (cos = 1)        -> rank 1
//	C: 45° away from the query    (cos ≈ 0.707)     -> rank 2
//	B: orthogonal to the query    (cos = 0)         -> rank 3
//
// If the SQL accidentally re-introduced an FTS clause or a merged
// scoring expression, B (which happens to share zero query terms)
// could ride on a tiebreaker — pinning the strict cos-only order
// catches that regression.
func TestQueryEpisodicEventsByVector_OrdersByCosine(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "8")
	require.NoError(t, repository.ApplyMigrations(t.Context(), pg.Pool))

	repo := repository.New(pg.Pool)

	userID := uuid.New()
	workspaceID := uuid.New()
	sessionID := uuid.New()

	// Sessions row first (FK on episodic.events.session_id).
	_, err := pg.Pool.Exec(t.Context(), `
		INSERT INTO episodic.sessions (id, user_id, started_at, event_count)
		VALUES ($1, $2, now(), 3)`, sessionID, userID)
	require.NoError(t, err)
	_, err = pg.Pool.Exec(t.Context(), `
		INSERT INTO episodic.session_workspaces (session_id, workspace_id)
		VALUES ($1, $2)`, sessionID, workspaceID)
	require.NoError(t, err)

	// Query and seed vectors are constructed in the (axis 0, axis 1)
	// plane (remaining 6 axes are zero). pgvector's cosine_ops compares
	// angle, not magnitude — so [1,0,...] vs [1,0,...] cos = 1, vs
	// [1,1,0,...] cos ≈ 0.707, vs [0,1,0,...] cos = 0.
	queryVec := []float64{1, 0, 0, 0, 0, 0, 0, 0}
	embA := []float64{1, 0, 0, 0, 0, 0, 0, 0} // parallel
	embB := []float64{0, 1, 0, 0, 0, 0, 0, 0} // orthogonal
	embC := []float64{1, 1, 0, 0, 0, 0, 0, 0} // 45°

	type seed struct {
		id  uuid.UUID
		emb []float64
		txt string
	}
	evA := seed{uuid.New(), embA, "alpha"}
	evB := seed{uuid.New(), embB, "bravo"}
	evC := seed{uuid.New(), embC, "charlie"}

	for i, e := range []seed{evA, evB, evC} {
		_, err := pg.Pool.Exec(t.Context(), `
			INSERT INTO episodic.events (id, session_id, user_id, type, text, raw_event, embedding, created_at)
			VALUES ($1, $2, $3, 'user', $4, '{}'::jsonb, ($5::text)::vector, now() + make_interval(secs => $6::float))`,
			e.id, sessionID, userID, e.txt, vectorLiteral8From(e.emb), float64(i)*0.001)
		require.NoError(t, err)
	}

	hits, err := repo.QueryEpisodicEventsByVector(t.Context(), repository.EpisodicVectorParams{
		UserID:         userID,
		WorkspaceID:    workspaceID,
		QueryEmbedding: queryVec,
		Limit:          10,
	})
	require.NoError(t, err)
	require.Len(t, hits, 3, "all three vector-bearing events match")

	// Strict cos-only order: A (parallel) > C (45°) > B (orthogonal).
	assert.Equal(t, evA.id, hits[0].Event.ID, "parallel vector ranks 1")
	assert.Equal(t, evC.id, hits[1].Event.ID, "45° vector ranks 2")
	assert.Equal(t, evB.id, hits[2].Event.ID, "orthogonal vector ranks 3")

	// Score sanity: Semantic monotonically decreasing, FTS == 0
	// across the board (FTS is not consulted on this tier), and
	// Merged tracks Semantic so callers see a single sort key.
	for i, h := range hits {
		assert.Equal(t, 0.0, h.FTS, "hit %d: FTS must be zero on pure-cosine tier", i)
		assert.Equal(t, h.Semantic, h.Merged, "hit %d: Merged must track Semantic", i)
	}
	assert.Greater(t, hits[0].Semantic, hits[1].Semantic, "parallel > 45°")
	assert.Greater(t, hits[1].Semantic, hits[2].Semantic, "45° > orthogonal")
}

// TestQueryEpisodicEventsByVector_WorkspaceIsolation pins the
// security boundary: an event whose session lives in workspace A must
// not surface for a query against workspace B, even when the user_id
// matches. Mirrors the analogous test on the session-vector tier —
// every recall path must honor workspace scoping.
func TestQueryEpisodicEventsByVector_WorkspaceIsolation(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "8")
	require.NoError(t, repository.ApplyMigrations(t.Context(), pg.Pool))

	repo := repository.New(pg.Pool)

	userID := uuid.New()
	wsA := uuid.New()
	wsB := uuid.New()
	sessA := uuid.New()
	evA := uuid.New()

	emb := []float64{1, 0, 0, 0, 0, 0, 0, 0}

	_, err := pg.Pool.Exec(t.Context(), `
		INSERT INTO episodic.sessions (id, user_id, started_at, event_count)
		VALUES ($1, $2, now(), 1)`, sessA, userID)
	require.NoError(t, err)
	_, err = pg.Pool.Exec(t.Context(), `
		INSERT INTO episodic.session_workspaces (session_id, workspace_id)
		VALUES ($1, $2)`, sessA, wsA)
	require.NoError(t, err)
	_, err = pg.Pool.Exec(t.Context(), `
		INSERT INTO episodic.events (id, session_id, user_id, type, text, raw_event, embedding, created_at)
		VALUES ($1, $2, $3, 'user', 'a', '{}'::jsonb, ($4::text)::vector, now())`,
		evA, sessA, userID, vectorLiteral8From(emb))
	require.NoError(t, err)

	queryVec := []float64{1, 0, 0, 0, 0, 0, 0, 0}

	// Query workspace B — workspace A's event must NOT leak.
	hits, err := repo.QueryEpisodicEventsByVector(t.Context(), repository.EpisodicVectorParams{
		UserID:         userID,
		WorkspaceID:    wsB,
		QueryEmbedding: queryVec,
		Limit:          10,
	})
	require.NoError(t, err)
	assert.Empty(t, hits, "workspace B query must not see workspace A's event")

	// Query workspace A — evA must show up.
	hits, err = repo.QueryEpisodicEventsByVector(t.Context(), repository.EpisodicVectorParams{
		UserID:         userID,
		WorkspaceID:    wsA,
		QueryEmbedding: queryVec,
		Limit:          10,
	})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, evA, hits[0].Event.ID)
}
