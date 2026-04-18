# Encoding Eval Harness Design (Tier 1)

Simplex v0.5 specification for the dev-time encoding quality evaluation loop
that drives Phase 2 (Chapterhouse late-chunking) iteration.

Companion to:
- [multi-granularity encoding design](./2026-04-16-multi-granularity-encoding-design.md)
- [multi-granularity encoding implementation](./2026-04-16-multi-granularity-encoding-implementation.md)

## Purpose

Phase 2 work is an encoding-quality problem, not a retrieval-ranking problem.
Iterating on encoding code (pooling strategies, sliding window behavior, turn
boundary alignment) through the full LongMemEval-S benchmark is too slow --
a full re-ingest takes hours. This harness gives seconds-scale feedback on the
question: "does this encoding strategy produce turn embeddings that rank the
target turn highly for a query that should hit it?"

It is the inner loop. Tier 2 (small-dataset DB roundtrip) and Tier 3 (full
LongMemEval-S) are the outer loops. A change that fails Tier 1 should never
reach Tier 2.

## Scope

- Runs in-process in Python, no database
- Takes seconds per full run across ~30-50 curated cases
- Produces numeric scores (top-1 / top-3 / top-5 / MRR) that replace R@5 as
  the iteration signal for encoding-level changes
- Accepts any encoder function conforming to a small interface so alternate
  strategies can be A/B compared in one run
- Cases are hand-curated, grown over time, not auto-generated

## Non-scope

- Does not replace LongMemEval-S for commit-worthy samsara iterations
- Does not exercise the pg_ghola schema, recall function, or cognitive scoring
- Does not measure retrieval latency, only encoding quality
- Does not test Chapterhouse MCP wiring, transaction atomicity, or ingestion
  (those belong to Tier 2)

---

DATA: EncodingEvalCase
  id: string, stable identifier like "kube-oomkill-backref-01"
  category: string, one of: self-contained | back-reference | forward-reference
                           | long-session | short-session | multi-topic
                           | identity-baseline
  session_text: string, the full session text
  turns: list of {role, content, char_start, char_end}
  query: string, the retrieval query
  target_position: integer, 0-indexed turn that should rank highest
  secondary_positions: list of integer, other turns that should rank in top-K
                       (optional; empty if only one turn is relevant)
  notes: string, why this case matters (freeform)

DATA: EncodingEvalResult
  case_id: string
  category: string
  target_position: integer
  ranked_positions: list of integer, turn positions sorted by cosine desc
  target_rank: integer, 1-indexed rank of target_position in ranked_positions
                (0 means target not in results at all -- pathological)
  top1_hit: boolean
  top3_hit: boolean
  top5_hit: boolean
  reciprocal_rank: float, 1.0 / target_rank (0.0 if target_rank == 0)
  cosines_by_position: map of position -> cosine, for diagnostic inspection

DATA: EncoderStrategy
  name: string, short identifier like "late-chunk-last-token"
  encode_fn: callable, (session_text, turns, model) -> list of turn embeddings
                       in the same order as turns
  model: sentence_transformers.SentenceTransformer instance
  metadata: dict, freeform (model name, pooling mode, window size, etc.)

DATA: EncodingEvalSummary
  strategy_name: string
  n_cases: integer
  top1_rate: float, fraction of cases where target_rank == 1
  top3_rate: float
  top5_rate: float
  mrr: float, mean reciprocal rank
  per_category: map of category -> {n, top1, top3, top5, mrr}
  per_case: list of EncodingEvalResult

---

CONSTRAINT: cases_are_curated
  EncodingEvalCases are hand-written, not auto-generated from LongMemEval.
  Each case documents a specific encoding hypothesis in its `notes` field.
  Cases accumulate over time as iteration exposes failure modes; the eval set
  grows but never shrinks (cases may be fixed, not deleted).

CONSTRAINT: cases_cover_failure_modes
  The case set MUST include at least one example from each category:
    - self-contained: target turn is meaningful in isolation
      (expected: isolated and late-chunked both rank it high)
    - back-reference: target turn references earlier turns without naming
      them ("the same error from Tuesday")
      (expected: late-chunked wins meaningfully over isolated encoding)
    - forward-reference: query matches context established by later turns
      (expected: isolated wins or ties, since causal attention doesn't help)
    - long-session: session exceeds model max_seq_length, triggers sliding
      window path (expected: late-chunked produces sensible rankings)
    - short-session: single turn or two turns
      (expected: late-chunked approximates isolated)
    - multi-topic: session covers two distinct topics, query targets one
      (expected: late-chunked correctly ranks the relevant-topic turn
       even when its isolated embedding is similar to the other topic)
    - identity-baseline: same turn text, query that should trivially match
      (expected: target_rank == 1 regardless of strategy; serves as canary
       for broken encoders)

CONSTRAINT: determinism_required
  Given a fixed model version, dtype, device class, and case set, the harness
  MUST produce identical EncodingEvalSummary values across runs. If floating-
  point non-determinism is observed, report it as a bug and pin the source
  (dtype, backend, device).

CONSTRAINT: no_network_or_database
  The harness does not call Chapterhouse, does not insert into Postgres, does
  not query pg_ghola, does not contact vLLM or any HTTP service. It loads the
  embedding model directly via sentence-transformers and operates on Python
  objects.

CONSTRAINT: model_pinning_matches_phase_2
  The model, dtype, and device used here MUST match the pinned configuration
  Chapterhouse uses at ingest and query time
  (Qwen/Qwen3-Embedding-0.6B, bfloat16, CUDA where available). Mismatch makes
  Tier 1 results non-predictive of Tier 2/3 outcomes.

CONSTRAINT: ranking_scope_is_within_session
  Target rank is computed among the session's own turns only, not against
  a corpus. This isolates the encoding-quality signal: "given the session
  this query is about, does the encoder rank the right turn highest?"
  Cross-session retrieval is Tier 2/3's concern.

---

FUNCTION: run_encoding_eval(cases, strategy) -> EncodingEvalSummary

  RULES:
    - for each case in cases:
      - call strategy.encode_fn(case.session_text, case.turns, strategy.model)
        expected return: list of N embeddings (one per turn) each shape (dim,)
        L2-normalized
      - encode case.query via strategy.model (native sentence-level encoding)
        with normalize_embeddings=True
      - compute cosine = dot(turn_embedding, query_embedding) for each turn
      - rank turn positions by cosine descending
      - find target_rank: 1-indexed position of case.target_position in ranks
      - compute top1_hit, top3_hit, top5_hit, reciprocal_rank
      - record EncodingEvalResult
    - aggregate across cases to produce EncodingEvalSummary
      - aggregate per category as well as overall
    - do NOT mutate the strategy, cases, or model state
    - do NOT print to stdout (return the summary; caller decides output)

  DONE_WHEN:
    - len(summary.per_case) == len(cases)
    - every EncodingEvalResult has target_rank in [1, len(case.turns)]
      except pathological strategies that produce NaN/inf cosines
    - summary.top1_rate, top3_rate, top5_rate, mrr computed across all cases
    - per_category breakdown has entries for all categories present in cases

  EXAMPLES:
    -- happy path, good strategy
    cases = load_cases("eval_cases.jsonl")  # 40 cases
    strategy = EncoderStrategy(
        name="late-chunk-last-token",
        encode_fn=late_chunk_encode_last_token,
        model=SentenceTransformer("Qwen/Qwen3-Embedding-0.6B"),
        metadata={"pooling": "last-token", "window": "single-pass"},
    )
    summary = run_encoding_eval(cases, strategy)
    assert summary.top1_rate > 0.6  # target is top-1 in most cases
    assert summary.mrr > 0.75       # when not top-1, usually top-3

    -- A/B comparison in one session
    baseline = EncoderStrategy(name="isolated", encode_fn=encode_each_turn, ...)
    candidate = EncoderStrategy(name="late-chunk", encode_fn=late_chunk_encode, ...)
    baseline_summary = run_encoding_eval(cases, baseline)
    candidate_summary = run_encoding_eval(cases, candidate)
    -- candidate should beat baseline on back-reference category specifically;
    -- ties or small losses elsewhere are acceptable

  ERRORS:
    - strategy.encode_fn returns len != len(case.turns) -> fail with
      "encoder returned N embeddings for M turns" and the case id
    - embedding contains NaN or inf -> fail with "non-finite embedding from
      strategy {name} on case {id} position {p}"
    - model fails to load -> propagate (caller problem)
    - cases file missing -> propagate

  NOT_ALLOWED:
    - silently skip cases that fail
    - auto-generate cases from LongMemEval or any external source
    - mutate the input case list
    - cache results across strategies (each run is fresh)

---

FUNCTION: load_cases(path) -> list of EncodingEvalCase

  RULES:
    - read JSONL file at path (one case per line)
    - validate each case has all required fields (id, category, session_text,
      turns, query, target_position)
    - validate category is one of the allowed values
    - validate target_position is in [0, len(turns))
    - validate each turn's char_start/char_end produces a non-empty span
    - validate concatenating turns in order reconstructs session_text exactly
      (no gaps, no overlaps, no whitespace differences beyond what the turns
       themselves contain)

  DONE_WHEN:
    - returns list of EncodingEvalCase, in file order
    - every case passes validation

  EXAMPLES:
    load_cases("chapterhouse/encoding/eval_cases.jsonl") -> 40 cases

  ERRORS:
    - file does not exist -> FileNotFoundError
    - JSON parse error on any line -> fail with line number and the error
    - missing required field -> fail with case id (or line number) and field name
    - invalid category -> fail with case id and the bad value
    - turn reconstruction mismatch -> fail with case id, show expected vs actual
    - duplicate case ids -> fail with the duplicate id

---

FUNCTION: score_strategy_cli(cases_path, strategy_name) -> exit_code

  RULES:
    - CLI entry point. Argument parsing:
        --cases PATH (default: chapterhouse/encoding/eval_cases.jsonl)
        --strategy NAME (required; must be a registered strategy)
        --compare NAME (optional; second strategy for A/B output)
        --json (flag; emit JSON instead of human-readable table)
        --category NAME (optional; filter to a single category)
    - load cases via load_cases
    - look up strategies by name in a registry (plain dict in the harness
      module for now)
    - run eval, print summary
    - without --compare: print overall + per-category + top-K per-case table
    - with --compare: print side-by-side deltas per category
    - exit 0 on success
    - exit 1 on any handled error (file not found, unknown strategy,
      encoding failure); print a brief error to stderr

  DONE_WHEN:
    - running `python -m chapterhouse.encoding.eval --strategy late-chunk` prints
      a summary and exits 0
    - running with --compare prints a baseline vs candidate delta table
    - running with --json emits parseable JSON

  EXAMPLES:
    $ python -m chapterhouse.encoding.eval --strategy late-chunk
    Strategy: late-chunk-last-token
    Cases: 40
    Top-1: 62.5%  Top-3: 82.5%  Top-5: 95.0%  MRR: 0.742

    Per category:
      back-reference     (8) : top1=50.0%  top3=87.5%  mrr=0.688
      self-contained     (12): top1=75.0%  top3=91.7%  mrr=0.833
      ...

    $ python -m chapterhouse.encoding.eval --strategy late-chunk --compare isolated
    Category            isolated    late-chunk   delta
    back-reference      top1=12.5%  top1=50.0%   +37.5pp
    self-contained      top1=75.0%  top1=75.0%    0.0pp
    ...
    Overall top1        47.5%       62.5%        +15.0pp
    Overall mrr         0.612       0.742        +0.130

  ERRORS:
    - unknown strategy name -> stderr "unknown strategy '{name}'; registered:
      [...]" exit 1
    - cases file issues -> surface the load_cases error, exit 1
    - encoding failure on any case -> print the case id and error, exit 1

---

FUNCTION: register_strategy(name, encode_fn, model_factory, metadata) -> None

  RULES:
    - adds to the module-level strategy registry
    - model_factory is a zero-arg callable returning a SentenceTransformer
      (deferred load so `run_encoding_eval` can load only what's needed)
    - duplicate registration of the same name fails loudly

  DONE_WHEN:
    - strategy is retrievable by name via get_strategy(name)

  EXAMPLES:
    register_strategy(
        name="late-chunk-last-token",
        encode_fn=late_chunk_encode_last_token,
        model_factory=lambda: SentenceTransformer("Qwen/Qwen3-Embedding-0.6B"),
        metadata={"pooling": "last-token", "window_mode": "single-pass-or-slide"},
    )

    register_strategy(
        name="isolated",
        encode_fn=encode_each_turn_in_isolation,
        model_factory=lambda: SentenceTransformer("Qwen/Qwen3-Embedding-0.6B"),
        metadata={"pooling": "native", "context": "none"},
    )

  ERRORS:
    - duplicate name -> fail with "strategy '{name}' already registered"

  NOT_ALLOWED:
    - overwriting an existing registration silently

---

## Seed cases (initial population)

The initial `eval_cases.jsonl` ships with ~40 cases. Each category gets at
least 4 cases. The seed set is built by translating the following prompts
(abbreviated here) into proper EncodingEvalCase records:

**self-contained** (target turn is meaningful standalone)
- "restaurant recommendation in a dinner plans session"
- "K8s pod limit in a debugging session"
- "user preference statement in an onboarding session"
- "direct factual answer in a Q&A exchange"

**back-reference** (target turn references earlier content without naming)
- "that same error from Tuesday" referring to a turn from earlier
- "the chef was incredible" after restaurant naming (our probe case)
- "bump it to 8Gi" after limit discussion (our probe case)
- "book there again for our anniversary" after restaurant praise

**forward-reference** (query matches context established later)
- "what was the cause of the issue" where the cause is revealed later
- "who did you settle on" where the decision appears after initial options

**long-session** (>32K tokens, exercises sliding window)
- synthetic very-long session with target turn near start, middle, end
  (three separate cases)
- real long LongMemEval session sampled and adapted (one case)

**short-session** (1-2 turns total)
- single-turn: query matches the only turn
- two-turn: query matches the second turn's content

**multi-topic** (session covers distinct topics)
- session alternates between K8s debugging and Python library choice;
  query targets K8s
- session covers travel plans and restaurant research; query targets one

**identity-baseline** (trivial match, canary)
- query is a near-exact substring of the target turn
- query is a direct question with the answer as target turn

Case IDs follow a stable scheme: `{category}-{subject}-{nn}` e.g.
`back-reference-kube-oomkill-01`, `self-contained-sushi-01`.

---

## File layout

```
chapterhouse/encoding/
    late_chunk.py        # Phase 2 encoder (specced separately)
    eval.py              # this harness
    eval_cases.jsonl     # curated cases (grows over time)
    strategies.py        # strategy registry + reference implementations:
                         #   late_chunk_encode_last_token
                         #   late_chunk_encode_mean_pool (for comparison)
                         #   encode_each_turn_in_isolation (baseline)
                         #   sliding_window_encode (long-session variant)
```

Tests live alongside:
```
chapterhouse/encoding/tests/
    test_eval.py         # harness unit tests (mocked encode_fn)
    test_cases_valid.py  # validates eval_cases.jsonl passes load_cases
```

## Iteration cadence

- Every commit touching `late_chunk.py` or `strategies.py` runs
  `python -m chapterhouse.encoding.eval --strategy late-chunk` as a pre-commit
  or CI check.
- Regressions (top-1 down >5pp, MRR down >0.05) block the commit pending
  investigation.
- New encoding hypotheses start as a new registered strategy; `--compare`
  against the current production strategy is the first data point.
- When an encoding change lands in Chapterhouse, log the Tier 1 summary
  in the corresponding iter doc alongside any Tier 2/3 results.

## Growth policy

- Cases are added when:
  - A Phase 2 bug is discovered (add a case that reproduces the failure)
  - A new encoding hypothesis needs a discriminating test
  - LongMemEval reveals a systematic encoding failure pattern (translate one
    concrete example into a case)
- Cases are never deleted, only fixed. If a case becomes wrong (e.g., the
  annotated target was mistaken), edit it and record the change in a
  CHANGELOG section of the jsonl file's sibling CHANGELOG.md.
- The `notes` field of each case should explain why it exists, so future
  additions don't duplicate existing coverage.

## Out of scope (explicitly deferred)

- Automated case generation from LongMemEval (might revisit if curation
  becomes a bottleneck)
- Encoding latency measurement (not the quality question)
- Sub_mneme database integration tests (that's Tier 2)
- Cross-session retrieval tests (that's Tier 2/3)
- Cost reporting / compute budget tracking
