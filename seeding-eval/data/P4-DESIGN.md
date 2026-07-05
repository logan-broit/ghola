# P4: Spreading Activation as Recurrent Settle — Design

**Date:** 2026-07-05 (approved by Logan same day)
**Hypothesis memory:** p4-spreading-activation-as-recurrent-settle
**Prior art to respect:** 2b.3 one-hop expansion REVERTED (-36pp; tier-exclusive
RRF mass; bridge-32 4->1). Recorded lesson: never a new RRF tier; option B =
score channel. Hebbian re-rank structurally capped by candidate-set
incompleteness (26/32 GT events unreachable).

## Hypothesis

Bridge cases fail because GT events are graph-reachable but not query-near.
Spreading activation as a converging dynamical system (personalized-PageRank
fixed point over the Hebbian graph, decay as the damping/contraction term),
run as candidate-set EXPANSION into the cross-encoder rerank pool (zero RRF
mass), recovers them without re-importing 2b.3's noise.

## Success bar (agreed)

PASS iff ALL: bridge top-5 >= 1/3 of the re-derived miss-set; aggregate P@5
(prim) >= the regenerated bed's OWN prim baseline (0.644 was the May bed's
number and does not transfer; first regeneration attempt at n=50 threads gave
a ~11-case bed — rerun at n=500 threads 2026-07-05); LongMemEval R@5 gate
>= 96.9%.
FAIL any leg -> revert + record findings (2b.3 style). Flag-gated throughout;
unbounded latency during measurement (experiment-first), tune-and-remeasure
only on PASS.

## Phase 0 — regenerate the eval bed (the May bed is gone)

/tmp/seeding-eval-rich wiped; schema drifted since May (events lost
workspace_id; associations now 11.4M rows total). Plan: seeding-extract from
the same GitHub source repo/params as the May run; re-ingest via
cmd/import-logs; re-run baselines (prim on/off); re-derive the structural-miss
("bridge") set under TODAY'S pipeline (v2-m3 reranker + websearch FTS — the
set will approximate, not equal, the old 32). Persist cases, buckets,
miss-set, and baseline reports in seeding-eval/data/ (large files gitignored,
README with counts committed). Never /tmp again.

## The settle (chapterhouse-side, in the episodic handler)

- Seeds: the query's vector union FTS hits with normalized retrieval scores.
- Neighborhood: frontier expansion via LookupAssociations, hop cap 3, node
  cap ~2000, hebbian edges symmetrized, weights row-normalized.
- Iterate a <- (1-lambda)*s + lambda*W'^T a until ||delta a||_1 < eps or 20
  iterations. lambda default 0.7 (contraction => guaranteed convergence).
- KNOWN LEVER (spec-review finding): MaxIters=20 is a latency choice, not a
  convergence guarantee — sparse neighborhoods (a 3-node chain needs ~41
  iters) get a truncated-but-monotone approximation; dense Hebbian cliques
  mix in far fewer. If eval shows failures on low-activity queries, raise
  MaxIters or add an adaptive stop first.
- Output: NEW `expansion` sub-list in the episodic query response:
  {event_id, activation, text} for top-M non-seed nodes (M default 25).
  Explicitly NOT an RRF tier.

## Ghola-core integration (core.go seam, post-FuseRRF)

Expansion candidates append to the rerank pool with rrf=0.
- Config A ("expand"): final score from cross-encoder only (their RRF term
  is zero in FuseScores).
- Config B ("channel"): FuseScores gains an activation term with weight w_a
  (default 0.2), three-way blend rrf/rerank/activation.
Flag on /v1/recall: settle: off|expand|channel (+ lambda, hop cap, node cap,
M, eps, w_a with defaults). Threaded through MCP recall + Python harness
(--settle).

## Eval protocol

Run matrix on regenerated bed: {baseline-prim, settle-expand, settle-channel}
x n=500; bridge miss-set breakdown reported separately per config; latency
recorded per config. LongMemEval R@5 gate on the winning config.

## Testing

Go unit: convergence/contraction on synthetic graph; determinism;
seed-score weighting; hop/node cap enforcement; foreign-clique suppression
regression (single-edge foreign clique < multi-path thread member activation
— encodes the 2b.3 failure shape). Handler tests: expansion sub-list shape,
flag off => absent. Core tests: rrf=0 for expanded, config A/B fusion.
Harness: --settle flag plumbed.

## Failure protocol

One-commit revert; findings to memory + docs/status/ either way (a falsified
P4 is paper-#2 content too).

## Rejected

Cross-tier-validated expansion (vector top-200 gate): re-couples the
mechanism to vector reach exactly where the diagnosis says vector fails —
weakens the hypothesis test.
