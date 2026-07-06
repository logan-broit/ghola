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
| `BENCH_SETTLE` | _(unset)_ | `expand` or `channel` turns on P4 spreading activation (settle); unset is byte-identical to the pre-P4 request |
| `BENCH_ACTIVATION_WEIGHT` | _(unset)_ | channel-mode fusion weight, e.g. `0.40`; unset uses the server default. Only read when `BENCH_SETTLE` is set |
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
# Optional: add the [rate] extra (tiktoken) for a real cl100k rate-axis count in
# the rate-distortion sweep; without it the rate falls back to a char-ratio
# estimate. e.g. pip install -e 'bench/longmemeval/qa[dev,rate]'
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
| `--state` | alongside `--out` | JSON file recording each stage's batch_id + request fingerprint (batches backend only) |
| `--fresh` | off | ignore state and submit a new batch (batches backend only) |
| `--adopt <batch_id>` | off | take an existing batch_id verbatim instead of submitting — recovers an orphaned paid batch the auto-adoption missed (batches backend only) |
| `--backend` | `batches` | execution backend: `batches` (needs an API key) or `claude-code` (subscription, no key — see below) |
| `--parallel` | `2` | claude-code backend: concurrent `claude -p` calls (keep low) |
| `--timeout-s` | `300` | claude-code backend: per-call wall-clock timeout |

**Model surface:** `claude-opus-4-8`, adaptive thinking (`{"type":
"adaptive"}`), NO `temperature`/`top_p`/`top_k` (they 400 on Opus 4.8), no
assistant prefills.

**Resume semantics:** each stage persists its batch_id + a fingerprint of
the request set (custom_ids plus the full request params, so a changed K /
prompt / context forces a fresh submission — resume means identical work).
Re-running an interrupted job resumes by polling the in-flight batch
instead of resubmitting. A crash mid-submit leaves a pending marker; the
next run tries to adopt the orphaned batch (loud warning), and `--adopt`
is the manual override. `--fresh` always forces a new submission. Batch
submissions are billed at the 50% batch price.

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

### Running without an API key (Claude Code backend)

No `ANTHROPIC_API_KEY`? Run both stages through Claude Code's headless mode
against a subscription instead of the Batches API. Pass
`--backend claude-code` — the same two commands, no key needed:

```sh
# reader (no ANTHROPIC_API_KEY required):
lme-qa-run \
  --dataset ~/longmemeval-ghola/data/longmemeval_s_cleaned.json \
  --run     ~/longmemeval-ghola/results/ghola_v2_s_<ts>.jsonl \
  --out     answers.jsonl --k 10 \
  --backend claude-code --parallel 2

# judge:
lme-qa-judge \
  --dataset ~/longmemeval-ghola/data/longmemeval_s_cleaned.json \
  --answers answers.jsonl \
  --out     judgments.jsonl --report report.md \
  --backend claude-code --parallel 2
```

The backend drives one `claude -p ... --output-format json` subprocess per
question (model still pinned to `claude-opus-4-8`). `LME_QA_CLAUDE_BIN`
overrides the binary path (default: `claude` on `PATH`).

**Subscription usage windows are the real constraint.** The 500-question
reader pass is *millions* of input tokens (each question carries a
top-10-session context), so a full run will span multiple subscription usage
windows. That is expected, not an error: when a window is exhausted the run
stops submitting, finishes in-flight calls, saves progress, and tells you to
re-run after the window resets. Resume is **per-question** — each completed
answer/judgment row is appended (and fsync'd) to `--out` the moment it lands
(durable before the stage finishes, so a hard crash mid-stage loses nothing),
and a re-run skips question_ids already recorded as succeeded (prior failures
are retried). The file is **append-only** — never rewritten — so a question
that errored then succeeded on a later run leaves *both* rows on disk; that is
harmless because every reader dedups **last-wins by question_id** (the stale
errored row is superseded by the later succeeded one, and status reflects the
final outcome). So just re-run the same command after the window resets; it
picks up where it left off. **Run it in tmux**
(`tmux new-session -d -s lme-qa ...`) so a dropped terminal doesn't kill a
multi-hour, multi-window job.

**Keep `--parallel` low (2).** The bottleneck is the usage window, not local
concurrency — flooding it with parallel calls just exhausts the window faster
with no throughput gain (and risks tripping rate limits sooner).

**Isolation flags (why MCP / tools / plugins are stripped).** A bare
`claude -p` session loads the operator's MCP servers (e.g. ghola) and plugins,
which would let the reader reach memory and tools the benchmark must *not*
give it — silently contaminating the result. Every call therefore passes
`--tools ""`, `--strict-mcp-config`, `--no-session-persistence`, and a full
`--system-prompt` replacement, and the runner asserts isolation from the CLI's
init event: it warns loudly if a *connected* MCP server or a *fireable* tool
shows up. (One benign exception: claude 2.1.170 injects an `LSP` tool that
`--tools ""` does not strip; it cannot fire on a non-interactive `-p` prompt,
so it is allowed and does not trip the warning.) If you see an isolation
warning, the number from that run is not trustworthy — fix the leak and re-run.

`--state` / `--fresh` / `--adopt` are **batches-only** (they manage the
Batches API's batch lifecycle); the claude-code backend resumes from the
`--out` file itself, so those flags are ignored on that path.

## Rate-distortion sweep (P5 instrument)

The reader's accuracy depends on how much context it gets. The
rate-distortion sweep traces that tradeoff: it varies the distilled-context
**token budget** and plots the resulting *rate* (the emitted-context token
count) against QA accuracy (the *distortion*, reported as the wrong-fraction
`1 - accuracy`) to map the frontier. A **compressor**
sits between session selection (`context.select_sessions`) and the reader
prompt: it transforms the selected sessions down to a budget before the prompt
is built. Four no-new-model baselines ship:

| Compressor | Granularity | What it does |
|---|---|---|
| `full` | — | render every selected session; ignores the budget. The right edge of the curve (current production behavior, byte-identical to the pre-P5 path). |
| `truncate_tokens` | byte | render all sessions, then hard-cut the joined text at the budget. Relevance-blind strawman — cuts mid-session. |
| `topk_sessions` | session | keep whole sessions in chronological order until the next would exceed the budget; never splits a session. |
| `extractive_relevance` | turn | score each turn against the query (truthsayer `/v1/rerank` reranker, already up at `:8085`), greedily keep the most relevant turns within the budget, regroup under their sessions in chronological order. Per-turn, relevance-aware. |

The reader takes `--compressor` (default `full`), `--budget` (approximate target
tokens; `extractive_relevance` also honors `--scorer`, `truthsayer`|`guild`),
and `--rate-tokenizer` (`auto`|`tiktoken`|`char`, default `auto`). The plotted
*rate* axis is the **emitted-context token count** — the memory payload the
reader actually emits, measured at build time by the rate tokenizer (tiktoken
cl100k when the optional `[rate]` extra is installed, else the dependency-free
char-ratio fallback). The SAME tokenizer both budgets the compressor and
measures the rate, so `--budget 1000` and the recorded rate share a unit. The
operating point you want is the **knee of the best curve**: the fewest tokens at
which Claude still answers correctly.

> **Why the rate is NOT `usage.input_tokens`.** An earlier version of this
> instrument used the reader's `usage.input_tokens` as the rate axis. On the
> `claude-code` backend that field is Claude Code's **fixed harness overhead**
> (~3279 tokens — verified identical for a one-word prompt and a 22k-token
> context); the real payload lands in `cache_creation_input_tokens` /
> `cache_read_input_tokens`, split unpredictably by prompt caching. So every
> sweep setting reported the same ~3279 rate and the frontier was flat-on-the-x
> garbage. The fix records `context_tokens` (the emitted-context token count) per
> reader row and aggregates on that. Do not re-introduce a `usage.input_tokens`
> rate axis on the claude-code backend.

### Sweep → aggregate flow

```sh
# settings.json: the (compressor, budget) grid to sweep.
cat > settings.json <<'JSON'
[
  {"compressor": "full",                 "budget": null},
  {"compressor": "truncate_tokens",      "budget": 4000},
  {"compressor": "topk_sessions",        "budget": 4000},
  {"compressor": "extractive_relevance", "budget": 4000},
  {"compressor": "truncate_tokens",      "budget": 1000},
  {"compressor": "extractive_relevance", "budget": 1000}
]
JSON

# 1. sweep: run the reader+judge once per (compressor, budget, sample) leaf.
lme-qa-sweep \
  --dataset ~/longmemeval-ghola/data/longmemeval_s_cleaned.json \
  --run     ~/longmemeval-ghola/results/ghola_v2_s_<ts>.jsonl \
  --outdir  sweep/ --settings settings.json \
  --samples 1 --backend claude-code --parallel 2

# 2. aggregate: join rate (answers) to distortion (judgments) -> the frontier.
lme-qa-rd --outdir sweep/
# -> sweep/rd-curve.jsonl, sweep/rd-curve.md, sweep/rd-curve.png (PNG best-effort)
```

**Leaf layout + resume.** Each `(compressor, budget, sample)` is a *leaf*:
`sweep/<compressor>__b<budget>/s<i>/{answers,judgments}.jsonl`. A leaf is a
normal `lme-qa-run` + `lme-qa-judge` invocation pointed at the leaf's own
`--out`/`--state`, so the **per-question** resume (append-only, last-wins,
usage-window-aware) operates unchanged within each leaf. The sweep adds only
**leaf-level** resume on top: a leaf whose `judgments.jsonl` already covers
every question (last-wins status==succeeded for each) is skipped wholesale on a
re-run — a second invocation over a finished sweep runs **zero** `claude`
calls. The sweep is **window-aware** the same way the reader/judge are: a stage
that runs without bringing its leaf to completion means the subscription usage
window is exhausted (or the failures are systemic) — the sweep stops, reports
which leaves remain, and a re-run after the window resets picks up from the
first unfinished leaf. Run it in tmux (`tmux new-session -d -s lme-qa-sweep
...`) for the same multi-window reasons as a plain QA run.

**Aggregation.** `lme-qa-rd` walks the leaf tree and, per setting, joins each
question's rate (`answers.context_tokens` — the emitted-context token count the
reader measured) to its verdict (`judgments.label`), dedups last-wins per qid,
averages samples per question first, then averages questions to the setting's
`mean_rate` + `accuracy` (`distortion == 1 - accuracy`). A pre-fix run that has
no `context_tokens` falls back to `usage.input_tokens` with a stderr warning (see
the harness-overhead note above); the `rate_tokenizer` name is carried onto the
curve rows + the `rd-curve.md` header so the unit is on the artifact. It emits `rd-curve.jsonl` (one row per setting),
`rd-curve.md` (a table sorted by mean rate, calling out the
truncate-vs-extractive accuracy gap at the nearest shared budget — the core
question: does per-turn relevance selection beat a blind byte cut at the same
budget?), and a best-effort `rd-curve.png`. The PNG needs the optional `[plot]`
extra (`pip install 'bench/longmemeval/qa[plot]'`); without matplotlib the table
still ships and the PNG is skipped with a one-line stderr note.

**`--samples` and self-consistency.** `--samples` runs each setting more than
once (each sample is its own leaf dir) and averages — but it is only meaningful
if `claude -p` answers are *stochastic*. Run a small pilot before trusting it:
5 questions at `--samples 3`, then inspect whether the three answers per
question differ. If they are identical, keep **`--samples 1`** and rely on
eval-set size (the full LongMemEval-S set) for the curve's resolution rather
than re-sampling the same deterministic answer. Document the observed variance
when you publish a curve. The default is `--samples 1`.

> **Self-consistency pilot — observed:** the prior QA-accuracy runs were
> reproducible across three identical retrieval runs (see the variance caveat
> retired in `docs/benchmarks.md`), so the reader path showed no run-to-run
> variance worth re-sampling. Default `--samples 1` accordingly; re-run the
> 5-question pilot if the model/serving path changes and re-confirm before
> raising it.

**Stage B (the follow-on).** These four compressors are the *baseline*
frontier — the no-new-model bar that the next stage exists to beat. Stage B
adds learned/model-driven compressors (`perplexity_prune`, `llm_distill`); the
point of committing this baseline curve first is to have a frontier to measure
that improvement against.

> **Not built here:** the actual measurement run over a real retrieve run (and
> the committed curve) is operator-run — it needs the live stack + the
> subscription window and is executed window-aware like the QA runs, not part
> of this instrument's code.

## Internal vs upstream

`seeding-eval/` at the repo root is the **internal** sweep harness — a
small, fast, ghola-only test that runs tuning passes over RRF_K /
RERANK_TOPK / RERANK_WEIGHT before escalating a change to the full
LongMemEval-S run. Different scope, different cost; use both.
