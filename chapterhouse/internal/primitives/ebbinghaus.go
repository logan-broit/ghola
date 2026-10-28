package primitives

// ebbinghaus.go — Ebbinghaus forgetting-curve primitive (Phase 2b stub).
//
// Spec: extension/src/scoring.rs::ebbinghaus_decay_inner. The full
// implementation models retention as R = exp(-elapsed / strength) (or
// the equivalent power-law variant); here we expose the call signature
// so D1's primitives package import compiles and downstream wiring can
// land before Phase 2b lights this up.
//
// Currently returns 0 — fusion is expected to zero-weight the
// Ebbinghaus term until the real implementation arrives.

import "time"

// EbbinghausDecay will compute the Ebbinghaus retention factor given
// the elapsed time since last reinforcement and a memory-strength
// parameter (lifetime / stability). Phase 2b. Currently returns 0
// (zero-weighted in fusion).
//
// Spec: extension/src/scoring.rs::ebbinghaus_decay_inner.
func EbbinghausDecay(elapsed time.Duration, strength float64) float64 {
	return 0
}
