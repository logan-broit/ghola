# HOLA Surprise-Retention Pilot — Correlational Analysis

**Date:** 2026-07-05  
**Workspace:** `a4f8bdd2-65c0-44b6-990d-ef2d1f8dc479`  
**Status:** Read-only analysis, no code changes, no commits.

---

## 1. Method

### Motivation

HOLA (arXiv 2607.02303) shows that in a bounded exact memory store, what to
keep matters more than how recent: items should be retained by *surprise* (how
badly the compressed tier failed to predict them), not recency. The matching
ablation reports recall scores of 0.58 (surprise-gated) vs 0.24 (recency-gated)
vs 0.14 (no filter). This pilot asks: on the seeding-eval corpus, are the
answer-bearing events (ground-truth set) disproportionately the surprising ones?

### Semantic-tier-absent deviation

HOLA's retention signal is computed as the residual between a new event's
embedding and its best reconstruction from the *semantic (compressed) tier*.
The eval workspace has no semantic tier — all 4,890 events are raw episodic
records, none have been distilled.

**Proxy used:** corpus-density surprise.

    surprise(e) = 1 - mean_cosine_similarity(e, k=10 nearest neighbors, excl. self)

Justification: low local density in embedding space means no other event would
have predicted this one; this is the "the model would not have generated this
from context" intuition without requiring a trained delta rule. It is cruder than
HOLA's residual — it measures semantic novelty relative to the corpus, not
task-relevant prediction error — but it is computable on the available data.

### Data

- **Corpus:** `episodic.events` for workspace `a4f8bdd2-65c0-44b6-990d-ef2d1f8dc479`.
  4,890 unique events, all with embeddings (vector(1024), Qwen3-Embedding-0.6B).
  All 4,890 events are type=`user`; content type is carried in the `tags` array
  (`type:commit`, `type:pr`, `type:issue`).
- **Ground truth:** `seeding-eval/data/results-baseline-prim/per-case-traces.jsonl`,
  union of `ground_truth_event_ids` across `variant=none` rows (132 cases).
  699 unique GT event IDs. All 699 are present in the workspace (0 missing).
- **Background:** 4,191 workspace events not in the GT set.

No background sampling was needed — 4,890 events fit comfortably in memory.

### Computation

1. Pulled all 4,890 event embeddings from Postgres via `COPY TO STDOUT`.
2. L2-normalized all vectors.
3. Computed the full (4890 x 4890) cosine similarity matrix in batches of 500
   (float32, ~96 MB peak RAM). Set self-similarity to -inf.
4. For each event, took the mean cosine similarity of its k=10 nearest
   neighbors; surprise = 1 - that mean.
5. Mann-Whitney U statistic computed by rank-sum formula; z-score via normal
   approximation (no scipy available in seeding-eval/.venv); two-tailed p-value
   via the error function.

---

## 2. Results

### 2.1 Overall surprise distributions

| Set | n | Median | Mean | Std | p10 | p90 |
|-----|---|--------|------|-----|-----|-----|
| GT (answer-bearing) | 699 | 0.2493 | 0.2458 | 0.0706 | 0.1662 | 0.3249 |
| Background | 4191 | 0.2513 | 0.2463 | 0.0678 | 0.1593 | 0.3253 |
| **Delta (GT - BG)** | | **-0.0019** | **-0.0005** | | | |

The distributions are essentially identical. GT events are, if anything, very
slightly *less* surprising than background (negative delta of 0.002 — less than
one-thirtieth of a standard deviation).

### 2.2 Mann-Whitney U test

| Statistic | Value |
|-----------|-------|
| U_GT | 1,453,850 |
| U_BG | 1,475,659 |
| z (GT > BG direction) | -0.316 |
| p (two-tailed, normal approx.) | 0.752 |
| Effect size r = z / sqrt(N) | -0.0045 |

**Conclusion:** fail to reject H0. No statistically significant difference.
Effect size is negligible (|r| < 0.005).

### 2.3 Lift curve

If surprise-gated retention preserved only the top-X% most surprising events,
what fraction of GT events would survive?

| Retention budget | Events kept | GT events kept | GT recall | Lift vs random |
|-----------------|-------------|----------------|-----------|----------------|
| Top 10% | 489 | 70 / 699 | 0.100 | 1.00x |
| Top 25% | 1,222 | 168 / 699 | 0.240 | 0.96x |
| Top 50% | 2,445 | 341 / 699 | 0.488 | 0.98x |
| Top 75% | 3,667 | 533 / 699 | 0.763 | 1.02x |

All lifts are within rounding of 1.0x. Surprise-sorted retention is
indistinguishable from random retention on this corpus. Retaining the top 25%
most surprising events misses 60% of GT events — the same as randomly keeping
any 25%.

---

## 3. Event-type analysis

The workspace contains three content types:

| Type | n | % GT | Median surprise |
|------|---|------|-----------------|
| commit | 3,390 | 16.7% | 0.2349 |
| pr | 750 | 17.6% | 0.2844 |
| issue | 750 | 0.0% | 0.2735 |

Issues contribute zero GT events: the seeding-eval cases are answered entirely
by commits and PRs. The type-level surprise ordering (pr > issue > commit)
reflects structural embedding density rather than answer-bearing relevance.

### Within-type: do GT events rank higher by surprise than same-type BG events?

| Type | n_GT | n_BG | GT med | BG med | Delta | z | p | r |
|------|------|------|--------|--------|-------|---|---|---|
| commit | 567 | 2,823 | 0.2393 | 0.2337 | +0.0056 | +2.38 | 0.017 | +0.041 |
| pr | 132 | 618 | 0.2814 | 0.2849 | -0.0034 | -0.17 | 0.865 | -0.006 |
| issue | 0 | 750 | n/a | 0.2735 | n/a | n/a | n/a | n/a |

The commits result (z=2.38, p=0.017) is the only nominally significant finding.
However:

- Effect size r=0.041 is negligible by any convention (small = 0.1).
- The commit lift curve confirms this: keeping the top 25% most surprising
  commits recovers 26.8% of GT commits (lift=1.07x). The top 10% recovers 9.5%
  of GT commits (lift=0.95x — below random). There is no operational value.
- With three simultaneous tests (one per content type), the p=0.017 result does
  not survive even a Bonferroni correction (threshold = 0.017).
- The signal most likely reflects the fact that commits answering specific bugs
  touch unusual code paths (specific diff text), which inflates their embedding
  distance from the commit-cluster center. This is a corpus-density artifact, not
  a task-relevant surprise signal.

**Type confound verdict:** surprise is partly confounded by type (commits score
~0.015 lower than PRs/issues), but this confound does not explain the null
result — the null holds within each type separately.

---

## 4. Verdict

**Does surprise-gated retention (density proxy) preserve what recall needs,
on this corpus?**

**No. The signal is absent.**

Quantitatively: overall lift is 1.00x at all budget levels; Mann-Whitney
p=0.752, r=-0.005. The density proxy for surprise is orthogonal to GT membership.
The small commit-level signal (r=0.04) is statistically marginal and operationally
inert.

This is a negative result, not a null-hypothesis-confirmed result. Three
alternative explanations are possible:

1. **The proxy is wrong.** Corpus-density surprise (kNN distance) measures how
   isolated an event is in the joint embedding space of 4,890 events. HOLA's
   actual signal is the residual between a *new* event and what the compressed
   (semantic) tier predicted — a temporal, task-shaped residual, not a static
   pairwise distance. These are different quantities. An event can be
   semantically isolated and answer-irrelevant (a rare commit to an obscure
   module), or semantically common and answer-bearing (a standard API change
   that fixes the exact reported bug).

2. **The corpus structure doesn't give surprise the right signal.** The
   seeding-eval workspace was built by seeding one event per commit/PR/issue
   in a large repository. Every event is already a high-level summary (not raw
   tool output, raw conversation turns, or repeated boilerplate). This is the
   opposite of the HOLA setting where the compressive tier must generalize over
   many similar streaming events and the surprising ones stick out. Here, all
   events are pre-distilled; the density distribution is flatter.

3. **GT events are not structurally unusual.** The seeding-eval GT set is
   defined by causal relevance to a bug: the commit that fixed it, the PR that
   reviewed it. Causal relevance and embedding-space isolation are unrelated
   properties. The fix commit for a common React rendering bug is probably
   embedded close to other React rendering commits.

---

## 5. What a real residual test would need

A genuine HOLA pilot on ghola requires:

1. **A populated semantic tier.** The semantic tier must have been trained or
   seeded from episodic events (P5/P6 consolidation). Without it, there is
   nothing to compute a residual against.

2. **Residual computation:** for each episodic event, embed it and find its
   nearest semantic neighbor (or k-NN weighted reconstruction); residual =
   distance from event embedding to that reconstruction. This is the quantity
   HOLA's beta gate acts on.

3. **Temporal ordering.** HOLA's retention decision is made *at ingestion time*,
   not post-hoc. The residual should be computed against the semantic tier state
   at the moment the event arrived, not against a tier that has already
   consolidated all events.

4. **A richer event mix.** The signal should be strongest in a workspace with
   redundant events (many similar assistant turns, repeated tool outputs) mixed
   with rare decisive events (the one commit that closed the bug). The seeding
   corpus has no redundancy — every event is pre-deduplicated.

5. **Outcome variable.** The pilot should measure whether semantic-tier-residual
   correlates with `needed_by_recall` labels derived from seeding-eval or a
   similar harness.

The density proxy tested here is a reasonable first approximation given the
available data, but the negative result is informative rather than conclusive:
it rules out the cheap proxy, not the HOLA principle.

---

## Centroid-residual pass

**Date:** 2026-07-05
**Method:** Rate-limited codebook (KMeans at k in {32, 64, 128}) over the same
4,890 L2-normalized embeddings. KMeans++ initialization, seed=42, 50 iterations
maximum, cosine distance via normalized dot product (numpy-only, no sklearn).
Residual: `residual(e) = 1 - cosine_sim(e, nearest centroid)`.

---

### CR-1. Residual distributions

| k | Set | n | Median | Mean | Std | p10 | p90 |
|---|-----|---|--------|------|-----|-----|-----|
| 32 | GT | 699 | 0.2388 | 0.2394 | 0.0630 | 0.1677 | 0.3147 |
| 32 | Background | 4191 | 0.2368 | 0.2389 | 0.0644 | 0.1622 | 0.3208 |
| 64 | GT | 699 | 0.2187 | 0.2201 | 0.0633 | 0.1471 | 0.2993 |
| 64 | Background | 4191 | 0.2168 | 0.2188 | 0.0627 | 0.1429 | 0.2978 |
| 128 | GT | 699 | 0.2032 | 0.2035 | 0.0651 | 0.1317 | 0.2806 |
| 128 | Background | 4191 | 0.1997 | 0.2001 | 0.0632 | 0.1220 | 0.2771 |

All three codebooks show GT events are slightly higher-residual than background (delta
+0.0020 to +0.0035), but the distributions overlap almost completely (delta is
less than one-eighteenth of a standard deviation).

---

### CR-2. Mann-Whitney rank-sum tests

| k | U_GT | U_BG | z | p (two-tailed) | Effect r |
|---|------|------|---|----------------|----------|
| 32 | 1,488,192 | 1,441,318 | +0.678 | 0.498 | +0.010 |
| 64 | 1,500,246 | 1,429,262 | +1.027 | 0.304 | +0.015 |
| 128 | 1,512,972 | 1,416,537 | +1.395 | 0.163 | +0.020 |

**None reach significance at any conventional alpha (0.05, 0.01, 0.001).** There
is a weak positive trend as k increases (finer codebook produces slightly more
separation), but the effect sizes remain negligible by any convention (small = 0.1).

---

### CR-3. Retention lift curves

Fraction of the 699 GT events surviving if only the top-X% highest-residual events
are kept.

| Retention budget | k=32 GT recall (lift) | k=64 GT recall (lift) | k=128 GT recall (lift) |
|-----------------|----------------------|----------------------|----------------------|
| Top 10% (n=489) | 0.090 (0.90x) | 0.106 (1.06x) | 0.110 (1.10x) |
| Top 25% (n=1,222) | 0.253 (1.01x) | 0.259 (1.04x) | 0.259 (1.04x) |
| Top 50% (n=2,445) | 0.508 (1.02x) | 0.515 (1.03x) | 0.525 (1.05x) |
| Top 75% (n=3,668) | 0.778 (1.04x) | 0.773 (1.03x) | 0.774 (1.03x) |

At 25% retention (the operationally relevant budget): lift is 1.01x to 1.04x.
Retaining the top 25% by centroid residual recovers 177-181 GT events out of 699
— essentially identical to randomly retaining any 25% (random expectation: 175).
The improvement at 10% for k=128 (1.10x) is the strongest signal seen, but even
at 1.10x lift the top-10% bucket contains only 77 of 699 GT events; a 90% discard
rate eliminates 89% of the answer-bearing events. There is no operational value.

---

### CR-4. Within-type slices

| k | Type | n_GT | n_BG | GT med | BG med | Delta | z | p | r |
|---|------|------|------|--------|--------|-------|---|---|---|
| 32 | commit | 567 | 2823 | 0.2316 | 0.2254 | +0.0063 | +1.98 | 0.047 | +0.034 |
| 32 | pr | 132 | 618 | 0.2803 | 0.2793 | +0.0010 | -0.70 | 0.483 | -0.026 |
| 64 | commit | 567 | 2823 | 0.2124 | 0.2082 | +0.0043 | +1.96 | 0.050 | +0.034 |
| 64 | pr | 132 | 618 | 0.2460 | 0.2516 | -0.0056 | -0.27 | 0.784 | -0.010 |
| 128 | commit | 567 | 2823 | 0.1964 | 0.1889 | +0.0075 | +2.92 | 0.003 | +0.050 |
| 128 | pr | 132 | 618 | 0.2319 | 0.2306 | +0.0013 | +0.57 | 0.568 | +0.021 |
| all | issue | 0 | 750 | n/a | — | n/a | n/a | n/a | n/a |

The commit slice produces nominally significant results at all k values, most
strongly at k=128 (z=2.92, p=0.003). However:

- Effect size at k=128 is r=0.050 — still negligible (small = 0.1).
- The commit result at k=32 (p=0.047) and k=64 (p=0.050) sits right at the
  alpha=0.05 threshold. With three simultaneous type-slice tests, a Bonferroni
  correction shifts the threshold to 0.017: k=32 and k=64 commit results do not
  survive; k=128 does (p=0.003 < 0.017), but the effect size remains tiny.
- The commit k=128 result echoes the pattern from the density-surprise pass
  (r=0.041, p=0.017): commits answering specific bugs sit slightly further from
  their cluster centroid because they touch unusual code paths — a structural
  corpus artifact, not a task-relevant signal.
- The PR slice is null at all k (p range: 0.483 to 0.784).

---

### CR-5. Spearman correlation between density-surprise and centroid-residual

| k | Spearman rho | t-statistic (df=4888) |
|---|-------------|----------------------|
| 32 | 0.738 | 76.5 |
| 64 | 0.790 | 90.0 |
| 128 | 0.776 | 86.1 |

The two metrics are strongly correlated (rho 0.74-0.79). They are not independent
signals on this corpus. The centroid-residual is largely measuring the same quantity
as the kNN density proxy: how isolated an event's embedding is relative to the rest
of the corpus. A finer codebook (k=128) captures more local structure, producing
marginally better GT discrimination, but the underlying information is the same.

---

### CR-6. Verdict

**Does centroid-residual (rate-limited codebook) separate GT from background where
density-surprise did not?**

**No. Both metrics null at the corpus level.** The centroid-residual produces
slightly stronger (though still insignificant) overall separation than the density
proxy (p=0.163 vs p=0.752 at the best k), and a commit-level signal at k=128 that
survives Bonferroni correction (r=0.050) — but the operational lift at 25%
retention budget is 1.04x vs random, identical to the density proxy result.

The two metrics are measuring the same underlying dimension (embedding isolation),
not independent aspects of surprise. Running both does not triangulate toward a
positive finding.

**Combined interpretation (density-surprise + centroid-residual):**

Both proxy metrics for HOLA's retention signal null on this corpus. This is a
corpus-structure verdict, not a verdict on the HOLA principle:

1. **These two metrics are not the actual HOLA signal.** HOLA's residual is
   computed against a *trained semantic tier at ingestion time* — a temporal,
   task-shaped delta rule. Static geometric isolation in a pre-distilled corpus
   is a different quantity.

2. **This corpus cannot test HOLA's principle.** The seeding-eval workspace
   contains one pre-distilled summary per commit/PR/issue — no redundancy, no
   boilerplate, no repeated tool outputs. The HOLA setting requires a stream of
   similar events where the semantic tier has generalized over patterns and the
   surprising ones are exceptions. That precondition is absent here.

3. **What a proper test bed needs** (reiterating Section 5 with updated evidence):
   - A populated semantic tier that has consolidated from episodic events (P5/P6
     consolidation running in production).
   - A corpus with natural redundancy: raw assistant turns, repeated tool outputs,
     overlapping observations of the same codebase state.
   - Retention decisions computed at ingestion time against the semantic tier state
     at that moment, not post-hoc against a static embedding matrix.
   - The **dogfooding workspace** (ghola's own conversation history, once the
     semantic tier is populated) is the natural next test bed: it will contain
     genuine redundancy (repeated recall queries, similar assistant turns, tool
     outputs from the same files), and recall-feedback signals (`feedback` scores)
     can serve as weak labels for "this memory was useful". The prior pilot and
     this pass have established the analysis pipeline; plugging in the dogfooding
     workspace and a populated semantic tier is the concrete follow-up.

**No follow-up is recommended on this corpus.** The pipeline is validated and
ready for the dogfooding workspace when the semantic tier is available.
