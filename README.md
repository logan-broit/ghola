# pg_ghola

**Cognitive memory primitives for PostgreSQL.**

A Postgres extension that implements neuroscience-inspired memory as composable SQL functions over [pgvector](https://github.com/pgvector/pgvector)-enabled tables. Memories decay with time, strengthen through use, form associations automatically through co-activation, and track confidence via Bayesian updating.

The primary interface is a single `recall()` function that fuses vector similarity, full-text search, temporal relevance, and associative context into one ranked result set.

```sql
SELECT * FROM pg_ghola.recall(
    workspace_id := 'your-workspace'::uuid,
    query_text   := 'kubernetes pod scheduling',
    query_embedding := embedding_from_your_model,
    limit_n      := 10
);
```

## Why

Current AI memory systems (vector databases, key-value stores, context windows) treat storage as static retrieval. They answer *"what data matches this query?"* but not *"what should I be thinking about right now?"*

pg_ghola brings the cognitive model to Postgres: memories decay, strengthen through use, form associations automatically, and track confidence — on a foundation with ACID transactions, streaming replication, RLS, and the full Postgres ecosystem.

The extension is the product. No sidecar, no MCP server. SQL in, results out.

## Cognitive Foundations

| Model | Function | What It Does | Source |
|-------|----------|-------------|--------|
| **ACT-R** | `actr_activation()` | Memory availability from frequency + recency of access | Anderson 1993 |
| **Ebbinghaus** | `ebbinghaus_decay()` | Forgetting curve with spacing-aware stability | Ebbinghaus 1885 |
| **Hebbian** | background worker | "Neurons that fire together wire together" — co-activated memories form associations | Hebb 1949 |
| **Bayesian** | `bayesian_update()` | Confidence tracking with evidence-based updating | Standard inference |

Every primitive is independently callable. Users can compose custom retrieval pipelines from the parts, or use the composite `recall()` function.

## Requirements

| Dependency | Version | Notes |
|------------|---------|-------|
| PostgreSQL | 18+ | Target runtime |
| Rust | 1.94+ | Stable toolchain |
| [pgrx](https://github.com/pgcentralfoundation/pgrx) | 0.17.x | `pg18` feature flag |
| [pgvector](https://github.com/pgvector/pgvector) | 0.8.2+ | Vector similarity |

## Quick Start

```bash
# Install pgrx
cargo install cargo-pgrx --version "=0.17.0"
cargo pgrx init --pg18 $(which pg_config)

# Build and install
cargo pgrx install --release
```

```sql
CREATE EXTENSION vector;
CREATE EXTENSION pg_ghola;

-- Optional: configure embedding dimensions (default 768)
SELECT pg_ghola.configure_dimensions(384);  -- e.g. for bge-small
```

## Architecture

```
Client (AI agent, app, SQL session)
│
▼
pg_ghola.recall(workspace, query, embedding)
│
├──────────────┬──────────────┐
▼              ▼              ▼
pgvector HNSW  FTS ts_rank    ACT-R activation
(similarity)   (BM25)         (per-row, live)
│              │              │
└──────┬───────┘              │
       ▼                      ▼
  content_match          temporal_weight
  (0.6·vec + 0.4·fts)   softplus(B + 4·heb)
       │                      │
       └──────────┬───────────┘
                  ▼
     content × temporal × confidence
                  │
                  ▼
          ORDER BY score DESC LIMIT n
                  │
                  ▼
       record_co_activation()   ← fire-and-forget enqueue
                  │
                  ▼
          Return recall_result


Background (async, in-process):

co_activation_queue
        │
        ▼
  Hebbian Worker (adaptive poll)
        │
  ┌─────┴─────┐
  ▼            ▼
UPDATE       UPDATE mnemes
associations (access_count,
(weights)     last_access)
  │
  ▼
Hourly: decay + prune stale associations
```

**The feedback loop:** recall results feed co-activation events → strengthen associations → influence future recall scores → produce new co-activation events. The system learns from usage.

## Schema

### Core Tables

**`pg_ghola.mnemes`** — Memory store (Greek *mneme*: memory)

| Column | Type | Description |
|--------|------|-------------|
| `id` | `uuid` | Primary key |
| `workspace_id` | `uuid` | Tenant isolation |
| `concept` | `text` | Short label |
| `content` | `text` | Full content |
| `embedding` | `vector(768)` | Semantic embedding (configurable) |
| `search_vector` | `tsvector` | Auto-generated FTS vector |
| `confidence` | `float8` | Bayesian confidence \[0.025, 0.975\] |
| `access_count` | `int` | Retrieval count |
| `last_access` | `timestamptz` | Last retrieval |
| `state` | `text` | `active`, `archived`, `dormant` |
| `memory_type` | `text` | `factual`, `experiential`, `working` |
| `tier` | `text` | `core`, `index`, `state` |
| `tags` | `text[]` | Free-form tags |
| `session_id` | `uuid` | Episodic session grouping |
| `expires_at` | `timestamptz` | TTL for working memories |

**`pg_ghola.associations`** — Typed links between mnemes

| Column | Type | Description |
|--------|------|-------------|
| `src_id` / `dst_id` | `uuid` | Linked mneme pair (`src_id < dst_id` enforced) |
| `association_type` | `text` | `hebbian`, `contradicts`, `supersedes`, `supports`, `session` |
| `weight` | `float8` | Strength \[0, 1\] |
| `co_activations` | `int` | Co-activation event count |

**`pg_ghola.co_activation_queue`** — Pending Hebbian processing events

Durable queue table (not LISTEN/NOTIFY). Each row holds `mneme_ids[]` and `scores[]` from a recall event.

## Scoring Primitives

### ACT-R Activation

```sql
SELECT pg_ghola.actr_activation(access_count := 13, last_access := now() - interval '10 days');
-- ~2.08
```

Base-level activation from Anderson's ACT-R (1993). Models how cognitively available a memory is based on frequency and recency of access.

```
B(M) = ln(n + 1) - d × ln(max(age_days, 1min) / (n + 1))
```

Where `n = access_count + 1`, `d = 0.5` (power-law decay exponent).

### Ebbinghaus Decay

```sql
SELECT pg_ghola.ebbinghaus_decay(
    now() - interval '30 days',   -- last_access
    50,                            -- access_count
    now() - interval '180 days'   -- created_at
);
-- ~0.78 (high stability from spaced access)
```

Spacing-aware retention. Stability increases with spaced repetition (Cepeda et al. 2006):

```
stability = clamp(ln(n+1) × 20 × (1 + 0.5 × tanh(avg_interval / 7d)), 14d, 365d)
retention = max(0.05, exp(-days_since_access / stability))
```

Massed access (50 accesses in 1 day) produces low stability (~0.12 after 30 days). Spaced access produces high stability (~0.78).

### Bayesian Confidence

```sql
SELECT pg_ghola.bayesian_update(0.5, 0.95);  -- neutral → confirmed: ~0.925
SELECT pg_ghola.bayesian_update(0.8, 0.10);  -- confident → contradicted: ~0.32
```

Laplace-smoothed Bayesian update. Result always in \[0.025, 0.975\] — never reaches certainty.

Evidence conventions:

| Value | Meaning |
|-------|---------|
| 0.95 | User confirmed |
| 0.65 | Co-activation reinforcement |
| 0.50 | Neutral |
| 0.10 | Contradiction detected |
| 0.05 | User rejected |

## Composite Recall

```sql
SELECT * FROM pg_ghola.recall(
    'workspace-uuid'::uuid,
    'how does pod scheduling work',
    query_embedding,
    10,                    -- limit
    0.0,                   -- min_confidence
    NULL,                  -- default weights
    'factual',             -- memory_type filter (optional)
    'personal',            -- scope filter (optional)
    ARRAY['k8s'],          -- tag filter (optional, AND semantics)
    'session-uuid'::uuid   -- session boost (optional, +0.3)
);
```

**Returns** `pg_ghola.recall_result`:

| Field | Description |
|-------|-------------|
| `mneme_id` | Memory UUID |
| `score` | Final composite score |
| `content_match` | Vector + FTS fusion |
| `activation` | ACT-R base-level |
| `hebbian_boost` | Association weight sum |
| `confidence` | Bayesian confidence |
| `concept` / `content` | Memory text |

**Scoring pipeline:**

1. **Candidate selection**: union of HNSW top-k and FTS top-k (pool = 3 × limit)
2. **Content match**: `0.6 × cosine + 0.4 × tanh(bm25)`
3. **Temporal weight**: `softplus(activation + 4.0 × hebbian_boost) / (1 + softplus(0))`
4. **Composite**: `content_match × temporal_weight × confidence`
5. **Side effect**: enqueue co-activation event for background processing

**Type-aware scoring:**

| Type | Effect |
|------|--------|
| `working` memory | 2× ACT-R decay (fades faster) |
| `core` tier | Confidence floor at 0.30 |
| `state` tier | Excluded from Hebbian learning |
| Expired | Excluded from all results |

### Association Types & Scoring Influence

| Type | Recall Boost | Created By |
|------|-------------|------------|
| `hebbian` | `+weight × 1.0` | Background worker (co-activation) |
| `supports` | `+weight × 0.5` | `mark_supports()` |
| `session` | `+weight × 0.3` | Auto-trigger on insert |
| `contradicts` | `-weight × 0.5` | `resolve_contradiction('confirmed')` |
| `supersedes` | No scoring; archives older mneme | `mark_supersedes()` |

## Background Worker

In-process Postgres background worker (pgrx). Polls the co-activation queue, computes Hebbian weight updates, and runs periodic maintenance.

```
# postgresql.conf
shared_preload_libraries = 'pg_ghola'
pg_ghola.database = 'memories'
```

**Adaptive polling:**

| State | Interval | Transition |
|-------|----------|------------|
| Active | 100ms | No rows for 30s → Idle |
| Idle | 1s | No rows for 5min → Dormant |
| Dormant | 5s | Rows found → Active |

**Hebbian processing (per batch of up to 100 events):**

1. Generate all (i, j) pairs from each event's mneme IDs
2. Aggregate pairs: `signal = Σ(score_i × score_j)`
3. Update weight in log-space: `new = min(1.0, exp(ln(current) + signal × ln(1.01)))`
4. Single transaction: update associations, delete consumed queue rows, bump access counts

**Periodic maintenance:**

| Job | Interval | Action |
|-----|----------|--------|
| Association decay | 1 hour | 0.1% weight reduction on stale associations (>1 day old) |
| Association pruning | 1 hour | Remove associations with weight < 0.001 |
| Dormant archival | 6 hours | Archive active mnemes >90 days inactive, confidence < 0.3 |
| State cleanup | 6 hours | Archive state-tier mnemes >24 hours inactive |
| Working memory expiry | 10 minutes | Archive past `expires_at` |

**Monitoring:**

```sql
SELECT * FROM pg_ghola.get_worker_stats();
```

Without `shared_preload_libraries`, everything works — process events manually with `pg_ghola.process_co_activation_batch(100)` or via pg_cron.

## Contradiction Detection

An `AFTER INSERT` trigger flags potential contradictions when a new mneme has high cosine similarity (≥ 0.85) to existing active mnemes in the same workspace.

```sql
-- Review pending contradictions
SELECT * FROM pg_ghola.get_pending_contradictions('workspace-id'::uuid);

-- Confirm: penalizes newer mneme, creates 'contradicts' association
SELECT pg_ghola.resolve_contradiction(candidate_id, 'confirmed');

-- Dismiss: no side effects
SELECT pg_ghola.resolve_contradiction(candidate_id, 'dismissed');
```

## Multi-Tenancy

All tables include `workspace_id`. The extension works with Postgres RLS out of the box:

```sql
ALTER TABLE pg_ghola.mnemes ENABLE ROW LEVEL SECURITY;
CREATE POLICY workspace_isolation ON pg_ghola.mnemes
    USING (workspace_id = current_setting('app.workspace_id')::uuid);
```

## Constants

| Constant | Value | Source |
|----------|-------|--------|
| ACT-R decay exponent (d) | 0.5 | Anderson 1993 |
| ACT-R Hebbian scale | 4.0 | Empirical |
| Hebbian learning rate | 0.01 (multiplicative) | Conservative |
| Association weight cap | 1.0 | By design |
| Association decay | 0.999/hour | Derived |
| Association prune threshold | 0.001 | Derived |
| Bayesian Laplace α / scale | 0.025 / 0.95 | Standard smoothing |
| Ebbinghaus stability range | 14–365 days | Ebbinghaus 1885 |
| Ebbinghaus retention floor | 0.05 | Prevents total forgetting |
| Spacing optimal interval | 7 days | Cepeda et al. 2006 |
| Content weights (semantic/FTS) | 0.6 / 0.4 | Default fusion |
| Worker batch size | 100 | Tunable |

## Development

```bash
cargo pgrx test pg18      # Run tests
cargo pgrx schema pg18    # Generate SQL
cargo pgrx run pg18       # Interactive psql with extension loaded
```

## Roadmap

- [ ] Predictive activation / transition tracking (sequential pattern learning)
- [ ] Association graph traversal (BFS/DFS with weight thresholds)
- [ ] Embedding generation integration (compose with pgai)

## References

- Anderson, J.R. (1993). *Rules of the Mind*. Hillsdale, NJ: Erlbaum. — The ACT-R base-level activation equation. [CMU ACT-R](http://act-r.psy.cmu.edu/?post_type=publications&p=13882)

- Cepeda, N.J. et al. (2006). Distributed practice in verbal recall tasks: A review and quantitative synthesis. *Psychological Bulletin, 132*, 354–380. — Meta-analysis of the spacing effect (317 experiments). [PubMed](https://pubmed.ncbi.nlm.nih.gov/16719566/)

- Ebbinghaus, H. (1885). *Über das Gedächtnis*. Leipzig: Duncker & Humblot. — The forgetting curve and spacing effect. [Internet Archive](https://archive.org/details/berdasgedchtnis01ebbigoog)

- Hebb, D.O. (1949). *The Organization of Behavior*. New York: Wiley. — Neurons that fire together wire together. [PubMed](https://pubmed.ncbi.nlm.nih.gov/10643472/)

## License

See [LICENSE](LICENSE) file.
