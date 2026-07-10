package consolidation_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/consolidation"
)

func TestSelectRepresentatives_DeterministicAndBounded(t *testing.T) {
	// 6 candidates in 2D; centroid at origin-ish. k=4.
	cands := []consolidation.Candidate{
		{ID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Embedding: []float32{1, 0}},
		{ID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), Embedding: []float32{0.9, 0.1}},
		{ID: uuid.MustParse("00000000-0000-0000-0000-000000000003"), Embedding: []float32{0, 1}},
		{ID: uuid.MustParse("00000000-0000-0000-0000-000000000004"), Embedding: []float32{-1, 0}},
		{ID: uuid.MustParse("00000000-0000-0000-0000-000000000005"), Embedding: []float32{0, -1}},
		{ID: uuid.MustParse("00000000-0000-0000-0000-000000000006"), Embedding: []float32{0.8, 0.2}},
	}
	centroid := []float32{0.5, 0.5}

	got1 := consolidation.SelectRepresentatives(cands, centroid, 4)
	require.Len(t, got1, 4, "returns exactly k")
	// Determinism: identical input -> identical output order.
	got2 := consolidation.SelectRepresentatives(cands, centroid, 4)
	require.Equal(t, ids(got1), ids(got2))
	// k > len -> returns all, no panic.
	all := consolidation.SelectRepresentatives(cands[:2], centroid, 4)
	require.Len(t, all, 2)
	// empty -> empty, no panic.
	require.Empty(t, consolidation.SelectRepresentatives(nil, centroid, 4))
}

func ids(cs []consolidation.Candidate) []uuid.UUID {
	out := make([]uuid.UUID, len(cs))
	for i, c := range cs {
		out[i] = c.ID
	}
	return out
}
