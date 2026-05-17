# ghola

Tiered memory for AI agents. Speaks MCP and HTTP/JSON; records every
turn into a per-session local store, consolidates into a shared
Postgres-backed corpus, recalls with a 5-tier RRF fan-out plus
cross-encoder rerank. Workspace scoping keeps recall focused on the
current task.

## Install

Build from source. Requires Go 1.22+ and Docker. A CUDA GPU is
**recommended** for the embedder + reranker (the as-shipped dev stack
uses vLLM, which needs CUDA), but the models themselves
(`Qwen3-Embedding-0.6B`, `bge-reranker-v2-m3`) run on CPU — swap the
embedder service for a CPU-friendly server (TEI, sentence-transformers)
to deploy GPU-free. The reranker service flips to CPU with
`TRUTHSAYER_DEVICE=cpu`.

```sh
git clone https://github.com/logan-broit/ghola
cd ghola
make all
```

For just the local daemon binaries (`ghola`, `ghola-mcp`):

```sh
make service
```

See [`docs/development.md`](docs/development.md) for component-level
targets, the dev stack, and env knobs.

## Launch

Full local stack — Postgres + chapterhouse + ghola + mentat +
truthsayer + GPU embedder, all built locally on first run:

```sh
make dev-up
```

Just the ghola HTTP daemon (assumes a reachable chapterhouse via
`CHAPTERHOUSE_URL`):

```sh
./ghola serve
```

## Connect

**MCP** (Claude Code, Cursor, anything speaking MCP):

```jsonc
// ~/.claude.json
"mcpServers": {
  "ghola": { "command": "/path/to/ghola-mcp" }
}
```

MCP tools surfaced: `session_start`, `session_end`, `list_sessions`,
`record`, `branch`, `bookmark`, `navigate`, `recall`, `forget`,
`share`, `consolidate`, `expand_session_workspace`.

**HTTP/JSON** (any client):

```sh
curl -fsS -X POST http://localhost:7421/v1/session_start \
  -H 'Content-Type: application/json' \
  -d '{"user_id":"00000000-0000-0000-0000-000000000001","cwd":"/path/to/project"}' \
  | jq .
```

## Docs

- [`docs/recall-pipeline.md`](docs/recall-pipeline.md) — 5-tier RRF fan-out + rerank
- [`docs/workspaces.md`](docs/workspaces.md) — workspace scoping + session mapping
- [`docs/development.md`](docs/development.md) — build targets, dev stack, env vars
- [`docs/benchmarks.md`](docs/benchmarks.md) — LongMemEval-S results
- [`docs/layout.md`](docs/layout.md) — monorepo map

## License

MIT — see [`LICENSE`](LICENSE).
