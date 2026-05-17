# encoding-eval -- Phase 2 Tier 1 samsara loop

Dev-time encoding quality evaluation for pg_ghola multi-granularity encoding.
Runs in seconds, no database, no network. Use this to iterate on encoding
strategies (late chunking, pooling, sliding window) before paying the cost
of a Chapterhouse deploy + LongMemEval re-ingest.

See the companion specs in `pg_ghola/docs/plans/`:
- `2026-04-16-multi-granularity-encoding-design.md` -- architectural design
- `2026-04-16-multi-granularity-encoding-implementation.md` -- implementation
  touchpoints across pg_ghola, embed_server.py, ch-server (Go)
- `2026-04-17-encoding-eval-harness-design.md` -- this harness's Simplex spec

## Layout

```
encoding-eval/
    README.md         # this file
    strategies.py     # reference encoder implementations + registry
    eval.py           # the harness (CLI + run_encoding_eval)
    eval_cases.jsonl  # curated cases (grows over time)
```

## Dependencies

Shares the `longmemeval-ghola` venv (sentence-transformers 5.4.0, torch
2.11.0+cu130, transformers 5.5.0). No new packages required.

## Usage

```bash
# Activate a venv with the required deps (sentence-transformers, etc.)
source /path/to/longmemeval/.venv/bin/activate

cd _chapterhouse/encoding-eval

# Run a single strategy
python eval.py --strategy late-chunk-last-token

# A/B compare candidate vs baseline
python eval.py --strategy late-chunk-last-token --compare isolated

# Filter to one category
python eval.py --strategy late-chunk-last-token --category back-reference

# JSON output for scripting / iter ledger
python eval.py --strategy late-chunk-last-token --json
```

Single-strategy run: ~20-30 seconds (includes model load, amortized across
cases). Amortized per-case time after warmup: sub-second.

## Registered strategies

- `late-chunk-last-token` -- full session forward pass, extract last-token
  hidden state per turn span (the candidate). Matches Qwen3-Embedding's
  native last-token pooling, so turn embeddings share the same representation
  space as query embeddings.
- `late-chunk-mean-pool` -- same single forward pass, but mean-pool each turn
  span. Ablation control: verified against query embeddings by the research
  probe at ~0.72 cosine vs native (wrong pooling, expected to underperform).
- `isolated` -- baseline. Encode each turn text in isolation via the model's
  native sentence-level encoding. No session context.
- `sliding-window-last-token` -- long-session variant. Windows of 32K tokens
  with 50% stride; each turn's embedding comes from the window where it is
  most centrally positioned.

Add new strategies via `register_strategy(name, encode_fn, model_factory,
metadata)` in `strategies.py`.

## Eval cases

Cases live in `eval_cases.jsonl`, one JSON object per line. Each case is a
(session, turns, query, target_turn_index) tuple with a category label.
Categories:

- `self-contained` -- target turn is meaningful standalone
- `back-reference` -- target turn references earlier content implicitly
- `forward-reference` -- query matches context established by later turns
- `long-session` -- session exceeds 32K tokens (exercises sliding window)
- `short-session` -- 1 or 2 turns
- `multi-topic` -- session covers distinct topics; query targets one
- `identity-baseline` -- trivial match (canary for broken encoders)

See the harness design doc for the schema and growth policy.

## Iteration contract

Every commit touching `strategies.py` or adding a new strategy runs the full
harness as a pre-commit check. Regression thresholds:
- Top-1 accuracy drop > 5pp on any category -> investigate before merge
- MRR drop > 0.05 overall -> investigate before merge
- New strategies must be A/B compared against `isolated` in the commit message

Numbers from each meaningful iteration go into `pg_ghola/docs/plans/` with
the `YYYY-MM-DD-iter-N-*.md` pattern, alongside whatever Tier 2/3 results
come later.

## Graduation to Tier 2

When a strategy clears Tier 1 convincingly, it gets ported into the
embedding server at `pg_ghola/analysis/embed_server.py` as the `/v1/embeddings/
late-chunk` endpoint implementation. Chapterhouse's new `EmbedLateChunk`
provider method calls that endpoint. Tier 2 iteration then happens against
a local pg_ghola + ch-server + embed_server stack with a LongMemEval-S subset.
