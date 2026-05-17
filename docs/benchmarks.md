# Benchmarks

Recall pipeline is benchmarked against
[LongMemEval-S](https://github.com/xiaowu0162/LongMemEval).

## Current numbers

| Metric | Value |
|---|---|
| **R@5** | **99.4%** |
| R@1 | 94.0% |
| R@10 | 99.6% |
| MRR | 0.962 |

500 questions, run `2026-05-17` against the stack with
`BAAI/bge-reranker-v2-m3` fp16 on 8k context (the upgrade that landed
in commit `ca2eaec` + `593c293`; replaces the earlier
`bge-reranker-base` baseline of R@5=97.4% / R@1=91.4% / R@10=98.2% /
MRR=0.939). Past the 96.6% [MemPalace](https://github.com/MemPalace/mempalace)
reference. No LLM-as-judge, no leaderboard tricks — workspace scoping
plus the 5-tier RRF fan-out plus cross-encoder rerank
(see [recall-pipeline.md](recall-pipeline.md)).

### Per-category breakdown

| Category | N | R@1 | R@5 | R@10 | MRR |
|---|---|---|---|---|---|
| knowledge-update | 78 | 98.7% | 100.0% | 100.0% | 0.994 |
| multi-session | 133 | 92.5% | 99.2% | 99.2% | 0.952 |
| single-session-assistant | 56 | 100.0% | 100.0% | 100.0% | 1.000 |
| single-session-preference | 30 | 73.3% | 96.7% | 100.0% | 0.830 |
| single-session-user | 70 | 97.1% | 100.0% | 100.0% | 0.982 |
| temporal-reasoning | 133 | 93.2% | 99.2% | 99.2% | 0.958 |

`single-session-preference` remains the stickiest category and is the
open work for the next recall-pipeline iteration.

## Running the harness

The ghola backend adapter lives in this repo at
[`bench/longmemeval/ghola_backend.py`](../bench/longmemeval/ghola_backend.py).
The upstream [LongMemEval](https://github.com/xiaowu0162/LongMemEval)
harness + dataset stay external. Drop the adapter into a LongMemEval
clone's `backends/` dir per
[`bench/longmemeval/README.md`](../bench/longmemeval/README.md), then:

```sh
# from your LongMemEval clone, with ghola running (make dev-up):
GHOLA_V2_DELEGATE=1 .venv/bin/python run.py retrieve \
  --backend ghola_v2 --dataset s
.venv/bin/python run.py evaluate --run results/ghola_v2_s_<ts>.jsonl
```

`GHOLA_V2_DELEGATE=1` routes recall through `core.Recall` end-to-end
(workspace scoping + RRF fusion + rerank); without it the adapter
falls back to a local cross-encoder rerank for comparison.

## Internal seeding-eval

`seeding-eval/` in this repo is a smaller, faster harness used to
sweep recall tuning (RRF_K, RERANK_TOPK, RERANK_WEIGHT) before
escalating a change to the full LongMemEval-S run. See
`seeding-eval/README.md` (if present) or `seeding-eval/pyproject.toml`
for entry points.
