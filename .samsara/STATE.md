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

- [ ] Analyze single-session-preference failures (0.0% R@5) -- why does retrieval completely fail?
- [ ] Analyze single-session-user failures (1.4% R@5) -- similar pattern to preferences?
- [ ] Analyze multi-session failures (6.0% R@5) -- cross-session retrieval weakness
- [ ] Temporal retrieval pathway -- use content_dates column in recall CTE
- [ ] Session-context boosting -- leverage session associations for same-session queries
- [ ] Preference-aware retrieval -- intent='preference' should boost preference mnemes

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
