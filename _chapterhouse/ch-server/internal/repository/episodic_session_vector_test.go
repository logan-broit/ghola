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

// vectorLiteral8 builds an 8-dim pgvector text literal — matches the
// EMBEDDING_DIM=8 the test fixtures use for fast HNSW seeds. Centralized
// here so the seed/query embeddings stay in sync if the dim ever shifts.
func vectorLiteral8(fill float64) string {
	parts := make([]string, 8)
	for i := range parts {
		parts[i] = strconv.FormatFloat(fill, 'f', -1, 64)
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// TestQueryEpisodicSessionVector_RanksNearestNeighborFirst pins the
// load-bearing invariant for the session-vector tier: a session whose
// l1_embedding is closer (cosine) to the query embedding ranks above
// one that's further. This is the reason the tier exists — paraphrase-
// style queries that miss per-event embeddings can hit on the
// session-level pooled embedding.
//
// Seeded with two sessions:
//   sess-near: l1_embedding ≈ query           -> high cosine similarity
//   sess-far:  l1_embedding orthogonal-ish    -> low cosine similarity
//
// Query at sess-near's vector should return both, with sess-near first.
func TestQueryEpisodicSessionVector_RanksNearestNeighborFirst(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "8")
	require.NoError(t, repository.ApplyMigrations(t.Context(), pg.Pool))

	repo := repository.New(pg.Pool)

	userID := uuid.New()
	workspaceID := uuid.New()
	sessNear := uuid.New()
	sessFar := uuid.New()

	// Two sessions with l1_embedding seeded directly. Cosine on
	// vec_cosine_ops compares angle, not magnitude, so we deliberately
	// pick orthogonal-ish fills (all-0.1 vs alternating-sign) to make
	// the ranking stable irrespective of dim/scale tweaks.
	nearLit := vectorLiteral8(0.1)
	// far: alternating sign — cos(near, far) << cos(near, near).
	farParts := make([]string, 8)
	for i := range farParts {
		v := 0.1
		if i%2 == 1 {
			v = -0.1
		}
		farParts[i] = strconv.FormatFloat(v, 'f', -1, 64)
	}
	farLit := "[" + strings.Join(farParts, ",") + "]"

	for _, s := range []struct {
		id  uuid.UUID
		emb string
		txt string
	}{
		{sessNear, nearLit, "near session chunk"},
		{sessFar, farLit, "far session chunk"},
	} {
		_, err := pg.Pool.Exec(t.Context(), `
			INSERT INTO episodic.sessions (id, user_id, started_at, event_count, l1_embedding, l1_chunk_text)
			VALUES ($1, $2, now(), 0, ($3::text)::vector, $4)`,
			s.id, userID, s.emb, s.txt)
		require.NoError(t, err)
		_, err = pg.Pool.Exec(t.Context(), `
			INSERT INTO episodic.session_workspaces (session_id, workspace_id)
			VALUES ($1, $2)`, s.id, workspaceID)
		require.NoError(t, err)
	}

	queryEmb := make([]float64, 8)
	for i := range queryEmb {
		queryEmb[i] = 0.1
	}

	hits, err := repo.QueryEpisodicSessionVector(t.Context(), repository.EpisodicSessionVectorParams{
		UserID:         userID,
		WorkspaceID:    workspaceID,
		QueryEmbedding: queryEmb,
		Limit:          10,
	})
	require.NoError(t, err)
	require.Len(t, hits, 2, "both sessions match (HNSW returns all neighbors)")
	assert.Equal(t, sessNear, hits[0].SessionID, "near session must rank first")
	assert.Equal(t, sessFar, hits[1].SessionID)

	// Cosine similarity is in [0, 2] for non-normalized vectors via
	// 1 - <=>. Near session's score must be strictly higher than far's.
	assert.Greater(t, hits[0].Score, hits[1].Score)
	// Chunk text is carried so cross-encoder rerank can score full
	// session text without an extra round-trip.
	assert.Equal(t, "near session chunk", hits[0].SessionChunkText)
}

// TestQueryEpisodicSessionVector_WorkspaceIsolation pins the
// load-bearing security boundary: a session with l1_embedding in
// workspace A must not be returned by a query against workspace B,
// even when the user_id matches. Without this check the new tier
// would silently leak cross-workspace, defeating the scoping primitive
// that the corpus-shape experiment established was load-bearing for
// recall quality (and the security primitive that the workspace-
// scoping PR just shipped).
func TestQueryEpisodicSessionVector_WorkspaceIsolation(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "8")
	require.NoError(t, repository.ApplyMigrations(t.Context(), pg.Pool))

	repo := repository.New(pg.Pool)

	userID := uuid.New()
	wsA := uuid.New()
	wsB := uuid.New()
	sessA := uuid.New()

	nearLit := vectorLiteral8(0.1)

	_, err := pg.Pool.Exec(t.Context(), `
		INSERT INTO episodic.sessions (id, user_id, started_at, event_count, l1_embedding, l1_chunk_text)
		VALUES ($1, $2, now(), 0, ($3::text)::vector, 'A-chunk')`,
		sessA, userID, nearLit)
	require.NoError(t, err)
	_, err = pg.Pool.Exec(t.Context(), `
		INSERT INTO episodic.session_workspaces (session_id, workspace_id)
		VALUES ($1, $2)`, sessA, wsA)
	require.NoError(t, err)

	queryEmb := make([]float64, 8)
	for i := range queryEmb {
		queryEmb[i] = 0.1
	}

	// Query workspace B — sessA in workspace A must NOT leak.
	hits, err := repo.QueryEpisodicSessionVector(t.Context(), repository.EpisodicSessionVectorParams{
		UserID:         userID,
		WorkspaceID:    wsB,
		QueryEmbedding: queryEmb,
		Limit:          10,
	})
	require.NoError(t, err)
	assert.Empty(t, hits, "workspace B query must not see workspace A's session")

	// Query workspace A — sessA must show up.
	hits, err = repo.QueryEpisodicSessionVector(t.Context(), repository.EpisodicSessionVectorParams{
		UserID:         userID,
		WorkspaceID:    wsA,
		QueryEmbedding: queryEmb,
		Limit:          10,
	})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, sessA, hits[0].SessionID)
}

// TestQueryEpisodicSessionVector_ScopedToCaller verifies the per-user
// ACL: another user's matching session must not appear, even when the
// workspace_id matches (overlapping workspaces are common in homelab/
// team setups).
func TestQueryEpisodicSessionVector_ScopedToCaller(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "8")
	require.NoError(t, repository.ApplyMigrations(t.Context(), pg.Pool))

	repo := repository.New(pg.Pool)

	owner := uuid.New()
	stranger := uuid.New()
	workspaceID := uuid.New()
	ownerSess := uuid.New()
	strangerSess := uuid.New()

	nearLit := vectorLiteral8(0.1)

	for _, s := range []struct {
		id, user uuid.UUID
		txt      string
	}{
		{ownerSess, owner, "owner-chunk"},
		{strangerSess, stranger, "stranger-chunk"},
	} {
		_, err := pg.Pool.Exec(t.Context(), `
			INSERT INTO episodic.sessions (id, user_id, started_at, event_count, l1_embedding, l1_chunk_text)
			VALUES ($1, $2, now(), 0, ($3::text)::vector, $4)`,
			s.id, s.user, nearLit, s.txt)
		require.NoError(t, err)
		_, err = pg.Pool.Exec(t.Context(), `
			INSERT INTO episodic.session_workspaces (session_id, workspace_id)
			VALUES ($1, $2)`, s.id, workspaceID)
		require.NoError(t, err)
	}

	queryEmb := make([]float64, 8)
	for i := range queryEmb {
		queryEmb[i] = 0.1
	}

	hits, err := repo.QueryEpisodicSessionVector(t.Context(), repository.EpisodicSessionVectorParams{
		UserID:         owner,
		WorkspaceID:    workspaceID,
		QueryEmbedding: queryEmb,
		Limit:          10,
	})
	require.NoError(t, err)
	require.Len(t, hits, 1, "stranger's session must not leak to owner's query")
	assert.Equal(t, ownerSess, hits[0].SessionID)
}

// TestQueryEpisodicSessionVector_SkipsNullEmbedding pins that sessions
// without a populated l1_embedding (mentat hasn't run yet, or session
// is still open) are excluded from the result set instead of NULL-
// scored. The HNSW index is partial WHERE l1_embedding IS NOT NULL;
// the SQL must mirror that gate.
func TestQueryEpisodicSessionVector_SkipsNullEmbedding(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "8")
	require.NoError(t, repository.ApplyMigrations(t.Context(), pg.Pool))

	repo := repository.New(pg.Pool)

	userID := uuid.New()
	workspaceID := uuid.New()
	sessNoEmbed := uuid.New()

	// Session row with NO l1_embedding (mentat hasn't run yet).
	_, err := pg.Pool.Exec(t.Context(), `
		INSERT INTO episodic.sessions (id, user_id, started_at, event_count)
		VALUES ($1, $2, now(), 0)`, sessNoEmbed, userID)
	require.NoError(t, err)
	_, err = pg.Pool.Exec(t.Context(), `
		INSERT INTO episodic.session_workspaces (session_id, workspace_id)
		VALUES ($1, $2)`, sessNoEmbed, workspaceID)
	require.NoError(t, err)

	queryEmb := make([]float64, 8)
	for i := range queryEmb {
		queryEmb[i] = 0.1
	}

	hits, err := repo.QueryEpisodicSessionVector(t.Context(), repository.EpisodicSessionVectorParams{
		UserID:         userID,
		WorkspaceID:    workspaceID,
		QueryEmbedding: queryEmb,
		Limit:          10,
	})
	require.NoError(t, err)
	assert.Empty(t, hits, "session without l1_embedding must not surface")
}
