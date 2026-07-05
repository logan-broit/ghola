package core

// FuseScores blends RRF, cross-encoder rerank, and (optionally) settle
// activation scores using a weighted sum of [0,1]-normalized values,
// mirroring the bench reference (longmemeval-ghola/backends/ghola_v2.py
// Stage 3) and extending it with the P4 activation channel:
//
//	Two-channel (activation == nil or wActivation == 0):
//	  final[id] = (1-wRerank) * rrfNorm  +  wRerank * rerankNorm
//
//	Three-channel (config B / "channel" mode):
//	  final[id] = (1-wRerank-wActivation) * rrfNorm
//	            + wRerank * rerankNorm
//	            + wActivation * actNorm
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
// activation may be nil or empty (two-channel path). For ids missing
// from activation the activation term contributes zero — expansion
// hits that are in the pool via RRF need no activation boost.
//
// Callers are responsible for ensuring wRerank+wActivation <= 1 (a
// validation error is returned by Recall before reaching this
// function). FuseScores does not re-validate — hot path.
//
// The fallback path (no rerank score) emits rrfNorm + wActivation*actNorm;
// when wActivation is high and rrfNorm is large this can exceed 1.0 and
// outscore fully-reranked hits. This is intentional — the "trust the RRF
// prior" convention extended to the activation channel. Measurement output
// containing scores > 1.0 is expected, not a bug.
//
// Returns the fused score map keyed by id; callers re-sort.
func FuseScores(rrf, rerank, activation map[string]float64, wRerank, wActivation float64) map[string]float64 {
	rrfMax := maxOrOne(rrf)
	rerankMax := maxOrOne(rerank)
	actMax := maxOrOne(activation)
	rrfWeight := 1.0 - wRerank - wActivation
	out := make(map[string]float64, len(rrf))
	for id, r := range rrf {
		rrfNorm := r / rrfMax
		actNorm := activation[id] / actMax // 0.0 when id absent (zero value)
		if rk, ok := rerank[id]; ok {
			rerankNorm := rk / rerankMax
			out[id] = rrfWeight*rrfNorm + wRerank*rerankNorm + wActivation*actNorm
		} else {
			// No rerank score: trust the RRF prior. See doc above.
			// Activation term still applies when channel mode is active.
			out[id] = rrfNorm + wActivation*actNorm
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
