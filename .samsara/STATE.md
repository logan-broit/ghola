# State

## Current Baseline (v0.0.5 + concept enrichment, 2026-04-11)

```
                         R@1     R@5    R@10     MRR       N
------------------------------------------------------------
Overall                14.6%   29.2%   41.8%   0.213     500
------------------------------------------------------------
knowledge-update       20.5%   61.5%   82.1%   0.374      78
multi-session           1.5%   11.3%   22.6%   0.059     133
single-session-asst    91.1%  100.0%  100.0%   0.955      56
single-session-pref     0.0%    0.0%    3.3%   0.006      30
single-session-user     1.4%    4.3%    8.6%   0.027      70
temporal-reasoning      2.3%   18.0%   39.1%   0.103     133
```

## Iteration History

| Iter | R@5 | Delta | What | Kept? | Details |
|------|-----|-------|------|-------|---------|
| 0 | 18.0% | +4.4 | Four-pathway retrieval (semantic, lexical, entity, cluster) | yes | [000.md](iterations/000.md) |
| 1 | 18.0% | +0.0 | Preference pathway analysis -- root cause is encoding-time | reverted | [001.md](iterations/001.md) |
| 2 | 29.2% | +11.2 | Concept enrichment: user-turn text at weight A in tsvector | yes | [002.md](iterations/002.md) |
| 3 | 28.6% | -0.6 | ts_rank_cd for tie-breaking -- smaller FTS values regressed | reverted | [003.md](iterations/003.md) |
| 4 | 12.4% | -16.8 | OR-based lexical filter -- precision loss > recall gain | reverted | [004.md](iterations/004.md) |
| 5 | 28.4% | -0.8 | Temporal stop word stripping -- displaced marginal hits | reverted | [005.md](iterations/005.md) |

## Key Constraints Discovered

- Session-level embeddings for long sessions produce diluted representations (Iter 1)
- Benchmark requires truncate + re-ingest for fair comparison (access_count drift, Iter 1)
- Intent classifier misses 41% of preference sessions (Iter 1)
- concept field was wasted on metadata; enrichment with user-turn text = +11.2pp (Iter 2)
- ts_rank_cd breaks ties but reduces FTS contribution via tanh() (Iter 3)
- OR-based FTS filter floods pool, precision loss > recall gain (Iter 4)
- Modifying existing lexical pathway displaces marginal hits globally (Iter 5)
- Retrieval-time fixes have diminishing returns; encoding-time changes needed (Iter 1-5)

## What To Try Next

Retrieval-time candidate generation changes are showing diminishing returns (3 reverts
in a row). The remaining weak categories need encoding-time intervention:

1. **Multi-granularity encoding** (high impact, high effort): Extract per-turn or per-fact
   sub-mnemes during gating. Helps single-session-user, multi-session, temporal simultaneously.
   Requires schema changes + embedding generation at gating time.

2. **Additive fallback lexical pathway** (low effort, limited impact): New CTE with cleaned
   query, fires only when strict lexical returns 0 results. Avoids displacing existing hits.
   Estimated +1 temporal query per benchmark.

3. **Temporal retrieval pathway** (moderate effort): Use content_dates column in a new CTE.
   Many temporal-reasoning queries include date cues not leveraged today.

## Roadmap

- [ ] Multi-granularity encoding (per-turn sub-mnemes)
- [ ] Additive fallback lexical pathway (cleaned query, fires on zero strict results)
- [ ] Temporal retrieval pathway (content_dates CTE)
- [ ] Session-context boosting (leverage session associations)
- [ ] Fix recall function lifecycle (avoid manual CREATE FUNCTION per deploy)
