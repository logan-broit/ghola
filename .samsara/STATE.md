# State

## Current Baseline (v0.0.5, 2026-04-10)

```
                         R@1     R@5    R@10     MRR       N
------------------------------------------------------------
Overall                 7.4%   18.0%   24.6%   0.119     500
------------------------------------------------------------
knowledge-update       21.8%   38.5%   52.6%   0.293      78
multi-session           1.5%    6.0%    9.8%   0.033     133
single-session-assistant   25.0%   53.6%   66.1%   0.370      56
single-session-preference    0.0%    0.0%    0.0%   0.000      30
single-session-user     0.0%    1.4%    2.9%   0.009      70
temporal-reasoning      3.0%   15.8%   22.6%   0.083     133
```

## Historical Baselines

| Version | R@5 | Notes |
|---------|-----|-------|
| v0.0.3 (pre-gating) | 19.4% | Pure HNSW + FTS fallback |
| v0.0.4 (FTS-gated) | 13.6% | FTS gate silenced semantic pathway (regression) |
| v0.0.5 (multi-pathway, no entities) | 16.0% | Gating worker crash-looping, no entity/cluster coverage |
| v0.0.5 (multi-pathway, full coverage) | 18.0% | All 4 pathways active, entities + clusters populated |

## Roadmap

### Immediate (eval-driven, one per samsara iteration)

- [x] Analyze single-session-preference failures (0.0% R@5) -- Iter 1: root cause is encoding-time, not retrieval-time
- [ ] Clean benchmark protocol -- truncate + re-ingest before measuring (eliminates access_count drift)
- [ ] Encoding-time preference extraction -- heuristic extraction of user preference statements during gating
- [ ] Analyze single-session-user failures (1.4% R@5) -- likely same diluted-embedding problem as preferences
- [ ] Analyze multi-session failures (6.0% R@5) -- cross-session retrieval weakness
- [ ] Temporal retrieval pathway -- use content_dates column in recall CTE
- [ ] Session-context boosting -- leverage session associations for same-session queries

### Architecture (larger efforts, plan before implementing)

- [ ] Fix recall function lifecycle -- avoid manual CREATE FUNCTION after every deploy
- [ ] Interference patterns -- similar memories competing/inhibiting (lateral inhibition)
- [ ] Attention/salience weighting -- recent access patterns modulate retrieval priority
- [ ] Hierarchical clustering -- replace flat k-means with nested structure
- [ ] Consolidation-driven re-embedding -- recluster after significant Hebbian weight changes

## Iteration Log

### Iteration 0 (2026-04-10, manual)

**What**: Replaced FTS-gated HNSW with four-pathway union (semantic, lexical, entity, cluster).
Renamed Hebbian worker to consolidation worker. Added linfa k-means clustering.

**Hypothesis**: FTS gate was silencing semantically relevant candidates. Additive pathways
should recover lost recall without sacrificing FTS benefits.

**Result**: R@5 13.6% -> 18.0%. Recovered most of the regression. Entity pathway and
cluster pathway both contributing. knowledge-update improved dramatically (23.1% -> 38.5%).

**Bugs found**: pgvector lacks scalar arithmetic, gating worker processed 1 item/5s,
trailing comma in SQL CTE, recall functions not recreated on deploy, ndarray version mismatch.

**Next**: Analyze the three near-zero categories to understand failure modes.

### Iteration 1 (2026-04-11, samsara)

**What**: Deep analysis of single-session-preference failures (0.0% R@5). Tried adding a
5th retrieval pathway that searches within preference-intent mnemes for recommendation queries.
Reverted after benchmark showed ambiguous results due to access_count drift.

**Hypothesis**: Adding a context-dependent retrieval pathway (Godden & Baddeley, 1975)
that filters by intent='preference' for recommendation-style queries would surface
preference answer sessions that are lost in the general candidate pool.

**Analysis findings**:
1. Preference queries are generic ("Can you recommend video editing resources?") while
   answer sessions are specific (18+ turns, 25K chars about Adobe Premiere Pro).
2. Entity pathway is completely dead for these queries -- no extractable proper nouns.
3. Lexical pathway includes answer sessions (rank ~58 of 90) but they get outscored.
4. 59% of preference answer sessions have intent='preference', 32% have 'question', 9% 'plan'.
5. The intent classification misses 41% of preference answer sessions.

**Implementation**: Added `is_recommendation_query()` + preference CTE to recall.
Tightened keywords to 0% false positive rate for non-preference categories.

**Result**: Preference R@5 stayed at 0.0%. One hit at rank 5 (paintings query) appeared but
was from access_count drift, not the pathway (answer session had intent='plan', not 'preference').
Overall scores showed ~5pp R@5 regression, but this was confirmed as access_count drift:
knowledge-update (0% recommendation detection) swung by 11.6pp between consecutive runs.

**Root cause identified**: The problem is NOT candidate generation -- answer sessions DO enter
the candidate pool via the lexical pathway. The problem is that their composite scores are
genuinely lower than competing candidates because:
- Session-level embeddings (25K chars) produce diluted representations vs generic queries
- Access_count rich-get-richer effect systematically disadvantages never-retrieved mnemes
- No amount of retrieval-pathway changes can overcome this -- the scoring sees them as less relevant

**Key insight**: Preference retrieval requires ENCODING-TIME changes, not RETRIEVAL-TIME changes.
The embedding of a 25K-char conversation doesn't capture "this user prefers Premiere Pro" --
that preference signal is diluted across 18 turns of technical discussion.

**Methodological issue**: Benchmark comparisons are unreliable without clean database state.
Access_count accumulates across runs, creating rich-get-richer drift. Future iterations MUST
truncate and re-ingest before benchmarking (adds ~17 minutes but produces fair comparisons).

**Reverted**: Yes -- code change reverted and original version redeployed.

**Next**: Two possible approaches for next iteration:
1. **Encoding-time preference extraction** (recommended): During gating, extract a
   preference summary and store it as additional metadata or as a separate sub-mneme.
   Requires either (a) LLM call during gating, or (b) heuristic extraction of user
   preference statements ("I enjoy X", "I prefer Y", "I use Z").
2. **Multi-scale embedding**: Store multiple embeddings per mneme at different granularities
   (full session, first user turn, preference statements only). Requires schema changes.

Before implementing either, the next iteration should start with a CLEAN benchmark
(truncate + re-ingest) to establish a reliable baseline.
