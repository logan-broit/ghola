# Multi-Granularity Encoding Implementation

Simplex v0.5 specification for the code changes that implement the
[multi-granularity encoding design](./2026-04-16-multi-granularity-encoding-design.md).

REVISED 2026-04-17 for the Go Chapterhouse reality.

REVISED 2026-04-18 to drop late chunking in favor of isolated per-turn
encoding, after Tier 1 samsara evidence (commits a39fac5, cbf71eb, 4d74450
in chapterhouse) showed:
  - The "correct" last-token late chunking (matching Qwen3's native pooling)
    LOSES to isolated encoding by -2.0pp top-1 overall.
  - The best ensemble variants (mean-pool late chunking) win +9.9pp on a
    conversational-memory biased eval set, but lose -20pp on self-contained
    turns (factual content) and are fragile to workload shifts.
  - The oracle ceiling for any ingest-time encoding selection is +11.3pp
    over isolated, across a 60pp+ total gap to state-of-the-art systems.
  - Store-both-max / store-both-mean lose to pure mean-pool due to
    aggregation dynamics (inflated non-target scores, diluted contextual
    discrimination).

Net: encoding complexity is not the lever that meaningfully closes the
competitive gap. pg_ghola's differentiation is the cognitive primitives
(Hebbian, contradiction detection, consolidation, temporal decay,
confidence evolution, gating, multi-pathway consensus), none of which
are exercised on cold benchmarks. This iteration of Phase 2 commits to
the simplest encoding that works on ALL workloads, so engineering
bandwidth can move to the primitives where the differentiation lives.

## Architecture decision

**Isolated per-turn encoding via the existing endpoint.** Chapterhouse
sends each turn's text to the existing `/v1/embeddings` endpoint on
`embed_server.py` (which already handles batched input: give it a list
of strings, get back a list of embeddings). No new endpoint required.
No late chunking. No sliding window. No new pooling logic.

Why this change from the earlier late-chunking plan:
  - Simpler: zero changes to the embedding server. No new code path to
    maintain or debug.
  - Works on all workloads: conversational AND factual. No regression
    on self-contained turns.
  - Faster to ship: Chapterhouse Go changes are minimal.
  - The encoding still matches Phase 1's schema (1024d vectors per
    sub_mneme) without any reinterpretation.
  - Leaves the door open for encoding upgrades later (the sub_mneme
    schema is agnostic to HOW embeddings are produced).

## Three touchpoints (simplified from four)

1. **pg_ghola extension (Rust)** -- sub_mnemes table, recall changes.
   **DONE as of commit 80e120f**.
2. **Embedding server (Python)** -- **NO CHANGES**. The existing
   `/v1/embeddings` endpoint already accepts batched input and returns
   normalized embeddings. Chapterhouse sends it a list of turn texts.
3. **Chapterhouse (Go)** -- `RememberWithTurns` method that calls
   `provider.Embed(turns_as_string_list)` once per ingest, writes the
   parent mneme plus N sub_mnemes atomically via pgx.
4. **Benchmark harness (Python)** -- TRUNCATE+re-ingest via the new path.

## Preconditions

- pg_ghola schema migrations are additive (no existing data destroyed).
- The existing embedding server on port :8082 runs Qwen3-Embedding-0.6B
  via sentence-transformers 5.4.0 with normalize_embeddings=True.
- The existing OpenAI-compatible endpoint handles both single-string
  and list-of-strings input (verified in
  pg_ghola/analysis/embed_server.py).

---

## Phase 1: pg_ghola schema + recall

### FUNCTION: create_sub_mnemes_table -> migration

  FILE: src/schema.rs (new extension_sql! block)

  RULES:
    - add a new extension_sql! block after create_mnemes_table
    - table name: sub_mnemes
    - columns: id uuid pk, mneme_id uuid FK, position smallint, role text,
               content text, embedding vector(1024), search_vector tsvector
               GENERATED from content, token_start int, token_end int
    - FK with ON DELETE CASCADE so deleting a parent cascades to sub_mnemes
    - UNIQUE(mneme_id, position)
    - role CHECK constraint: ('user', 'assistant', 'system', 'tool')
    - search_vector generated from content only (NO concept enrichment):
        to_tsvector('english', content)
    - indexes:
        HNSW on embedding (vector_cosine_ops)
        GIN on search_vector
        btree on mneme_id (for grouping sub_mneme hits by parent)
        btree on (mneme_id, position) (for ordered turn retrieval)
    - dimension reconfiguration must cascade to sub_mnemes.embedding column
      (update existing configure_dimensions function)

  DONE_WHEN:
    - CREATE EXTENSION pg_ghola creates both mnemes and sub_mnemes
    - dropping a mneme cascades and removes its sub_mnemes
    - HNSW and GIN indexes exist and are used by EXPLAIN plans
    - configure_dimensions(D) reconfigures both mnemes.embedding and
      sub_mnemes.embedding atomically

  EXAMPLES:
    -- cascade delete
    INSERT INTO mnemes (...) RETURNING id;  -- parent_id
    INSERT INTO sub_mnemes (mneme_id, position, ...) VALUES (parent_id, 0, ...);
    DELETE FROM mnemes WHERE id = parent_id;
    -> sub_mnemes row is also deleted

    -- index usage
    EXPLAIN SELECT id FROM sub_mnemes ORDER BY embedding <=> '[...]'::vector LIMIT 30;
    -> plan uses sub_mnemes_embedding_hnsw_idx

  ERRORS:
    - FK violation on insert (no parent mneme) -> standard Postgres FK error
    - duplicate (mneme_id, position) -> unique violation error
    - dimension mismatch on embedding -> standard pgvector error

### FUNCTION: recall_with_submnemes -> modified recall_inner

  FILE: src/recall.rs

  RULES:
    - modify the semantic pathway CTE:
        FROM: HNSW search on mnemes.embedding
        TO:   HNSW search on sub_mnemes.embedding, join to mnemes for filters
              and cognitive state, keep best cosine_sim per parent mneme_id
    - modify the lexical pathway CTE:
        FROM: FTS on mnemes.search_vector
        TO:   FTS on sub_mnemes.search_vector, join to mnemes, keep best
              ts_rank per parent
    - entity pathway unchanged (entities column is on mnemes)
    - cluster pathway unchanged (cluster_id on mnemes)
    - after pathway union and dedup, fetch:
        - best-matching sub_mneme.content per parent (for result.content)
        - best-matching sub_mneme.position per parent (for matched_position)
    - if a parent has NO sub_mnemes (legacy data), fall back to the pre-change
      behavior: use parent.embedding and parent.search_vector
    - composite scoring unchanged

  GRACEFUL_DEGRADATION:
    - empty sub_mnemes table -> all candidates fall back to parent embedding
      and parent search_vector. recall still works, no regression from iter 9.
    - partial population (some parents have sub_mnemes, some don't) -> per-row
      fallback via COALESCE or LEFT JOIN pattern.

  DONE_WHEN:
    - recall() signature and RecallResult contract unchanged externally
      (matched_position added as new field, backward-compatible)
    - all existing recall() examples in tests still pass
    - new test: ingest session with sub_mnemes, query matching a specific
      turn, verify matched_position is the expected turn
    - new test: legacy mneme without sub_mnemes still returns results
    - performance: recall latency within 2x of current on pool_size=30

  EXAMPLES:
    -- turn-specific retrieval
    -- session about K8s debugging, turn 3 mentions Qwen3 memory needs
    recall(ws, "what memory does Qwen3 need", emb, 10, 0.0)
      -> mneme X with matched_position=3, content=the turn 3 text
      (NOT the full session content)

    -- legacy fallback
    -- mneme Y has no sub_mnemes, created before migration
    recall(ws, "any query", emb, 10, 0.0)
      -> mneme Y returned with content=parent content
         matched_position=NULL or 0 (fallback marker)

    -- cognitive scoring still dominates
    -- two parents, both best sub_mneme has cosine=0.9
    -- parent A confidence=0.9, parent B confidence=0.3
    -> parent A ranks higher via confidence multiplier

  ERRORS:
    - sub_mnemes FK orphan (parent missing) -> fail loudly during recall,
      surface as a data integrity error (should never happen with CASCADE)
    - embedding dim mismatch -> existing pgvector error

  NOT_ALLOWED:
    - search sub_mnemes with workspace_id filter missing (must always scope
      via join to mnemes for workspace isolation)
    - return sub_mneme uuids as result mneme_ids (contract is parent uuids)
    - score sub_mnemes with confidence, ACT-R, Hebbian (cognitive scoring
      stays on parent)

### FUNCTION: add_matched_position_to_recall_result -> new struct field

  FILE: src/types.rs (if present) or wherever RecallResult is defined

  RULES:
    - extend RecallResult composite type with matched_position smallint
    - default value NULL (for legacy fallback path)
    - update TableIterator return type in recall_inner
    - update create_type_recall_result extension_sql to add the column
      (schema migration for existing installations)

  DONE_WHEN:
    - new column surfaces in the recall() result set
    - SQL clients can SELECT (r).matched_position FROM ghola.recall(...) r
    - existing clients that don't read matched_position are unaffected

  EXAMPLES:
    SELECT (r).mneme_id, (r).content, (r).matched_position
    FROM ghola.recall(ws_id, 'query', emb, 10) r;
    -> matched_position is the 0-indexed turn that produced the best match

---

## Phase 2: Chapterhouse RememberWithTurns (Go)

No embedding server changes. The existing `/v1/embeddings` endpoint on
`pg_ghola/analysis/embed_server.py` already accepts `{"input": [str, ...]}`
and returns one embedding per input string, L2-normalized. That's the
primitive we need.

### FUNCTION: Embed (existing Provider method, used for batched turn encoding)

  FILE: chapterhouse/ch-server/internal/embedding/openai.go (NO CHANGES)

  Confirm the current OpenAIProvider.Embed signature accepts a list of
  strings and returns `[][]float32` aligned with the input order. If the
  current signature is single-string (`Embed(ctx, text) ([]float32, ...)`),
  add a sibling `EmbedBatch(ctx, texts) ([][]float32, ...)`. Either
  way, per-turn encoding uses batched input so each `remember_with_turns`
  call makes at most TWO HTTP round-trips: one for the session-level
  parent embedding, one for the batch of turn embeddings.

  NOT_ALLOWED:
    - make N separate HTTP calls (one per turn); inefficient and creates
      variable latency per ingest
    - implement embedding locally in Go

### FUNCTION: RememberWithTurns (Go) -> parent mneme id

  FILE: chapterhouse/ch-server/internal/mneme/store.go (extend existing
        remember path or add RememberWithTurns method alongside it)

  RULES:
    - inputs match the existing Chapterhouse remember path plus structured
      turn boundaries from the MCP request payload. Each turn carries
      {role, content, char_start, char_end}; concatenating contents in
      order must reconstruct session_text.
    - Validate turn reconstruction: concatenation of turn.content values
      equals session_text. On mismatch, fail before any DB writes.
    - BEGIN pgx transaction
    - Build the parent content and concept using existing Chapterhouse
      logic (concept derivation, metadata). session_text becomes the
      parent's `content` column.
    - Call provider.Embed(session_text) for the parent embedding.
    - Call provider.Embed([turn0.content, turn1.content, ...]) ONCE for
      all turn embeddings in one HTTP round-trip. Each turn is encoded
      in isolation (no cross-turn attention); the endpoint does batched
      native sentence-level encoding.
    - INSERT parent mneme (mnemes table) with concept, content,
      embedding, metadata, session_id, tags, etc.
    - INSERT one sub_mneme row per turn:
        mneme_id = parent mneme id
        position = turn index (0-indexed)
        role = turn.role
        content = turn.content (exact original text)
        embedding = the isolated per-turn vector
        token_start = turn.char_start
        token_end = turn.char_end
      NOTE: token_start/token_end in the sub_mneme schema hold CHARACTER
      offsets, not token offsets. The column names are preserved from the
      Phase 1 schema to avoid another migration; the semantics are
      "character span into parent session_text". Document this in the
      Chapterhouse code comments.
    - COMMIT transaction (atomic: parent + all sub_mnemes land together)
    - Enqueue parent mneme for gating worker and contradiction worker
      (existing behavior)
    - Return parent mneme id

  DONE_WHEN:
    - Parent mneme and all sub_mnemes exist after commit
    - count(sub_mnemes WHERE mneme_id = parent) == len(turns)
    - Parent content == session_text (exact)
    - For each sub_mneme i: session_text[s.token_start:s.token_end] == s.content
    - Parent enqueued in gating_queue and contradiction_queue
    - Atomic: if any sub_mneme insert fails, the parent insert rolls back
    - Ingest latency: within 2x of the existing single-mneme remember path
      (two HTTP calls instead of one; ingest is a write-path operation so
      this latency is acceptable)

  EXAMPLES:
    -- happy path: 8-turn K8s debugging session
    RememberWithTurns(ctx, ws, sessionText, turns[:8], metadata)
      -> returns parent uuid; sub_mnemes count == 8
      -- two HTTP calls to embed_server (1 parent + 1 batch of 8 turns)

    -- single-turn session
    RememberWithTurns(ctx, ws, "short fact", turns[:1], metadata)
      -> returns parent uuid; sub_mnemes count == 1

    -- long session (>32K tokens in parent)
    -- The parent embedding call truncates at Qwen3-Embedding's 32K max_seq_length.
    -- Individual turns are typically much shorter and encode without truncation.
    -- The parent embedding becoming truncated is acceptable because the parent
    -- embedding is for cognitive operations, not primary retrieval; sub_mnemes
    -- carry the retrievable content.

    -- transaction rollback
    -- simulated DB failure mid-insert
    RememberWithTurns(ctx, ws, sessionText, turns, metadata) -> error
      -- BEGIN rolled back; neither parent nor sub_mnemes exist

  ERRORS:
    - turns list empty -> return error "session must have >= 1 turn"
    - turn reconstruction mismatch (concat of turn.content != session_text)
      -> return error with the position where divergence starts
    - embedding server returns non-1024d vectors -> return error
    - len(turn embeddings) != len(turns) -> return error (server bug)
    - embedding dim != configured pg_ghola dim -> Postgres error surfaced
    - DB transaction failure -> rollback, return wrapped error

  NOT_ALLOWED:
    - insert sub_mnemes without a parent mneme
    - make one HTTP call per turn (use batched Embed)
    - compute embeddings locally in Go
    - leave partial state (parent without sub_mnemes) on error

### CONSTRAINT: chapterhouse_model_pinning
  The embedding server must run Qwen3-Embedding-0.6B via sentence-transformers
  5.4.0 or later, with normalize_embeddings=True. These values pin the
  embedding engine for BOTH ingest-time turn encoding AND query-time
  embedding. Cross-engine mismatch (iters 12, 15 in the samsara history)
  produces 4-16pp R@5 variance and invalidates all downstream measurements.

### CONSTRAINT: mcp_turn_structure_preserved
  RememberWithTurns REQUIRES structured turn boundaries from the MCP
  request payload. Do NOT split session text with regex or whitespace
  heuristics (brittle, locale-dependent, incorrect for multi-line turns).
  Callers that submit raw session text without turn structure fall back
  to the existing Remember path (one mneme, no sub_mnemes), which still
  works correctly via the recall_legacy fallback pathways in pg_ghola.

### CONSTRAINT: isolated_encoding_only
  Turn embeddings are produced by encoding each turn's content IN
  ISOLATION via the existing sentence-level endpoint. No late chunking.
  No cross-turn attention. No session-context awareness. This decision
  was made on 2026-04-18 after Tier 1 samsara evidence showed no
  aggregation strategy over isolated+contextual embeddings beats pure
  mean-pool late chunking, and no fixed encoding strategy captures more
  than ~11pp of the ~60pp gap to state-of-the-art retrieval systems.

  Revising this decision requires:
    - Evidence from Tier 1 samsara (encoding-eval harness) showing a
      candidate strategy beats isolated by >5pp top-1 across the full
      151-case set AND on a factual-heavy expansion of that set;
    - A Tier 2 measurement on a LongMemEval subset showing the candidate
      translates from in-memory cosine ranking to database-roundtrip
      R@5 without regression;
    - Explicit documentation updating THIS constraint with the new
      strategy's trade-offs.

---

## Phase 3: Benchmark re-ingest

### FUNCTION: reingest_longmemeval -> populated database

  FILE: longmemeval-ghola/scripts/reingest_submnemes.py

  RULES:
    - TRUNCATE mnemes CASCADE (removes mnemes, sub_mnemes, associations,
      queues; preserves cluster_centroids? - drop those too)
    - load LongMemEval-S dataset
    - for each session in the dataset:
        extract structured turns (the dataset preserves turn boundaries)
        call remember_with_turns(ws, session_text, turns, metadata)
    - respect rate limits (existing 200ms throttle, Retry-After handling)
    - verify post-ingest:
        count(mnemes) == 19195
        count(sub_mnemes) > 10 * count(mnemes) on average
        every parent mneme has >= 1 sub_mneme

  DONE_WHEN:
    - database is populated with parent mnemes and sub_mnemes
    - gating queue and contradiction queue drain within expected time
    - HNSW index on sub_mnemes is built and used by EXPLAIN
    - binary COPY dump taken for pinned re-runs

  EXAMPLES:
    python reingest_submnemes.py --dataset longmemeval-s --workspace ghola-bench
    -> stdout reports ingestion progress
    -> final: 19,195 parents, ~190K sub_mnemes, indexes ready

  ERRORS:
    - Chapterhouse unavailable -> exponential backoff, eventual failure
    - disk space exhausted -> fail loudly, don't partial-commit
    - embedding dim mismatch between configure_dimensions and model output
      -> fail loudly with configuration instructions

### FUNCTION: run_benchmark_baseline -> R@5 metric

  FILE: longmemeval-ghola/scripts/run_benchmark.py (existing)

  RULES:
    - full retrieval-time state reset (existing:
      access_count=0, hebbian assoc truncate, co_activation_queue truncate)
    - mean of 3 runs per iteration
    - record per-category breakdown
    - compare to iter 9 baseline (27.5% R@5 overall)
    - hypothesis: per-turn granularity (isolated encoding, not session-level)
      achieves >35% R@5. The gain comes from fine-grained content matching,
      not from contextual encoding cleverness. Variance budget: >2pp R@5
      improvement required to be considered significant.

  DONE_WHEN:
    - 3 benchmark runs complete with full state reset between each
    - results written to results/iter16_per_turn_encoding.json
    - cross-run variance reported alongside mean

  EXAMPLES:
    python run_benchmark.py --iter 16 --runs 3
    -> stdout reports per-run R@5, final mean, variance
    -> hypothesis tested: per-turn isolated encoding improves R@5 by >2pp

  ERRORS:
    - variance > 5pp -> flag for investigation (should be <2pp with pinned DB)
    - R@5 regression vs iter 9 -> flag for investigation, do NOT auto-revert;
      per-turn granularity is a major architectural change, not a tuning knob

---

## Out of Scope (explicitly deferred)

- **Late chunking** and all variants (last-token, mean-pool, sliding window).
  Tier 1 samsara evidence on 151 cases showed late chunking's best variant
  wins +9.9pp on conversational-biased data but loses -20pp on self-contained
  content. Net improvement is capped below the ~11pp oracle ceiling, which
  is a small fraction of the 60pp gap to state-of-the-art retrieval. The
  complexity is not justified. Captured in encoding-eval/ repo for future
  reference.
- **Store-both architectures**. Tier 1 showed simple MAX and MEAN aggregations
  over isolated+contextual embeddings LOSE to pure mean-pool. Query-time
  routing could close the gap but adds classifier complexity below the
  variance budget.
- **Adaptive encoding with self-cosine classifier**. Deferred pending
  evidence that the oracle ceiling is worth chasing on real workloads.
- Consolidation-as-abstraction-change (re-embed aged mnemes at coarser
  granularity; represent episodic -> semantic shift). Still in the roadmap
  but Phase 3+.
- Stella v5 1.5B model upgrade. Available on HuggingFace (verified
  2026-04-14); deferred pending evidence that model quality is a bottleneck
  after primitives work has been exercised on longitudinal data.
- Turn-level Hebbian associations and turn-level cognitive scoring. Keep
  cognitive state on parent mnemes; sub_mnemes are retrieval primitives.
- ColBERT-style late interaction at query time. Out of scope for the
  current Postgres + pgvector stack.
- Per-turn concept enrichment. The iter 13 FTS saturation finding shows
  the enrichment should stay at the parent level; raw turn content goes
  into sub_mneme search_vector.

## Test Matrix

| Test | Layer | What it verifies |
|---|---|---|
| schema cascade delete | Rust | FK + ON DELETE CASCADE works |
| HNSW index usage | Rust | EXPLAIN plans show index scan on sub_mnemes |
| recall legacy fallback | Rust | mnemes without sub_mnemes still return results |
| recall turn-specific match | Rust | matched_position is set correctly |
| recall workspace isolation | Rust | sub_mneme searches scope to workspace |
| turn batch embedding | Go | Embed([text, text, ...]) returns N 1024d vectors in order |
| turn reconstruction check | Go | concat of turn contents equals session_text before any DB write |
| RememberWithTurns atomicity | Go | transaction rollback on mid-insert failure |
| RememberWithTurns count | Go | sub_mnemes count matches turns count |
| ingest latency | Go | within 2x of existing single-mneme remember path |
| cross-engine consistency | Benchmark | query-time model matches ingest-time model |
| variance budget | Benchmark | <2pp across 3 runs on pinned DB |
| hypothesis | Benchmark | R@5 improves >2pp vs iter 9 (27.5%) |
