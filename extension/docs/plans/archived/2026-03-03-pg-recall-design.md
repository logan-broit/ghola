# pg_ghola: Cognitive Memory Primitives for Postgres

FUNCTION pg_ghola_extension(postgres, pgvector) -> cognitive_memory_system
  A Postgres extension that implements neuroscience-inspired memory primitives --
  ACT-R activation, Hebbian association learning, Bayesian confidence, and
  Ebbinghaus decay -- as composable SQL functions over pgvector-enabled tables.

  Memories decay with time, strengthen through use, form associations
  automatically through co-activation, and track confidence via Bayesian
  updating. The primary interface is a single `recall()` function that fuses
  vector similarity, full-text search, temporal relevance, and associative
  context into one ranked result set.

RULES
  - The extension is the product. No MCP server, no sidecar. SQL in, results out.
  - Depend on pgvector for vector search. Do not reinvent similarity indexing.
  - Pure extension (Approach A): schema, functions, types, and background worker
    all ship as one compiled unit installed via CREATE EXTENSION.
  - Rust (1.94+) via pgrx (0.17.x). Type safety for the scoring hot path,
    bgworker support for Hebbian processing.
  - Targets PostgreSQL 18.3+ with pgvector 0.8.2+.
  - Multi-tenant via workspace_id column. Works with Postgres RLS out of the box.
  - Every scoring primitive is independently callable. Users can compose custom
    retrieval pipelines from the parts.
  - The composite recall() function is the recommended entry point for most users.
  - v0.1 scope: scoring + Hebbian associations + composite retrieval.
    Contradiction detection, predictive transitions, and typed associations
    are deferred to v0.2+.

DONE_WHEN
  - CREATE EXTENSION pg_ghola creates the schema, functions, types, and starts
    the background worker.
  - pg_ghola.recall() returns ranked results fusing vector + FTS + ACT-R +
    Hebbian + confidence in a single query.
  - Associations form automatically from co-activation events without user
    intervention.
  - The background worker processes the co-activation queue, updates association
    weights, and tracks access patterns.
  - All cognitive primitives (actr_activation, bayesian_update, ebbinghaus_decay)
    are callable independently.
  - The extension works under streaming replication, connection pooling, and RLS.

---

## Foundations

The cognitive primitives in this extension are drawn from established research
in cognitive psychology and neuroscience:

  - ACT-R (Adaptive Control of Thought -- Rational): John Anderson's unified
    theory of cognition (1993, 1998). The base-level activation equation models
    how memory availability depends on frequency and recency of access. One of
    the most validated cognitive architectures in psychology, used in hundreds
    of published studies.

  - Hebb's Rule: Donald Hebb's associative learning principle (1949, "The
    Organization of Behavior"). Neurons that fire together wire together.
    Co-activated memories form associations that strengthen with repetition.
    The foundation of modern connectionism and associative memory research.

  - Ebbinghaus Forgetting Curve: Hermann Ebbinghaus's discovery (1885) that
    memory retention decays exponentially over time, with the rate modulated by
    the spacing effect -- spaced repetition produces more stable memories than
    massed repetition.

  - Bayesian Inference: Standard probabilistic reasoning for updating beliefs
    given new evidence. Applied here as a confidence tracking mechanism where
    each memory carries an epistemic weight that updates as reinforcing or
    contradicting signals arrive.

Current AI memory systems (vector databases, key-value stores, context windows)
treat storage as static retrieval. They answer "what data matches this query?"
but not "what should I be thinking about right now?" pg_ghola brings the
cognitive model to Postgres: memories decay, strengthen through use, form
associations automatically, and track confidence -- on a foundation with ACID
transactions, streaming replication, RLS, and the full Postgres ecosystem.

---

## Schema

FUNCTION create_schema() -> tables, indexes

RULES
  - Core memory table is named `mnemes` (Greek mneme: memory).
  - Canonical pair ordering on associations enforced by CHECK constraint.
  - Co-activation queue is a durable table, not LISTEN/NOTIFY.
  - FTS vector is a generated column, always in sync with content.

```sql
CREATE TABLE pg_ghola.mnemes (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  uuid NOT NULL,
    concept       text NOT NULL,
    content       text NOT NULL,
    embedding     vector(384) NOT NULL,
    search_vector tsvector GENERATED ALWAYS AS (
        setweight(to_tsvector('english', concept), 'A') ||
        setweight(to_tsvector('english', content), 'B')
    ) STORED,
    confidence    float NOT NULL DEFAULT 0.5,
    access_count  int NOT NULL DEFAULT 0,
    last_access   timestamptz NOT NULL DEFAULT now(),
    created_at    timestamptz NOT NULL DEFAULT now(),
    state         text NOT NULL DEFAULT 'active'
        CHECK (state IN ('active', 'archived', 'dormant'))
);

CREATE TABLE pg_ghola.associations (
    src_id         uuid NOT NULL REFERENCES pg_ghola.mnemes(id),
    dst_id         uuid NOT NULL REFERENCES pg_ghola.mnemes(id),
    weight         float NOT NULL DEFAULT 0.01,
    co_activations int NOT NULL DEFAULT 0,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (src_id, dst_id),
    CHECK (src_id < dst_id)
);

CREATE TABLE pg_ghola.co_activation_queue (
    id           bigserial PRIMARY KEY,
    workspace_id uuid NOT NULL,
    mneme_ids    uuid[] NOT NULL,
    scores       float[] NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX ON pg_ghola.mnemes USING hnsw (embedding vector_cosine_ops);
CREATE INDEX ON pg_ghola.mnemes USING gin (search_vector);
CREATE INDEX ON pg_ghola.mnemes (workspace_id, last_access DESC);
CREATE INDEX ON pg_ghola.associations (dst_id, src_id);
```

DONE_WHEN
  - Tables exist in pg_ghola schema after CREATE EXTENSION.
  - CHECK constraint on associations prevents src_id >= dst_id.
  - HNSW and GIN indexes are created.
  - search_vector auto-updates when concept or content changes.

---

## Scoring Primitives

FUNCTION actr_activation(access_count int, last_access timestamptz) -> float
  Computes ACT-R base-level activation (Anderson 1993). Returns how
  cognitively available a memory is right now, based on how often and how
  recently it has been accessed.

  Formula:
    B(M) = ln(n + 1) - d * ln(max(age_days, age_floor) / (n + 1))

  Where:
    n         = access_count + 1 (avoid ln(0))
    d         = 0.5 (power-law decay exponent)
    age_days  = days since last_access
    age_floor = 1/(24*60) days (1 minute minimum)

RULES
  - Returns raw activation, not clamped. Caller applies softplus if needed.
  - age_floor prevents division by zero for just-accessed mnemes.
  - Computed at query time from current wall clock. No stored state mutation.

EXAMPLES
  actr_activation(13, now() - interval '10 days')  -> ~2.08
  actr_activation(0,  now() - interval '1400 days') -> ~-3.27
  -- softplus(2.08) / softplus(-3.27) ≈ 37x advantage

---

FUNCTION ebbinghaus_decay(last_access timestamptz, access_count int, created_at timestamptz) -> float
  Computes Ebbinghaus retention factor with spacing-aware stability.

  Stability formula:
    base      = ln(n + 1) * 20.0
    spacing   = tanh(avg_days_between_accesses / 7.0)
    stability = clamp(base * (1 + 0.5 * spacing), 14.0, 365.0)

  Decay formula:
    R = max(0.05, exp(-days_since_access / stability))

RULES
  - Floor of 0.05 prevents complete forgetting.
  - Stability clamped to [14, 365] days.
  - Spaced access patterns produce higher stability than massed access.
  - avg_days_between_accesses = lifespan_days / max(access_count, 1).

EXAMPLES
  -- 50 accesses over 6 months, last accessed 30 days ago
  ebbinghaus_decay(now()-'30d', 50, now()-'180d') -> ~0.78 (high stability)
  -- 50 accesses in 1 day, last accessed 30 days ago
  ebbinghaus_decay(now()-'30d', 50, now()-'1d')   -> ~0.12 (low stability, massed)

---

FUNCTION bayesian_update(prior float, evidence float) -> float
  Updates confidence via Bayes' rule with Laplace smoothing.

  Formula:
    posterior = (prior * evidence) / max(prior * evidence + (1-prior)*(1-evidence), 1e-9)
    result    = 0.95 * posterior + 0.025

  Evidence constants (conventions, not enforced):
    0.05  user rejected
    0.10  contradiction detected
    0.50  neutral (no information)
    0.65  co-activation reinforcement
    0.95  user confirmed

RULES
  - Result is always in [0.025, 0.975]. Never reaches 0 or 1.
  - Laplace smoothing (alpha=0.025, scale=0.95) prevents certainty lock.
  - Multiple evidence signals chain: call repeatedly for cumulative update.

EXAMPLES
  bayesian_update(0.5, 0.95)  -> ~0.925  (neutral -> confirmed)
  bayesian_update(0.8, 0.10)  -> ~0.32   (confident -> contradicted)
  bayesian_update(0.32, 0.10) -> ~0.078  (second contradiction, confidence collapses)

---

FUNCTION softplus(x float) -> float
  Numerically stable softplus: ln(1 + exp(x)).

RULES
  - For x > 20, returns x directly (avoids exp overflow).
  - Maps (-inf, inf) -> (0, inf).

EXAMPLES
  softplus(0.0)  -> 0.6931  (ln(2))
  softplus(2.0)  -> 2.1269
  softplus(-5.0) -> 0.0067
  softplus(25.0) -> 25.0    (overflow guard)

---

## Composite Retrieval

FUNCTION recall(
    workspace_id uuid,
    query_text text,
    query_embedding vector(384),
    limit_n int DEFAULT 10,
    min_confidence float DEFAULT 0.0,
    weights pg_ghola.score_weights DEFAULT NULL
) -> SETOF pg_ghola.recall_result
  Full cognitive recall pipeline. Fuses vector similarity, full-text search,
  ACT-R temporal activation, Hebbian association strength, and Bayesian
  confidence into a single ranked result set.

  Scoring pipeline:
    1. Candidate selection: HNSW nearest neighbors + FTS ts_rank
    2. Content match:       0.6 * vector_cosine + 0.4 * tanh(bm25)
    3. Temporal weight:     softplus(actr_activation + 4.0 * hebbian_boost)
    4. Normalization:       temporal / (1 + softplus(0))
    5. Composite:           content_match * temporal_weight * confidence
    6. Side effect:         Enqueue co-activation event for top-n results

  Return type (pg_ghola.recall_result):
    mneme_id      uuid
    score         float     -- final composite
    content_match float     -- vector + FTS fusion
    activation    float     -- ACT-R base-level
    hebbian_boost float     -- association weight sum
    confidence    float     -- current Bayesian confidence
    concept       text
    content       text

RULES
  - Candidate pool is the union of HNSW top-k and FTS top-k, deduplicated.
    Pool size is 3 * limit_n to give scoring room to rerank.
  - Hebbian boost for a candidate is the sum of association weights to any
    other candidate in the current result set. This means connected clusters
    of mnemes reinforce each other within a single recall.
  - After scoring, the top-n mneme IDs and their scores are enqueued as a
    co-activation event. This is fire-and-forget (INSERT into queue table).
  - The recall itself does NOT update access_count or last_access. The
    background worker handles that when it processes the co-activation event.
  - If weights parameter is NULL, uses defaults:
    semantic=0.6, fts=0.4, actr_decay=0.5, hebbian_scale=4.0.
  - Only returns mnemes with state='active' and confidence >= min_confidence.

EXAMPLES
  -- Basic recall
  SELECT * FROM pg_ghola.recall(
      '550e8400-e29b-41d4-a716-446655440000',
      'kubernetes pod scheduling',
      (SELECT embedding FROM my_embeddings WHERE id = 1),
      10
  );

  -- High-confidence only
  SELECT * FROM pg_ghola.recall(
      '550e8400-e29b-41d4-a716-446655440000',
      'database migration strategy',
      my_embedding,
      5,
      0.7  -- only confident memories
  );

---

## Hebbian Operations

FUNCTION record_co_activation(workspace_id uuid, mneme_ids uuid[], scores float[]) -> void
  Enqueue a co-activation event for background processing. Use this when
  your application has its own notion of "these mnemes were relevant together"
  outside of the recall() function (which enqueues automatically).

RULES
  - mneme_ids and scores arrays must have equal length.
  - Enqueue is a single INSERT, never blocks on processing.
  - Duplicate events are fine; the worker aggregates correctly.

EXAMPLES
  SELECT pg_ghola.record_co_activation(
      '550e8400-e29b-41d4-a716-446655440000',
      ARRAY['id1', 'id2', 'id3']::uuid[],
      ARRAY[0.9, 0.7, 0.5]
  );

---

FUNCTION get_associations(mneme_id uuid, min_weight float DEFAULT 0.01)
  -> SETOF (related_id uuid, weight float)
  Return all associations for a mneme above the weight threshold.

RULES
  - Handles both directions (src_id and dst_id) transparently.
  - Results ordered by weight descending.

---

## Confidence Operations

FUNCTION update_confidence(mneme_id uuid, evidence float) -> float
  Apply Bayesian evidence to a mneme's confidence. Returns new confidence.

RULES
  - Reads current confidence, applies bayesian_update, writes back.
  - Single atomic UPDATE.

---

FUNCTION confirm_recall(mneme_ids uuid[]) -> void
  Convenience: apply evidence=0.95 to all provided mnemes.
  Use after a user indicates recall results were helpful.

---

## Background Worker

FUNCTION hebbian_worker() -> background_process
  In-process Postgres background worker registered via pgrx. Polls the
  co_activation_queue table, computes Hebbian weight updates, and maintains
  access tracking.

RULES
  - Single worker per Postgres instance, shared across all workspaces.
  - Starts automatically on extension load.
  - Batch size: up to 100 queue rows per cycle.
  - Adaptive polling:
      Active   (processed rows last cycle)  -> 100ms
      Idle     (no rows for > 30s)          -> 1s
      Dormant  (no rows for > 5min)         -> 5s
    Any row found resets to Active.
  - Processing per batch:
      1. Generate all (i, j) pairs from each event's mneme_ids.
      2. Aggregate pairs across batch: signal = sum(score_i * score_j).
      3. For each pair, compute new weight in log-space:
           new = min(1.0, exp(ln(current) + signal * ln(1.01)))
         Cold start: if current <= 0, seed at 0.01.
      4. Single transaction: UPDATE associations, DELETE consumed queue rows,
         UPDATE mnemes SET access_count = access_count + 1, last_access = now().
  - Hourly decay pass:
      UPDATE associations SET weight = weight * 0.999
        WHERE updated_at < now() - interval '1 day';
      DELETE FROM associations WHERE weight < 0.001;
  - On shutdown: drain remaining queue rows, then exit.

ERRORS
  - If batch transaction fails, queue rows remain for retry on next cycle.
  - Worker crash: Postgres restarts it automatically (pgrx bgworker restart policy).
  - Queue grows unboundedly if worker falls behind: monitor via worker_stats().

---

FUNCTION worker_stats() -> record
  Returns background worker state for monitoring.

  Fields:
    state              text        -- 'active', 'idle', 'dormant'
    queue_depth        bigint      -- current co_activation_queue row count
    batches_processed  bigint      -- lifetime batch count
    pairs_updated      bigint      -- lifetime association updates
    last_batch_at      timestamptz
    last_decay_at      timestamptz

---

## Architecture

```
                       Client (AI agent, app, SQL session)
                                     |
                                     v
                   pg_ghola.recall(workspace, query, embedding)
                                     |
                   +-----------------+-----------------+
                   |                 |                 |
                   v                 v                 v
             pgvector HNSW      FTS ts_rank       ACT-R score
             (similarity)    (query matching)   (per-row, live)
                   |                 |                 |
                   +--------+--------+                 |
                            |                          |
                            v                          v
                     content_match              temporal_weight
                   (0.6*vec + 0.4*fts)      softplus(B + 4*heb)
                            |                          |
                            +------------+-------------+
                                         |
                                         v
                             content * temporal * confidence
                                         |
                                         v
                                  ORDER BY score DESC
                                  LIMIT n
                                         |
                                         v
                             record_co_activation()
                             (fire-and-forget enqueue)
                                         |
                                         v
                             Return recall_result set


       Background (async, in-process):

             co_activation_queue
                     |
                     v
             Hebbian Worker (poll loop)
                     |
             +-------+-------+
             |               |
             v               v
       UPDATE           UPDATE mnemes
       associations     (access_count,
       (weights)         last_access)
             |
             v
       Hourly: decay pass
       (prune dead associations)
```

The feedback loop: recall results feed co-activation events, which strengthen
associations, which influence future recall scores, which produce new
co-activation events. The system learns from usage.

What Postgres provides (not reimplemented):
  - Replication: streaming replication. Read replicas serve recall().
  - Multi-tenant: RLS on workspace_id.
  - Backup: pg_dump, WAL archiving, pgBackRest. Cognitive state is just rows.
  - Pooling: PgBouncer works. Background worker is in-process.
  - Monitoring: pg_stat_user_tables + worker_stats().
  - ACID: co-activation processing is transactional.
  - Indexing: HNSW (pgvector), GIN (FTS), B-tree (access patterns).

---

## Extension Packaging

```
pg_ghola/
  pg_ghola.control
  sql/
    pg_ghola--0.1.0.sql
  src/
    lib.rs          -- pgrx entry, extension registration, bgworker setup
    scoring.rs      -- actr_activation, ebbinghaus_decay, bayesian_update, softplus
    recall.rs       -- cognitive_recall composite function
    hebbian.rs      -- background worker
    types.rs        -- recall_result, score_weights custom types
  Cargo.toml        -- pgrx 0.17.x, pg18 feature flag
  README.md

Toolchain: Rust 1.94+ stable, pgrx 0.17.x, PostgreSQL 18.3+, pgvector 0.8.2+
```

---

## Deferred to v0.2+

- Contradiction detection (relationship type incompatibility matrix, confidence downgrade)
- Predictive activation / transition tracking (sequential pattern learning)
- Typed associations (supports, contradicts, depends_on, etc.)
- Embedding generation (users bring their own; compose with pgai if desired)
- Association graph traversal function (BFS/DFS with weight thresholds)
- Configurable vector dimensions (v0.1 hardcodes 384 for bge-small)
- MCP server layer (separate project, not part of the extension)

---

## Version Requirements

| Dependency | Minimum | Notes |
|------------|---------|-------|
| Rust | 1.94+ | Stable toolchain |
| pgrx | 0.17.x | With `pg18` feature flag |
| PostgreSQL | 18.3+ | GA since Sept 2025; 18.3 is latest patch |
| pgvector | 0.8.2+ | Fixes CVE-2026-3172 (parallel HNSW buffer overflow) |
| CNPG image | `ghcr.io/cloudnative-pg/postgresql:18.3-system-trixie` | Base for custom image |

---

## Constants Reference

| Constant | Value | Source |
|----------|-------|--------|
| ACT-R decay exponent (d) | 0.5 | Anderson 1993 (ACT-R default) |
| ACT-R Hebbian scale | 4.0 | Empirical tuning |
| Hebbian learning rate | 0.01 | Conservative multiplicative rate |
| Cold-start association weight | 0.01 | Bootstrap seed for new edges |
| Association weight cap | 1.0 | Bounded by design |
| Association decay factor | 0.999/hour | Derived |
| Association prune threshold | 0.001 | Derived |
| Bayesian Laplace alpha | 0.025 | Standard Laplace smoothing |
| Bayesian Laplace scale | 0.95 | Prevents certainty lock |
| Ebbinghaus default stability | 14 days | Ebbinghaus 1885 (typical retention) |
| Ebbinghaus max stability | 365 days | Upper bound on consolidation |
| Ebbinghaus floor | 0.05 | Prevents total forgetting |
| Stability growth rate | 20.0 | Calibrated to spacing literature |
| Spacing optimal interval | 7 days | Cepeda et al. 2006 (spacing effect) |
| Spacing bonus factor | 0.5 | Empirical tuning |
| Content weight: semantic | 0.6 | Default fusion ratio |
| Content weight: FTS | 0.4 | Default fusion ratio |
| Softplus overflow guard | x > 20 | Numerical stability |
| Age floor | 1 minute | Prevents division by zero |
| Worker batch size | 100 | Tunable |
| Worker poll: active | 100ms | Responsive under load |
| Worker poll: idle (>30s) | 1s | Reduced CPU when quiet |
| Worker poll: dormant (>5min) | 5s | Minimal overhead when inactive |

---

## References

- Anderson, J.R. (1993). *Rules of the Mind*. Hillsdale, NJ: Erlbaum.
  The book introducing ACT-R, including the base-level activation equation.
  [CMU ACT-R Project](http://act-r.psy.cmu.edu/?post_type=publications&p=13882)
  | [Routledge](https://www.routledge.com/Rules-of-the-Mind/Anderson/p/book/9780805812008)

- Cepeda, N.J., Pashler, H., Vul, E., Wixted, J.T., & Rohrer, D. (2006).
  Distributed practice in verbal recall tasks: A review and quantitative
  synthesis. *Psychological Bulletin, 132*, 354-380.
  Meta-analysis of the spacing effect across 317 experiments.
  [PubMed](https://pubmed.ncbi.nlm.nih.gov/16719566/)
  | [PDF](https://augmentingcognition.com/assets/Cepeda2006.pdf)

- Ebbinghaus, H. (1885). *Uber das Gedachtnis: Untersuchungen zur
  experimentellen Psychologie*. Leipzig: Duncker & Humblot.
  English translation: *Memory: A Contribution to Experimental Psychology* (1913).
  The original experimental study of the forgetting curve and spacing effect.
  [Internet Archive (original German)](https://archive.org/details/berdasgedchtnis01ebbigoog)

- Hebb, D.O. (1949). *The Organization of Behavior: A Neuropsychological
  Theory*. New York: Wiley.
  The source of Hebb's Rule: neurons that fire together wire together.
  [PubMed](https://pubmed.ncbi.nlm.nih.gov/10643472/)
  | [Taylor & Francis](https://www.taylorfrancis.com/books/mono/10.4324/9781410612403/organization-behavior-hebb)
