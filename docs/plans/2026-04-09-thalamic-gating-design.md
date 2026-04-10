# Thalamic Gating Design

## The Problem

pg_ghola searches all mnemes in a workspace via HNSW cosine similarity. With 19K+ memories, the correct result is a needle in a haystack. Systems like MemPalace achieve 96.6% R@5 by searching only ~40 candidates per question -- effectively perfect pre-filtering. Our gating (workspace + type + state) doesn't narrow by content relevance at all.

**Neuroscience basis:** The thalamus actively gates information flow to the cortex, controlled by top-down signals from the prefrontal cortex (Sherman & Guillery, 2002). The brain doesn't search all of memory for every query -- it narrows by multiple dimensions simultaneously before the hippocampus retrieves (Desimone & Duncan, 1995).

## Architecture: Two-Tier Gating

### Tier 1: Fast Gate (available immediately at INSERT)

Uses signals that already exist on every mneme with zero extraction cost:

| Signal | Column | Index | How it gates |
|--------|--------|-------|--------------|
| Full-text search | `search_vector` (tsvector) | GIN (exists) | BM25-style keyword pre-filter narrows to mnemes containing query terms |
| Tags | `tags` (text[]) | GIN (exists) | User-provided topic/project labels |
| Session | `session_id` (uuid) | B-tree (exists) | Groups related mnemes from same conversation |
| Memory type | `memory_type` (text) | B-tree (exists) | factual/experiential/working |
| Tier | `tier` (text) | B-tree (exists) | core/index/state |
| Temporal | `created_at` (timestamptz) | B-tree (exists) | Recency filtering |

**The key insight:** The FTS tsvector index already exists on every mneme. A query about "Sarah's dentist appointment" can match mnemes containing "Sarah" and "dentist" via tsvector before HNSW runs. This provides meaningful content-based narrowing from moment zero, including during bulk load.

### Tier 2: Deep Gate (available after async extraction)

Content-derived attributes extracted by a background worker:

| Attribute | Column (new) | Type | Index | Extraction |
|-----------|-------------|------|-------|------------|
| Entities | `entities` | `text[]` | GIN | NER heuristic (regex + patterns, LLM upgrade path) |
| Content dates | `content_dates` | `timestamptz[]` | GIN | Date/time regex extraction |
| Topic cluster | `cluster_id` | `integer` | B-tree | Nearest centroid from k-means on embeddings |
| Intent | `intent` | `text` | B-tree | Keyword classifier (decision/preference/fact/question/plan/experience) |

All new columns are nullable. NULL = not yet processed.

## Query-Time Pipeline

```
Query arrives
    |
    v
[Extract gating signals from query text]
  - Keywords for FTS
  - Entity mentions
  - Temporal references
  - Intent classification
    |
    v
[Tier 1: Fast Gate]
  FTS pre-filter: ts_rank(search_vector, query) > threshold
  -> Returns candidate set (target: 200-500 mnemes)
    |
    v
[Tier 2: Deep Gate] (if attributes populated)
  Further narrow by entities/dates/cluster/intent
  -> Reduces candidate set further
    |
    v
[HNSW rerank on gated set]
  Cosine similarity on remaining candidates
    |
    v
[Fallback check]
  If top result confidence < threshold:
    Widen to ungated HNSW (full workspace scan)
    Merge and re-rank results
```

## The Improvement Curve

The system starts useful and gets better over time:

- **t=0 (bulk load complete):** FTS fast gate narrows candidates by keyword match. HNSW runs on gated set. Better than ungated, available immediately.
- **t=hours (gating worker catches up):** Entity, temporal, cluster, and intent attributes populated. Deep gate further narrows candidates. Significant retrieval improvement.
- **t=days (Hebbian associations form):** Co-activation from real usage creates association boosts. Recall quality improves for frequently-accessed topics.
- **t=weeks (sleep consolidation runs):** Weak associations pruned, stale memories archived, confidence scores differentiated. Signal-to-noise ratio improves.

This is measurable: benchmark at each stage to show the improvement curve. The longitudinal evaluation that cold benchmarks miss.

## Schema Changes

New columns on `mnemes` table:

```sql
ALTER TABLE ghola.mnemes
    ADD COLUMN entities text[] DEFAULT NULL,
    ADD COLUMN content_dates timestamptz[] DEFAULT NULL,
    ADD COLUMN cluster_id integer DEFAULT NULL,
    ADD COLUMN intent text DEFAULT NULL
        CHECK (intent IN ('decision', 'preference', 'fact', 'question', 'plan', 'experience'));

CREATE INDEX mnemes_entities_gin_idx ON ghola.mnemes USING gin (entities) WHERE entities IS NOT NULL;
CREATE INDEX mnemes_content_dates_gin_idx ON ghola.mnemes USING gin (content_dates) WHERE content_dates IS NOT NULL;
CREATE INDEX mnemes_cluster_id_idx ON ghola.mnemes (cluster_id) WHERE cluster_id IS NOT NULL;
CREATE INDEX mnemes_intent_idx ON ghola.mnemes (intent) WHERE intent IS NOT NULL;
```

New queue table:

```sql
CREATE TABLE ghola.gating_queue (
    id bigserial PRIMARY KEY,
    workspace_id uuid NOT NULL,
    mneme_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);
```

## Gating Worker

Third background worker, same pattern as Hebbian and Contradiction workers:

- Drains `gating_queue`
- For each mneme: extract entities, dates, cluster_id, intent
- Updates mneme columns
- Adaptive polling: 5s active, 60s idle, 300s dormant

Extraction is heuristic-first (no LLM dependency):
- **Entities:** Capitalized word sequences, quoted terms, email addresses, @mentions
- **Content dates:** Regex for date patterns (ISO, natural language like "last Tuesday", relative dates)
- **Cluster ID:** Cosine distance to pre-computed cluster centroids (k-means on existing embeddings, recomputed during sleep consolidation)
- **Intent:** Keyword classifier (contains "decided"/"chose" -> decision, contains "prefer"/"like" -> preference, etc.)

## Recall Pipeline Changes

Modify `ghola.recall()` (or create `ghola.recall_v2()`) to:

1. Run FTS `ts_rank_cd()` on the query text against `search_vector`
2. Take top N (configurable, default 500) by FTS rank as the candidate set
3. If deep gate columns are populated on candidates, apply entity/date/cluster/intent filters
4. Run HNSW cosine similarity on the gated candidate set
5. If result confidence is below threshold, fall back to ungated HNSW and merge
6. Score using existing formula: content_match x temporal x hebbian x confidence

## What This Doesn't Include

- No LLM extraction (heuristics first, upgrade path later)
- No learned/adaptive gating (MemFactory territory)
- No new scoring signals -- gating filters, doesn't re-rank
- No changes to workspace semantics (workspace = ownership/access, not organization)

## Testing Strategy

1. **Unit test:** FTS pre-filter returns correct candidates for keyword queries
2. **Integration test:** Full pipeline with gating produces same-or-better results than ungated
3. **Benchmark:** Run LongMemEval-S with gating enabled, compare to ungated baseline
4. **Improvement curve:** Measure R@5 at t=0 (FTS only), t=extracted (deep gate), show improvement
