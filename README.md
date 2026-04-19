# Ghola — Tiered agent memory, one monorepo

Ghola is a shared brain for teams of agents, built on neuroscience-grounded
memory tiers (working / episodic / semantic) rather than a single
monolithic store. This repo holds everything needed to build, run, and
deploy the full system.

## Layout

- `cmd/ghola/` — local service, HTTP/JSON server on `localhost:7421`
- `cmd/ghola-mcp/` — local service, MCP wrapper (shares `internal/core`)
- `cmd/ch-server/` — Chapterhouse REST server (to be migrated from
  `_chapterhouse/` in Phase 2)
- `internal/` — shared Go libraries across the service + server
- `extension/` — `pg_ghola` Rust extension (pgrx, pgvector, HNSW) — the
  cognitive primitives that run inside Postgres
- `_chapterhouse/` — staging area for the former chapterhouse repo;
  contents migrate into `cmd/ch-server/` and `internal/` incrementally as
  Phases 2, 3, 8 touch each piece
- `clients/pi-mono-ext/` — TypeScript pi-mono extension
- `deploy/docker-compose/` — local-dev stack
- `docs/` — design, implementation plan, and architecture assets

## Design references

- [`docs/2026-04-19-greenfield-tiered-memory-design.md`](docs/2026-04-19-greenfield-tiered-memory-design.md)
- [`docs/2026-04-19-greenfield-tiered-memory-implementation.md`](docs/2026-04-19-greenfield-tiered-memory-implementation.md)
- [`docs/assets/GholaArchitecture.tsx`](docs/assets/GholaArchitecture.tsx)

## Build

See the root `Makefile`:

```
make extension   # cargo pgrx package for extension/
make server      # go build for cmd/ch-server/
make service     # go build for cmd/ghola/ + cmd/ghola-mcp/
make dev-up      # docker compose up on deploy/docker-compose/
make all         # everything
make test        # run Go + Rust tests
```
