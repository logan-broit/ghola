# Chapterhouse

MCP memory server for AI coding agents. Provides persistent, searchable memory across sessions via the [Model Context Protocol](https://modelcontextprotocol.io/).

Memory storage and retrieval is handled by [pg_recall](https://github.com/thinkwright/pg_recall), a PostgreSQL extension that combines content, vector embeddings, full-text search, and cognitive scoring (Hebbian reinforcement, temporal decay, supersession, contradiction detection) in a single table.

## Architecture

```
AI Agent (Claude Code, Codex, etc.)
    │
    │  MCP over Streamable HTTP
    ▼
┌──────────────┐     ┌───────────────────┐     ┌──────────────┐
│  ch-server   │────▶│  PostgreSQL 18    │     │  Embedding   │
│  (Go API)    │     │  pgvector         │     │  Provider    │
│              │     │  pg_recall        │     │              │
└──────────────┘     └───────────────────┘     └──────────────┘
    ▲
    │  HTTP proxy
┌──────────────┐
│   ch-web     │
│ (admin UI)   │
└──────────────┘
```

- **ch-server** — Go 1.24. API + MCP server. Stateful (`/mcp`) and stateless (`/mcp/stateless`) endpoints. Auth via API keys (`ch_k1_` prefix). Audit logging.
- **ch-web** — Admin console. User management, API keys, audit logs, health. Vanilla HTML/CSS/JS embedded in a Go binary via `embed.FS`.
- **pg_recall** — PostgreSQL extension. Typed mnemes, vector search, FTS, Hebbian scoring, temporal decay, supersession, contradiction detection.

## MCP Tools

| Tool | Description |
|------|-------------|
| `remember` | Store a memory with type, tags, scope, and session ID |
| `recall` | Search via semantic, keyword, or hybrid scoring |
| `forget` | Delete a memory by ID |
| `list_memories` | List with filters (type, tags, session) |
| `share_memory` | Change scope between personal and org |
| `export_memories` | Export to JSONL |
| `list_sessions` | Session aggregations with memory counts |
| `session_summary` | Breakdown of a specific session |
| `session_context` | Load all memories from a session |

### Memory types

- **factual** — standards, conventions, configurations. Persists indefinitely.
- **experiential** — lessons learned, debugging solutions. Weighted by use.
- **working** — session-scoped notes. Auto-expires after 7 days.

### Recall modes

- **hybrid** (default) — blends semantic similarity, keyword match, temporal recency, Hebbian activation, and confidence
- **semantic** — vector cosine similarity only
- **keyword** — PostgreSQL full-text search only

## Quick Start

### Prerequisites

- Go 1.24+
- PostgreSQL 18 with [pgvector](https://github.com/pgvector/pgvector) and [pg_recall](https://github.com/thinkwright/pg_recall)
- An OpenAI-compatible embedding API

### Build

```bash
cd ch-server && go build -o bin/ch-server ./cmd/api
cd ch-web && go build -o bin/ch-web ./cmd/server
```

### Run locally

```bash
DATABASE_PASSWORD=secret ./bin/ch-server
API_URL=http://localhost:8080 ./bin/ch-web
```

### Connect Claude Code

```bash
claude mcp add -s user -t http chapterhouse \
  http://localhost:8080/mcp \
  --header "Authorization: Bearer ch_k1_<your-api-key>"
```

`/mcp` supports session lifecycle tools. `/mcp/stateless` authenticates per-request without server-side session state.

### Deploy with Helm

```bash
helm upgrade --install ch-server ch-server/charts/ch-server \
  -n ch-system -f your-values.yaml

helm upgrade --install ch-web ch-web/charts/ch-web \
  -n ch-system -f your-values.yaml
```

## Configuration

### ch-server

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | Server port |
| `ENVIRONMENT` | `production` | `local` or `development` disables secure cookies |
| `DATABASE_HOST` | `localhost` | PostgreSQL host |
| `DATABASE_PORT` | `5432` | PostgreSQL port |
| `DATABASE_NAME` | `memories` | Database name |
| `DATABASE_USER` | `memory_api` | Database user |
| `DATABASE_PASSWORD` | — | Required |
| `EMBEDDING_PROVIDER` | `openai` | Any OpenAI-compatible API |
| `EMBEDDING_URL` | `https://api.openai.com` | Embedding endpoint |
| `EMBEDDING_MODEL` | `text-embedding-3-small` | Model name |
| `EMBEDDING_DIMENSIONS` | `768` | Vector dimensions |
| `EMBEDDING_API_KEY` | — | Provider API key |

### ch-web

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | Server port |
| `API_URL` | `http://ch-server:8080` | ch-server URL |

## Embeddings

Any OpenAI-compatible `/v1/embeddings` endpoint works. Tested providers:

- [HuggingFace TEI](https://github.com/huggingface/text-embeddings-inference) — self-hosted, CPU or GPU
- [OpenAI](https://openai.com/) — `text-embedding-3-small`, `text-embedding-3-large`
- [Ollama](https://ollama.com/) — `nomic-embed-text`, `bge-base-en-v1.5`
- [vLLM](https://docs.vllm.ai/) — any HuggingFace embedding model

Embedding dimensions must match between `EMBEDDING_DIMENSIONS` and pg_recall:

```sql
SELECT pg_recall.configure_dimensions(768);
```

Changing dimensions requires an empty `pg_recall.mnemes` table.

## Repository Structure

```
chapterhouse/
├── ch-server/
│   ├── cmd/api/              # Server entrypoint
│   ├── cmd/init/             # Init container (pg_recall verification)
│   ├── cmd/mcp-server/       # Standalone MCP server (stdio transport)
│   ├── internal/
│   │   ├── auth/             # API key, session, JWT auth
│   │   ├── mcp/              # MCP protocol + tool handlers
│   │   ├── mneme/            # pg_recall storage layer
│   │   ├── handler/          # Admin HTTP handlers
│   │   ├── embedding/        # Embedding client
│   │   └── config/           # Env-based configuration
│   ├── db/migrations/        # SQL migrations (8 files)
│   ├── charts/ch-server/     # Helm chart
│   └── Dockerfile
├── ch-web/
│   ├── cmd/server/
│   │   ├── main.go           # HTTP server (embed.FS + API proxy)
│   │   └── static/           # HTML, CSS, JS
│   ├── charts/ch-web/        # Helm chart
│   └── Dockerfile
├── deploy/
│   ├── examples/             # Example CNPG manifest
│   └── homelab/              # Homelab-specific config
├── Makefile
├── RUNBOOK.md
└── BUILD_AND_RELEASE.md
```

## Docs

- [RUNBOOK.md](RUNBOOK.md) — Infrastructure setup, migrations, secrets, Helm deployment, troubleshooting
- [BUILD_AND_RELEASE.md](BUILD_AND_RELEASE.md) — Building images, Helm charts, Makefile targets
- [ch-server/MEMORY_SYSTEM_GUIDE.md](ch-server/MEMORY_SYSTEM_GUIDE.md) — Memory system design and behavior
