package consolidation

import (
	"math"
	"sort"
	"time"
	"unicode/utf8"

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
			maxSim := math.Inf(-1)
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

const excerptMax = 500

// Rep is a chosen representative event: identity + bounded excerpt +
// created_at + its tags/entities. Serialized into the mneme's
// representatives JSON array.
type Rep struct {
	EventID   uuid.UUID `json:"event_id"`
	SessionID uuid.UUID `json:"session_id"`
	Excerpt   string    `json:"excerpt"`
	Score     float64   `json:"score"`
	CreatedAt time.Time `json:"-"`
	Tags      []string  `json:"-"`
	Entities  []string  `json:"-"`
}

// Aggregated is the per-cluster rollup written onto the mneme.
type Aggregated struct {
	Tags      []string
	Entities  []string
	SpanStart time.Time
	SpanEnd   time.Time
}

// Excerpt bounds text to excerptMax bytes, walking back to the last
// complete rune boundary at or before the cap so the result is always
// valid UTF-8 (never splits a multibyte rune). Deliberately a hard byte
// cap — the mneme carries a COPY, and 500 bytes is enough to seed
// recall without bloating the row.
func Excerpt(text string) string {
	return truncateRuneSafe(text, excerptMax)
}

// truncateRuneSafe bounds s to at most max bytes, walking back to the last
// complete rune boundary at or before max so the result is always valid
// UTF-8 (never splits a multibyte rune). Returns s unchanged when
// len(s) <= max. Shared by Excerpt and LLMClient.Label — both need the
// same byte-cap-without-mangling-UTF-8 behavior.
func truncateRuneSafe(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// Aggregate unions tags/entities across reps (ordered by descending
// frequency, ties alphabetical, capped at topN) and computes the
// span. Empty reps yields zero-value Aggregated (caller guards).
func Aggregate(reps []Rep, topN int) Aggregated {
	var agg Aggregated
	if len(reps) == 0 {
		return agg
	}
	agg.SpanStart = reps[0].CreatedAt
	agg.SpanEnd = reps[0].CreatedAt
	for _, r := range reps {
		if r.CreatedAt.Before(agg.SpanStart) {
			agg.SpanStart = r.CreatedAt
		}
		if r.CreatedAt.After(agg.SpanEnd) {
			agg.SpanEnd = r.CreatedAt
		}
	}
	agg.Tags = topByFreq(collect(reps, func(r Rep) []string { return r.Tags }), topN)
	agg.Entities = topByFreq(collect(reps, func(r Rep) []string { return r.Entities }), topN)
	return agg
}

func collect(reps []Rep, pick func(Rep) []string) []string {
	var out []string
	for _, r := range reps {
		out = append(out, pick(r)...)
	}
	return out
}

func topByFreq(items []string, topN int) []string {
	if len(items) == 0 {
		return nil
	}
	freq := map[string]int{}
	for _, it := range items {
		freq[it]++
	}
	uniq := make([]string, 0, len(freq))
	for k := range freq {
		uniq = append(uniq, k)
	}
	sort.Slice(uniq, func(i, j int) bool {
		if freq[uniq[i]] != freq[uniq[j]] {
			return freq[uniq[i]] > freq[uniq[j]]
		}
		return uniq[i] < uniq[j]
	})
	if topN > 0 && len(uniq) > topN {
		uniq = uniq[:topN]
	}
	return uniq
}
