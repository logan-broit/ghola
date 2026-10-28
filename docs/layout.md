# Repo layout

This monorepo holds the full Ghola system. Every component ships together
at a single SHA, from one Go module (`go.mod` at the root).

## Top-level

- `cmd/ghola/` — local service HTTP/JSON entrypoint. Binds
  `localhost:7421`. Built with `make service`.
- `cmd/ghola-mcp/` — local service MCP entrypoint. Shares
  `internal/core` with the HTTP server.
- `cmd/import-logs/` — one-shot tool that ingests existing JSONL
  conversation logs (Claude Code, pi-mono) into ghola via the same
  pipeline as live recording.
- `cmd/ch-web/` — chapterhouse admin UI (static files embedded via
  `embed.FS`, proxies `/api` to chapterhouse). Optional compose
  profile `web`.
- `attic/` — retired code kept for context and mining, not built or
  shipped. See `attic/README.md`.
- `attic/extension/` — `ghola` Rust extension (pgrx). Was the original
  home of the cognitive primitives (ACT-R, Ebbinghaus, Hebbian,
  Bayesian, contradiction) running inside Postgres. Production
  primitives now live in `chapterhouse/internal/primitives/`
  and run in the Go server; the Rust crate is retired to `attic/` as an
  algorithm reference (not built or wired into any Makefile target, the
  dev stack, or CI). Own `Cargo.toml`, own `Dockerfile.cnpg`. Cargo
  `name = "ghola"` — the `pg_` prefix is reserved by Postgres and was
  dropped.
- `chapterhouse/` — the recall backend. Part of the root Go module;
  its binaries stay under `chapterhouse/cmd/` so `chapterhouse/internal`
  remains private to chapterhouse (Go's internal-package rule), which
  keeps it easy to split out later.
- `mentat/` — Python FastAPI sidecar. HDBSCAN clustering over per-session
  L1 embeddings; chapterhouse calls it on the cluster cron.
- `truthsayer/` — Python sidecar. Cross-encoder reranker
  (`BAAI/bge-reranker-v2-m3` by default; fp16 on CUDA; 8k context).
  Called by ghola during stage-2 rerank.
- `scripts/ghola-capture-hook.sh` — Claude Code UserPromptSubmit + Stop
  hook; POSTs user/assistant turns to the daemon `/v1/record`
  (fire-and-forget). Fixture test alongside it.

## `internal/` — shared Go libraries

- `internal/core/` — canonical memory operations (`record`, `recall`,
  `branch`, `bookmark`, `navigate`, `share`, `consolidate`,
  `session_start`, `session_end`, `list_sessions`, `forget`,
  `expand_session_workspace`). Single source of truth for the ghola
  local service.
- `internal/sietch/` — per-session SQLite store. FTS5 + native float32
  embedding BLOBs (no sqlite-vec dep yet).
- `internal/encoding/` — continuous working → episodic consolidation
  worker. Watermark-driven, lossless, off the hot path.
- `internal/http/` — thin HTTP router mounted over `core.Core`.
- `internal/mcp/` — MCP tool registration wrapping the same core.
- `internal/chapterhouse/` — HTTP client FOR chapterhouse's
  `/v1/episodic/*` and `/v1/semantic/*` endpoints. Used by `core.Recall`.
- `internal/truthsayer/` — HTTP client for the truthsayer reranker
  service.
- `internal/importlogs/` — JSONL conversation log adapters used by
  `cmd/import-logs/`.

## `pkg/`

- `pkg/embedding/` — OpenAI-compatible embedding client (talks to
  guild). Shared by ghola, `cmd/import-logs` and chapterhouse.

## `chapterhouse/` — Go server

- `cmd/ch-api/`, `cmd/ch-init/`, `cmd/ch-worker/` — HTTP API,
  one-shot migrator, co-activation/consolidation worker.
- `db/migrations/` + `db/queries/` — legacy admin schema (users, API
  keys, admin sessions, audit log) and its sqlc inputs. Not applied by
  `ch-init`; see `chapterhouse/RUNBOOK.md`.

- `internal/handler/` — REST handlers (episodic + semantic).
- `internal/repository/` — Postgres layer (pgx), migrations under
  `internal/repository/migrations/`.
- `internal/semantic/` — mneme persistence + reconciler (computes
  l1_embedding / l1_chunk_text on closed sessions).
- `internal/mneme/` — legacy store over the retired extension's
  `ghola.*` schema. Not imported by any binary.
- `internal/primitives/` — cognitive primitives (ACT-R activation,
  Ebbinghaus decay, Hebbian co-activation, Bayesian confidence,
  contradiction). Migrated out of the retired `attic/extension/` Rust
  crate; runs in-process.
- `internal/consolidation/` — two jobs sharing a package: (1) drains
  the Hebbian co-activation queue and strengthens
  `semantic.associations` (`DrainAndStrengthen`, run on `cmd/ch-worker`'s
  tick loop); (2) the episodic→semantic nightly pipeline —
  reconcile (pool closed sessions to L1) → cluster via mentat →
  overlap-match/apply mnemes → selection-first enrichment → optional
  LLM label + workspace digest (`RunWorkspace`), gated by a
  per-workspace advisory lock and run both by `cmd/ch-worker`'s nightly
  schedule and the manual `POST /v1/semantic/consolidate` trigger. See
  [consolidation.md](consolidation.md).
- `internal/embedding/` — embedding provider interface over
  `pkg/embedding`.
- `internal/mentat/` — HTTP client for the mentat sidecar.
- `internal/auth/` + `internal/secrets/` + `internal/middleware/` —
  Bearer-token auth + request middleware.
- `internal/config/` — env config loader.
- `internal/testutil/` — test helpers.
- `internal/backfill/` + `cmd/ch-backfill-sessions/` — one-time,
  operator-run tool that segments an over-long episodic session into
  4h-gap episodes (dry-run by default, `--execute` to apply).

The older standalone MCP stdio server (`cmd/mcp-server/`) and the
frozen `internal/mcp_legacy/` package were removed pre-v0.1.0; new
MCP tools surface through ghola, not chapterhouse. Recover from git
history if you need the old code.

## Other

- `deploy/docker-compose/` — local dev stack: postgres + ch-init +
  chapterhouse + chapterhouse-worker + mentat + truthsayer + guild (or
  guild-stub), plus ghola (profile `all`) and ch-web (profile `web`).
  `docker-compose.mac.yml` is the Apple Silicon variant
  ([deploy-mac.md](deploy-mac.md)); `docker-compose.smoke.yml` is the
  `make smoke-predictive` overlay.
  Two images: `Dockerfile.golang` (`ghola/ghola:dev`, every Go binary;
  compose picks one per service via `entrypoint:`) and `Dockerfile.python`
  (`ghola/py:dev`, mentat + truthsayer; picked via `command:`).
- `deploy/cnpg-pg18/` — legacy CNPG PostgreSQL 18 + pg_recall image
  build, kept for reference; not used by the compose stack or by any
  current code path. Homelab cluster manifests live outside this repo
  (`homelab-k3s`).
- `docs/` — durable references only: this layout map, the chapterhouse
  OpenAPI spec, and any subproject status writeups that survive a
  cleanup pass. Process-artifact subtrees — `docs/plans/`,
  `docs/status/`, `docs/issues/`, `docs/archive/`, `docs/notes/`,
  `docs/specs/` — are gitignored anywhere in the tree (commit messages
  and merged code carry the durable record). `*.simplex` is ignored
  globally under the same rule, as are tool-session dirs `.claude/`,
  `.samsara/`, `.plex/`.
- `docs/api/v1-chapterhouse.yaml` — OpenAPI 3.1 spec for the internal
  chapterhouse HTTP surface (`/v1/episodic/*`, `/v1/semantic/*`).
  Tracked because it's documentation, not a plan; clients of
  chapterhouse are supposed to be able to read it. Keep in sync with
  `chapterhouse/internal/handler/`.
- `test/e2e/` — cross-binary end-to-end tests (`-tags=e2e`, need a
  running ghola at `GHOLA_E2E_URL`).
- `bench/`, `seeding-eval/` — LongMemEval-S adapter + QA stage and the
  smaller recall-tuning harness. See [benchmarks.md](benchmarks.md).

## Build orchestration

The root `Makefile` delegates per-component:

```
make server      # go build ./chapterhouse/...
make service     # go build for cmd/ghola/ + cmd/ghola-mcp/
make dev-up      # docker compose up on deploy/docker-compose/
make all         # everything
make test        # run Go tests
make smoke-predictive  # isolated smoke-test stack on alternate ports
```

The retired Rust crate under `attic/extension/` is not in the build
graph; build it by hand with `cargo` if you need to mine the algorithms.
