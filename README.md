# ghola

Tiered memory for AI agents. Speaks MCP and HTTP/JSON; records every
turn into a per-session local store, consolidates into a shared
Postgres-backed corpus, recalls with a 5-tier RRF fan-out plus
cross-encoder rerank. Workspace scoping keeps recall focused on the
current task.

## Install

Build from source. Requires Go 1.22+ and Docker. A GPU is
**recommended** for the embedder + reranker:

- **CUDA** — the dev stack ships vLLM for the embedder, which is
  CUDA-only. Reranker defaults to CUDA fp16.
- **Apple Silicon (Metal)** — reranker honors
  `TRUTHSAYER_DEVICE=mps`. Swap the vLLM embedder for an Ollama /
  llama.cpp / MLX service (Metal-native), since vLLM doesn't support
  Metal.
- **CPU-only** — same swap pattern; use TEI's CPU image or a
  `sentence-transformers` wrapper for the embedder, set
  `TRUTHSAYER_DEVICE=cpu`.

Models themselves (`Qwen3-Embedding-0.6B`, `bge-reranker-v2-m3`) run
fine on all three; the constraint is which serving stack you wire in
behind the embedder URL.

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

MCP tools surfaced (5 agent-facing): `record`, `recall`, `bookmark`,
`navigate`, `forget`. `record` accepts an optional `cwd` — when
`session_id` is omitted the service derives a workspace from `cwd`
and reuses or provisions a session inline (no `session_start`
bookkeeping for the model). Lifecycle and admin operations
(`session_start`, `session_end`, `list_sessions`, `branch`,
`expand_session_workspace`, `share`, `consolidate`) stay reachable
over HTTP at `/v1/*` for hosts that drive memory programmatically;
they're hidden from the model's tool catalog.

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
