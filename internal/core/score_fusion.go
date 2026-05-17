package core

// FuseScores blends RRF scores with cross-encoder rerank scores using
// a weighted sum of [0,1]-normalized values, mirroring the bench
// reference (longmemeval-ghola/backends/ghola_v2.py Stage 3):
//
//	final[id] = (1-w) * rrf[id]/maxRRF  +  w * rerank[id]/maxRerank
//
// This shape is the safety net for the bge-reranker-base failure mode
// where every candidate scores near zero and reranker order becomes
// noise — preserving an RRF-weighted floor keeps useful retrieval
// signal even when the cross-encoder is uncertain.
//
// Each denominator is floored at 1.0 (an empty/all-zero distribution
// becomes a no-op rather than NaN) — same guard the bench uses.
//
// rerank may be a strict subset of rrf's keys (the rerank pool can be
// narrower than the RRF pool, or some hits may be unrerankable —
// e.g., semantic mnemes have no Content for the cross-encoder to
// score). For ids missing from rerank we trust the RRF prior and use
// rrfNorm directly rather than treating the missing score as zero.
// Treating "couldn't be reranked" the same as "reranker scored zero"
// systematically penalized no-content tiers (semantic) and pushed
// them out of the top-N output even when their RRF rank was strong.
//
// Returns the fused score map keyed by id; callers re-sort.
func FuseScores(rrf, rerank map[string]float64, weight float64) map[string]float64 {
	rrfMax := maxOrOne(rrf)
	rerankMax := maxOrOne(rerank)
	rrfWeight := 1.0 - weight
	out := make(map[string]float64, len(rrf))
	for id, r := range rrf {
		rrfNorm := r / rrfMax
		if rk, ok := rerank[id]; ok {
			rerankNorm := rk / rerankMax
			out[id] = rrfWeight*rrfNorm + weight*rerankNorm
		} else {
			// No rerank score: trust the RRF prior. See doc above.
			out[id] = rrfNorm
		}
	}
	return out
}

// maxOrOne returns max(values, 1.0) — flooring at 1.0 protects callers
// from div-by-zero when every input is zero (or the map is empty).
func maxOrOne(m map[string]float64) float64 {
	max := 0.0
	for _, v := range m {
		if v > max {
			max = v
		}
	}
	if max <= 0 {
		return 1.0
	}
	return max
}
