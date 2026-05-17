package core

import "sort"

// ScoredDoc is one document with its fused RRF score.
type ScoredDoc struct {
	ID    string
	Score float64
}

// FuseRRF merges N best-first ranked lists with the standard reciprocal
// rank fusion formula (Cormack, Clarke, Buettcher 2009):
//
//	score(d) = sum over tiers t of  1 / (k + rank_t(d))
//
// A document missing from a list contributes nothing — equivalent to a
// rank deeper than any practical budget. Ties between lists are broken
// by the order ids first appear; ties on the final score are broken by
// id ascending so the result is deterministic across runs.
//
// k is the constant from the paper; smaller k lets the very top ranks
// dominate. 60 is the literature default and is what the bench backend
// (longmemeval-ghola/backends/ghola_v2.py) uses.
func FuseRRF(lists [][]string, k int) []ScoredDoc {
	if k <= 0 {
		k = 60
	}
	scores := map[string]float64{}
	firstSeen := map[string]int{}
	order := 0
	for _, list := range lists {
		for rank, id := range list {
			// rank in this loop is 0-indexed; +1 to align with the
			// 1-indexed paper formula.
			scores[id] += 1.0 / float64(k+rank+1)
			if _, ok := firstSeen[id]; !ok {
				firstSeen[id] = order
				order++
			}
		}
	}
	out := make([]ScoredDoc, 0, len(scores))
	for id, s := range scores {
		out = append(out, ScoredDoc{ID: id, Score: s})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		// Score tie: prefer the id we saw first (stable across runs).
		return firstSeen[out[i].ID] < firstSeen[out[j].ID]
	})
	return out
}
