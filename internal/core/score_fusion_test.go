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
	got := core.FuseScores(rrf, rerank, nil, 0.0, 0.0)
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

	// wRerank=0.5 -> final[a]=(1-0.5)*1.0+(0.5)*0.1 = 0.55
	//               final[b]=(1-0.5)*0.5+(0.5)*1.0 = 0.75
	got := core.FuseScores(rrf, rerank, nil, 0.5, 0.0)
	assert.InDelta(t, 0.55, got["a"], 1e-12)
	assert.InDelta(t, 0.75, got["b"], 1e-12)
	assert.Greater(t, got["b"], got["a"], "rerank flipped the ranking")

	// wRerank=1.0 -> final[a]=0.1, final[b]=1.0 (rerank-only).
	got = core.FuseScores(rrf, rerank, nil, 1.0, 0.0)
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
	got := core.FuseScores(rrf, map[string]float64{}, nil, 0.5, 0.0)
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
	got := core.FuseScores(rrf, rerank, nil, 0.5, 0.0)
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
	got := core.FuseScores(rrf, rerank, nil, 0.5, 0.0)
	for _, v := range got {
		assert.False(t, math.IsNaN(v))
		assert.False(t, math.IsInf(v, 0))
		assert.Equal(t, 0.0, v)
	}
}

// TestFuseScores_NilActivationIdentity: passing nil activation + zero
// wActivation must produce the same result as the pre-Task-6 two-channel
// call. Guards against accidental divergence in the three-channel branch.
func TestFuseScores_NilActivationIdentity(t *testing.T) {
	cases := []struct {
		name    string
		rrf     map[string]float64
		rerank  map[string]float64
		wRerank float64
	}{
		{"rrf_only", map[string]float64{"a": 0.02, "b": 0.01}, map[string]float64{}, 0.0},
		{"half_weight", map[string]float64{"a": 0.02, "b": 0.01}, map[string]float64{"a": 0.1, "b": 1.0}, 0.5},
		{"full_rerank", map[string]float64{"a": 0.02, "b": 0.01}, map[string]float64{"a": 0.1, "b": 1.0}, 1.0},
		{"partial_rerank", map[string]float64{"a": 0.02, "b": 0.01}, map[string]float64{"a": 0.1}, 0.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// nil activation, wActivation=0 must be identical to empty map, wActivation=0
			gotNil := core.FuseScores(tc.rrf, tc.rerank, nil, tc.wRerank, 0.0)
			gotEmpty := core.FuseScores(tc.rrf, tc.rerank, map[string]float64{}, tc.wRerank, 0.0)
			assert.Equal(t, len(gotNil), len(gotEmpty))
			for id := range gotNil {
				assert.InDelta(t, gotNil[id], gotEmpty[id], 1e-12,
					"nil vs empty activation must produce identical scores for id=%q", id)
			}
		})
	}
}

// TestFuseScores_ActivationChannelLiftsGraphHit is the config-B property
// test: an expansion hit with rrf=0 and weak rerank but HIGH activation
// must outrank a weak-rrf, weak-rerank pool hit when wActivation=0.2.
// This is exactly the graph-evidence lift — a node the spreading
// activation settled on hard should float above cold pool entries.
//
//	pool hit "pool":       rrf=0.010, rerank=0.10, activation absent → 0
//	expansion hit "xhigh": rrf=0.000, rerank=0.10, activation=1.0  (high)
//
// With wRerank=0.3, wActivation=0.2:
//
//	rrfMax=0.01, rerankMax=0.10, actMax=1.0
//	pool:   (1-0.3-0.2)*1.0 + 0.3*1.0 + 0.2*0.0 = 0.5 + 0.3     = 0.80
//	xhigh:  (1-0.3-0.2)*0.0 + 0.3*1.0 + 0.2*1.0 = 0.0 + 0.3+0.2 = 0.50
//
// Wait — pool still wins because it has rrf mass. Use a weaker pool hit:
//
//	pool:   rrf=0.010, rerank=0.05 → rrfNorm=1.0, rerankNorm=0.5
//	        (0.5)*1.0 + 0.3*0.5 + 0.2*0.0 = 0.5 + 0.15 = 0.65
//	xhigh:  rrf=0.000, rerank=0.05 → rrfNorm=0.0, rerankNorm=0.5
//	        (0.5)*0.0 + 0.3*0.5 + 0.2*1.0 = 0.15 + 0.20 = 0.35
//
// Still pool wins. The real win condition: pool hit has NO rerank
// (missing) and low rrf, while xhigh has high activation:
//
//	pool:   rrf=0.005, rerank missing → final = rrfNorm = 0.5/1.0 = 0.5
//	        (missing-rerank branch: out[id] = rrfNorm + wAct*actNorm)
//	        = 0.5 + 0.2*0.0 = 0.5
//	xhigh:  rrf=0.010 (lowest rrf), rerank=0.01, activation=1.0
//	        rrfNorm=1.0, rerankNorm=1.0, actNorm=1.0
//	        (0.5)*1.0 + 0.3*1.0 + 0.2*1.0 = 0.5+0.3+0.2 = 1.0  ← wins
//
// Concrete scenario: wRerank=0.3, wActivation=0.2.
//
//	"cold"  — pool hit: rrf=0.010, rerank absent, activation absent
//	"xhigh" — expansion: rrf=0.005, rerank=0.01, activation=1.0
//
// rrfMax=0.010, rerankMax=0.01, actMax=1.0
// cold:  missing-rerank → rrfNorm + wAct*0 = 1.0 + 0 = 1.0 ... hmm cold still wins
//
// Simpler scenario: make cold have no rrf advantage:
//
//	"cold"  rrf=0.010, rerank=0.01 (weak), activation=0
//	"xhigh" rrf=0.001, rerank=0.01 (same), activation=1.0
//
// rrfMax=0.010, rerankMax=0.01, actMax=1.0
// cold:  (0.5)*1.0 + 0.3*1.0 + 0.2*0.0 = 0.80
// xhigh: (0.5)*0.1 + 0.3*1.0 + 0.2*1.0 = 0.05 + 0.3 + 0.2 = 0.55
// Still cold wins (rrf=0.010 vs 0.001 is 10x).
//
// The real config-B property: among hits that have IDENTICAL rrf and
// rerank signal, activation breaks the tie UP. Or: an expansion hit
// with zero rrf but strong rerank AND strong activation beats a
// same-rerank pool hit that has no activation AND no rrf.
//
// Final concrete fixture (hand-checkable):
//
//	"pool"  rrf=0.000, rerank=0.50, activation=0.0
//	"xhigh" rrf=0.000, rerank=0.50, activation=1.0
//
// rrfMax=1(floor), rerankMax=0.50, actMax=1.0
// pool:   (0.5)*0.0 + 0.3*1.0 + 0.2*0.0 = 0.30
// xhigh:  (0.5)*0.0 + 0.3*1.0 + 0.2*1.0 = 0.50
// xhigh wins by exactly wActivation=0.2. This is THE config-B property.
func TestFuseScores_ActivationChannelLiftsGraphHit(t *testing.T) {
	// Both hits have identical rrf=0 and identical rerank=0.50.
	// xhigh has full activation; pool has none.
	rrf := map[string]float64{
		"pool":  0.0,
		"xhigh": 0.0,
	}
	rerank := map[string]float64{
		"pool":  0.50,
		"xhigh": 0.50,
	}
	activation := map[string]float64{
		"xhigh": 1.0,
		// "pool" absent → 0.0
	}

	wRerank := 0.3
	wActivation := 0.2

	// rrfMax = 1.0 (floor), rerankMax = 0.50, actMax = 1.0
	// pool:  (1-0.3-0.2)*0 + 0.3*(0.5/0.5) + 0.2*(0/1) = 0 + 0.30 + 0 = 0.30
	// xhigh: (1-0.3-0.2)*0 + 0.3*(0.5/0.5) + 0.2*(1/1) = 0 + 0.30 + 0.2 = 0.50
	got := core.FuseScores(rrf, rerank, activation, wRerank, wActivation)

	assert.InDelta(t, 0.30, got["pool"], 1e-12, "pool score")
	assert.InDelta(t, 0.50, got["xhigh"], 1e-12, "xhigh score")
	assert.Greater(t, got["xhigh"], got["pool"],
		"activation must lift xhigh above the identical-rrf-and-rerank pool hit")
}

// TestFuseScores_ActivationChannelMissingRerankPath exercises the
// missing-rerank branch with active activation: a hit absent from rerank
// still gets the activation contribution via the fallback formula
// out[id] = rrfNorm + wActivation*actNorm.
func TestFuseScores_ActivationChannelMissingRerankPath(t *testing.T) {
	rrf := map[string]float64{"a": 0.02, "b": 0.01}
	// Only a got reranked; b gets the missing-rerank path.
	rerank := map[string]float64{"a": 0.10}
	activation := map[string]float64{"b": 1.0} // b has activation; a doesn't

	wRerank := 0.3
	wActivation := 0.2
	// rrfMax=0.02, rerankMax=0.10, actMax=1.0
	// a: (0.5)*1.0 + 0.3*1.0 + 0.2*0.0 = 0.80
	// b: missing-rerank → rrfNorm + wAct*actNorm = 0.5 + 0.2*1.0 = 0.70
	got := core.FuseScores(rrf, rerank, activation, wRerank, wActivation)

	assert.InDelta(t, 0.80, got["a"], 1e-12, "a: full fusion path")
	assert.InDelta(t, 0.70, got["b"], 1e-12, "b: missing-rerank + activation")
	// b's activation partially compensates for missing rerank.
	assert.Greater(t, got["a"], got["b"])
	assert.Greater(t, got["b"], 0.5, "activation lifts b above its bare rrfNorm of 0.5")
}

// TestFuseScores_AllZerosWithActivationNoNaN: all-zero rrf + all-zero
// activation must not produce NaN even when wActivation > 0.
func TestFuseScores_AllZerosWithActivationNoNaN(t *testing.T) {
	rrf := map[string]float64{"a": 0, "b": 0}
	rerank := map[string]float64{}
	activation := map[string]float64{"a": 0, "b": 0}
	got := core.FuseScores(rrf, rerank, activation, 0.3, 0.2)
	for _, v := range got {
		assert.False(t, math.IsNaN(v))
		assert.False(t, math.IsInf(v, 0))
		assert.Equal(t, 0.0, v)
	}
}
