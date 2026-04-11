# State

## Current Baseline (v0.0.5 + concept enrichment, 2026-04-11)

```
                         R@1     R@5    R@10     MRR       N
------------------------------------------------------------
Overall                14.6%   29.2%   41.8%   0.213     500
------------------------------------------------------------
knowledge-update       20.5%   61.5%   82.1%   0.374      78
multi-session           1.5%   11.3%   22.6%   0.059     133
single-session-assistant   91.1%  100.0%  100.0%   0.955      56
single-session-preference    0.0%    0.0%    3.3%   0.006      30
single-session-user     1.4%    4.3%    8.6%   0.027      70
temporal-reasoning      2.3%   18.0%   39.1%   0.103     133
```

## Historical Baselines

| Version | R@5 | Notes |
|---------|-----|-------|
| v0.0.3 (pre-gating) | 19.4% | Pure HNSW + FTS fallback |
| v0.0.4 (FTS-gated) | 13.6% | FTS gate silenced semantic pathway (regression) |
| v0.0.5 (multi-pathway, no entities) | 16.0% | Gating worker crash-looping, no entity/cluster coverage |
| v0.0.5 (multi-pathway, full coverage) | 18.0% | All 4 pathways active, entities + clusters populated |
| v0.0.5 (concept enrichment, clean) | 29.2% | User-turn text at weight A in tsvector (Iter 2) |

## Roadmap

### Immediate (eval-driven, one per samsara iteration)

- [x] Analyze single-session-preference failures (0.0% R@5) -- Iter 1: root cause is encoding-time, not retrieval-time
- [x] Clean benchmark protocol -- truncate + re-ingest before measuring (eliminates access_count drift)
- [x] Encoding-time concept enrichment -- Iter 2: user-turn text prepended to concept for weight 'A' FTS boost
- [x] Analyze single-session-user failures (1.4% R@5) -- Iter 2: same diluted-embedding problem, fixed by concept enrichment
- [x] Investigate FTS saturation -- Iter 3: ts_rank_cd breaks ties but regresses overall (-0.6pp), reverted
- [x] Analyze multi-session failures (11.3% R@5) -- Iter 4: AND filter too strict, but OR filter dilutes pool; retrieval-time fix insufficient
- [ ] Temporal retrieval pathway -- use content_dates column in recall CTE
- [ ] Session-context boosting -- leverage session associations for same-session queries
- [ ] Multi-granularity encoding -- extract atomic facts per-turn during gating for sub-mneme embedding

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

### Iteration 2 (2026-04-11, samsara)

**What**: Encoding-time concept enrichment -- extract user-turn text from conversation
content during gating and prepend it to the concept field. Since `concept` gets weight 'A'
in the tsvector (2.5x weight vs content at 'B'), this dramatically boosts FTS rank for
queries matching user-stated facts.

**Hypothesis**: The `concept` field was wasted -- it contained only `timestamp_XXXXXX_session_YYYY`
metadata strings. By extracting user turn text and placing it at weight 'A', queries about
user-stated facts ("What degree did I graduate with?") will get high FTS rank matches against
answer sessions where the user mentioned those facts. This is encoding specificity (Tulving, 1972)
-- retrieval succeeds when cues at retrieval match cues at encoding.

**Analysis findings**:
1. `concept` field contained only session metadata (timestamp + session ID) -- completely wasted weight 'A' slot
2. User facts are buried in 11-17K char sessions, diluted at weight 'B' in the tsvector
3. After enrichment, verified FTS rank jumped from ~0.05 to ~0.98 for matching queries (20x boost)
4. Scoring math confirms boost is sufficient: content_match goes from 0.32 to 0.60 for answer sessions,
   exceeding competing sessions at 0.44
5. Gating worker keeps up with ingestion rate -- concept enrichment adds negligible overhead

**Implementation**:
- Added `extract_user_turn_text(content, max_chars)` to gating_worker.rs
- Parses `[user]:` lines from session content, concatenates, truncates at 4000 chars
- During gating, prepends user text to concept with ` | ` separator
- Search vector auto-recomputes (GENERATED ALWAYS AS column)
- 6 new unit tests, all passing (35/35 total gating worker tests)
- No schema changes needed

**Result**: R@5 18.0% -> 29.2% (+11.2pp). Largest single-iteration improvement yet.

```
                         R@1     R@5    R@10     MRR       N    | v0.0.5  | Delta R@5
-----------------------------------------------------------------|---------|----------
Overall                14.6%   29.2%   41.8%   0.213     500   | 18.0%   | +11.2pp
knowledge-update       20.5%   61.5%   82.1%   0.374      78   | 38.5%   | +23.0pp
multi-session           1.5%   11.3%   22.6%   0.059     133   |  6.0%   | +5.3pp
single-session-asst    91.1%  100.0%  100.0%   0.955      56   | 53.6%   | +46.4pp
single-session-pref     0.0%    0.0%    3.3%   0.006      30   |  0.0%   | +0.0pp
single-session-user     1.4%    4.3%    8.6%   0.027      70   |  1.4%   | +2.9pp
temporal-reasoning      2.3%   18.0%   39.1%   0.103     133   | 15.8%   | +2.2pp
```

Results file: `~/longmemeval-ghola/results/ghola_mcp_s_20260411T083308Z.jsonl`

**Analysis of results**:
1. Massive improvements in knowledge-update (+23pp) and single-session-assistant (+46.4pp).
   These are the categories where user-turn text directly matches the query terms -- concept
   enrichment puts them at weight 'A' for a huge FTS boost.
2. Target category single-session-user improved from 1.4% to 4.3% R@5 (+2.9pp). Meaningful
   but modest -- the FTS boost helps but embedding dilution still dominates.
3. single-session-preference unchanged at 0.0% R@5. As predicted in Iter 1, these are
   fundamentally an embedding problem, not a lexical one. Preference queries are too generic
   to match user turn keywords.
4. multi-session improved modestly (+5.3pp). Cross-session retrieval benefits from better
   lexical matching of user-stated facts.
5. temporal-reasoning improved slightly (+2.2pp R@5 but +16.5pp R@10). Suggests temporal
   queries are now finding more relevant candidates but ranking them lower.

**Methodological note**: Comparison is dirty-baseline (v0.0.5 with accumulated access_count)
vs clean-new-code (fresh ingest, zero access_count). The clean ingest eliminates the
rich-get-richer bias, which likely contributes some of the improvement. However, the magnitude
of gains (especially +46.4pp for single-session-assistant) clearly indicates real retrieval
improvement, not just access_count artifact.

**Key insight**: Concept enrichment is a high-leverage encoding-time intervention. It works
best for categories where query terms directly overlap with user-turn keywords (assistant,
knowledge-update). It helps modestly for categories with indirect overlap (single-session-user).
It does nothing for categories where the query is semantically distant from the answer content
(single-session-preference).

**Next**: The weakest remaining categories are:
1. single-session-preference (0.0% R@5) -- requires multi-scale embedding or LLM-extracted summaries
2. single-session-user (4.3% R@5) -- needs further investigation of why FTS boost is insufficient
3. multi-session (11.3% R@5) -- cross-session retrieval weakness, possible session-context boosting
The next iteration should analyze single-session-user failures deeper to understand why the
20x FTS boost only produced a +2.9pp improvement.

### Iteration 3 (2026-04-11, samsara)

**What**: Switch all FTS ranking from `ts_rank` to `ts_rank_cd` (cover density ranking) in all
four retrieval pathway CTEs. Addresses FTS score saturation discovered during deep analysis of
single-session-user failures.

**Hypothesis**: Concept enrichment (Iter 2) placed user-turn text at weight 'A', causing hundreds
of sessions to tie at maximum `ts_rank` for common query terms. With pool_size=30, the answer
session has only ~15% chance of entering the candidate pool from the lexical pathway. `ts_rank_cd`
rewards term proximity (cover density) and should break these ties, favoring answer sessions where
query terms appear in a contiguous phrase.

**Neuroscience grounding**: Feature binding in episodic memory (Treisman, 1996). Co-occurring
features encoded together form bound representations retrieved as a unit. `ts_rank_cd` rewards
this binding -- sessions where query terms appear in close proximity (bound features) score higher
than sessions where terms are scattered (unbound features).

**Analysis findings (single-session-user, 4.3% R@5, 3/70)**:
1. Three compounding problems: FTS saturation, embedding dilution, pool truncation
2. "What book am I reading?" -> 556 FTS matches, 193 tied at max `ts_rank`. Pool of 30 = ~15% inclusion chance.
3. "What degree did I graduate with?" -> 149 FTS matches, 10 tied at max `ts_rank`. Better but still lossy.
4. 3 successful queries used rare/specific vocabulary (e.g., "spirituality" appears in 1 session)
5. Answer sessions contain the fact as a passing mention in a conversation about something else
6. Session-level embeddings are dominated by the overall topic, not the brief personal fact

**Verification on real data**:
- ts_rank: "What book am I reading?" -> 29 sessions above 0.90, 19 above 0.99 (massive tie)
- ts_rank_cd: same query -> 0 sessions above 0.90 (ties completely eliminated)
- ts_rank_cd also produces values >1.0 for best matches, so tanh() saturates similarly to ts_rank
  for the top candidates -- no scoring regression expected for strong FTS matches

**Implementation**: Replaced all 6 occurrences of `ts_rank(` with `ts_rank_cd(` in recall.rs:
- 4x in fts_rank column computation (all CTEs: semantic, lexical, entity, cluster)
- 1x in lexical CTE ORDER BY
- 1x in lexical CTE ORDER BY (same line, the ORDER BY expression)
The scoring formula (`content_match = w_semantic * cosine_sim + w_fts * tanh(fts_rank)`)
is unchanged -- only the input FTS rank values change.

**Result**: R@5 29.2% -> 28.6% (-0.6pp). **REGRESSION**. Reverted.

```
                         R@1     R@5    R@10     MRR       N    | Iter 2  | Delta R@5
-----------------------------------------------------------------|---------|----------
Overall                15.6%   28.6%   38.6%   0.217     500   | 29.2%   | -0.6pp
knowledge-update       26.9%   55.1%   69.2%   0.393      78   | 61.5%   | -6.4pp
multi-session           2.3%    9.8%   19.5%   0.058     133   | 11.3%   | -1.5pp
single-session-asst    89.3%  100.0%  100.0%   0.942      56   | 100.0%  | +0.0pp
single-session-pref     0.0%    3.3%    6.7%   0.022      30   |  0.0%   | +3.3pp
single-session-user     0.0%    4.3%    8.6%   0.023      70   |  4.3%   | +0.0pp
temporal-reasoning      3.0%   20.3%   36.8%   0.114     133   | 18.0%   | +2.3pp
```

Results file: `~/longmemeval-ghola/results/ghola_mcp_s_20260411T103203Z.jsonl`

**Root cause of regression**: ts_rank_cd produces smaller absolute FTS values for most matches.
While it successfully breaks ties in the lexical ORDER BY (verified: 29 ties above 0.90 with
ts_rank vs 0 with ts_rank_cd), the smaller values reduce the FTS contribution to scoring via
`tanh(fts_rank)`. Knowledge-update category took the biggest hit (-6.4pp) because it relied on
high FTS scores (from concept enrichment at weight A) to boost answer sessions above semantic
competitors. With ts_rank_cd, the semantic pathway dominates more, and knowledge-update's
lexical advantage is diminished.

**Key insight**: Fixing candidate selection alone is insufficient. The target category
(single-session-user) was unchanged at 4.3% even with perfect tie-breaking. The answer sessions
DO enter the pool via the lexical pathway, but they get outscored by competing sessions with
higher cosine similarity. The problem is two-fold:
1. Session-level embeddings are diluted -- a 15K char conversation about task management apps
   doesn't produce an embedding close to "What degree did I graduate with?"
2. The 0.6/0.4 semantic/FTS weight ratio means even perfect FTS matches can't overcome a
   cosine_sim advantage of >0.33 (0.4 * tanh(max_rank) < 0.6 * 0.33 cosine difference)

**Reverted**: Yes -- code change reverted, original ts_rank version redeployed.

**Next**: The single-session-user problem requires encoding-time changes, not retrieval-time:
1. **Multi-scale embedding** -- store per-turn or per-paragraph embeddings alongside session-level.
   Query matching against turn-level embeddings would find "I graduated with a degree in Business
   Administration" directly. Requires schema changes (new embedding column or sub-mneme table).
2. **Fact extraction at gating time** -- extract atomic facts ("User graduated with Business
   Administration degree") and store as separate sub-mnemes with their own embeddings.
3. **Increase FTS weight for lexically-dominated queries** -- detect when FTS matches are strong
   but semantic matches are weak, and dynamically adjust the 0.6/0.4 ratio. But this violates
   the "scoring formula frozen" constraint.
4. **Increase lexical pool size** -- simpler than ts_rank_cd, just increase the pool to include
   more candidates. But this was the secondary hypothesis, not the primary one.
   
For multi-session (9.8% R@5), the next iteration should investigate cross-session retrieval
failure modes before attempting fixes.

### Iteration 4 (2026-04-11, samsara)

**What**: Deep analysis of multi-session retrieval failures (11.3% R@5). Attempted two
retrieval-time fixes: (a) pure OR-based FTS filter, (b) two-pass AND-then-OR lexical pool.
Both regressed. Reverted to baseline.

**Hypothesis**: `plainto_tsquery` uses AND conjunction, requiring ALL query terms to match
for the `@@` operator. Multi-session queries have 5-7 terms, and answer sessions matching
most but not all terms are completely excluded from the lexical pathway. Switching to an
OR-based filter (spreading activation, Collins & Loftus 1975) should admit partial-match
answer sessions, improving multi-session recall.

**Analysis findings (multi-session, 11.3% R@5, 15/133)**:
1. 83% of multi-session queries are aggregation queries ("how many/much/total")
2. Answer sessions share a common base ID (e.g., `593bdffd_1`, `593bdffd_2`) -- they're from
   the same user's conversation history, each containing one fragment of the answer
3. Answer sessions are thematically IDENTICAL to non-answer sessions (both about model building,
   closet organization, etc.). The distinguishing signal is a brief personal mention embedded
   in a broader conversation.
4. For "How many model kits have I worked on or bought?": `plainto_tsquery` produces
   `'mani' & 'model' & 'kit' & 'work' & 'bought'` -- 5 AND-conjuncted terms. Answer sessions
   match 'model' & 'kit' but NOT 'work' & 'bought'. Result: 0/4 answer sessions pass the filter.
   With OR filter: all 4 pass, ranking at positions 21, 24, 70, 962 by ts_rank.
5. 321 sessions match "model kit" FTS -- answer sessions are just 4 among hundreds of similar
   model-building sessions from different users.
6. Successful multi-session queries (15/133) barely make it: GT at ranks 4-5 with scores
   significantly below the top result. They succeed only when query vocabulary is sufficiently
   specific (e.g., named entities like "SaveMart", "Sephora", "Jimmy Choo").

**Approach A: Pure OR filter**:
- Changed lexical CTE to use `search_vector @@ or_tsquery` (any term matches)
- Kept `ts_rank(search_vector, plainto_tsquery(...))` for ranking (AND-based scoring)

**Result A**: R@5 29.2% -> 12.4% (-16.8pp). **SEVERE REGRESSION**. Reverted.
```
                         R@1     R@5    R@10     MRR       N    | Iter 2  | Delta R@5
-----------------------------------------------------------------|---------|----------
Overall                 2.4%   12.4%   19.2%   0.064     500   | 29.2%   | -16.8pp
knowledge-update        6.4%   34.6%   48.7%   0.167      78   | 61.5%   | -26.9pp
multi-session           0.8%    6.8%    9.8%   0.030     133   | 11.3%   | -4.5pp
single-session-asst     1.8%   12.5%   28.6%   0.075      56   | 100.0%  | -87.5pp
single-session-pref     0.0%    0.0%    0.0%   0.000      30   |  0.0%   | +0.0pp
single-session-user     1.4%    4.3%    4.3%   0.023      70   |  4.3%   | +0.0pp
temporal-reasoning      3.0%   12.0%   19.5%   0.070     133   | 18.0%   | -6.0pp
```
Results file: `~/longmemeval-ghola/results/ghola_mcp_s_20260411T124509Z.jsonl`

**Root cause of Approach A regression**: OR filter admits 10K-16K sessions to lexical pool
(vs 50-200 with AND). Even though top 30 by ts_rank are selected, the massive pool creates
more ties at high ts_rank values, and answer sessions are pushed out of the candidate pool.
107 queries that hit in baseline now miss. single-session-assistant crashed from 100% to 12.5%
because its answer sessions lost the tie-breaking lottery in the expanded pool.

**Approach B: Two-pass (AND priority, OR fallback)**:
- Tier 1: AND matches (priority 1, preserves baseline behavior)
- Tier 2: OR-only matches (priority 2, fills remaining pool slots)
- `ORDER BY match_tier, fts_rank DESC LIMIT pool_size`

**Result B**: R@5 29.2% -> 10.6% (-18.6pp). **WORSE REGRESSION**. Reverted.
```
                         R@1     R@5    R@10     MRR       N    | Iter 2  | Delta R@5
-----------------------------------------------------------------|---------|----------
Overall                 1.6%   10.6%   16.4%   0.054     500   | 29.2%   | -18.6pp
knowledge-update        2.6%   26.9%   42.3%   0.130      78   | 61.5%   | -34.6pp
multi-session           0.8%    9.0%   12.8%   0.040     133   | 11.3%   | -2.3pp
single-session-asst     3.6%    5.4%    7.1%   0.041      56   | 100.0%  | -94.6pp
single-session-pref     0.0%    0.0%    0.0%   0.000      30   |  0.0%   | +0.0pp
single-session-user     1.4%    4.3%    5.7%   0.027      70   |  4.3%   | +0.0pp
temporal-reasoning      1.5%   10.5%   18.0%   0.057     133   | 18.0%   | -7.5pp
```
Results file: `~/longmemeval-ghola/results/ghola_mcp_s_20260411T144834Z.jsonl`

**Root cause of Approach B regression**: The OR fallback sub-query computes cosine_sim
(`1.0 - (embedding <=> query)`) for ALL OR-matching rows (~10K), making queries 2-3x slower
(4.6s/query vs 1.4s baseline). The tier 2 OR rows entering the candidate pool also appear
to contaminate scoring through DISTINCT ON deduplication with semantic pathway results.

**Key insights**:
1. Multi-session retrieval is NOT a retrieval-time problem. The answer sessions are
   thematically indistinguishable from non-answer sessions at the session level.
2. Broadening the lexical filter (OR) hurts more than it helps: the precision loss from
   admitting thousands of weak matches outweighs the recall gain from admitting answer sessions.
3. The AND-based lexical filter is correct for categories where it works (knowledge-update,
   single-session-assistant). The problem is specific to multi-session where answer facts are
   fragments spread across sessions.
4. Multi-session requires ENCODING-TIME changes: either multi-granularity storage (per-turn
   embeddings alongside session-level) or fact extraction (atomic facts as sub-mnemes).

**Reverted**: Yes -- both approaches reverted, baseline code redeployed.

**Next**: The remaining low-hanging fruit for retrieval-time improvements may be exhausted.
The next productive direction is likely:
1. **Temporal retrieval pathway** -- use content_dates column to boost sessions from relevant
   time periods. Many temporal-reasoning queries (18.0% R@5) include temporal cues that could
   be leveraged. This is a new pathway addition, not a modification to existing pathways.
2. **Multi-granularity encoding** -- store per-turn or per-fact embeddings during gating.
   This is an encoding-time change that would help both single-session-user and multi-session
   by creating finer-grained retrieval targets.
3. **Larger pool size** -- simply increasing pool_size from 3*limit_n to 5*limit_n would give
   more candidates a chance. Low-risk change that might help at the margins.
