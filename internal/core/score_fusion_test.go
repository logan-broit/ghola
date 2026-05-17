package core_test

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/logan-broit/ghola/internal/core"
)

// TestFuseScores_NoRerank pins the weight=0 degenerate case: rerank
// scores are ignored entirely and final ranking is RRF-normalized.
// Used when the operator wants to ablate cross-encoder contribution
// without unsetting Truthsayer.
func TestFuseScores_NoRerank(t *testing.T) {
	rrf := map[string]float64{"a": 0.020, "b": 0.010, "c": 0.005}
	rerank := map[string]float64{"a": 0.01, "b": 0.99, "c": 0.50}
	got := core.FuseScores(rrf, rerank, 0.0)
	require := assert.New(t)
	// rrfMax=0.02; rrf_norm: a=1.0, b=0.5, c=0.25
	require.InDelta(1.0, got["a"], 1e-12)
	require.InDelta(0.5, got["b"], 1e-12)
	require.InDelta(0.25, got["c"], 1e-12)
	// Rerank ignored: the b/c rerank winners do NOT promote.
	require.Greater(got["a"], got["b"])
	require.Greater(got["b"], got["c"])
}

// TestFuseScores_NormalFusion exercises the documented blend: a
// cross-encoder-favored doc rises past an RRF-favored one when the
// weighted sum tips the balance. Concrete numbers chosen so the
// arithmetic is hand-checkable.
func TestFuseScores_NormalFusion(t *testing.T) {
	// rrf: a wins (2x), but b's rerank dominates 10:1.
	rrf := map[string]float64{"a": 0.020, "b": 0.010}
	rerank := map[string]float64{"a": 0.10, "b": 1.00}

	// w=0.5 -> final[a]=(1-0.5)*1.0+(0.5)*0.1 = 0.55
	//          final[b]=(1-0.5)*0.5+(0.5)*1.0 = 0.75
	got := core.FuseScores(rrf, rerank, 0.5)
	assert.InDelta(t, 0.55, got["a"], 1e-12)
	assert.InDelta(t, 0.75, got["b"], 1e-12)
	assert.Greater(t, got["b"], got["a"], "rerank flipped the ranking")

	// w=1.0 -> final[a]=0.1, final[b]=1.0 (rerank-only).
	got = core.FuseScores(rrf, rerank, 1.0)
	assert.InDelta(t, 0.10, got["a"], 1e-12)
	assert.InDelta(t, 1.00, got["b"], 1e-12)
}

// TestFuseScores_RerankDownFallback simulates the case where the
// cross-encoder service is unreachable and the recall path passes an
// empty rerank map. Every id is missing from rerank → FuseScores
// trusts the RRF prior and uses rrfNorm directly. No half-weighted
// penalty for hits the reranker never got to score.
func TestFuseScores_RerankDownFallback(t *testing.T) {
	rrf := map[string]float64{"a": 0.020, "b": 0.010, "c": 0.005}
	got := core.FuseScores(rrf, map[string]float64{}, 0.5)
	require := assert.New(t)
	// rrfMax=0.02 → rrfNorm: a=1.0, b=0.5, c=0.25. With rerank empty
	// every id falls into the missing-rerank branch → final == rrfNorm.
	require.InDelta(1.00, got["a"], 1e-12)
	require.InDelta(0.50, got["b"], 1e-12)
	require.InDelta(0.25, got["c"], 1e-12)
	require.Greater(got["a"], got["b"])
	require.Greater(got["b"], got["c"])
}

// TestFuseScores_PartialRerankUsesRRFForMissing pins the load-bearing
// case PR4 surfaced: when the rerank pool is a strict subset of the
// RRF pool (e.g., semantic mnemes have no Content for the cross-
// encoder to score), missing-rerank entries must use rrfNorm rather
// than zero. Without this, no-content tiers get systematically pushed
// out of the top-N output despite their RRF rank.
func TestFuseScores_PartialRerankUsesRRFForMissing(t *testing.T) {
	rrf := map[string]float64{"a": 0.020, "b": 0.010}
	// Only a got reranked — b had no text to score.
	rerank := map[string]float64{"a": 0.10}
	got := core.FuseScores(rrf, rerank, 0.5)
	// rrfMax=0.02 → rrfNorm: a=1.0, b=0.5
	// rerankMax=0.10 → rerankNorm: a=1.0
	// a (rerank present): 0.5*1.0 + 0.5*1.0 = 1.0
	// b (rerank missing): rrfNorm = 0.5
	assert.InDelta(t, 1.0, got["a"], 1e-12)
	assert.InDelta(t, 0.5, got["b"], 1e-12)
	// b sits below a but is preserved at half-of-a, not zeroed out.
	assert.Greater(t, got["a"], got["b"])
	assert.Greater(t, got["b"], 0.0)
}

// TestFuseScores_AllZerosNoNaN nails the edge case: a workspace where
// every RRF score is zero (impossible in practice — RRF can't produce
// zero unless tiers are all empty — but defensive against a
// pathological input).
func TestFuseScores_AllZerosNoNaN(t *testing.T) {
	rrf := map[string]float64{"a": 0, "b": 0}
	rerank := map[string]float64{}
	got := core.FuseScores(rrf, rerank, 0.5)
	for _, v := range got {
		assert.False(t, math.IsNaN(v))
		assert.False(t, math.IsInf(v, 0))
		assert.Equal(t, 0.0, v)
	}
}
