# attic

Retired code kept for context and mining, not built or shipped.

- `extension/` — the original form of ghola: the whole memory system as
  nothing but Postgres and a pgrx extension. The cognitive-primitive
  algorithms (ACT-R, Ebbinghaus, Hebbian, Bayesian, contradiction) were
  ported to Go in `_chapterhouse/ch-server/internal/primitives/`; the
  crate stays here because the pure-Postgres design is worth revisiting
  and `ANALYSIS.md` documents the algorithms better than anywhere else.
  Not wired into any Makefile target or CI job.
