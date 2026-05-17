package primitives

// actr.go — ACT-R base-level activation primitive (Phase 2b stub).
//
// Spec: extension/src/scoring.rs::actr_activation_inner. The full
// implementation computes log Σ tᵢ⁻ᵈ over per-access elapsed times,
// optionally clipped + shifted; here we expose the call signature so
// D1's primitives package import compiles and downstream wiring can
// land before Phase 2b lights this up.
//
// Currently returns 0 — fusion is expected to zero-weight the ACT-R
// term until the real implementation arrives.

import "time"

// ActivationFor will compute ACT-R base-level activation for an event
// given its access count, last-access time, and the current time.
// Phase 2b. Currently returns 0 (zero-weighted in fusion).
//
// Spec: extension/src/scoring.rs::actr_activation_inner.
func ActivationFor(accessCount int, lastAccess time.Time, now time.Time) float64 {
	return 0
}
