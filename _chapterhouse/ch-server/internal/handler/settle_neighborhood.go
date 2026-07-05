package handler

// settle_neighborhood.go — frontier BFS neighborhood builder for the
// P4 recurrent-settle pipeline.
//
// BuildSettleGraph expands a seed set over the Hebbian association graph
// using BFS (up to HopCap hops, NodeCap total nodes), then symmetrizes
// and row-normalizes the resulting undirected weight map so it satisfies
// primitives.SettleGraph's contract (per-node outgoing weight sums <= 1).
//
// Interface choice — AssocLookup:
//   The handler package defines one-method interfaces for every narrow
//   repository dependency (precedent: coActivationEnqueuer in episodic.go).
//   This keeps the seam explicit for testability (tests inject a
//   fakeAssocLookup; production wires *repository.Repository directly).
//   Taking *repository.Repository as the concrete type would prevent the
//   fake and would couple this file to the real DB in unit tests.
//
// Symmetrization accumulates directed weights with SUM (not max):
//   The two directed weights A->B (recorded when A activated B) and
//   B->A (recorded when B activated A) are independent Hebbian evidence —
//   both directions fire when A and B co-occur in different orderings.
//   Summing them captures total co-activation evidence; a max would
//   discard half the signal.  Row-normalization happens after accumulation,
//   so the scale is bounded regardless.
//
// Admission ordering when NodeCap truncates a frontier:
//   Candidates are sorted by (max incident edge weight desc, UUID string
//   asc) — highest-signal nodes are admitted first, ties broken
//   deterministically by UUID so two runs with identical input produce
//   identical graphs.

import (
	"context"
	"sort"

	"github.com/google/uuid"

	"github.com/thinkwright/chapterhouse/ch-server/internal/primitives"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
)

// AssocLookup is the minimal interface over repository.Repository that
// BuildSettleGraph requires.  *repository.Repository satisfies it.
// Defined here (not in the repository package) to keep the seam in the
// handler layer and avoid an awkward inward dependency.
type AssocLookup interface {
	LookupAssociations(
		ctx context.Context,
		srcIDs []uuid.UUID,
		assocType string,
		workspaceID uuid.UUID,
	) (map[uuid.UUID][]repository.Association, error)

	// LookupAssociationsByDst fetches associations whose dst_event_id is
	// in dstIDs, keyed by dst_event_id.  Used alongside LookupAssociations
	// so the BFS traverses edges in both directions — storage is directed
	// (src fired before dst) but the neighborhood must be undirected.
	LookupAssociationsByDst(
		ctx context.Context,
		dstIDs []uuid.UUID,
		assocType string,
		workspaceID uuid.UUID,
	) (map[uuid.UUID][]repository.Association, error)
}

// BuildSettleGraph expands seedIDs over Hebbian associations via
// frontier BFS (at most p.HopCap hops, at most p.NodeCap total nodes),
// symmetrizes edges (each undirected pair gets the SUM of both directed
// weights as independent co-activation evidence), and row-normalizes
// each node's outgoing weights so the per-node sum is <= 1 — the
// required contract for primitives.SettleGraph.
//
// When NodeCap would be exceeded at a frontier, candidates are admitted
// in (max incident weight desc, UUID asc) order for determinism.
//
// Empty seedIDs returns an empty graph with no error.
func BuildSettleGraph(
	ctx context.Context,
	assocLookup AssocLookup,
	seedIDs []uuid.UUID,
	workspaceID uuid.UUID,
	assocType string,
	p primitives.SettleParams,
) (primitives.SettleGraph, error) {
	if len(seedIDs) == 0 {
		return primitives.SettleGraph{Adj: make(map[uuid.UUID][]primitives.SettleEdge)}, nil
	}

	// undirected holds the accumulated symmetric weight for each pair.
	// Keys are ordered (min, max) so A-B and B-A map to the same entry.
	type edgeKey struct{ lo, hi uuid.UUID }
	undirected := make(map[edgeKey]float64)

	// nodeSet tracks every node admitted to the graph so far.
	nodeSet := make(map[uuid.UUID]struct{})
	for _, id := range seedIDs {
		nodeSet[id] = struct{}{}
	}

	// maxIncident tracks the highest incoming weight seen for each
	// candidate (used for NodeCap admission ordering).
	maxIncident := make(map[uuid.UUID]float64)

	// frontier starts as the seed set.
	frontier := make([]uuid.UUID, len(seedIDs))
	copy(frontier, seedIDs)

	for hop := 0; hop < p.HopCap && len(frontier) > 0; hop++ {
		assocBySrc, err := assocLookup.LookupAssociations(ctx, frontier, assocType, workspaceID)
		if err != nil {
			return primitives.SettleGraph{}, err
		}
		assocByDst, err := assocLookup.LookupAssociationsByDst(ctx, frontier, assocType, workspaceID)
		if err != nil {
			return primitives.SettleGraph{}, err
		}

		// Merge both direction maps, deduplicating by (src,dst,type) so the
		// same DB row seen from both lookups is only accumulated once.
		// A row appears in assocBySrc when its src is in the frontier, and in
		// assocByDst when its dst is in the frontier.  When BOTH endpoints are
		// in the frontier the same row appears in both maps.
		type rowKey struct {
			src, dst    uuid.UUID
			assocType   string
		}
		seen := make(map[rowKey]struct{})
		type assocEntry struct {
			a       repository.Association
			nodeKey uuid.UUID // the frontier node this edge is incident on
		}
		var allAssocs []assocEntry

		for frontierID, assocs := range assocBySrc {
			for _, a := range assocs {
				k := rowKey{src: a.SrcEventID, dst: a.DstEventID, assocType: a.AssociationType}
				if _, dup := seen[k]; dup {
					continue
				}
				seen[k] = struct{}{}
				allAssocs = append(allAssocs, assocEntry{a: a, nodeKey: frontierID})
			}
		}
		for frontierID, assocs := range assocByDst {
			for _, a := range assocs {
				k := rowKey{src: a.SrcEventID, dst: a.DstEventID, assocType: a.AssociationType}
				if _, dup := seen[k]; dup {
					continue
				}
				seen[k] = struct{}{}
				allAssocs = append(allAssocs, assocEntry{a: a, nodeKey: frontierID})
			}
		}

		// Collect all new candidate nodes discovered this hop.
		// We need to decide which ones to admit before we add them,
		// because NodeCap may be hit mid-frontier.
		type candidate struct {
			id        uuid.UUID
			maxWeight float64
		}
		candidateMap := make(map[uuid.UUID]float64) // new nodes -> max weight to them

		for _, entry := range allAssocs {
			a := entry.a
			// Accumulate the symmetric edge weight.
			lo, hi := a.SrcEventID, a.DstEventID
			if lo.String() > hi.String() {
				lo, hi = hi, lo
			}
			undirected[edgeKey{lo, hi}] += a.Weight

			// The neighbor of the frontier node is whichever endpoint is NOT
			// the frontier node itself.
			var neighborID uuid.UUID
			if a.SrcEventID == entry.nodeKey {
				neighborID = a.DstEventID
			} else {
				neighborID = a.SrcEventID
			}

			// Track the highest weight to each new (not yet admitted) neighbor.
			if _, known := nodeSet[neighborID]; !known {
				if a.Weight > candidateMap[neighborID] {
					candidateMap[neighborID] = a.Weight
				}
			}
			// Also update maxIncident for the frontier node (already in nodeSet).
			if a.Weight > maxIncident[entry.nodeKey] {
				maxIncident[entry.nodeKey] = a.Weight
			}
		}

		// Sort candidates deterministically: highest max-incident weight
		// first; ties broken by UUID string ascending.
		candidates := make([]candidate, 0, len(candidateMap))
		for id, w := range candidateMap {
			candidates = append(candidates, candidate{id: id, maxWeight: w})
		}
		sort.Slice(candidates, func(i, j int) bool {
			if candidates[i].maxWeight != candidates[j].maxWeight {
				return candidates[i].maxWeight > candidates[j].maxWeight
			}
			return candidates[i].id.String() < candidates[j].id.String()
		})

		// Admit candidates until NodeCap is reached; admitted nodes form
		// the next frontier.
		nextFrontier := make([]uuid.UUID, 0, len(candidates))
		for _, c := range candidates {
			if len(nodeSet) >= p.NodeCap {
				break
			}
			nodeSet[c.id] = struct{}{}
			maxIncident[c.id] = c.maxWeight
			nextFrontier = append(nextFrontier, c.id)
		}
		frontier = nextFrontier
	}

	// Build symmetrized, row-normalized adjacency over the admitted node set.
	// raw[src][dst] = accumulated symmetric weight (same value both ways).
	raw := make(map[uuid.UUID]map[uuid.UUID]float64, len(nodeSet))
	for id := range nodeSet {
		raw[id] = make(map[uuid.UUID]float64)
	}

	for k, w := range undirected {
		// Only include edges where BOTH endpoints were admitted.
		_, hasLo := nodeSet[k.lo]
		_, hasHi := nodeSet[k.hi]
		if !hasLo || !hasHi {
			continue
		}
		// Symmetric: both directions get the same accumulated weight.
		raw[k.lo][k.hi] += w
		raw[k.hi][k.lo] += w
	}

	// Row-normalize: for each node divide all outgoing weights by
	// max(sum, 1.0) so the per-node outgoing sum is <= 1.
	// Dividing by max(sum, 1.0) — not sum itself — means nodes whose
	// outgoing weights already sum to <= 1 are left unchanged (no
	// artificial inflation of weak edges), while only nodes whose sum
	// exceeds 1 are scaled down.  This is the contract SettleGraph requires.
	adj := make(map[uuid.UUID][]primitives.SettleEdge, len(nodeSet))
	for src := range nodeSet {
		neighbors := raw[src]
		if len(neighbors) == 0 {
			// Isolated node (seed with no edges, or node admitted but all
			// edges trimmed by the node set).  Include with empty edge list
			// so the node participates in Settle.
			adj[src] = []primitives.SettleEdge{}
			continue
		}

		var total float64
		for _, w := range neighbors {
			total += w
		}
		divisor := total
		if divisor < 1.0 {
			divisor = 1.0
		}

		// Sort neighbors by (weight desc, UUID asc) for deterministic output.
		dsts := make([]uuid.UUID, 0, len(neighbors))
		for d := range neighbors {
			dsts = append(dsts, d)
		}
		sort.Slice(dsts, func(i, j int) bool {
			wi := neighbors[dsts[i]] / divisor
			wj := neighbors[dsts[j]] / divisor
			if wi != wj {
				return wi > wj
			}
			return dsts[i].String() < dsts[j].String()
		})

		edges := make([]primitives.SettleEdge, 0, len(dsts))
		for _, d := range dsts {
			edges = append(edges, primitives.SettleEdge{
				Dst: d,
				W:   neighbors[d] / divisor,
			})
		}
		adj[src] = edges
	}

	return primitives.SettleGraph{Adj: adj}, nil
}
