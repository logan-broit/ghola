# Multi-Granularity Encoding Implementation

Simplex v0.5 specification for the code changes that implement the
[multi-granularity encoding design](./2026-04-16-multi-granularity-encoding-design.md).

Three touchpoints:
1. **pg_ghola extension (Rust)** -- sub_mnemes table, recall changes
2. **Chapterhouse MCP (Python)** -- late-chunking encoder, remember_with_turns
3. **Benchmark harness (Python)** -- TRUNCATE+re-ingest via the new path

## Preconditions

- Design doc constraints are load-bearing and referenced throughout.
- Qwen3-Embedding-0.6B verified (2026-04-16): last-token pooling,
  max_seq_length 32,768, output_value="token_embeddings" works.
- sentence-transformers >= 5.4.0 pinned in Chapterhouse.
- pg_ghola schema migrations are additive (no existing data destroyed).

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

## Phase 2: Chapterhouse late-chunking encoder (Python)

### FUNCTION: late_chunk_encode(session_text, turns, model) -> list of (embedding, token_start, token_end)

  FILE: chapterhouse/encoding/late_chunk.py (new)

  RULES:
    - inputs:
        session_text: string (the full concatenated session)
        turns: list of (char_start, char_end, role) tuples
        model: SentenceTransformer instance (pinned: Qwen3-Embedding-0.6B)
    - tokenize session_text with model.tokenizer(return_offsets_mapping=True,
      truncation=False, add_special_tokens=True)
    - if n_tokens <= model.max_seq_length:
        forward pass: model.encode(session_text,
                                   output_value="token_embeddings",
                                   convert_to_tensor=True)
        for each turn (cs, ce, role):
            find last token index i such that:
                attention_mask[i] == 1 AND
                offsets[i] != (0,0) AND  # not a special token
                offsets[i][0] < ce AND offsets[i][1] > cs  # overlaps turn span
            embedding = token_embeddings[i] / L2_norm(token_embeddings[i])
            yield (embedding, offsets[i_first_in_span][0], offsets[i][1])
    - if n_tokens > model.max_seq_length:
        invoke sliding_window_encode(...) (see below)
    - return embeddings in order matching the turns list

  DONE_WHEN:
    - len(output) == len(turns)
    - every embedding is L2-normalized to 1.0 (tolerance 1e-4)
    - isolation test: encoding ["Single turn."] via late_chunk produces
      a vector whose cosine with model.encode("Single turn.",
      normalize_embeddings=True) is > 0.995 (not exactly 1.0 because
      session vs. single-turn framing can differ slightly)
    - context test: a turn's late-chunked embedding differs from the
      same turn's isolated embedding (cosine < 0.95 for turns with
      prior context in the session)

  EXAMPLES:
    -- short session, single forward pass
    session = "USER: ... ASSISTANT: ... USER: ..."
    turns = [(0, 30, 'user'), (31, 90, 'assistant'), (91, 130, 'user')]
    late_chunk_encode(session, turns, model) -> 3 (embedding, ts, te) tuples

    -- long session triggers sliding window
    session = (60K tokens of conversation)
    -> sliding_window_encode() is dispatched internally

  ERRORS:
    - turns list empty -> raise ValueError("no turns provided")
    - turn span produces no matching tokens -> raise ValueError with
      context (turn index, char span, token count)
    - model does not expose token_embeddings -> raise RuntimeError
      (should not happen with Qwen3-Embedding-0.6B but check in init)

  NOT_ALLOWED:
    - encode any turn in isolation as the "embedding" (defeats the purpose)
    - mean-pool the token embeddings (wrong for last-token-pooling models)
    - use a non-pinned model version

### FUNCTION: sliding_window_encode(session_text, turns, model, stride=0.5) -> list of (embedding, token_start, token_end)

  FILE: chapterhouse/encoding/late_chunk.py

  RULES:
    - window_size = model.max_seq_length (32,768 for Qwen3)
    - stride_tokens = int(window_size * stride)  # 50% = 16,384
    - tokenize full session once to get offsets and total n_tokens
    - construct windows: for i in range(0, n_tokens, stride_tokens):
        window = tokens[i : i + window_size]
        decode window back to text (via offsets) for model.encode()
        forward pass, store token embeddings by absolute token index
    - for each turn:
        find candidate windows that fully contain the turn's token range
        pick the window where the turn is most centrally positioned:
            score = min(turn_start - window_start, window_end - turn_end)
            (maximizes symmetric context around the turn)
        extract last-token embedding from that window's output
    - return embeddings aligned with turn order

  DONE_WHEN:
    - sessions up to 3 * window_size encode correctly
    - every turn's embedding comes from a window that fully contains it
    - in the overlap region, the preferred window is the more-centered one
    - unit test: force a 50K-token synthetic session, verify correctness

  EXAMPLES:
    -- 50K-token session, 200 turns, window=32K, stride=16K
    -> windows: [0:32K], [16K:48K], [32K:50K]
    -> turn at token 10K -> window 0 (most centered: min(10K, 22K) = 10K)
    -> turn at token 25K -> window 1 (most centered: min(9K, 23K) = 9K, vs window 0: min(25K, 7K) = 7K)
    -> turn at token 45K -> window 2

  ERRORS:
    - session has zero tokens -> raise ValueError
    - no window fully contains a turn (turn > window_size) -> raise
      ValueError with turn index and its length (pathological case;
      a single turn longer than 32K tokens is unexpected)

  NOT_ALLOWED:
    - run windows without overlap (stride >= 1.0)
    - pick windows where the turn touches the edge (a turn needs context
      on both sides or it degrades to isolated encoding)

### FUNCTION: remember_with_turns(workspace_id, session_text, turns, metadata) -> parent_mneme_id

  FILE: chapterhouse/remember.py (modified from existing remember)

  RULES:
    - inputs:
        workspace_id: uuid
        session_text: string (the full session)
        turns: list of {role, content, start, end} or MCP-structured turns
        metadata: dict (tags, session_id, memory_type, scope, expires_at, etc.)
    - BEGIN transaction
    - compute parent-level embedding:
        either model.encode(session_text, normalize=True) [native last-token
        pooling gives a summary vector] OR pool sub_mneme embeddings
        (design choice: go with native encoding of full session for simplicity,
        note that parent embedding is mostly for cognitive operations not
        retrieval)
    - compute parent concept: existing behavior (intent-aware enrichment
      by gating worker later; Chapterhouse writes a baseline)
    - INSERT parent mneme (mnemes table) with parent concept, parent content
      (= session_text), parent embedding, metadata columns
    - call late_chunk_encode(session_text, turn_spans, model)
    - INSERT one sub_mneme row per returned (embedding, token_start, token_end):
        mneme_id = parent mneme id
        position = turn index
        role = turn.role
        content = session_text[turn.char_start : turn.char_end]
        embedding = the late-chunked vector
        token_start, token_end from encoder
    - COMMIT transaction
    - enqueue parent for gating worker and contradiction worker
      (existing behavior)
    - return parent mneme id

  DONE_WHEN:
    - parent mneme and all sub_mnemes exist after commit
    - count(sub_mnemes WHERE mneme_id = parent) == len(turns)
    - parent content == session_text (exact)
    - sub_mneme contents reconstruct the session (with role markers) without
      loss
    - parent enqueued in gating_queue and contradiction_queue
    - atomic: if any sub_mneme insert fails, the parent insert rolls back

  EXAMPLES:
    -- happy path
    remember_with_turns(ws, "8-turn K8s debugging session", turns=[...])
      -> returns parent uuid; sub_mnemes count == 8

    -- single-turn session
    remember_with_turns(ws, "short fact", turns=[one_turn])
      -> returns parent uuid; sub_mnemes count == 1

    -- transaction rollback
    -- simulated failure mid-insert
    remember_with_turns(ws, "session", turns=[bad])
      -> BEGIN rolled back; neither parent nor sub_mnemes exist

  ERRORS:
    - turns list empty -> ValueError("session must have >= 1 turn")
    - turn char spans don't cover session_text fully (gaps or overlaps) ->
      ValueError with the gap/overlap location
    - embedding dim != configured pg_ghola dim -> Postgres error surfaced
    - DB transaction failure -> rollback, re-raise

  NOT_ALLOWED:
    - insert sub_mnemes without a parent mneme
    - insert sub_mnemes encoded independently of session context
    - leave partial state (parent without sub_mnemes) on error

### CONSTRAINT: chapterhouse_model_pinning
  Chapterhouse config fixes:
    MODEL_NAME="Qwen/Qwen3-Embedding-0.6B"
    SENTENCE_TRANSFORMERS_VERSION=">=5.4.0,<6.0"
    TORCH_DTYPE="bfloat16"  # default for Qwen3-Embedding
    DEVICE="cuda"           # documented; CPU works but is slow
  These values MUST match what the benchmark harness uses at query time.
  Cross-engine mismatch (iters 12, 15) produces 4-16pp variance.

### CONSTRAINT: mcp_turn_structure_preserved
  The Chapterhouse MCP remember endpoint currently accepts either raw session
  text or structured turn lists. remember_with_turns REQUIRES structured turns.
  Callers providing raw text must be updated to pass MCP turn messages or
  a Chapterhouse-side splitter runs (NOT a text heuristic -- use the actual
  message boundaries from the MCP protocol).

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
    - hypothesis: multi-granularity encoding with late chunking achieves
      >40% R@5 (conservative). Variance budget: >2pp R@5 improvement required.

  DONE_WHEN:
    - 3 benchmark runs complete with full state reset between each
    - results written to results/iter16_late_chunking.json
    - cross-run variance reported alongside mean

  EXAMPLES:
    python run_benchmark.py --iter 16 --runs 3
    -> stdout reports per-run R@5, final mean, variance
    -> hypothesis tested: multi-granularity improves R@5 by >2pp

  ERRORS:
    - variance > 5pp -> flag for investigation (should be <2pp with pinned DB)
    - R@5 regression vs iter 9 -> flag for investigation, do NOT auto-revert;
      multi-granularity is a major architectural change, not a tuning knob

---

## Out of Scope (explicitly deferred)

- Consolidation-as-abstraction-change (re-embed aged mnemes at coarser
  granularity; represent episodic -> semantic shift)
- Stella v5 1.5B model upgrade
- Turn-level Hebbian associations
- Turn-level cognitive scoring (confidence, ACT-R on sub_mnemes)
- ColBERT-style late interaction at query time
- Per-turn concept enrichment
- Adaptive chunking (attention-pattern-driven chunk boundaries at encoding
  time; current design uses MCP-provided turn boundaries)

## Test Matrix

| Test | Layer | What it verifies |
|---|---|---|
| schema cascade delete | Rust | FK + ON DELETE CASCADE works |
| HNSW index usage | Rust | EXPLAIN plans show index scan on sub_mnemes |
| recall legacy fallback | Rust | mnemes without sub_mnemes still return results |
| recall turn-specific match | Rust | matched_position is set correctly |
| recall workspace isolation | Rust | sub_mneme searches scope to workspace |
| late_chunk_encode short session | Python | single forward pass path |
| late_chunk_encode long session | Python | sliding window path |
| late_chunk_encode idempotency | Python | same input -> same output (deterministic) |
| remember_with_turns atomicity | Python | transaction rollback on mid-insert failure |
| remember_with_turns count | Python | sub_mnemes count matches turns count |
| cross-engine consistency | Benchmark | query-time model matches ingest-time model |
| variance budget | Benchmark | <2pp across 3 runs on pinned DB |
| hypothesis | Benchmark | R@5 improves >2pp vs iter 9 (27.5%) |
