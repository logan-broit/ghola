package primitives_test

// fusion_test.go — pure-function tests for the Score combiner.
//
// No PG, no goroutines, no mocks. Score is a linear combination of
// per-primitive inputs and weights; tests exercise the default-weights
// path, the all-zero short-circuit, and weighted blending.

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/primitives"
)

func TestScore_HebbianOnly_ReturnsHebbian(t *testing.T) {
	parts := primitives.ScoreParts{Hebbian: 0.7, ACTR: 0.0, Bayesian: 0.0, Ebbinghaus: 0.0}
	weights := primitives.DefaultWeights() // {Hebbian:1.0, others:0.0}

	score := primitives.Score(parts, weights)

	require.InDelta(t, 0.7, score, 1e-9)
}

func TestScore_AllZero_ReturnsZero(t *testing.T) {
	score := primitives.Score(primitives.ScoreParts{}, primitives.DefaultWeights())

	require.Equal(t, 0.0, score)
}

func TestScore_WeightsAreApplied(t *testing.T) {
	parts := primitives.ScoreParts{Hebbian: 1.0, ACTR: 1.0}
	weights := primitives.ScoreWeights{Hebbian: 0.6, ACTR: 0.4}

	score := primitives.Score(parts, weights)

	require.InDelta(t, 1.0, score, 1e-9) // 0.6*1.0 + 0.4*1.0 = 1.0
}
