# Chapterhouse

The recall backend behind ghola. Stores the shared episodic tier
(sessions, events, shares) and the semantic tier (mnemes, associations)
in Postgres + pgvector, and serves them over an internal HTTP/JSON API.

Agents don't talk to chapterhouse directly. They talk to the ghola
daemon (`cmd/ghola`, or `cmd/ghola-mcp` for MCP clients), which calls
chapterhouse through `internal/chapterhouse/client.go`. The old
chapterhouse MCP endpoints (`/mcp`, `/mcp/stateless`) were removed in
v2; see the top-level [README](../README.md#connect) for agent setup.

## Architecture

```
agent ──MCP/HTTP──▶ ghola daemon ──HTTP──▶ ch-api ──▶ Postgres + pgvector ◀── ch-worker
                                             │                                 │
                                             └──▶ guild (embeddings) ◀─────────┤
                                             └──▶ mentat (pool, cluster) ◀─────┘
ch-web (admin UI) ──/api proxy──▶ ch-api
```

| Binary | Path | Role |
|---|---|---|
| `ch-api` | `chapterhouse/cmd/ch-api` | HTTP API on `:8080` |
| `ch-init` | `chapterhouse/cmd/ch-init` | One-shot: applies embedded migrations, then exits |
| `ch-worker` | `chapterhouse/cmd/ch-worker` | Drains the co-activation queue every `WORKER_TICK_SECONDS`; runs nightly consolidation when `CONSOLIDATE_WORKSPACES` and `MENTAT_URL` are set |
| `ch-backfill-sessions` | `chapterhouse/cmd/ch-backfill-sessions` | Operator tool: splits an over-long session into idle-gap segments |
| `ch-web` | `cmd/ch-web` | Admin console (embedded static files + `/api` proxy) |

## HTTP surface

| Route | Auth | Purpose |
|---|---|---|
| `GET /health`, `GET /ready` | none | Liveness / DB readiness |
| `POST /v1/episodic/{ingest,query,share,forget}` | API key (none if `AUTH_DISABLED`) | Episodic tier, called by ghola |
| `POST /v1/semantic/query` | same | Semantic recall |
| `POST /v1/semantic/consolidate` | same | Manual episodic→semantic batch (needs `MENTAT_URL`) |
| `/api/v1/user/*`, `/api/v1/admin/*` | session cookie (+ admin role) | ch-web: users, API keys, audit log |

The `/v1` contract is specified in
[`docs/api/v1-chapterhouse.yaml`](../docs/api/v1-chapterhouse.yaml).
Consolidation is described in
[`docs/consolidation.md`](../docs/consolidation.md).

## Build and run

One Go module at the repo root:

```bash
go build ./chapterhouse/...        # or: make server
go build -o bin/ch-api ./chapterhouse/cmd/ch-api
```

The normal way to run it is the compose stack, which builds
`ghola/ghola:dev` from `deploy/docker-compose/Dockerfile.golang` and
runs `ch-init`, `chapterhouse` (ch-api) and `chapterhouse-worker` from
it:

```bash
make dev-up
```

See [RUNBOOK.md](RUNBOOK.md) for operations.

## Configuration (ch-api)

| Variable | Default | Description |
|---|---|---|
| `SERVER_HOST` / `SERVER_PORT` | `0.0.0.0` / `8080` | Listen address |
| `ENVIRONMENT` | `local` | `local`/`development` disable secure cookies; `production` refuses `AUTH_DISABLED=true` |
| `DATABASE_HOST`, `_PORT`, `_NAME`, `_USER`, `_PASSWORD`, `_SSL_MODE` | `localhost`, `5432`, `memories`, `memory_api`, empty, `prefer` | Postgres |
| `AUTH_DISABLED` | `false` | Local dev: skip API keys on `/v1`; every request acts as `AUTH_DEFAULT_USER` |
| `AUTH_DEFAULT_USER` | `00000000-0000-0000-0000-000000000000` | User when `AUTH_DISABLED=true` |
| `EMBEDDING_URL`, `EMBEDDING_MODEL`, `EMBEDDING_API_KEY` | `https://api.openai.com`, `text-embedding-3-small`, empty | OpenAI-compatible embedder (ingest backstop, digests) |
| `EMBEDDING_DIMENSIONS` | `768` | Must match `EMBEDDING_DIM` used by `ch-init` |
| `MENTAT_URL` | empty | Enables semantic recall, the L1 reconciler and manual consolidation |
| `CONSOLIDATE_LLM_URL`, `_MODEL`, `_API_KEY` | empty, `local-model`, empty | Optional LLM labels/digests during consolidation |
| `CORS_ORIGINS` | empty | Comma-separated allowlist |

`ch-init` reads the same `DATABASE_*` variables plus `EMBEDDING_DIM`
(required; substituted into the vector column width). `ch-worker`
reads `DATABASE_URL`. ch-web reads `PORT` (`8080`) and `API_URL`
(`http://ch-server:8080`; compose sets `http://chapterhouse:8080`).

## Layout

```
chapterhouse/
├── cmd/{ch-api,ch-init,ch-worker,ch-backfill-sessions}/
├── internal/
│   ├── handler/              # REST handlers
│   ├── repository/           # pgx store; migrations/ embedded, applied by ch-init
│   ├── semantic/             # mneme persistence + L1 reconciler
│   ├── consolidation/        # co-activation drain + nightly pipeline
│   ├── primitives/           # ACT-R, decay, Hebbian, Bayesian, contradiction
│   ├── auth/, middleware/    # API key, session auth
│   └── config/               # env config
├── db/migrations/            # admin schema + sqlc input (applied by ch-init)
├── encoding-eval/            # encoding strategy eval harness
├── RUNBOOK.md
└── SECURITY_STATEMENT.md
```
