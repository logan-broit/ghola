package consolidation

import (
	"math"

	"github.com/google/uuid"
)

// Candidate is one session (or event) considered for representative
// selection: an id + its embedding.
type Candidate struct {
	ID        uuid.UUID
	Embedding []float32
}

const mmrLambda = 0.5

// SelectRepresentatives returns up to k diverse candidates via Maximal
// Marginal Relevance against centroid: the first pick maximizes cosine to
// the centroid (the medoid), each subsequent pick maximizes
// lambda*rel - (1-lambda)*maxSimToSelected. Deterministic: ties in the
// MMR score break by smallest UUID (settle.go precedent). k<=0 or empty
// input returns nil; k>len returns all.
func SelectRepresentatives(cands []Candidate, centroid []float32, k int) []Candidate {
	if len(cands) == 0 || k <= 0 {
		return nil
	}
	if k > len(cands) {
		k = len(cands)
	}
	rel := make([]float64, len(cands))
	for i, c := range cands {
		rel[i] = cosine(c.Embedding, centroid)
	}
	selected := make([]Candidate, 0, k)
	used := make([]bool, len(cands))
	for len(selected) < k {
		bestIdx := -1
		bestScore := math.Inf(-1)
		for i, c := range cands {
			if used[i] {
				continue
			}
			var maxSim float64
			for _, s := range selected {
				sim := cosine(c.Embedding, s.Embedding)
				if sim > maxSim {
					maxSim = sim
				}
			}
			score := mmrLambda*rel[i] - (1-mmrLambda)*maxSim
			if len(selected) == 0 {
				score = rel[i] // first pick: pure relevance (medoid)
			}
			if score > bestScore || (score == bestScore && bestIdx >= 0 && lessUUID(c.ID, cands[bestIdx].ID)) {
				bestScore = score
				bestIdx = i
			}
		}
		if bestIdx < 0 {
			break
		}
		used[bestIdx] = true
		selected = append(selected, cands[bestIdx])
	}
	return selected
}

func cosine(a, b []float32) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	var dot, na, nb float64
	for i := 0; i < n; i++ {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
