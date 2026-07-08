package repository_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

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

func TestInsertAndReadLevel1Mnemes(t *testing.T) {
	repo := newSemRepo(t)
	ctx := context.Background()
	ws := uuid.New()
	m1 := uuid.New()
	m2 := uuid.New()

	id, err := repo.InsertMneme(ctx, ws, vec1024(0.1), []uuid.UUID{m1, m2})
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, id)

	got, err := repo.WorkspaceLevel1Mnemes(ctx, ws)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, id, got[0].ID)
	require.ElementsMatch(t, []uuid.UUID{m1, m2}, got[0].MemberIDs)
	require.InDelta(t, 0.5, got[0].Confidence, 1e-9)
}

func TestReinforceMnemeBumpsConfidenceAndCaps(t *testing.T) {
	repo := newSemRepo(t)
	ctx := context.Background()
	ws := uuid.New()
	m1 := uuid.New()

	id, err := repo.InsertMneme(ctx, ws, vec1024(0.1), []uuid.UUID{m1})
	require.NoError(t, err)

	// Reinforce 12x — confidence should climb by 0.05 and cap at 0.99.
	newMembers := []uuid.UUID{m1, uuid.New()}
	for i := 0; i < 12; i++ {
		require.NoError(t, repo.ReinforceMneme(ctx, id, vec1024(0.2), newMembers))
	}
	got, err := repo.WorkspaceLevel1Mnemes(ctx, ws)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.InDelta(t, 0.99, got[0].Confidence, 1e-9, "confidence caps at 0.99")
	require.ElementsMatch(t, newMembers, got[0].MemberIDs)
}
