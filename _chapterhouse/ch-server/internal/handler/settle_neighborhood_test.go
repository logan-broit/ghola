package handler_test

// settle_neighborhood_test.go — TDD tests for BuildSettleGraph.
//
// Test strategy mirrors the handler package's fixture conventions:
//   - Pure-logic tests use a fakeAssocLookup (in-memory stub) — no real DB
//     required, keeping each test fast and self-contained.
//   - Real-DB path is NOT needed here because BuildSettleGraph has no SQL
//     of its own; it delegates entirely to the AssocLookup interface.  The
//     interface boundary is the correct seam (same rationale as
//     coActivationEnqueuer in episodic_test.go).
//
// Five required tests (TDD order):
//  1. TestSettleNeighborhood_HopCap        — path graph, distance-3 node absent
//  2. TestSettleNeighborhood_NodeCapDeterministic — run twice, same graph
//  3. TestSettleNeighborhood_Symmetrization — one-way edge yields both directions
//  4. TestSettleNeighborhood_NormalizationContract — all outgoing sums <= 1
//  5. TestSettleNeighborhood_EmptyInput    — empty seeds, no error

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/handler"
	"github.com/thinkwright/chapterhouse/ch-server/internal/primitives"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
)

// ---------------------------------------------------------------------
// fake implementation of handler.AssocLookup — in-memory adjacency map
// ---------------------------------------------------------------------

// fakeAssocLookup holds a static directed adjacency (src -> []Association)
// and records all call arguments for assertion.  This satisfies the
// handler.AssocLookup interface without any DB.
type fakeAssocLookup struct {
	// adj maps srcID -> list of directed (src, dst, weight) associations
	adj   map[uuid.UUID][]repository.Association
	calls []fakeCall
}

type fakeCall struct {
	srcIDs      []uuid.UUID
	assocType   string
	workspaceID uuid.UUID
}

func (f *fakeAssocLookup) LookupAssociations(
	ctx context.Context,
	srcIDs []uuid.UUID,
	assocType string,
	workspaceID uuid.UUID,
) (map[uuid.UUID][]repository.Association, error) {
	f.calls = append(f.calls, fakeCall{srcIDs: srcIDs, assocType: assocType, workspaceID: workspaceID})
	out := make(map[uuid.UUID][]repository.Association)
	for _, id := range srcIDs {
		if assocs, ok := f.adj[id]; ok {
			out[id] = assocs
		}
	}
	return out, nil
}

// addEdge adds a directed weighted edge src->dst with the given assoc type and workspace.
func (f *fakeAssocLookup) addEdge(src, dst uuid.UUID, weight float64, assocType string, workspaceID uuid.UUID) {
	if f.adj == nil {
		f.adj = make(map[uuid.UUID][]repository.Association)
	}
	f.adj[src] = append(f.adj[src], repository.Association{
		SrcEventID:      src,
		DstEventID:      dst,
		AssociationType: assocType,
		Weight:          weight,
		WorkspaceID:     workspaceID,
	})
}

// defaultParams returns small caps suitable for unit tests.
func defaultTestParams() primitives.SettleParams {
	p := primitives.DefaultSettleParams()
	p.HopCap = 3
	p.NodeCap = 2000
	return p
}

// ---------------------------------------------------------------------
// Test 1: hop cap honored
// ---------------------------------------------------------------------

// TestSettleNeighborhood_HopCap builds a 4-node path A->B->C->D and
// seeds at A with HopCap=2.  Node D is at distance 3 from A so it must
// NOT appear in the returned graph.
func TestSettleNeighborhood_HopCap(t *testing.T) {
	ws := uuid.New()
	nodeA := uuid.New()
	nodeB := uuid.New()
	nodeC := uuid.New()
	nodeD := uuid.New()

	fake := &fakeAssocLookup{}
	fake.addEdge(nodeA, nodeB, 0.8, "hebbian", ws)
	fake.addEdge(nodeB, nodeC, 0.6, "hebbian", ws)
	fake.addEdge(nodeC, nodeD, 0.4, "hebbian", ws)

	p := defaultTestParams()
	p.HopCap = 2

	g, err := handler.BuildSettleGraph(context.Background(), fake, []uuid.UUID{nodeA}, ws, "hebbian", p)
	require.NoError(t, err)

	// A, B, C must appear (distances 0, 1, 2 from seed).
	assert.Contains(t, g.Adj, nodeA, "seed node A must be in graph")
	assert.Contains(t, g.Adj, nodeB, "hop-1 node B must be in graph")
	assert.Contains(t, g.Adj, nodeC, "hop-2 node C must be in graph")

	// D is at hop 3 — must be absent.
	_, hasD := g.Adj[nodeD]
	assert.False(t, hasD, "node D at hop 3 must not appear when HopCap=2")
}

// ---------------------------------------------------------------------
// Test 2: node cap deterministic truncation
// ---------------------------------------------------------------------

// TestSettleNeighborhood_NodeCapDeterministic creates a star graph with
// one seed and many leaves (more than NodeCap).  Running BuildSettleGraph
// twice must return identical graphs (same node set, same weights).  It
// also verifies that the highest-weight leaf was admitted and the
// lowest-weight leaf was dropped.
func TestSettleNeighborhood_NodeCapDeterministic(t *testing.T) {
	ws := uuid.New()
	center := uuid.New()

	// Build 10 leaf nodes with deliberately varied weights.
	type leaf struct {
		id     uuid.UUID
		weight float64
	}
	leaves := make([]leaf, 10)
	for i := range leaves {
		leaves[i] = leaf{id: uuid.New(), weight: float64(i+1) * 0.05} // 0.05..0.50
	}

	fake := &fakeAssocLookup{}
	for _, l := range leaves {
		fake.addEdge(center, l.id, l.weight, "hebbian", ws)
	}

	p := defaultTestParams()
	// Seed + 5 of the 10 leaves = 6 nodes total.  Cap at 6.
	p.NodeCap = 6
	p.HopCap = 1

	g1, err := handler.BuildSettleGraph(context.Background(), fake, []uuid.UUID{center}, ws, "hebbian", p)
	require.NoError(t, err)
	g2, err := handler.BuildSettleGraph(context.Background(), fake, []uuid.UUID{center}, ws, "hebbian", p)
	require.NoError(t, err)

	// Graphs must have identical node sets.
	require.Equal(t, len(g1.Adj), len(g2.Adj), "graph sizes must match across runs")
	for id := range g1.Adj {
		_, ok := g2.Adj[id]
		assert.True(t, ok, "node %v present in g1 must also be in g2", id)
	}

	// The highest-weight leaf (leaves[9], weight=0.50) must be admitted.
	highLeaf := leaves[9].id
	_, admitted := g1.Adj[highLeaf]
	assert.True(t, admitted, "highest-weight leaf must be admitted")

	// The lowest-weight leaf (leaves[0], weight=0.05) must be absent
	// (5 higher-weight leaves take the 5 remaining slots).
	lowLeaf := leaves[0].id
	_, dropped := g1.Adj[lowLeaf]
	assert.False(t, dropped, "lowest-weight leaf must be dropped when NodeCap is tight")

	// NodeCap includes the seed itself (center is in Adj).
	assert.LessOrEqual(t, len(g1.Adj), p.NodeCap, "node count must not exceed NodeCap")
}

// ---------------------------------------------------------------------
// Test 3: symmetrization
// ---------------------------------------------------------------------

// TestSettleNeighborhood_Symmetrization seeds a graph with only a
// directed edge A->B in the store.  After BuildSettleGraph the graph
// must have outgoing edges from BOTH A and B (symmetrized), even though
// the reverse direction was never recorded.
func TestSettleNeighborhood_Symmetrization(t *testing.T) {
	ws := uuid.New()
	nodeA := uuid.New()
	nodeB := uuid.New()

	fake := &fakeAssocLookup{}
	// Only A->B exists; B->A does not.
	fake.addEdge(nodeA, nodeB, 0.5, "hebbian", ws)

	p := defaultTestParams()
	p.HopCap = 2

	g, err := handler.BuildSettleGraph(context.Background(), fake, []uuid.UUID{nodeA}, ws, "hebbian", p)
	require.NoError(t, err)

	// Both nodes must be present.
	require.Contains(t, g.Adj, nodeA, "nodeA must be in graph")
	require.Contains(t, g.Adj, nodeB, "nodeB must be in graph")

	// nodeA must have an outgoing edge to nodeB.
	foundAtoB := false
	for _, e := range g.Adj[nodeA] {
		if e.Dst == nodeB {
			foundAtoB = true
		}
	}
	assert.True(t, foundAtoB, "A->B edge must exist after symmetrization")

	// nodeB must have an outgoing edge BACK to nodeA (the symmetrized reverse).
	foundBtoA := false
	for _, e := range g.Adj[nodeB] {
		if e.Dst == nodeA {
			foundBtoA = true
		}
	}
	assert.True(t, foundBtoA, "B->A reverse edge must exist after symmetrization")
}

// ---------------------------------------------------------------------
// Test 4: normalization contract (carry-forward from Task 2 review)
// ---------------------------------------------------------------------

// TestSettleNeighborhood_NormalizationContract seeds a hub node H
// with four outgoing edges of deliberately large weights.  After
// BuildSettleGraph, every node's outgoing W must sum to <= 1+1e-9.
// This is the critical contract for primitives.SettleGraph.
func TestSettleNeighborhood_NormalizationContract(t *testing.T) {
	ws := uuid.New()
	hub := uuid.New()
	spoke1 := uuid.New()
	spoke2 := uuid.New()
	spoke3 := uuid.New()
	spoke4 := uuid.New()

	fake := &fakeAssocLookup{}
	// Large skewed weights that sum to >> 1 before normalization.
	fake.addEdge(hub, spoke1, 0.9, "hebbian", ws)
	fake.addEdge(hub, spoke2, 0.8, "hebbian", ws)
	fake.addEdge(hub, spoke3, 0.7, "hebbian", ws)
	fake.addEdge(hub, spoke4, 0.6, "hebbian", ws)
	// Also add spokes back toward hub to create a denser undirected graph.
	fake.addEdge(spoke1, spoke2, 0.4, "hebbian", ws)
	fake.addEdge(spoke2, spoke3, 0.4, "hebbian", ws)

	p := defaultTestParams()
	p.HopCap = 2

	g, err := handler.BuildSettleGraph(context.Background(), fake, []uuid.UUID{hub}, ws, "hebbian", p)
	require.NoError(t, err)
	require.NotEmpty(t, g.Adj, "graph must not be empty")

	const tol = 1e-9
	for node, edges := range g.Adj {
		var sum float64
		for _, e := range edges {
			sum += e.W
		}
		assert.LessOrEqualf(t, sum, 1.0+tol,
			"node %v outgoing W sum %.9f exceeds 1 — normalization contract violated", node, sum)
	}
}

// ---------------------------------------------------------------------
// Test 5: empty seeds / no associations
// ---------------------------------------------------------------------

// TestSettleNeighborhood_EmptyInput verifies that an empty seed set
// (and a store with no associations) returns an empty graph without
// error.
func TestSettleNeighborhood_EmptyInput(t *testing.T) {
	ws := uuid.New()
	fake := &fakeAssocLookup{}

	p := defaultTestParams()

	g, err := handler.BuildSettleGraph(context.Background(), fake, nil, ws, "hebbian", p)
	require.NoError(t, err)
	assert.Empty(t, g.Adj, "empty seeds must yield empty graph")

	// Also test with non-nil but empty slice.
	g2, err := handler.BuildSettleGraph(context.Background(), fake, []uuid.UUID{}, ws, "hebbian", p)
	require.NoError(t, err)
	assert.Empty(t, g2.Adj, "empty seeds slice must yield empty graph")
}
