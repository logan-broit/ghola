package primitives

// hebbian.go — per-query Hebbian boost computation.
//
// This is the read-side companion to repository.UpsertAssociation. The
// write side (extension/src/hebbian.rs / internal/consolidation/) folds
// co-activation pairs into semantic.associations and updates weights.
// At query time, given a candidate set produced by the multi-ranking
// recall pipeline, BoostsFor sums the association weights between
// candidates so events whose neighbors are also relevant get a boost.
//
// Spec: extension/src/hebbian.rs (the per-query boost equivalent — we
// do NOT port the bgworker queue-drain logic; that lives in
// internal/consolidation/ per Task C2). We DO match the formula
// exactly: boost = Σ weight(c -> d) where d ∈ candidates.

import (
	"github.com/google/uuid"

	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
)

// BoostsFor computes per-candidate Hebbian boost scores. For each event
// in `candidates`, sums the weights of associations to other events
// that are also in `candidates`. Events whose neighbors are also
// relevant get higher boosts; associations that point outside the
// candidate set contribute zero.
//
// The returned map always has an entry for every candidate (zero when
// no in-set neighbors exist), so callers can iterate candidates and
// look up boosts unconditionally without a map-miss branch.
//
// Pure function: no I/O, no goroutines, deterministic. The
// `associations` map is the shape returned by
// repository.LookupAssociations — keyed by src_event_id, with each
// slice element carrying DstEventID + Weight.
//
// Ports the per-query boost computation from extension/src/hebbian.rs;
// formula-preserving with the Rust reference.
func BoostsFor(
	candidates []uuid.UUID,
	associations map[uuid.UUID][]repository.Association,
) map[uuid.UUID]float64 {
	candSet := make(map[uuid.UUID]bool, len(candidates))
	for _, c := range candidates {
		candSet[c] = true
	}

	boosts := make(map[uuid.UUID]float64, len(candidates))
	for _, c := range candidates {
		var sum float64
		for _, a := range associations[c] {
			if candSet[a.DstEventID] {
				sum += a.Weight
			}
		}
		boosts[c] = sum
	}
	return boosts
}
