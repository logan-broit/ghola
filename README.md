# Chapterhouse

Cognitive memory for AI agents. Memories that strengthen through use, fade when irrelevant, and evolve as understanding changes. Built on primitives modeled after human cognition.

## Why Chapterhouse

AI assistants forget everything between sessions. Most solutions bolt a vector database onto an MCP wrapper and call it memory. Chapterhouse is different — it implements cognitive primitives that model how memory actually works:

- **Hebbian reinforcement** — memories recalled together strengthen together
- **Temporal decay** — unused memories lose activation, keeping recall sharp
- **Supersession** — when understanding changes, new memories replace the old while preserving lineage
- **Contradiction detection** — conflicting knowledge surfaces rather than silently coexisting
- **Typed knowledge** — facts, experiences, and working context each behave differently

All of this runs inside PostgreSQL via [pg_recall](https://github.com/thinkwright/pg_recall), a purpose-built extension that unifies content, embeddings, and cognitive scoring in a single store.

```
You: "Remember that our API uses snake_case for all JSON fields"
  → stored as a factual mneme, embedded, scored, searchable

You: "What are our API conventions?"
  → recalled via semantic + keyword + temporal + Hebbian scoring
  → the memory strengthens through use
```

## Architecture

```
Claude Code / Codex / AI Agent
    │
    │  MCP over Streamable HTTP
    ▼
┌──────────────┐     ┌───────────────────┐     ┌──────────────┐
│  ch-server   │────▶│  PostgreSQL 18    │     │  Embedding   │
│  (Go API)    │     │  pgvector         │     │  Provider    │
│              │     │  pg_recall        │     │  (TEI, etc.) │
└──────────────┘     └───────────────────┘     └──────────────┘
    ▲
    │  HTTP proxy
┌──────────────┐
│   ch-web     │
│ (admin UI)   │
└──────────────┘
```

**ch-server** — Go API and MCP server. Handles memory storage, cognitive recall, user management, API keys, and audit logging. Exposes both stateful (`/mcp`) and stateless (`/mcp/stateless`) MCP endpoints.

**ch-web** — Admin console for user management, API key lifecycle, audit logs, and system health. Vanilla HTML/CSS/JS served by a Go binary — zero npm dependencies.

**pg_recall** — PostgreSQL extension providing cognitive memory primitives: typed mnemes, Hebbian reinforcement, temporal decay, supersession, contradiction detection, and multi-signal recall scoring.

## MCP Tools

| Tool | Description |
|------|-------------|
| `remember` | Store a memory with type, tags, scope, and session ID |
| `recall` | Search memories using semantic, keyword, or hybrid scoring |
| `forget` | Remove a specific memory |
| `list_memories` | Browse memories with filters |
| `share_memory` | Change scope between personal and org-wide |
| `export_memories` | Export memories to JSONL |
| `list_sessions` | List recent sessions with memory counts |
| `session_summary` | Get a detailed breakdown of a specific session |
| `session_context` | Load all memories from a session for context resumption |

### Memory Types

| Type | Behavior |
|------|----------|
| **factual** | Standards, configurations, conventions. Persists indefinitely. |
| **experiential** | Lessons learned, debugging solutions. Carries weight over time. |
| **working** | Session-scoped notes. Auto-expires after 7 days. |

### Recall Modes

| Mode | Behavior |
|------|----------|
| **hybrid** | Blends semantic similarity, keyword matching, temporal recency, Hebbian activation, and confidence (default) |
| **semantic** | Pure vector similarity search |
| **keyword** | Full-text search with PostgreSQL tsvector |

### Session Lifecycle

Memories can be grouped by session ID, enabling temporal queries and session resumption:
- `list_sessions` — "what sessions have I had recently?"
- `session_summary` — "what happened in that session?"
- `session_context` — "load that session's context so we can continue"

## Quick Start

### Prerequisites

- Go 1.24+
- PostgreSQL 18 with [pgvector](https://github.com/pgvector/pgvector) and [pg_recall](https://github.com/thinkwright/pg_recall)
- Embedding provider: any OpenAI-compatible API

### Build

```bash
# API server
cd ch-server && go build -o bin/ch-server ./cmd/api

# Admin console
cd ch-web && go build -o bin/ch-web ./cmd/server
```

### Run Locally

```bash
# Start ch-server (needs PostgreSQL with pg_recall, embedding provider)
DATABASE_PASSWORD=secret ./bin/ch-server

# Start ch-web (proxies API to ch-server)
API_URL=http://localhost:8080 ./bin/ch-web
```

### Connect Claude Code

```bash
claude mcp add -s user -t http chapterhouse \
  http://localhost:8080/mcp \
  --header "Authorization: Bearer ch_k1_<your-api-key>"
```

Use `/mcp` for full session lifecycle support. Use `/mcp/stateless` if you only need stateless memory tools.

### Deploy with Helm

```bash
helm upgrade --install ch-server ch-server/charts/ch-server \
  -n ch-system -f your-values.yaml

helm upgrade --install ch-web ch-web/charts/ch-web \
  -n ch-system -f your-values.yaml
```

See [RUNBOOK.md](RUNBOOK.md) for the full deployment guide.

## Configuration

### ch-server

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | Server port |
| `ENVIRONMENT` | `production` | Set to `local` or `development` to disable secure cookies |
| `DATABASE_HOST` | `localhost` | PostgreSQL host |
| `DATABASE_PORT` | `5432` | PostgreSQL port |
| `DATABASE_NAME` | `memories` | Database name |
| `DATABASE_USER` | `memory_api` | Database user |
| `DATABASE_PASSWORD` | — | Database password (required) |
| `EMBEDDING_PROVIDER` | `openai` | Any OpenAI-compatible API |
| `EMBEDDING_URL` | `https://api.openai.com` | Embedding service URL |
| `EMBEDDING_MODEL` | `text-embedding-3-small` | Embedding model name |
| `EMBEDDING_DIMENSIONS` | `768` | Embedding vector dimensions |
| `EMBEDDING_API_KEY` | — | API key for the embedding provider |

### ch-web

| Variable | Default | Description |
|----------|---------|-------------|
| `PORT` | `8080` | Server port |
| `API_URL` | `http://ch-server:8080` | ch-server backend URL |

## Embedding Providers

Chapterhouse uses vector embeddings for semantic search. Any **OpenAI-compatible** embedding API works:

| Provider | Hosting | Notes |
|----------|---------|-------|
| [HuggingFace TEI](https://github.com/huggingface/text-embeddings-inference) | Self-hosted | Recommended. CPU or GPU inference. |
| [OpenAI](https://openai.com/) | Hosted | `text-embedding-3-small` or `text-embedding-3-large` |
| [Together.ai](https://together.ai/) | Hosted | Various open models |
| [Ollama](https://ollama.com/) | Local | `nomic-embed-text`, `bge-base-en-v1.5` |
| [vLLM](https://docs.vllm.ai/) | Self-hosted | Any HuggingFace embedding model |

### Recommended Model

`Alibaba-NLP/gte-modernbert-base` — 768 dimensions, 8192 token context, top-tier code retrieval performance ([COIR 79.31](https://huggingface.co/Alibaba-NLP/gte-modernbert-base)). Runs efficiently on CPU via TEI.

### Dimension Configuration

Embedding dimensions must match between Chapterhouse (`EMBEDDING_DIMENSIONS`) and pg_recall:

```sql
SELECT pg_recall.configure_dimensions(768);
```

Changing dimensions requires an empty mnemes table. If migrating models, re-embed existing memories after reconfiguring.

## Repository Structure

```
chapterhouse/
├── ch-server/
│   ├── cmd/api/              # API server entrypoint
│   ├── cmd/init/             # Init container (pg_recall verification)
│   ├── cmd/mcp-server/       # Standalone MCP server (stdio transport)
│   ├── internal/
│   │   ├── auth/             # API key, session, JWT, composite auth
│   │   ├── mcp/              # MCP protocol + 9 tool handlers
│   │   ├── mneme/            # pg_recall storage adapter
│   │   ├── handler/          # Admin HTTP handlers
│   │   ├── embedding/        # OpenAI-compatible embedding client
│   │   └── config/           # Environment-based configuration
│   ├── db/migrations/        # SQL schema migrations (8 files)
│   ├── charts/ch-server/     # Helm chart
│   └── Dockerfile
├── ch-web/
│   ├── cmd/server/
│   │   ├── main.go           # Go HTTP server (embed.FS + API proxy)
│   │   └── static/           # HTML, CSS, JS (zero build tools)
│   ├── charts/ch-web/        # Helm chart
│   └── Dockerfile
├── deploy/
│   ├── examples/             # Example CNPG manifest
│   └── homelab/              # Homelab deployment config
│       ├── deploy.sh
│       ├── ch-server-values.yaml
│       ├── ch-web-values.yaml
│       └── infra/            # PostgreSQL, TEI, ingress manifests
├── Makefile
├── RUNBOOK.md                # Deployment operations guide
├── BUILD_AND_RELEASE.md      # Build and release guide
└── README.md
```

## Documentation

- [RUNBOOK.md](RUNBOOK.md) — Deployment operations: infrastructure setup, migrations, secrets, Helm, ingress, troubleshooting
- [BUILD_AND_RELEASE.md](BUILD_AND_RELEASE.md) — Building images, Helm charts, Makefile reference
- [ch-server/MEMORY_SYSTEM_GUIDE.md](ch-server/MEMORY_SYSTEM_GUIDE.md) — How the memory system works in practice
