package consolidation_test

import (
	"context"
	"sort"
	"strings"
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

// TestApplyClusters_SplitDoesNotClobber guards the cluster-split case: one
// existing mneme owning {a,b,c,d} is re-clustered into {a,b} and {c,d}.
// Both new assignments overlap the SAME existing mneme, so a matcher that
// reads a single static snapshot reinforces it twice (last-write-wins),
// collapsing two clusters into one row. The working-set view must let the
// second cluster fall through to an insert, yielding two mnemes.
func TestApplyClusters_SplitDoesNotClobber(t *testing.T) {
	repo := newSemRepo(t)
	ctx := context.Background()
	ws := uuid.New()
	a, b, c, d := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	// One existing mneme owns all four members.
	_, err := repo.InsertMneme(ctx, ws, vec1024(0.1), []uuid.UUID{a, b, c, d})
	require.NoError(t, err)

	// A re-cluster SPLITS it into {a,b} and {c,d}.
	assigns := []consolidation.ClusterAssignment{
		{MemberIDs: []uuid.UUID{a, b}, Centroid: vec1024(0.2)},
		{MemberIDs: []uuid.UUID{c, d}, Centroid: vec1024(0.3)},
	}
	_, err = consolidation.ApplyClusters(ctx, repo, ws, assigns)
	require.NoError(t, err)

	got, err := repo.WorkspaceLevel1Mnemes(ctx, ws)
	require.NoError(t, err)
	require.Len(t, got, 2, "a split must yield two mnemes, not clobber one")

	// Memberships partition {a,b,c,d} into exactly {a,b} and {c,d}.
	sets := make(map[string]bool)
	for _, m := range got {
		sets[memberKey(m.MemberIDs)] = true
	}
	require.True(t, sets[memberKey([]uuid.UUID{a, b})], "one mneme keeps {a,b}")
	require.True(t, sets[memberKey([]uuid.UUID{c, d})], "the other holds {c,d}")
}

// memberKey renders a member set as an order-independent string key.
func memberKey(ids []uuid.UUID) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = id.String()
	}
	sort.Strings(s)
	return strings.Join(s, ",")
}
