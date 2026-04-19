# Repo layout

This monorepo holds the full Ghola system. Every component ships together
at a single SHA.

## Top-level

- `cmd/ghola/` — local service HTTP/JSON entrypoint. Binds
  `localhost:7421`.
- `cmd/ghola-mcp/` — local service MCP entrypoint. Shares
  `internal/core`.
- `cmd/ch-server/` — Chapterhouse REST server entrypoint. **Not yet
  migrated** — lives at `_chapterhouse/ch-server/cmd/ch-server/` until
  Phase 3.
- `extension/` — `pg_ghola` Rust extension (pgrx). The cognitive
  primitives (ACT-R, Hebbian, Bayesian, contradiction) that run inside
  Postgres. Own `Cargo.toml`, own `Dockerfile.cnpg`.
- `_chapterhouse/` — staging area for the former `chapterhouse` repo.
  Contents migrate into `cmd/ch-server/` + `internal/handler/` +
  `internal/repository/` + `internal/pipeline_b/` incrementally as
  Phases 2, 3, 8 touch each piece. Treat as temporary.

## `internal/` — shared Go libraries

- `internal/core/` — canonical memory operations (`record`, `recall`,
  `branch`, `bookmark`, `share`, `consolidate`, `session_start`,
  `session_end`, `list_sessions`, `feedback`). Single source of truth
  for the ghola local service.
- `internal/sietch/` — per-session SQLite store. Uses `sqlite-vec` and
  FTS5.
- `internal/pipeline_a/` — continuous working → episodic consolidation
  worker. No LLM, watermark-driven, lossless.
- `internal/http/` — thin HTTP router mounted over `core.Core`.
- `internal/mcp/` — MCP tool registration wrapping the same core.
- `internal/chapterhouse/` — HTTP client FOR Chapterhouse's
  `/v1/episodic/*` and `/v1/semantic/*` endpoints. Used by the ghola
  local service.

Future additions (migrated from `_chapterhouse/` during phases 2, 3, 8):

- `internal/handler/` — chapterhouse REST handlers (episodic +
  semantic).
- `internal/repository/` — chapterhouse Postgres layer, migrations.
- `internal/pipeline_b/` — nightly LLM-assisted episodic → semantic
  distillation.
- `internal/types/` — shared DTOs used by both sides of the REST API.

## Other

- `clients/pi-mono-ext/` — TypeScript pi-mono extension (HTTP/JSON
  client against `localhost:7421`).
- `deploy/docker-compose/` — local dev stack (Postgres + Chapterhouse
  + Melange + optional Mentat + Ghola).
- `docs/` — design, simplex spec, implementation plan, architecture
  asset.
- `test/` — cross-binary integration and acceptance-criteria tests.

## Build orchestration

The root `Makefile` delegates per-component. During transition, the
`server` target still `cd`s into `_chapterhouse/ch-server/` because
that's where the Go module lives. Once chapterhouse content migrates
into the root module (end of Phase 3), the target simplifies to
`go build ./cmd/ch-server`.
