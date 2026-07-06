# P4.2 Edge-Quality Forensics (2026-07-05)

## Purpose

Determine what structural features distinguish the noise winners (LOSS mechanism) from
GT nodes in the 30 LOSS + 13 WIN cases at w=0.45, to guide or kill the edge-penalty
direction (hub/degree normalization, promiscuity downweighting, saturation curve changes).

---

## Method

**Case derivation** (from `results-baseline-prim/per-case-traces.jsonl` and
`results-p4-channel045/per-case-traces.jsonl`, variant=none):
- LOSS: baseline hit (hit_p_at_5=1) AND channel045 miss (hit_p_at_5=0) — 30 cases total
- WIN: baseline miss AND channel045 hit — 13 cases total

**Sample analyzed**: 15 LOSS (sampled for runtime) + all 13 WIN.

**Node classes defined per case**:
- NOISE: top-5 channel045 nodes NOT in ground_truth
- GT_LOSS: GT nodes NOT in channel045 top-5 (displaced)
- GT_WIN: GT nodes IN channel045 top-5 (promoted)

This yielded 75 NOISE records, 49 GT_LOSS records, 23 GT_WIN records.

**Features computed from `semantic.associations`
(workspace_id=a4f8bdd2-65c0-44b6-990d-ef2d1f8dc479) and `episodic.events`**:
- `degree`: count of edges (src OR dst) for the node in the eval workspace
- `coact`: sum of co_activations across all workspace edges
- `seed_attach`: number of 1-hop seed neighbors (seeds = baseline top_k event_ids)
- `intra_cluster`: fraction of edges among node's top-10 co-neighbors that are connected
- `max_edge_to_seed`: max undirected edge weight to any seed neighbor
- `sess_size`: event_count of the node's session (from `episodic.sessions`)
- `shares_sess_with_seed`: boolean — does the node share a session_id with any seed?

**Penalty simulation**: re-ran settle offline for all 28 cases under degree^alpha edge
normalization (alpha in {0.0, 0.5, 1.0}) using the pre-fetched edge subgraph, then
reapplied channel fusion at w=0.45.

---

## Per-Class Feature Table

| Feature             | NOISE (n=75) | GT_LOSS (n=49) | GT_WIN (n=23) |
|---------------------|--------------|----------------|---------------|
| degree (median)     | 4.0          | 0.0            | 4.0           |
| coact (median)      | 4.0          | 0.0            | 4.0           |
| seed_attach (median)| 1.0          | 0.0            | 1.0           |
| intra_cluster (med) | 0.833        | 0.000          | 1.000         |
| max_edge_to_seed    | 0.181        | 0.000          | 0.181         |
| zero-degree %       | 5.3%         | **85.7%**      | 4.3%          |
| shares session %    | 60.0%        | 100.0%         | 100.0%        |

---

## The Separating Feature

**Global degree (edges in workspace a4f8bdd2) is the dominant separator, F1=0.932.**

Threshold sweep: predict-noise if degree >= 3.
- Precision 0.958, Recall 0.907, F1 0.932 (NOISE vs GT_LOSS, node level).

At the case level, the rule "if any GT node has degree=0, this case will be a LOSS"
achieves P=0.923, R=0.800, F1=0.857 (12/15 LOSS correctly predicted, 12/13 WIN).

No other feature adds discriminative power: all GT_LOSS zero-degree nodes also have
coact=0, seed_attach=0, intra_cluster=0, max_edge_to_seed=0 by construction (they have
no edges at all). The session-sharing feature is useless: both GT_LOSS and GT_WIN nodes
share a session with seeds 100% of the time.

---

## Root Cause: Workspace Partitioning

The zero-degree condition is not intrinsic to the GT nodes — it is a data artifact.

All 42 GT_LOSS zero-degree nodes were checked against the full association table.
**Every one of them has edges, but under workspace 4d189ec1 (old workspace, 500
sessions, 13,454 edges), not under the eval workspace a4f8bdd2 (750 sessions, 8,842
edges).**

The settle graph is built by `BuildSettleGraph` with `workspaceID=a4f8bdd2`. The DB
query is workspace-scoped, so edges in 4d189ec1 are never loaded. The GT nodes exist in
the event table (events are session-scoped, not workspace-scoped) but their Hebbian
co-activation history is in the wrong workspace. The settle cannot reach them regardless
of weight, damping, or iteration count.

The noise winners are expansion-tier nodes (94.7% of NOISE) whose Hebbian edges ARE in
workspace a4f8bdd2. The settle activates them correctly; there is nothing wrong with the
algorithm or these nodes' structural role.

**Summary of the two LOSS sub-populations:**

1. **12/15 LOSS cases** (primary): GT nodes have zero workspace-a4f8bdd2 degree.
   The settle graph simply does not contain them. These are invisible to every graph
   mechanism, not just to settle.

2. **3/15 LOSS cases** (secondary, #53937, #54210, #55862): GT nodes ARE in the
   graph with degree 2-3, but noise winners have degree 4-9 (median 5). The GT nodes
   receive settle activation but are outvoted by higher-degree nodes that accumulate
   more cross-session mass.

---

## Penalty Simulation

Penalty tested: multiply each edge weight by 1/degree(dst)^alpha, then row-normalize.
Alphas tested: 0.0 (no change), 0.5 (sqrt), 1.0 (full degree).

| alpha | LOSS hits / total | WIN hits / total |
|-------|-------------------|------------------|
| 0.0   | 10 / 15           | 3 / 13           |
| 0.5   | 10 / 15           | 3 / 13           |
| 1.0   | 10 / 15           | 3 / 13           |

**Result: zero effect.** The penalty makes no difference to top-5 outcomes.

Reason: the penalty can only redistribute activation among nodes that are already in
the graph. For 85.7% of LOSS GT nodes, those nodes are not in the graph at all — the
penalty has nothing to act on. For the 3 secondary-mechanism cases, the GT degree (2-3)
is too low and noise degree (4-9) too high; even full normalization leaves noise ahead
because the GT nodes still lose in absolute activation mass.

---

## Recommendation: Do Not Build Edge Penalties

The edge-penalty direction (hub normalization, promiscuity downweighting, saturation
curves) is **not supported**. The zero-penalty sim result is the proof: the optimization
surface these changes address does not contain the LOSS events.

Two actual root causes, and their remedies:

### 1. Workspace migration gap (primary cause, 12/15 LOSS cases)

The phase0 bed regeneration created workspace a4f8bdd2 with 750 sessions and 8,842
Hebbian edges. A prior workspace 4d189ec1 holds 500 of those same sessions' Hebbian
histories with 13,454 edges (richer because it captured more co-activation rounds or
ran a longer queue drain). 42 GT nodes that are reachable in 4d189ec1 are invisible
in a4f8bdd2 because their sessions' co-activation events went to the old workspace.

**Fix**: determine which sessions are present in both workspaces and re-derive or copy
their associations into a4f8bdd2. Alternatively, re-run the full Hebbian queue drain
for all 750 workspace sessions (the P4 phase0 notes say the co-activation queue was
"fully drained before measurement" — verify this is complete for all sessions, not just
the 500 from the old workspace).

This is a data completeness issue, not an algorithmic one. Fixing it does not require
any changes to settle, edge weighting, or the graph structure.

### 2. Degree imbalance (secondary cause, 3/15 LOSS cases)

Where GT nodes ARE in the graph, the noise wins because noise has 2-3x higher degree.
This is consistent with the foreign-clique hypothesis from P4-FINDINGS, but at a much
smaller scale than anticipated. The current damped fixed-point formulation suppresses
most of it (P4-FINDINGS confirms config B at 0.2 does not regress); the residual is
3 of 15 sampled LOSS cases.

If the workspace gap is fixed first (cause 1), the degree-imbalance population shrinks
to those 3 cases, which may be within acceptable tolerance. If further work is warranted
after a clean bed measurement, the appropriate intervention is **per-session mass
capping** (limit total activation that any single session can contribute), not
per-node degree normalization. Per-session capping would suppress dense same-session
clusters without penalizing legitimately high-degree cross-session nodes.

---

## What a Clean Measurement Looks Like

1. Confirm that all 750 workspace sessions have complete Hebbian edge coverage under
   workspace a4f8bdd2 (check: count(associations in WS) per session, compare to
   expected clique size given session event_count).
2. If sessions are under-covered, run `consolidate` / co-activation queue drain for
   those sessions scoped to workspace a4f8bdd2.
3. Re-run the P4 eval matrix on the corrected bed.

Only after a clean bed measurement can the true noise/GT separation be assessed.
The current result means: **most LOSSes are a measurement artifact, not a signal about
settle's behavior on real data**.

---

## Appendix: Key Numbers

- Eval workspace: a4f8bdd2-65c0-44b6-990d-ef2d1f8dc479 (750 sessions, 8,842 edges)
- Old workspace: 4d189ec1-c093-44ab-b0d5-144d280b5ffc (500 sessions, 13,454 edges)
- Bridge set: 55 cases
- At w=0.45: 13 WINs (12 bridge), 30 LOSSes
- Sample analyzed: 15 LOSS + 13 WIN
- GT_LOSS nodes with zero degree in eval WS: 42/49 = 85.7%
- All 42 have edges in old workspace: 100%
- GT_WIN nodes with zero degree: 1/23 = 4.3% (this one WIN is via episodic tier, not settle)
- NOISE tier distribution: expansion 94.7%, episodic 4.0%, session_vector 1.3%
- Best single-feature separator (degree >= 3 = noise): P=0.958, R=0.907, F1=0.932
- Degree penalty simulation result: zero effect at alpha 0.5 and 1.0
