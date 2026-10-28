package primitives

// bayesian.go — Bayesian confidence update primitive (Phase 2b stub).
//
// Spec: extension/src/scoring.rs::bayesian_update_inner. The full
// implementation does a posterior update — odds-form Bayes against the
// prior with a likelihood ratio derived from `evidence` ∈ [0,1]; here
// we expose the call signature so D1's primitives package import
// compiles and downstream wiring can land before Phase 2b lights this
// up.
//
// Currently returns 0 — fusion is expected to zero-weight the
// Bayesian term until the real implementation arrives.

// BayesianUpdate will compute a posterior confidence given a prior and
// an evidence signal in [0,1]. Phase 2b. Currently returns 0
// (zero-weighted in fusion).
//
// Spec: extension/src/scoring.rs::bayesian_update_inner.
func BayesianUpdate(prior float64, evidence float64) float64 {
	return 0
}
