# Chapterhouse

Persistent agentic memory for AI assistants. Store, search, and recall context across sessions using the [Model Context Protocol](https://modelcontextprotocol.io/) (MCP).

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
│  ch-server   │────▶│  PostgreSQL   │     │  Ollama  │
│  (Go API)    │────▶│  (Qdrant)     │     │ (embed)  │
└──────────────┘     └───────────────┘     └──────────┘
    ▲
    │  HTTP proxy
┌──────────────┐
│   ch-web     │
│ (admin UI)   │
└──────────────┘
```

**ch-server** — Go API and MCP server. Handles memory storage, semantic search, user management, API keys, and audit logging.

**ch-web** — Vanilla JS admin console. User management, API key management, audit logs, and system health. Zero npm dependencies — just HTML, CSS, JS served by a Go binary.

## MCP Tools

| Tool | Description |
|------|-------------|
| `remember` | Store a memory with optional tags, type, and scope |
| `recall` | Search memories using semantic similarity or keywords |
| `forget` | Remove a specific memory |
| `list_memories` | Browse memories with filters |
| `share_memory` | Change scope between personal and org-wide |
| `export_memories` | Export memories to JSONL |

Memory types: **factual** (standards, configs), **experiential** (lessons learned), **working** (temporary, auto-expires in 7 days)

## Prerequisites

- Go 1.24+
- PostgreSQL 16
- [Qdrant](https://qdrant.tech/) vector database
- Embedding provider: [Ollama](https://ollama.com/) with `nomic-embed-text` (or vLLM)
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
# Start ch-server (needs PostgreSQL, Qdrant, Ollama running)
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
│   ├── cmd/init/             # Init container (DB ping + Qdrant collection setup)
│   ├── cmd/mcp-server/       # Standalone MCP server
│   ├── cmd/reindex/          # Reindex all memories in Qdrant
│   ├── internal/
│   │   ├── auth/             # API key, session, JWT, composite auth
│   │   ├── mcp/              # MCP protocol implementation
│   │   ├── handler/          # HTTP handlers
│   │   ├── embedding/        # Embedding providers (Ollama, OpenAI-compatible)
│   │   └── vector/           # Qdrant client
│   ├── db/migrations/        # SQL schema migrations
│   ├── charts/ch-server/     # Helm chart
│   └── Dockerfile
├── ch-web/
│   ├── cmd/server/
│   │   ├── main.go           # Go HTTP server (embed.FS + API proxy)
│   │   └── static/           # HTML, CSS, JS (zero build tools)
│   ├── charts/ch-web/        # Helm chart
│   └── Dockerfile
├── deploy/examples/          # Example Kubernetes manifests (CNPG, Qdrant)
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
| `QDRANT_HOST` | `localhost` | Qdrant host |
| `QDRANT_GRPC_PORT` | `6334` | Qdrant gRPC port |
| `EMBEDDING_PROVIDER` | `ollama` | `ollama` or `openai` (any OpenAI-compatible API) |
| `EMBEDDING_URL` | `http://localhost:11434` | Embedding service URL |
| `EMBEDDING_MODEL` | `nomic-embed-text` | Embedding model name |
| `EMBEDDING_API_KEY` | — | API key for OpenAI-compatible providers |

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
| `nomic-embed-text` | 768 | Ollama |
| `text-embedding-3-small` | 1536 | OpenAI |

### Dimension mismatch warning

The embedding dimensions configured in Chapterhouse **must match** the Qdrant collection dimensions exactly. If you change embedding models with different dimensions:

1. Update `EMBEDDING_MODEL` and `EMBEDDING_DIMENSIONS` in your configuration
2. **Drop and recreate** the Qdrant collection (dimensions are set at creation time)
3. Run a full reindex to regenerate all vectors

```bash
# Example: reindex after changing embedding model
kubectl exec -n ch-system deploy/ch-server -- /app/ch-reindex
```

Failure to match dimensions will cause indexing errors. There is no automatic migration -- changing dimensions requires a full reindex.

## License

Apache License 2.0 -- see [LICENSE](LICENSE).
