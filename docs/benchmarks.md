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

## Pinned configuration (2026-05-17 run)

The published numbers were produced with exactly this stack; treat any
deviation as a different experiment.

| Knob | Value |
|---|---|
| RRF_K | 60 |
| RERANK_TOPK | 50 |
| RERANK_WEIGHT | 0.5 |
| RERANK_TIMEOUT | 30s |
| TIER_TIMEOUT | 10s |
| Reranker | BAAI/bge-reranker-v2-m3, fp16, max_length 8192 (CUDA) |
| Embedder | Qwen3-Embedding-0.6B, 1024-dim, served by vLLM (fp16) |
| Dataset | LongMemEval-S, 500 questions |
| Harness | GHOLA_V2_DELEGATE=1 (core.Recall end-to-end) |

These are the `core.Config` defaults (`internal/core/core.go`,
`New()`); `RRF_K`, `RERANK_TOPK`, `RERANK_WEIGHT`, and
`GHOLA_TIER_TIMEOUT_MS` are env-overridable (`cmd/ghola/main.go`).
`RERANK_TIMEOUT` is a code default only. Reranker/embedder values are
the committed `deploy/docker-compose/docker-compose.yml` defaults
(`truthsayer` and `guild` services).

Caveats, stated plainly:

- Variance: re-validated 2026-06-10/11 with three independent retrieval
  runs against the same corpus and configuration — every metric
  reproduced identically (R@1 94.0 / R@5 99.4 / R@10 99.6 / MRR 0.962,
  per-category breakdown included). The pipeline is deterministic;
  run-to-run variance is nil. The 2026-06 runs also executed on the
  post-refactor codebase (recall fan-out rewrite, shared embedding
  client, per-tier timeouts), confirming those changes behavior-neutral.
- These are retrieval metrics (R@k over evidence sessions), not
  end-to-end QA accuracy. They are not comparable to QA-accuracy numbers
  other systems report on the same benchmark.
- Serving-stack sensitivity: these numbers were produced with the
  embedder served by vLLM at fp16. Serving the same model through a
  different stack or quantization (e.g. llama.cpp with a Q8_0 GGUF)
  changes the embeddings — re-validate before comparing against these
  numbers.

## QA accuracy (methodology pinned; first run pending)

The numbers above are **retrieval** metrics (R@k over evidence sessions).
End-to-end QA accuracy is a separate, additional metric: it measures
whether a reader, given the retrieved context, produces an answer a judge
scores as correct. The stage is implemented in
[`bench/longmemeval/qa/`](../bench/longmemeval/qa/) (the `lme-qa` package);
the methodology is pinned here so the first run is reported against a fixed
configuration, but no QA-accuracy numbers have been produced yet.

| Knob | Value |
|---|---|
| Reader | `claude-opus-4-8`, adaptive thinking, Batches API |
| Judge | `claude-opus-4-8`, adaptive thinking, Batches API |
| Judge prompts | upstream [LongMemEval](https://github.com/xiaowu0162/LongMemEval) `evaluate_qa.py` (`get_anscheck_prompt`), ported verbatim |
| Judge verdict parsing | leading-token `yes` check — deliberately stricter than upstream's substring rule, since our judge is not `max_tokens`-capped to 10 and may emit visible preamble |
| Reader context | top-10 retrieved sessions per question (matches the published R@10) |
| Abstention (`_abs`) | handled per upstream protocol (abstention judge prompt; bucketed into the base question_type) |
| Dataset | LongMemEval-S, 500 questions |
| Access path | the Batches API **or** Claude Code headless mode (`claude -p`) with MCP/tools/plugins stripped via isolation flags; both pin `claude-opus-4-8`. The report footer records which path produced the number (`via batches` / `via claude-code`). |

Stated plainly:

- QA accuracy depends on the **reader** and the **judge** as much as on
  retrieval. A perfect retriever still loses points if the reader
  misreads the context or the judge scores strictly. Numbers will be
  reported only with all three (reader, judge, retrieval config) pinned.
- Retrieval R@k and QA accuracy are **different metrics and must not be
  cross-compared** — neither against each other nor against another
  system's number measured with a different reader/judge.
- The reader and judge both run as Opus 4.8 (operator decision); a
  different judge model would produce different accuracy on the same
  answers.
- Claude Code headless mode is a **different serving harness** than the
  raw Batches API: the model is the same (`claude-opus-4-8`), but the
  wrapper around it differs. The report footer records the access path
  (`via batches` / `via claude-code`) so any run is auditable, and the
  claude-code path strips MCP servers, tools, and plugins so the reader
  cannot reach context the benchmark withholds.

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
