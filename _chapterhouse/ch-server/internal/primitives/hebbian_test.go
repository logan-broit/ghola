package primitives_test

// hebbian_test.go — pure-function tests for BoostsFor.
//
// No PG, no goroutines, no mocks. Each test fabricates uuid.UUID values
// and an in-memory map[uuid.UUID][]repository.Association and asserts
// the boost map shape.

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/primitives"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
)

func TestBoostsFor_NoAssociations_ReturnsZeros(t *testing.T) {
	eA, eB, eC := uuid.New(), uuid.New(), uuid.New()
	candidates := []uuid.UUID{eA, eB, eC}

	boosts := primitives.BoostsFor(candidates, map[uuid.UUID][]repository.Association{})

	require.Equal(t, 0.0, boosts[eA])
	require.Equal(t, 0.0, boosts[eB])
	require.Equal(t, 0.0, boosts[eC])
}

func TestBoostsFor_SymmetricLink_BoostsBoth(t *testing.T) {
	eA, eB := uuid.New(), uuid.New()
	candidates := []uuid.UUID{eA, eB}
	assoc := map[uuid.UUID][]repository.Association{
		eA: {{DstEventID: eB, Weight: 0.5}},
		eB: {{DstEventID: eA, Weight: 0.5}},
	}

	boosts := primitives.BoostsFor(candidates, assoc)

	require.InDelta(t, 0.5, boosts[eA], 1e-9)
	require.InDelta(t, 0.5, boosts[eB], 1e-9)
}

func TestBoostsFor_LinkToOutsideCandidate_NoBoost(t *testing.T) {
	eA := uuid.New()
	eOutside := uuid.New() // intentionally not in candidates
	candidates := []uuid.UUID{eA}
	assoc := map[uuid.UUID][]repository.Association{
		eA: {{DstEventID: eOutside, Weight: 0.9}},
	}

	boosts := primitives.BoostsFor(candidates, assoc)

	require.Equal(t, 0.0, boosts[eA])
}

func TestBoostsFor_MultipleLinks_SumsWeights(t *testing.T) {
	eA, eB, eC := uuid.New(), uuid.New(), uuid.New()
	candidates := []uuid.UUID{eA, eB, eC}
	assoc := map[uuid.UUID][]repository.Association{
		eA: {
			{DstEventID: eB, Weight: 0.3},
			{DstEventID: eC, Weight: 0.4},
		},
	}

	boosts := primitives.BoostsFor(candidates, assoc)

	require.InDelta(t, 0.7, boosts[eA], 1e-9)
}
