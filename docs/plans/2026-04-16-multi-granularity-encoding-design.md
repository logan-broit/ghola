# Multi-Granularity Encoding Design

Simplex v0.5 specification for per-turn encoding with late chunking in pg_ghola.

## Context

Iter 9 baseline is 27.5% R@5 on LongMemEval-S. BM25 alone achieves ~70%.
Stella v5 1.5B achieves ~85%. MemPalace with verbatim storage achieves 96.6%.
The gap is not cognitive scoring or retrieval pathways. The gap is encoding granularity.

Current state: one mneme per session, one embedding per mneme, one embedding per
median 2,245-token session. A query about a specific fact competes against a
session-averaged embedding. The FTS pathway saturates at ts_rank=1.0 for 80%+ of
candidates after concept enrichment (iter 2), providing zero discrimination.

Decision: encode at turn-level granularity while preserving session context via
late chunking. Store turn-level embeddings and turn-level text in a new table
linked to parent mnemes. Cognitive primitives (confidence, ACT-R, Hebbian,
gating, consolidation, contradiction) remain on parent mnemes. Retrieval searches
turn-level embeddings, aggregates to parent mnemes, scores with cognitive state.

## Neuroscience Basis

| Mechanism | Brain Analog | Reference |
|---|---|---|
| Full-context encoding | Temporal context model, contextual binding | Howard & Kahana, 2002 |
| Per-turn retrieval granularity | Pattern separation (dentate gyrus) | Yassa & Stark, 2011 |
| Session-level cognitive state | Hippocampal indexing theory | Teyler & DiScenna, 1986 |
| Attention-determined context scope | Cortical attention gating | Desimone & Duncan, 1995 |

Late chunking mirrors episodic encoding: the entire experience is processed with
full attention context, then discrete events (turns) are extracted from that
context-aware representation. Each event carries contextual awareness of the
surrounding experience without averaging it.

## Design Decisions

1. **Storage model**: new `sub_mnemes` table with foreign key to `mnemes`.
   Preserves existing cognitive architecture. Parent mneme is the unit of
   cognitive processing. Sub-mneme is the unit of retrieval.

2. **Encoding method**: late chunking with last-token pooling per turn.
   Pass the full session through Qwen3-Embedding-0.6B once to get token-level
   hidden states. For each turn, extract the hidden state at the LAST real
   token of that turn's character span. Qwen3-Embedding is a decoder model
   with causal attention; the last token of turn N has attended to all
   preceding tokens (turns 0..N-1 plus turn N's own tokens). The mean-pool
   approach used in encoder-style embedders does NOT apply here -- it
   produces embeddings with cosine ~0.72 vs the native last-token output
   (verified on 2026-04-16 in /tmp/late_chunk_probe.py).

3. **Long-session handling**: sliding window when session exceeds model context.
   Qwen3-Embedding-0.6B max_seq_length is 32,768 tokens, which fits the
   LongMemEval-S median session (~2,245 tokens) ~14x over. Sliding window
   is rarely triggered in practice but required for correctness on edge-case
   long sessions. Windows overlap by 50% stride. Each turn's embedding comes
   from the window where the turn is most centrally positioned.

4. **Embedding engine**: sentence-transformers with Qwen3-Embedding-0.6B pinned.
   Consistent engine for stored and query embeddings. Float32 dtype for
   reproducibility. Accepts model-upgrade path (Stella v5 1.5B) as future work.

5. **FTS on original turn text**: sub_mneme search_vector is generated from raw
   turn content, not enriched concept. Breaks the ts_rank saturation observed
   in iter 13. Each turn has discriminative FTS rank against query.

6. **Parent mneme embedding**: retained for cognitive operations that need a
   session-level representation (session-level Hebbian associations, cluster
   assignment). Computed as mean of sub_mneme embeddings OR from the full
   session pooled embedding (implementation choice).

7. **Migration**: schema additive. Existing mnemes remain. Re-ingest benchmark
   dataset to populate sub_mnemes for fair measurement.

---

DATA: SubMneme
  id: uuid, primary key
  mneme_id: uuid, references mnemes(id) on delete cascade
  position: smallint, 0-indexed order within parent session
  role: string, one of user, assistant, system, tool
  content: text, original turn text (not enriched)
  embedding: vector(1024), late-chunked turn embedding
  search_vector: tsvector generated from content
  token_start: integer, token offset in parent session encoding window
  token_end: integer, token offset in parent session encoding window
  UNIQUE(mneme_id, position)

DATA: EncodingWindow
  session_text: string
  token_offset: integer, starting token in the session
  token_length: integer, tokens in this window
  embeddings: array of vector(1024), one per token
  turn_boundaries: list of (position, token_start, token_end)

DATA: RecallResult (unchanged contract; changed internals)
  mneme_id: uuid, parent mneme id
  score: float8, composite score using best-matching sub_mneme content_match
  content_match: float8, derived from best sub_mneme cosine and fts
  activation: float8, ACT-R on parent mneme access_count and last_access
  hebbian_boost: float8, Hebbian association boost on parent mneme
  confidence: float8, parent mneme confidence
  concept: string, parent mneme concept
  content: text, best-matching sub_mneme content (not full session)
  matched_position: smallint, sub_mneme position that produced the best match

---

CONSTRAINT: submneme_owns_content_match
  sub_mneme embedding and search_vector are the ONLY sources of content_match.
  parent mneme embedding is not consulted during content matching.
  this separates "is the content relevant" (sub_mneme) from
  "is the memory trustworthy and recent" (parent mneme).

CONSTRAINT: parent_mneme_owns_cognitive_state
  confidence, access_count, last_access, tier, state, memory_type, scope, tags,
  session_id, entities, content_dates, cluster_id, intent all remain on mnemes.
  sub_mnemes have no cognitive state of their own.
  Hebbian associations link mnemes (not sub_mnemes).
  contradiction detection compares mnemes (not sub_mnemes).

CONSTRAINT: late_chunking_not_independent_encoding
  per-turn embeddings MUST come from a forward pass that includes the full
  session context (or a sliding window containing the turn plus surrounding
  turns). encoding each turn text in isolation is NOT late chunking and is
  explicitly forbidden. the attention mechanism is the context-binding signal.

CONSTRAINT: embedding_engine_consistency
  stored and query embeddings MUST be generated by the same engine, version,
  dtype, and hardware class. cross-engine mismatch (iter 12, 15) produces
  4-16pp variance that masks all other signals. pin sentence-transformers
  with Qwen3-Embedding-0.6B at float32. document the exact version.

CONSTRAINT: sliding_window_for_long_sessions
  when session token length exceeds model max context, process in overlapping
  windows. each turn's embedding comes from the window where it is most
  centrally positioned (maximum symmetric context on both sides). a turn at
  the start of the session uses the first window; a turn in the middle uses
  a window centered on it.

CONSTRAINT: schema_migration_is_additive
  mnemes table is not modified. sub_mnemes is a new table. existing mnemes
  without sub_mnemes continue to work via the parent embedding fallback
  (see recall_with_submneme_fallback). re-ingest populates sub_mnemes for
  benchmark measurement; production data migrates incrementally.

CONSTRAINT: recall_contract_preserved
  the recall() SQL function signature does not change.
  the RecallResult struct does not change (matched_position added as new field).
  workspace isolation, confidence filtering, memory_type/scope/tags/session_id
  filtering, co-activation enqueue, and expired working memory exclusion all
  behave identically.

---

FUNCTION: encode_session_with_late_chunking(session_text, turn_boundaries, model) -> list of SubMneme

  RULES:
    - tokenize the full session once with the embedding model's tokenizer
      using return_offsets_mapping to preserve char-to-token alignment
    - if total tokens <= model max context:
      - single forward pass with output_value="token_embeddings"
      - for each turn, find the LAST real token whose char-offset overlaps
        the turn's character span (skip special tokens where offset == (0, 0)
        and padding tokens where attention_mask == 0)
      - extract the hidden state at that last-token index
      - L2-normalize the extracted vector
    - if total tokens > model max context:
      - construct sliding windows of model_max_context tokens with 50% stride
      - forward pass each window
      - for each turn, select the window where the turn is most centered
        (maximizes min(tokens_before_turn, tokens_after_turn) within window)
      - extract the last-token hidden state from the selected window's output
    - produce one SubMneme per turn with:
      - embedding = mean-pooled token embeddings (context-aware)
      - content = original turn text (unmodified)
      - position = index in turn_boundaries list
      - role = turn.role
      - token_start, token_end = offsets in parent session tokenization

  DONE_WHEN:
    - len(result) == len(turn_boundaries)
    - every SubMneme has a non-zero embedding
    - every SubMneme's content matches the source turn text exactly
    - embeddings are L2-normalized (or matching the model's expected output)

  EXAMPLES:
    -- short session, single pass
    (session of 1500 tokens, 8 turns, qwen3) -> 8 SubMnemes
    -- every embedding derived from the same forward pass
    -- turn 3's embedding attends to turns 1-8 via transformer attention

    -- long session, sliding window
    (session of 12000 tokens, 40 turns, qwen3 with 8192 ctx) -> 40 SubMnemes
    -- turns 1-20 use window[0:8192]
    -- turns 20-40 use window[4096:12288]
    -- turn 20 uses whichever window centers it better

    -- single-turn session
    (session of 1 turn, 500 tokens, qwen3) -> 1 SubMneme
    -- embedding equivalent to encoding the turn text directly

  ERRORS:
    - turn_boundaries empty -> fail with "no turns provided"
    - turn token spans don't match session tokenization -> fail with "turn boundary mismatch at position N"
    - model returns no token embeddings -> fail with "model does not expose token-level embeddings"
    - any unhandled condition -> fail with descriptive message

  NOT_ALLOWED:
    - encode any turn in isolation without session context
    - use a different model or engine than the one pinned for queries
    - truncate sessions to fit max context without using sliding window
    - modify turn content for encoding (e.g., concept enrichment at turn level)

---

FUNCTION: recall_with_submnemes(workspace_id, query_text, query_embedding, limit_n, min_confidence, weights, filters) -> list of RecallResult

  BASELINE:
    reference: recall_multi_pathway as of iter 9
    preserve:
      - workspace isolation
      - confidence filtering
      - co-activation enqueue
      - expired working memory exclusion
      - composite scoring formula
      - memory_type, scope, tags, session_id, entity, intent filtering
      - four pathway union (semantic, lexical, entity, cluster)
    evolve:
      - semantic pathway: HNSW on sub_mnemes.embedding, aggregate to parent by MAX cosine
      - lexical pathway: FTS on sub_mnemes.search_vector, aggregate to parent by MAX ts_rank
      - entity pathway: unchanged (entities stored on parent mneme)
      - cluster pathway: unchanged (cluster_id on parent mneme)
      - content_match derived from best sub_mneme per parent, not parent embedding
      - result.content returns the best-matching sub_mneme content
      - result includes matched_position

  RULES:
    - semantic pathway searches sub_mnemes HNSW, limit pool_size * 3 hits
    - group hits by sub_mnemes.mneme_id, keep MAX cosine_sim per parent
    - lexical pathway searches sub_mnemes FTS, limit pool_size * 3 hits
    - group hits by sub_mnemes.mneme_id, keep MAX ts_rank per parent
    - entity and cluster pathways query mnemes directly (unchanged)
    - for each candidate parent mneme, fetch best sub_mneme content and position
    - for parent mnemes without any sub_mnemes (legacy data), fall back to
      parent embedding and parent search_vector (graceful degradation)
    - join candidate parents to mnemes for cognitive state
    - compute composite scores using existing formula with new content_match
    - sort, truncate, enqueue co-activation

  DONE_WHEN:
    - all four pathways attempted
    - content_match sourced from sub_mnemes where available, parent otherwise
    - results ranked and truncated
    - co-activation enqueued

  EXAMPLES:
    -- preserved: workspace isolation
    (ws_A, "query", emb, 10, 0, defaults) -> only ws_A parent mnemes

    -- evolved: specific-fact query matches specific turn
    -- session 17 has turn 4 "I recommend Sushi Nakazawa on W 58th"
    -- query "which sushi place did you mention"
    (ws, "which sushi place did you mention", emb, 10, 0, defaults)
      -> session 17 ranked high; matched_position=4;
         content="I recommend Sushi Nakazawa on W 58th"
      -- NOT the full session text

    -- evolved: FTS discriminates (no longer saturated)
    -- sub_mneme ts_rank on raw turn text varies across candidates
    -- not all candidates return ts_rank=1.0

    -- evolved: legacy mneme fallback
    -- mneme created before sub_mneme migration
    (ws_mixed, "query", emb, 10, 0, defaults)
      -> legacy mneme scored via parent embedding (no regression)
      -> new mneme scored via best sub_mneme

    -- preserved: cognitive scoring on parent
    -- two sessions with identical best content_match
    -- session A confidence=0.9, session B confidence=0.3
    -> session A outranks session B via confidence multiplier

  ERRORS:
    - embedding dimension mismatch -> fail with "expected N, not M"
    - sub_mnemes references non-existent parent -> fail with "orphan sub_mneme {id}"
    - any unhandled condition -> fail with descriptive message

  NOT_ALLOWED:
    - score sub_mnemes with cognitive primitives (confidence, ACT-R, Hebbian)
    - return sub_mneme ids as results (contract is parent mneme ids)
    - skip co-activation enqueue
    - modify the composite scoring formula

---

FUNCTION: remember_with_turns(workspace_id, session, turns, metadata) -> parent mneme id

  RULES:
    - insert one parent mneme with session-level concept, content, embedding
      (concept and parent embedding behavior unchanged from current remember)
    - call encode_session_with_late_chunking(session.text, turns, model)
    - insert one sub_mneme row per returned SubMneme, all linked to parent mneme
    - insert within a single transaction (parent + all sub_mnemes atomic)
    - enqueue parent mneme for gating worker (entity extraction, intent, cluster)
    - enqueue parent mneme for contradiction worker

  DONE_WHEN:
    - parent mneme exists in mnemes table
    - len(sub_mnemes) == len(turns) for that parent
    - all sub_mneme embeddings are derived from late-chunked encoding
    - parent enqueued for async workers

  EXAMPLES:
    (ws, 8-turn session about K8s, turns=[...], meta) -> parent uuid; 8 sub_mnemes

    -- single-turn session
    (ws, 1-turn session, turns=[t0], meta) -> parent uuid; 1 sub_mneme

    -- empty session
    (ws, "", turns=[], meta) -> error (see ERRORS)

  ERRORS:
    - turns list empty -> fail with "session must have at least one turn"
    - session text mismatches concatenated turn text -> fail with "turn reconstruction mismatch"
    - any transaction failure -> rollback parent and sub_mnemes together
    - any unhandled condition -> fail with descriptive message

  NOT_ALLOWED:
    - insert sub_mnemes without a parent mneme
    - insert sub_mnemes encoded independently of session context
    - partial inserts (parent without sub_mnemes, or some sub_mnemes missing)

---

CONSTRAINT: benchmark_reingest_required
  fair measurement of multi-granularity encoding requires clean re-ingest of
  the LongMemEval-S dataset through remember_with_turns. existing mnemes
  without sub_mnemes score via parent embedding fallback and do not reflect
  the new design. TRUNCATE mnemes (cascades to sub_mnemes) before the
  measurement run. variance budget (>2pp R@5) applies.

CONSTRAINT: backward_compatibility_via_fallback
  mnemes without sub_mnemes MUST continue to score correctly via parent
  embedding and parent search_vector. this is the graceful degradation path
  for production data that hasn't been re-ingested. it is not the target
  state for benchmark measurement.

CONSTRAINT: hnsw_index_on_submneme_embedding
  the HNSW index on sub_mnemes.embedding is the new hot path for semantic
  retrieval. at ~10 turns per session average and 19K sessions, the index
  holds ~190K vectors. pgvector HNSW handles this scale. the index on
  mnemes.embedding remains for fallback but is secondary.

CONSTRAINT: gin_index_on_submneme_search_vector
  the GIN index on sub_mnemes.search_vector is the new hot path for lexical
  retrieval. the tsvector is generated from raw turn content (not concept
  enrichment), which eliminates ts_rank saturation.

CONSTRAINT: token_boundary_determinism
  turn_boundaries in remember_with_turns MUST come from structured conversation
  data (MCP turn messages), not from text heuristics. splitting on "\n\nuser:"
  or similar is brittle. Chapterhouse provides turn structure; preserve it end
  to end.

CONSTRAINT: sliding_window_overlap_required
  sliding windows MUST overlap. a non-overlapping window risks placing a turn
  at the very start or end of its window with zero context on one side.
  50% stride is the minimum. turns spanning window boundaries use the window
  where the turn is most centrally positioned.

CONSTRAINT: model_token_embedding_access
  VERIFIED 2026-04-16: Qwen3-Embedding-0.6B exposes pre-pooling token
  embeddings via sentence-transformers' output_value="token_embeddings".
  Shape matches tokenizer n_tokens x 1024 in bfloat16. The model uses
  last-token pooling (cosine 1.0 vs native; mean-pool scores 0.72).
  max_seq_length is 32,768 tokens. No fallback needed.

CONSTRAINT: decoder_causal_attention_semantics
  Qwen3-Embedding-0.6B is a decoder-style model with causal attention.
  The hidden state at the last token of turn N has attended to all tokens
  at positions 0..last_idx_of_turn_N. This is the context-awareness that
  late chunking provides: turn N's embedding carries information from
  turns 0..N-1 through causal attention. Later turns do NOT influence
  earlier turns' embeddings. This is acceptable: at retrieval time the
  query is independent, and the retrieval target is "which turn was this
  information expressed in" -- the answer turn carries its own context plus
  all prior context, which is typically what queries reference.

---

## Implementation Phases

Phase 1: schema migration
  - CREATE TABLE sub_mnemes with indexes
  - verify pgvector HNSW on 1024d vectors at ~200K scale
  - update pg_ghola extension to include sub_mneme management

Phase 2: encoding pipeline (Chapterhouse)
  - implement encode_session_with_late_chunking
  - verify Qwen3-Embedding-0.6B exposes token_embeddings
  - implement sliding window logic for long sessions
  - pin sentence-transformers version

Phase 3: recall changes (pg_ghola)
  - modify recall_multi_pathway to query sub_mnemes for semantic and lexical
  - implement fallback path for legacy mnemes without sub_mnemes
  - add matched_position to RecallResult (additive, backward compatible)

Phase 4: benchmark measurement
  - TRUNCATE and re-ingest LongMemEval-S via remember_with_turns
  - run benchmark with mean of 3 full-reset runs
  - compare R@5 to iter 9 baseline (27.5%)
  - target: >50% R@5 (variance budget >2pp)

## Out of Scope

- Consolidation-as-abstraction-change (backlog): sub_mneme embeddings evolving
  with age, representation shift from episodic to semantic. This design keeps
  sub_mnemes static once created.
- Embedding model upgrade to Stella v5 1.5B: captured as future work, not part
  of this iteration.
- ColBERT-style token-level late interaction at query time: too complex for
  the current Rust/pgvector stack.
- Cross-session sub_mneme associations (Hebbian at turn granularity): complexity
  not justified until turn-level retrieval is proven.
