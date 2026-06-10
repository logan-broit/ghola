# Recall pipeline

`core.Recall` (`internal/core/core.go`) fans out across five tiers and
fuses by Reciprocal Rank Fusion (k=60), then optionally reranks via the
truthsayer cross-encoder and blends with score fusion.

## Tiers

| Tier | Source | Returns | Why |
|---|---|---|---|
| `working` | sietch (per-session SQLite) | events from the current session | recent context the session DB hasn't shipped yet |
| `episodic` | chapterhouse `/v1/episodic/query` | events scored by per-event embedding | the dense workhorse for surface-similar passages |
| `keyword` | chapterhouse `/v1/episodic/query_keyword` | events scored by `ts_rank_cd` over tsvector FTS | catches literal phrase / proper-noun / code-identifier hits the dense path smooths over |
| `semantic` | chapterhouse `/v1/semantic/query` | mneme prototypes from mentat's HDBSCAN | clustered patterns across sessions |
| `session_vector` | chapterhouse `/v1/episodic/query_session_vector` | sessions scored by per-session pooled embedding (`l1_embedding`) | the topic signal — catches paraphrase queries where event-level embedding misses but session-level hits |

Every tier is workspace-scoped. `core.Recall` takes a `workspace_id`
on the input; when it is omitted the workspace is derived from `cwd`
(the same mapping `record` uses), so MCP agents can recall with the
directory they already know. Only when both are absent does recall
return a validation error. `session_workspaces` (N:N) determines which
sessions each query sees. See [workspaces.md](workspaces.md).

Tiers fail independently: each gets its own timeout
(`GHOLA_TIER_TIMEOUT_MS`, default 10s) and a failed or timed-out tier
is dropped from the fan-out and reported in the response's `degraded`
field rather than failing the recall; only an all-tiers failure errors.
Events recorded while the embedder was down are FTS-recallable
immediately and join the vector path once the encoding worker's
backfill pass embeds them.

## Fusion

RRF fuses ranked lists with `score = sum(1 / (k + rank))` across tiers.
The `RRF_K` env var (default 60) tunes the curve. See
`internal/core/rrf.go` and `internal/core/rrf_test.go` (the test file
spells out the math line-by-line).

## Rerank

If a truthsayer instance is configured (`TRUTHSAYER_URL`), the top
`RERANK_TOPK` (default 50) fused results go through a cross-encoder
rerank pass, then blend with the fusion score weighted by
`RERANK_WEIGHT` (default 0.5). See `internal/core/core.go:31-50` for
the knobs and the sweep history that produced these defaults.

## Tuning knobs

| Env var | Default | What |
|---|---|---|
| `RRF_K` | 60 | RRF fusion constant |
| `GHOLA_TIER_TIMEOUT_MS` | 10000 | per-tier recall timeout (ms); a tier that exceeds it degrades instead of failing the recall |
| `RERANK_TOPK` | 50 | how many fused results to rerank |
| `RERANK_WEIGHT` | 0.5 | rerank vs fusion score blend |
| `TRUTHSAYER_URL` | (unset) | rerank service URL; rerank skipped if unset |
| `TRUTHSAYER_MODEL` | `BAAI/bge-reranker-v2-m3` | cross-encoder model (truthsayer side) |
