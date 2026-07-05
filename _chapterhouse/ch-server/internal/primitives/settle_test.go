package primitives_test

// settle_test.go — pure-function tests for the fixed-point spreading
// activation core (Settle + TopExpansion).
//
// No PG, no goroutines, no mocks. Fixtures are literal in-memory
// SettleGraph values; UUIDs are constructed from fixed bytes where a
// test needs a known lexical order (tie-breaking / determinism), and
// via uuid.New() where identity alone matters.

import (
	"math/rand"
	"sort"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/primitives"
)

// fixedUUID builds a deterministic UUID whose string form sorts by the
// leading byte, so tests can reason about tie-break order without
// relying on uuid.New()'s randomness.
func fixedUUID(b byte) uuid.UUID {
	var u uuid.UUID
	u[0] = b
	return u
}

// symNorm takes an undirected edge list (src, dst, weight) and returns
// a SettleGraph that is symmetrized (both directions present) and
// row-normalized (each node's outgoing weights sum to <= 1). This
// mirrors what the caller-side neighborhood builder is specified to do,
// so every fixture below can be written as plain undirected edges.
func symNorm(nodes []uuid.UUID, edges [][3]interface{}) primitives.SettleGraph {
	// Accumulate symmetric raw weights.
	raw := make(map[uuid.UUID]map[uuid.UUID]float64)
	for _, n := range nodes {
		raw[n] = make(map[uuid.UUID]float64)
	}
	for _, e := range edges {
		a := e[0].(uuid.UUID)
		b := e[1].(uuid.UUID)
		w := e[2].(float64)
		raw[a][b] += w
		raw[b][a] += w
	}
	// Row-normalize.
	adj := make(map[uuid.UUID][]primitives.SettleEdge, len(nodes))
	for _, n := range nodes {
		var sum float64
		for _, w := range raw[n] {
			sum += w
		}
		// Deterministic edge ordering by dst string so the fixture is
		// stable regardless of map iteration order.
		dsts := make([]uuid.UUID, 0, len(raw[n]))
		for d := range raw[n] {
			dsts = append(dsts, d)
		}
		sort.Slice(dsts, func(i, j int) bool { return dsts[i].String() < dsts[j].String() })
		out := make([]primitives.SettleEdge, 0, len(dsts))
		for _, d := range dsts {
			w := raw[n][d]
			if sum > 0 {
				w /= sum
			}
			out = append(out, primitives.SettleEdge{Dst: d, W: w})
		}
		adj[n] = out
	}
	return primitives.SettleGraph{Adj: adj}
}

// TestSettleConvergesOnChain: A->B->C symmetrized, seed A=1.0.
// Expect genuine Eps-convergence — the normal exit path, strictly
// before the MaxIters hard stop (iterations > 1 and < MaxIters) — a
// monotone activation gradient A > B > C, and all-positive mass:
// activation flows down the chain and decays with distance.
//
// A bare 3-node chain is a pathologically slow mixer: with default
// Lambda=0.7 the L1 delta only crosses Eps=1e-6 around iteration 41,
// so the design default of 20 iterations would hard-stop here. Real
// neighborhoods are densely connected and mix in a handful of
// iterations; to exercise the convergence path (not the hard stop) on
// this deliberately sparse fixture we give it the headroom to converge.
func TestSettleConvergesOnChain(t *testing.T) {
	a := fixedUUID(0x01)
	b := fixedUUID(0x02)
	c := fixedUUID(0x03)
	nodes := []uuid.UUID{a, b, c}
	g := symNorm(nodes, [][3]interface{}{
		{a, b, 1.0},
		{b, c, 1.0},
	})

	seeds := map[uuid.UUID]float64{a: 1.0}
	p := primitives.DefaultSettleParams()
	p.MaxIters = 100 // headroom so the sparse chain reaches Eps-convergence

	act, iters := primitives.Settle(seeds, g, p)

	require.Greater(t, iters, 1, "should take more than one iteration to propagate down the chain")
	require.Less(t, iters, p.MaxIters, "must Eps-converge strictly before the hard stop")

	require.Greater(t, act[a], act[b], "seed node holds the most mass")
	require.Greater(t, act[b], act[c], "activation decays with hop distance")
	require.Greater(t, act[a], 0.0)
	require.Greater(t, act[b], 0.0)
	require.Greater(t, act[c], 0.0)
}

// buildDeterministicFixture returns a ~12-node graph plus a seed set.
// Edges and node identities are fully fixed (no RNG), so two Settle
// runs must produce byte-identical maps and TopExpansion must produce
// identical slices.
func buildDeterministicFixture() ([]uuid.UUID, primitives.SettleGraph, map[uuid.UUID]float64) {
	n := make([]uuid.UUID, 12)
	for i := range n {
		n[i] = fixedUUID(byte(0x10 + i))
	}
	edges := [][3]interface{}{
		{n[0], n[1], 0.9},
		{n[1], n[2], 0.8},
		{n[2], n[3], 0.7},
		{n[0], n[3], 0.4},
		{n[3], n[4], 0.6},
		{n[4], n[5], 0.5},
		{n[5], n[6], 0.5},
		{n[6], n[7], 0.5},
		{n[2], n[8], 0.3},
		{n[8], n[9], 0.3},
		{n[9], n[10], 0.3},
		{n[10], n[11], 0.3},
		{n[7], n[11], 0.2},
	}
	g := symNorm(n, edges)
	// Two seeds sharing equal mass.
	seeds := map[uuid.UUID]float64{n[0]: 0.5, n[1]: 0.5}
	return n, g, seeds
}

// TestSettleDeterministic: identical inputs -> identical outputs across
// two runs, both for the activation map and for TopExpansion (including
// an engineered exact tie broken by UUID string order).
func TestSettleDeterministic(t *testing.T) {
	_, g, seeds := buildDeterministicFixture()
	p := primitives.DefaultSettleParams()

	act1, it1 := primitives.Settle(seeds, g, p)
	act2, it2 := primitives.Settle(seeds, g, p)

	require.Equal(t, it1, it2, "iteration count must be reproducible")
	require.Equal(t, act1, act2, "activation map must be reproducible bit-for-bit")

	exp1 := primitives.TopExpansion(act1, seeds, p.TopM)
	exp2 := primitives.TopExpansion(act2, seeds, p.TopM)
	require.Equal(t, exp1, exp2, "expansion slice must be reproducible")

	// Engineered exact tie: two non-seed nodes with byte-identical
	// activation must be ordered by UUID string ascending.
	tieHi := fixedUUID(0xF0)
	tieLo := fixedUUID(0xA0)
	seed := fixedUUID(0x01)
	tieAct := map[uuid.UUID]float64{
		seed:  1.0,
		tieHi: 0.5,
		tieLo: 0.5, // exact tie with tieHi
	}
	tieSeeds := map[uuid.UUID]float64{seed: 1.0}
	got := primitives.TopExpansion(tieAct, tieSeeds, 10)
	require.Equal(t, []uuid.UUID{tieLo, tieHi}, got,
		"exact activation ties break by UUID string ascending (0xA0.. before 0xF0..)")
}

// TestSettleSeedWeighting: two seeds X=0.9, Y=0.1, each with one
// private neighbor over identical edge weights. The heavier seed's
// neighbor must end up with more mass than the lighter seed's neighbor
// — the personalization vector, not just topology, drives the result.
func TestSettleSeedWeighting(t *testing.T) {
	x := fixedUUID(0x01)
	y := fixedUUID(0x02)
	xn := fixedUUID(0x03) // X's private neighbor
	yn := fixedUUID(0x04) // Y's private neighbor
	nodes := []uuid.UUID{x, y, xn, yn}
	g := symNorm(nodes, [][3]interface{}{
		{x, xn, 1.0},
		{y, yn, 1.0}, // identical weight, disjoint from X's branch
	})

	seeds := map[uuid.UUID]float64{x: 0.9, y: 0.1}
	p := primitives.DefaultSettleParams()

	act, _ := primitives.Settle(seeds, g, p)

	require.Greater(t, act[xn], act[yn],
		"heavier-seeded neighbor receives more activation over identical topology")
}

// TestSettleForeignCliqueSuppression is THE 2b.3 regression.
//
// The 2b.3 one-hop expansion gave a foreign session clique — a dense
// group attached to the query by a SINGLE edge from ONE seed — the same
// standing as true thread members, which are reachable from MANY seeds.
// That noise displaced validated hits (-36pp P@5). The settle must
// suppress the foreign clique instead: mass reaching it is gated by the
// single bottleneck edge and the 1-lambda contraction, whereas thread
// members are re-injected by every seed inside the thread each
// iteration.
//
// Fixture:
//
//	(i)  thread  = 6-node complete graph; 3 of its nodes are seeds
//	     (equal mass). Every non-seed thread node touches all 3 seeds.
//	(ii) foreign = separate 6-node complete graph, attached to exactly
//	     ONE seed by a single edge. Its nodes are reachable only through
//	     that one bottleneck.
//
// Assert: min(activation over non-seed thread nodes)
//
//	> max(activation over foreign-clique nodes).
func TestSettleForeignCliqueSuppression(t *testing.T) {
	// Thread: 6 nodes t0..t5; seeds are t0,t1,t2.
	thread := make([]uuid.UUID, 6)
	for i := range thread {
		thread[i] = fixedUUID(byte(0x20 + i))
	}
	// Foreign: 6 nodes f0..f5, none seeded.
	foreign := make([]uuid.UUID, 6)
	for i := range foreign {
		foreign[i] = fixedUUID(byte(0x40 + i))
	}

	nodes := append(append([]uuid.UUID{}, thread...), foreign...)

	var edges [][3]interface{}
	// Complete graph over the thread.
	for i := 0; i < len(thread); i++ {
		for j := i + 1; j < len(thread); j++ {
			edges = append(edges, [3]interface{}{thread[i], thread[j], 1.0})
		}
	}
	// Complete graph over the foreign clique.
	for i := 0; i < len(foreign); i++ {
		for j := i + 1; j < len(foreign); j++ {
			edges = append(edges, [3]interface{}{foreign[i], foreign[j], 1.0})
		}
	}
	// SINGLE bottleneck edge: one seed (t0) -> one foreign node (f0).
	edges = append(edges, [3]interface{}{thread[0], foreign[0], 1.0})

	g := symNorm(nodes, edges)

	// Three equal-mass seeds inside the thread.
	seeds := map[uuid.UUID]float64{
		thread[0]: 1.0 / 3.0,
		thread[1]: 1.0 / 3.0,
		thread[2]: 1.0 / 3.0,
	}
	p := primitives.DefaultSettleParams()

	act, _ := primitives.Settle(seeds, g, p)

	// Minimum over NON-SEED thread members (t3,t4,t5).
	minThread := act[thread[3]]
	for _, n := range thread[3:] {
		if act[n] < minThread {
			minThread = act[n]
		}
	}
	// Maximum over the entire foreign clique.
	maxForeign := act[foreign[0]]
	for _, n := range foreign {
		if act[n] > maxForeign {
			maxForeign = act[n]
		}
	}

	require.Greater(t, minThread, maxForeign,
		"multi-seed thread members must dominate a single-edge-attached foreign clique (2b.3 regression)")
}

// TestSettleContractionGuarantee: a dense random graph with lambda=0.99.
// The map is a contraction (lambda<1 + row-normalization), so Settle
// must always terminate — either by Eps-convergence or by hitting the
// MaxIters hard stop. Either way it returns and iters <= MaxIters.
func TestSettleContractionGuarantee(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	const nNodes = 30
	nodes := make([]uuid.UUID, nNodes)
	for i := range nodes {
		nodes[i] = fixedUUID(byte(0x80 + i))
	}
	var edges [][3]interface{}
	for i := 0; i < nNodes; i++ {
		for j := i + 1; j < nNodes; j++ {
			if rng.Float64() < 0.6 { // dense
				edges = append(edges, [3]interface{}{nodes[i], nodes[j], rng.Float64() + 0.1})
			}
		}
	}
	g := symNorm(nodes, edges)

	seeds := map[uuid.UUID]float64{nodes[0]: 1.0}
	p := primitives.DefaultSettleParams()
	p.Lambda = 0.99 // slowest contraction we allow; must still terminate

	act, iters := primitives.Settle(seeds, g, p)

	require.LessOrEqual(t, iters, p.MaxIters, "MaxIters is the hard stop")
	require.GreaterOrEqual(t, iters, 1)
	require.NotEmpty(t, act, "returns activations for all graph nodes")
	// Mass stays finite and non-negative under a contraction.
	for _, n := range nodes {
		require.GreaterOrEqual(t, act[n], 0.0)
	}
}

// TestTopExpansionExcludesSeedsAndCaps: seeds never appear in the
// output; the slice is capped at TopM; and when TopM exceeds the number
// of available non-seed nodes, all of them are returned in order.
func TestTopExpansionExcludesSeedsAndCaps(t *testing.T) {
	seed := fixedUUID(0x01)
	n1 := fixedUUID(0x11)
	n2 := fixedUUID(0x12)
	n3 := fixedUUID(0x13)
	n4 := fixedUUID(0x14)

	act := map[uuid.UUID]float64{
		seed: 5.0, // highest, but a seed -> excluded
		n1:   0.9,
		n2:   0.4,
		n3:   0.7,
		n4:   0.1,
	}
	seeds := map[uuid.UUID]float64{seed: 1.0}

	// Cap smaller than available non-seeds.
	capped := primitives.TopExpansion(act, seeds, 2)
	require.Len(t, capped, 2, "output capped at TopM")
	require.NotContains(t, capped, seed, "seeds never appear in expansion")
	require.Equal(t, []uuid.UUID{n1, n3}, capped, "top-2 by activation desc: n1(0.9) then n3(0.7)")

	// Cap larger than available non-seeds -> all non-seeds, ordered.
	all := primitives.TopExpansion(act, seeds, 100)
	require.Len(t, all, 4, "returns all non-seeds when TopM exceeds count")
	require.NotContains(t, all, seed)
	require.Equal(t, []uuid.UUID{n1, n3, n2, n4}, all,
		"ordered by activation desc: 0.9, 0.7, 0.4, 0.1")
}
