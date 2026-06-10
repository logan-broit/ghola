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

## Internal vs upstream

`seeding-eval/` at the repo root is the **internal** sweep harness — a
small, fast, ghola-only test that runs tuning passes over RRF_K /
RERANK_TOPK / RERANK_WEIGHT before escalating a change to the full
LongMemEval-S run. Different scope, different cost; use both.
