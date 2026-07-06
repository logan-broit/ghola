# P4 Recurrent-Settle: Findings (2026-07-05)

> **2026-07-05 LATE ADDENDUM — THE VERDICT BELOW IS INVALIDATED.** Forensics
> (P42-FORENSICS.md) found the measurement ran on a graph missing 66% of its
> edges: `semantic.associations`' primary key omits workspace_id, so pair rows
> for overlapping threads were captured by the stale May workspace and were
> invisible to the eval workspace's settle. The bed has been repaired (25,784
> edges, uniform weights) and the matrix re-run as task8-v2; see the v2
> section/report for the standing verdict. The schema bug (workspace_id not in
> the associations PK) is a latent production defect needing its own fix.

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

## V2: the standing verdict (repaired bed, 25,784 edges, 2026-07-05)

| config       | P@5 none | P@5 correct | bridge/51 |
|--------------|----------|-------------|-----------|
| noprim       | 0.545    | 0.545       | 0         |
| prim         | 0.568    | 0.576       | 0*        |
| expand (A)   | 0.568    | 0.576       | 0         |
| channel@0.2  | 0.621    | 0.621       | 4         |
| channel@0.45 | 0.652    | 0.636       | **25**    |

*bridge set (51 cases) = none-variant misses under both v2 baselines.

**Success bar: PASS on both measured legs.** Bridge 25/51 (49%) >= 1/3;
aggregate P@5 0.652 >= own baseline 0.568 (+8.4pp, an outright improvement,
not merely no-regression). The v1 aggregate crash was the missing-edge
artifact: with 66% of true edges absent, activation amplified noise; with the
full graph it amplifies threads. Config A (expansion validated only by the
cross-encoder) remains null on the honest graph: graph evidence must enter
the SCORE (config B), not pass through a text-pair validator.

Remaining leg before shipping as anything but an opt-in flag: the
LongMemEval R@5 >= 96.9% gate under channel mode (the bench retrieval
harness does not yet carry the settle flag; flag-off default is
byte-identical and needs no gate).

## Activation-weight sweep (2026-07-05, repaired bed)

| w    | P@5 none | P@5 correct | bridge/51 |
|------|----------|-------------|-----------|
| 0    | 0.568    | 0.576       | 0         |
| 0.20 | 0.621    | 0.621       | 4         |
| 0.25 | 0.621    | 0.652       | 4         |
| 0.30 | 0.652    | 0.636       | 11        |
| 0.35 | 0.720    | 0.636       | 21        |
| 0.40 | 0.697    | 0.652       | 24        |
| 0.45 | 0.652    | 0.636       | 25        |
| 0.49 | 0.614    | 0.606       | 25        |

Inverted-U: aggregate peaks at w=0.35 (+15.2pp over baseline), bridge
saturates ~25 by w=0.45, both decline toward 0.49 — no motivation to raise
the RerankWeight ceiling. The 0.35-0.45 plateau is the working range; the
peak vs plateau-center difference is within single-sample noise (n=132,
samples=1). Recommended shipping default when channel mode is enabled:
w=0.40 (plateau center, 0.697/24). Default-on for recall still gated on the
LongMemEval R@5 >= 96.9 check under channel mode.

## LongMemEval R@5 gate — PASS, default flipped (2026-07-06)

The default-on gate ran as paired same-day LongMemEval runs (500 questions,
reranker on, identical config except the settle flag):

| Config | R@1 | R@5 | R@10 | MRR |
|---|---|---|---|---|
| baseline (settle off) | 94.0% | 99.4% | 99.6% | 0.962 |
| channel@0.40 | 93.6% | 99.6% | 99.8% | 0.960 |

R@5 moved +0.2pp against the `>= 99.4%` no-regression bar (the deterministic
baseline's own R@5, reproduced a fourth time by the off leg). **Verdict:
PASS.** The multi-session category — the closest LME analog to the bridge
queries settle exists for — went R@5 99.2 -> 100.0 and R@10 99.2 -> 100.0.

The server default is now flipped to `settle=channel`, `activation_weight=0.40`,
with `GHOLA_SETTLE=off` as the deployment kill-switch (restores the pre-P4
pipeline for every unset request). Full paired numbers, per-category deltas,
latency, and caveats: `docs/benchmarks.md`, "Settle gate (2026-07-06 run)".
