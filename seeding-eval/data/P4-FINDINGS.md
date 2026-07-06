# P4 Recurrent-Settle: Findings (2026-07-05)

Verdict against the pre-registered success bar: **FAIL** — the settle ships
flag-gated OFF (default path byte-identical to pre-P4, verified by review and
tests). The mechanism stays in the codebase for follow-up work.

## Bed

Regenerated from vercel/next.js, 750 merged-PR threads, 132 held-out cases,
workspace a4f8bdd2-65c0-44b6-990d-ef2d1f8dc479, 8,842 hebbian associations,
co-activation queue fully drained before measurement. Fresh baselines:
noprim P@5(none) 0.538, prim 0.538 (correct_era 0.545/0.561). Bridge set
re-derived correctly (bridge-misses-corrected.txt): 55 cases whose none-variant
missed top-5 under BOTH baselines. NOTE: the phase0 driver's original miss
derivation was buggy (wrote all held cases); the corrected set is authoritative.
May-2026 numbers (P@5 0.644, bridge-32) belong to a different bed and do not
transfer.

## Results (none-variant P@5 / bridge hits of 55)

| config              | P@5 none | P@5 correct | bridge |
|---------------------|----------|-------------|--------|
| baseline (prim)     | 0.538    | 0.561       | 0*     |
| expand (config A)   | 0.538    | 0.561       | 0      |
| channel w=0.2 (B)   | 0.538    | 0.583       | 2      |
| channel w=0.45 (B)  | 0.409    | 0.553       | 12     |

*zero by construction. Success bar required >= 18/55 AND no aggregate
regression: no config satisfies both legs.

## Interpretation

1. **Config A (expansion validated only by the cross-encoder) is a true null.**
   The mechanism engaged (expansion candidates entered the rerank pool; 13
   reached top-20 across all traces) but the reranker almost never lifted
   them, and P@5/bridge outcomes are identical to baseline. This mirrors the
   companion measurement paper's core finding from the other side: bridge
   events are not query-near as TEXT, so a text-pair scorer cannot validate
   them. Graph reachability alone cannot be cashed in through a text-only
   filter.
2. **Config B shows a real dose-response: the converged activation carries
   bridge signal.** 2 -> 12 recoveries as w goes 0.2 -> 0.45. The fixed-point
   settle finds true thread members. But at uniform weight the same channel
   lifts foreign-clique noise, costing 12.9pp aggregate at w=0.45. Precision,
   not recall, is the failure mode — the same shape as 2b.3, softened but not
   solved by the fixed-point formulation.
3. **The falsified prediction is itself informative**: the design assumed the
   damped fixed point would suppress foreign cliques enough for a uniform
   score weight to be safe. It suppresses them (config B at 0.2 does not
   regress) but not enough to buy the bridge at an affordable weight.

## Next hypothesis (P4.1, not built)

Adaptive gating instead of uniform weight: apply the activation channel only
where the primary retrieval signal is weak (low top-RRF mass or low top
rerank score — exactly the bridge shape), and keep it off elsewhere. This is
the HOLA lesson transposed: spend the trusted-but-noisy channel only where
the primary representation is weakest. A per-query gate would have kept the
w=0.45 bridge gains while forfeiting none of the aggregate on the 77 cases
the baseline already solves.

## Operational notes

- Branch feat/p4-recurrent-settle (15 commits): all code review-gated; final
  integration review approve-for-measurement; flag-off path verified
  byte-identical at every layer.
- LongMemEval R@5 gate not run (moot on FAIL; no config ships).
- Raw artifacts: results-baseline-{noprim,prim}/, results-p4-{expand,channel,
  channel045}/, per-case traces, this file's numbers reproducible via
  task8-matrix.sh + probe045.log.

## Appendix: P4.1 gating premise test (2026-07-05, offline, same artifacts)

Premise: a query-time confidence gate (open the activation channel only when
primary retrieval is weak) could keep the w=0.45 bridge gains without the
aggregate cost. Tested retroactively on the existing per-case traces.

Result: **falsified at the available-signal level.**
- w=0.45 decomposition: 13 WINs (12 bridge), 30 LOSSes, rest unchanged.
- No trace-level feature separates WIN from LOSS: per-query normalization
  flattens top-1 fused score (~1.0 for all); top1-top5 margin runs BACKWARD
  (WIN median 0.164 vs LOSS 0.086); episodic-in-top5, tier diversity,
  ground-truth size, top-1 tier: indistinguishable.
- Ceiling check: even a perfect oracle gate ("apply the channel only to
  baseline misses") reaches 12/55 bridge at w=0.45 — still under the 18/55
  bar. The channel at this weight does not recover enough bridges even with
  free perfect selectivity.

Implications for any P4.1: (a) the gate needs signals the pipeline does not
currently expose (raw pre-normalization rerank logits, absolute RRF mass,
seed-to-graph association density) — new telemetry, an instrumented
experiment, not a retrofit; (b) higher weights under a gate MIGHT recover
more bridges but are unmeasured; (c) the LOSS population is broadly
distributed, not a removable subpopulation. P4.1 as originally sketched is
not supported by the existing evidence; the cheap offline test prevented
building it on a false assumption.
