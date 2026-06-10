# LongMemEval backend adapter

`ghola_backend.py` is the [LongMemEval](https://github.com/xiaowu0162/LongMemEval)
adapter that routes the bench's per-question retrieval through ghola's
HTTP API (default `http://localhost:7421`). It's the same code that
produced the R@5=99.4% numbers in [`docs/benchmarks.md`](../../docs/benchmarks.md).

The full harness lives upstream — only the adapter is tracked here, so
this repo doesn't carry a fork of someone else's eval code or the
~100MB dataset.

## Setup

```sh
# 1. Clone LongMemEval and install its deps somewhere outside this repo.
git clone https://github.com/xiaowu0162/LongMemEval ~/longmemeval
cd ~/longmemeval
python -m venv .venv && .venv/bin/pip install -r requirements.txt
.venv/bin/pip install httpx          # adapter dep
.venv/bin/pip install sentence-transformers  # only if you'll run the
                                              # local-rerank fallback

# 2. Drop the adapter into the harness's backends/ directory.
cp /path/to/ghola/bench/longmemeval/ghola_backend.py backends/ghola_v2.py

# 3. Fetch the dataset per LongMemEval's README into data/.
```

## Run

ghola must be reachable at `GHOLA_URL` (default `http://localhost:7421`).
Bring it up with `make dev-up` in the ghola repo first.

```sh
cd ~/longmemeval
GHOLA_V2_DELEGATE=1 .venv/bin/python run.py retrieve \
  --backend ghola_v2 --dataset s

# Score the run that retrieve printed:
.venv/bin/python run.py evaluate --run results/ghola_v2_s_<timestamp>.jsonl
```

`GHOLA_V2_DELEGATE=1` is the supported path — ghola owns the whole
recall pipeline (workspace scoping + 5-tier RRF fan-out + rerank).
Without it, the adapter falls back to a local cross-encoder rerank on
top of a flat vector query (legacy comparison path; needs
`sentence-transformers`).

## Env knobs the adapter honors

| Var | Default | What |
|---|---|---|
| `GHOLA_URL` | `http://localhost:7421` | ghola daemon URL |
| `GHOLA_V2_DELEGATE` | `0` | `1` skips the local rerank — trust ghola end-to-end |
| `TEMPORAL_FILTER` | `0` | `1` enables a date-window post-filter (negative-result experiment, off by default) |
| `RERANK_DATASET` | `s` | LongMemEval split label, used when the adapter needs to pull haystack text |
| `INCLUDE_SEMANT` | `1` | `0` drops the semantic mneme tier from Stage 1 |
| `GHOLA_ALLOW_DEGRADED` | _(unset)_ | `1` downgrades a degraded-recall abort to a stderr warning (debug only — see below) |

## Degraded-recall guard

`core.Recall` degrades tier-by-tier: if a stage (e.g. semantic, rerank)
times out or errors, recall drops that stage's contribution and returns
what it has, with the dropped stage names in a `degraded` field on the
response (omitted when recall ran clean). During a scored run a silently
degraded recall lowers R@k with no signal, so the adapter raises a
`RuntimeError` naming the degraded stages and the offending question/query
the moment it sees a non-empty `degraded`. Set `GHOLA_ALLOW_DEGRADED=1` to
downgrade the abort to a single loud stderr warning and continue — debug
escape hatch only; numbers from such a run are not trustworthy.

> After merging changes to `ghola_backend.py`, refresh the deployed copy at
> `~/longmemeval-ghola/backends/ghola_v2.py` from this file — the harness
> imports that copy, not this one.

## QA-accuracy stage (`qa/`)

`qa/` is a self-contained Python package (`lme-qa`) that turns ghola's
retrieve results into an end-to-end QA-accuracy number, using Claude Opus
4.8 as both reader and judge. It is **separate from R@k**: retrieval R@k and
QA accuracy are different metrics and must not be cross-compared.

```sh
# Install (Python >=3.12; needs the anthropic SDK):
pip install -e 'bench/longmemeval/qa[dev]'
export ANTHROPIC_API_KEY=sk-ant-...        # both stages require it
```

Run order — retrieve first (with the degraded guard active), then read,
then judge:

```sh
# 0. retrieve (see above) -> results/ghola_v2_s_<ts>.jsonl

# 1. reader: build context from the top-10 retrieved sessions per question
#    and answer via one Opus 4.8 Batches run.
lme-qa-run \
  --dataset ~/longmemeval-ghola/data/longmemeval_s_cleaned.json \
  --run     ~/longmemeval-ghola/results/ghola_v2_s_<ts>.jsonl \
  --out     answers.jsonl --k 10

# 2. judge: score answers against gold with the upstream LongMemEval judge
#    prompts via a second Opus 4.8 Batches run; writes a per-category report.
lme-qa-judge \
  --dataset ~/longmemeval-ghola/data/longmemeval_s_cleaned.json \
  --answers answers.jsonl \
  --out     judgments.jsonl --report report.md
```

| Knob | Default | What |
|---|---|---|
| `--k` | `10` | top-K retrieved sessions used to build reader context (matches the published R@10) |
| `max_session_chars` | `24000` | per-session character cap in `context.build_context` — generous (p99 of real sessions is ~20.7k chars); only bounds a runaway session |
| reader `max_tokens` | `8000` | adaptive-thinking tokens count toward output, so the reader gets headroom |
| judge `max_tokens` | `2048` | the judge answers "yes"/"no" only |
| `--state` | alongside `--out` | JSON file recording each stage's batch_id + request fingerprint |
| `--fresh` | off | ignore state and submit a new batch |

**Model surface:** `claude-opus-4-8`, adaptive thinking (`{"type":
"adaptive"}`), NO `temperature`/`top_p`/`top_k` (they 400 on Opus 4.8), no
assistant prefills.

**Resume semantics:** each stage persists its batch_id + a fingerprint of
the request set (the sorted custom_ids) to the state file. Re-running an
interrupted job resumes by polling the in-flight batch instead of
resubmitting; a changed question set (fingerprint mismatch) or `--fresh`
forces a new submission. Batch submissions are billed at the 50% batch
price.

**Cost ballpark:** 500 questions, batched Opus 4.8 — roughly **$10-40**
per full run depending on context size (the reader's top-10-session context
dominates input tokens). Two batches per run (reader + judge); the judge is
cheap by comparison.

The judge prompts and the per-question-type + abstention selection logic
are ported verbatim from upstream LongMemEval
[`evaluate_qa.py`](https://github.com/xiaowu0162/LongMemEval/blob/main/src/evaluation/evaluate_qa.py)
(`get_anscheck_prompt`). `_abs` questions are scored with the abstention
prompt and aggregated into their base `question_type` bucket (upstream
behavior); the report adds a supplementary answerable/abstention split.

## Internal vs upstream

`seeding-eval/` at the repo root is the **internal** sweep harness — a
small, fast, ghola-only test that runs tuning passes over RRF_K /
RERANK_TOPK / RERANK_WEIGHT before escalating a change to the full
LongMemEval-S run. Different scope, different cost; use both.
