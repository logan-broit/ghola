package primitives

// settle.go — fixed-point spreading activation over the Hebbian graph.
//
// This is the P4 "settle" core: candidate-set expansion by running
// activation to a converged steady state on the association graph,
// rather than a single-hop pull (the reverted 2b.3 approach, which gave
// a foreign session clique attached by ONE edge the same standing as
// true thread members reachable from MANY seeds).
//
// Recurrence (one iteration): a1 = (1-Lambda)*s + Lambda*W'^T a0, where
// s is the normalized seed (personalization) vector, W' is the caller-
// symmetrized, row-normalized Hebbian adjacency, and Lambda is the
// damping/contraction term. The seed term sits OUTSIDE the Lambda
// factor: every iteration re-injects (1-Lambda) of each seed's mass, so
// nodes reachable from many seeds keep getting topped up while a clique
// behind a single bottleneck edge is starved by the contraction.
//
// Contraction: with row-normalized W' (each node's outgoing weights sum
// to <= 1) the operator norm of Lambda*W'^T is <= Lambda < 1, so the
// map is a contraction; by Banach it has a unique fixed point and the
// iteration converges geometrically. Eps-convergence (||delta||_1 < Eps)
// is therefore the normal exit; MaxIters is only a hard safety stop.
//
// Personalized-PageRank equivalence: this is exactly personalized
// PageRank with teleport probability (1-Lambda) and teleport
// distribution s, restricted to the Hebbian neighborhood.

import (
	"sort"

	"github.com/google/uuid"
)

// SettleParams configures the settle. Defaults come from
// DefaultSettleParams; HopCap/NodeCap are consumed by the (separate)
// neighborhood builder, not by Settle itself, but travel together as
// one config object.
type SettleParams struct {
	Lambda   float64 // damping/decay contraction, default 0.7
	Eps      float64 // L1 convergence threshold, default 1e-6
	MaxIters int     // hard stop, default 20
	HopCap   int     // used by the (later) neighborhood builder, default 3
	NodeCap  int     // ditto, default 2000
	TopM     int     // expansion candidates returned, default 25
}

// DefaultSettleParams returns the agreed defaults (see the P4 design:
// lambda 0.7 for guaranteed contraction, hop cap 3, node cap ~2000,
// top-M 25, eps 1e-6, 20 iterations).
func DefaultSettleParams() SettleParams {
	return SettleParams{
		Lambda:   0.7,
		Eps:      1e-6,
		MaxIters: 20,
		HopCap:   3,
		NodeCap:  2000,
		TopM:     25,
	}
}

// SettleEdge is a single outgoing edge with its (row-normalized) weight.
type SettleEdge struct {
	Dst uuid.UUID
	W   float64
}

// SettleGraph is the neighborhood adjacency the settle runs over. The
// caller is responsible for pre-symmetrizing the Hebbian edges and
// row-normalizing them (the sum of outgoing W per node is <= 1); Settle
// treats the weights as given and does not renormalize.
type SettleGraph struct {
	Adj map[uuid.UUID][]SettleEdge
}

// Settle runs the recurrence a1 = (1-Lambda)*s + Lambda*W'^T a0 to a
// fixed point over g, starting from the (caller-normalized) seed mass.
// It returns the converged activation for every node that appears in
// the graph or the seed set, plus the number of iterations used.
//
// Determinism: each iteration builds a1 as a fresh map (no in-place
// read-write hazard), and the diffusion contributions are accumulated
// in a fixed source order. That fixed order matters: contributions from
// different source nodes land in the same a1[dst] slot, and float64
// addition is not associative, so summing under Go's randomized map
// range order would flip the last ULP between runs. Iterating sources
// (and the node set) in UUID-sorted order makes the result bit-stable.
//
// Convergence: normal exit is sum(|a1-a0|) < Eps; MaxIters is the hard
// stop (see the package/file doc for the contraction argument).
func Settle(seeds map[uuid.UUID]float64, g SettleGraph, p SettleParams) (map[uuid.UUID]float64, int) {
	// Node universe: every graph node plus every seed (a seed with no
	// edges still holds its (1-Lambda) self-mass and must appear in the
	// output). Sorted once so every per-iteration pass is deterministic.
	nodeSet := make(map[uuid.UUID]struct{}, len(g.Adj)+len(seeds))
	for n := range g.Adj {
		nodeSet[n] = struct{}{}
	}
	for n := range seeds {
		nodeSet[n] = struct{}{}
	}
	nodes := make([]uuid.UUID, 0, len(nodeSet))
	for n := range nodeSet {
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].String() < nodes[j].String() })

	// a0 starts at the seed vector so the very first delta is meaningful
	// (a cold zero start would report a spurious first-iteration jump).
	a0 := make(map[uuid.UUID]float64, len(nodes))
	for _, n := range nodes {
		a0[n] = seeds[n]
	}

	iters := 0
	for iters < p.MaxIters {
		iters++

		// Fresh accumulator so we never read-write the same map.
		a1 := make(map[uuid.UUID]float64, len(nodes))

		// Diffusion term: push each node's current mass along its
		// outgoing (row-normalized) edges, accumulating into a1[dst].
		// Sources are visited in fixed (sorted) order so the summation
		// order into each a1[dst] is reproducible across runs.
		for _, src := range nodes {
			mass := a0[src]
			if mass == 0 {
				continue
			}
			for _, e := range g.Adj[src] {
				a1[e.Dst] += mass * e.W
			}
		}

		// Apply damping and re-inject the seed teleport term. The seed
		// term is OUTSIDE the Lambda factor: a1 = (1-Lambda)*s + Lambda*(W'^T a0).
		var delta float64
		for _, n := range nodes {
			v := (1-p.Lambda)*seeds[n] + p.Lambda*a1[n]
			a1[n] = v
			d := v - a0[n]
			if d < 0 {
				d = -d
			}
			delta += d
		}

		a0 = a1
		if delta < p.Eps {
			break
		}
	}

	return a0, iters
}

// TopExpansion returns the top-M non-seed nodes by activation
// descending, with exact ties broken by UUID string ascending for
// determinism. Seeds are always excluded (they are already in the
// candidate set). If topM exceeds the number of non-seed nodes, all of
// them are returned in order.
func TopExpansion(act map[uuid.UUID]float64, seeds map[uuid.UUID]float64, topM int) []uuid.UUID {
	type node struct {
		id  uuid.UUID
		act float64
	}
	candidates := make([]node, 0, len(act))
	for id, a := range act {
		if _, isSeed := seeds[id]; isSeed {
			continue
		}
		candidates = append(candidates, node{id: id, act: a})
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].act != candidates[j].act {
			return candidates[i].act > candidates[j].act // activation desc
		}
		return candidates[i].id.String() < candidates[j].id.String() // tie: UUID asc
	})

	if topM < len(candidates) {
		candidates = candidates[:topM]
	}

	out := make([]uuid.UUID, len(candidates))
	for i, c := range candidates {
		out[i] = c.id
	}
	return out
}
