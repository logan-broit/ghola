package consolidation_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/consolidation"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"
)

func newSemRepo(t *testing.T) *repository.Repository {
	t.Helper()
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "1024")
	require.NoError(t, repository.ApplyMigrations(context.Background(), pg.Pool))
	return repository.New(pg.Pool)
}

func vec1024(fill float32) []float32 {
	v := make([]float32, 1024)
	for i := range v {
		v[i] = fill
	}
	return v
}

func TestApplyClusters_InsertsThenReinforcesOnlyWhenMembershipChanged(t *testing.T) {
	repo := newSemRepo(t)
	ctx := context.Background()
	ws := uuid.New()
	a, b, c := uuid.New(), uuid.New(), uuid.New()

	// First run: one cluster {a,b} -> insert.
	assigns := []consolidation.ClusterAssignment{
		{MemberIDs: []uuid.UUID{a, b}, Centroid: vec1024(0.1)},
	}
	n, err := consolidation.ApplyClusters(ctx, repo, ws, assigns)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	got, err := repo.WorkspaceLevel1Mnemes(ctx, ws)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.InDelta(t, 0.5, got[0].Confidence, 1e-9)

	// Second run, SAME membership -> no reinforcement (idempotent).
	n, err = consolidation.ApplyClusters(ctx, repo, ws, assigns)
	require.NoError(t, err)
	require.Equal(t, 0, n, "unchanged membership must not reinforce")
	got, _ = repo.WorkspaceLevel1Mnemes(ctx, ws)
	require.InDelta(t, 0.5, got[0].Confidence, 1e-9, "confidence unchanged on no-op re-run")

	// Third run, membership grows to {a,b,c} -> reinforce (overlap w/ existing).
	assigns2 := []consolidation.ClusterAssignment{
		{MemberIDs: []uuid.UUID{a, b, c}, Centroid: vec1024(0.2)},
	}
	n, err = consolidation.ApplyClusters(ctx, repo, ws, assigns2)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	got, _ = repo.WorkspaceLevel1Mnemes(ctx, ws)
	require.Len(t, got, 1, "reinforce must not create a duplicate")
	require.InDelta(t, 0.55, got[0].Confidence, 1e-9)
}
