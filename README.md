# Chapterhouse

Context persistence and memory management for AI assistants. Store, search, and recall context across sessions using the [Model Context Protocol](https://modelcontextprotocol.io/) (MCP).

## What it does

AI assistants forget everything between sessions. Chapterhouse gives them persistent memory — facts, decisions, patterns, and lessons learned — retrievable through semantic search.

```
You: "Remember that our API uses snake_case for all JSON fields"
  → stored as a factual memory with tags [api, conventions]

You: "What are our API conventions?"
  → recalls relevant memories via semantic similarity
```

## Architecture

```
Claude Code / AI Assistant
    │
    │  MCP (HTTP + Bearer token)
    ▼
┌──────────────┐     ┌───────────────┐     ┌──────────┐
│  ch-server   │────▶│  PostgreSQL   │     │ Embedding│
│  (Go API)    │     │  + pg_recall  │     │ Provider │
└──────────────┘     └───────────────┘     └──────────┘
    ▲
    │  HTTP proxy
┌──────────────┐
│   ch-web     │
│ (admin UI)   │
└──────────────┘
```

**ch-server** — Go API and MCP server. Handles memory storage, semantic search, user management, API keys, and audit logging. Uses [pg_recall](https://github.com/thinkwright/pg_recall) for unified content + vector + scoring storage.

**ch-web** — Vanilla JS admin console. User management, API key management, audit logs, and system health. Zero npm dependencies — just HTML, CSS, JS served by a Go binary.

## MCP Tools

| Tool | Description |
|------|-------------|
| `remember` | Store a memory with optional tags, type, scope, and session ID |
| `recall` | Search memories using semantic similarity or keywords |
| `forget` | Remove a specific memory |
| `list_memories` | Browse memories with filters |
| `share_memory` | Change scope between personal and org-wide |
| `export_memories` | Export memories to JSONL |
| `list_sessions` | List recent sessions with memory counts and timestamps |
| `session_summary` | Get a detailed breakdown of a specific session |
| `session_context` | Load all memories from a session for context resumption |

Memory types: **factual** (standards, configs), **experiential** (lessons learned), **working** (temporary, auto-expires in 7 days)

Session lifecycle: memories can be grouped by session ID, enabling temporal queries ("what were we working on yesterday?") and session resumption ("continue where we left off").

## Prerequisites

- Go 1.24+
- PostgreSQL 17+ with [pgvector](https://github.com/pgvector/pgvector) and [pg_recall](https://github.com/thinkwright/pg_recall) extensions
- Embedding provider: any OpenAI-compatible API (Together.ai, OpenAI, Ollama, vLLM)
- Kubernetes cluster (for Helm deployment)

## Quick start

### Build

```bash
# API server
cd ch-server && go build -o bin/ch-server ./cmd/api

# Admin console
cd ch-web && go build -o bin/ch-web ./cmd/server
```

### Run locally

```bash
# Start ch-server (needs PostgreSQL with pg_recall, embedding provider)
DATABASE_PASSWORD=secret ./bin/ch-server

# Start ch-web (proxies API to ch-server)
API_URL=http://localhost:8080 ./bin/ch-web
```

### Connect Claude Code

```bash
claude mcp add -t http ch-memory http://localhost:8080/mcp/stateless \
  --header "Authorization: Bearer ch_k1_<your-api-key>"
```

### Deploy with Helm

```bash
helm upgrade --install ch-server ch-server/charts/ch-server -n ch-system
helm upgrade --install ch-web ch-web/charts/ch-web -n ch-system
```

## Repository structure

```
chapterhouse/
├── ch-server/
│   ├── cmd/api/              # API server entrypoint
│   ├── cmd/init/             # Init container (pg_recall extension verification)
│   ├── cmd/mcp-server/       # Standalone MCP server (stdio proxy)
│   ├── internal/
│   │   ├── auth/             # API key, session, JWT, composite auth
│   │   ├── mcp/              # MCP protocol implementation
│   │   ├── mneme/            # pg_recall storage adapter
│   │   ├── handler/          # HTTP handlers
│   │   └── embedding/        # Embedding providers (OpenAI-compatible)
│   ├── db/migrations/        # SQL schema migrations
│   ├── charts/ch-server/     # Helm chart
│   └── Dockerfile
├── ch-web/
│   ├── cmd/server/
│   │   ├── main.go           # Go HTTP server (embed.FS + API proxy)
│   │   └── static/           # HTML, CSS, JS (zero build tools)
│   ├── charts/ch-web/        # Helm chart
│   └── Dockerfile
├── deploy/examples/          # Example Kubernetes manifests (CNPG)
├── Makefile                  # Build, test, deploy targets
├── LICENSE                   # Apache 2.0
└── README.md
```

## Configuration

### ch-server

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | Server port |
| `DATABASE_HOST` | `localhost` | PostgreSQL host |
| `DATABASE_PORT` | `5432` | PostgreSQL port |
| `DATABASE_NAME` | `memories` | Database name |
| `DATABASE_USER` | `memory_api` | Database user |
| `DATABASE_PASSWORD` | — | Database password (required) |
| `EMBEDDING_PROVIDER` | `openai` | `openai` (any OpenAI-compatible API) |
| `EMBEDDING_URL` | `https://api.openai.com` | Embedding service URL |
| `EMBEDDING_MODEL` | `text-embedding-3-small` | Embedding model name |
| `EMBEDDING_DIMENSIONS` | `768` | Embedding vector dimensions |
| `EMBEDDING_API_KEY` | — | API key for the embedding provider |

### ch-web

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | Server port |
| `API_URL` | `http://ch-server:8080` | ch-server backend URL |

## Embedding Configuration

Chapterhouse uses vector embeddings for semantic memory search. Any **OpenAI-compatible** embedding API works, including:

- [Together.ai](https://together.ai/) (hosted)
- [OpenAI](https://openai.com/) (hosted)
- [Ollama](https://ollama.com/) (local)
- [vLLM](https://docs.vllm.ai/) (self-hosted)

### Known working models

| Model | Dimensions | Provider |
|-------|-----------|----------|
| `BAAI/bge-base-en-v1.5` | 768 | Together.ai, Ollama, vLLM |
| `Alibaba-NLP/gte-modernbert-base` | 768 | Together.ai |
| `nomic-embed-text` | 768 | Ollama |
| `text-embedding-3-small` | 1536 | OpenAI |

### Dimension configuration

Embedding dimensions are configured both in Chapterhouse (`EMBEDDING_DIMENSIONS`) and in pg_recall (`SELECT pg_recall.configure_dimensions(768)`). These must match. If you change embedding models with different dimensions:

1. Update `EMBEDDING_MODEL` and `EMBEDDING_DIMENSIONS` in your configuration
2. Run `SELECT pg_recall.configure_dimensions(<new_dims>)` in PostgreSQL (requires empty mnemes table)
3. Re-embed existing memories if migrating

## License

Apache License 2.0 -- see [LICENSE](LICENSE).
