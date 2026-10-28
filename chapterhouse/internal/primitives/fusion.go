package primitives

// fusion.go — score combiner for the multi-primitive recall pipeline.
//
// Each candidate event coming out of recall carries scores from several
// independent primitives (Hebbian co-activation boost, ACT-R activation,
// Bayesian surprise, Ebbinghaus retention). Score linearly combines
// them per a weight vector so the ranker has a single scalar to sort
// on. Phase 2a wires only the Hebbian channel; the ACT-R / Bayesian /
// Ebbinghaus stubs return zero today so DefaultWeights leaves them at
// zero too — adding non-zero weight before the stubs are real would
// just inject noise.
//
// Spec: extension/src/recall.rs composite formula (simplified for
// Phase 2a — same shape, fewer non-zero terms).

// ScoreParts holds the per-primitive scoring inputs for a single candidate.
type ScoreParts struct {
	Hebbian    float64
	ACTR       float64
	Bayesian   float64
	Ebbinghaus float64
}

// ScoreWeights holds the relative contribution of each primitive to the
// final fused score.
type ScoreWeights struct {
	Hebbian    float64
	ACTR       float64
	Bayesian   float64
	Ebbinghaus float64
}

// DefaultWeights returns the Phase 2a weighting: Hebbian=1.0, others=0.0
// (stubs are present but contribute nothing until Phase 2b lands).
func DefaultWeights() ScoreWeights {
	return ScoreWeights{Hebbian: 1.0}
}

// Score linearly combines the primitive scores per the weights.
//
// Pure function: no I/O, no goroutines, deterministic.
func Score(parts ScoreParts, w ScoreWeights) float64 {
	return parts.Hebbian*w.Hebbian +
		parts.ACTR*w.ACTR +
		parts.Bayesian*w.Bayesian +
		parts.Ebbinghaus*w.Ebbinghaus
}
