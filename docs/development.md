# Development

## Build

The root `Makefile` orchestrates per-component:

```sh
make extension     # cargo pgrx package for extension/ (dormant — algorithm reference)
make server        # go build for _chapterhouse/ch-server
make service       # go build for cmd/ghola + cmd/ghola-mcp
make all           # everything
make test          # run Go + Rust tests
make dev-up        # docker compose up on deploy/docker-compose
make dev-down      # tear it down
make smoke-predictive       # isolated smoke stack on alternate ports
make smoke-predictive-down  # tear it down
```

`make server` `cd`s into `_chapterhouse/ch-server/` because that
subproject has its own `go.mod`.

## Dev stack

`make dev-up` brings up: postgres (pgvector/pg17), ch-init (migration
runner), chapterhouse, ghola, mentat, truthsayer, and guild (or
guild-stub for deterministic-vector testing — `docker compose
--profile stub up -d`).

Service defaults:

| Service | Port | What |
|---|---|---|
| ghola | `:7421` | local HTTP daemon |
| chapterhouse | `:8080` | recall backend |
| guild | `:8082` | vLLM Qwen3-Embedding-0.6B |
| mentat | `:8084` | HDBSCAN clustering sidecar |
| truthsayer | `:8085` | cross-encoder reranker (bge-reranker-v2-m3 fp16) |

Override via `*_PORT` env vars in `deploy/docker-compose/.env`. See
`deploy/docker-compose/docker-compose.yml` for the full service graph.

## Hardware

The as-shipped dev stack expects a CUDA GPU for the embedder service
(guild = vLLM serving `Qwen3-Embedding-0.6B`). The reranker
(truthsayer) defaults to CUDA but flips to CPU with
`TRUTHSAYER_DEVICE=cpu`. Mentat is pure CPU (sklearn HDBSCAN).

**For CPU-only deployments**, swap the embedder. The models themselves
run on CPU (Qwen3-0.6B at ~2-3s/embed on a modern x86, bge-reranker-v2-m3
at ~50ms/pair). Replace the `guild` compose service with one of:

- [HuggingFace TEI](https://github.com/huggingface/text-embeddings-inference)
  CPU image (`ghcr.io/huggingface/text-embeddings-inference:cpu-1.5`),
  pointed at the same model. Keep `EMBEDDING_URL` and
  `EMBEDDING_MODEL` env vars unchanged.
- A `sentence-transformers` FastAPI wrapper (small Python service,
  ~30 lines).

The `guild-stub` profile (`docker compose --profile stub up -d`) swaps
in a deterministic hash stub for tests where you don't care about
embedding quality.

## Env vars

ghola env knobs are documented at the top of `cmd/ghola/main.go`. The
recall-tuning ones are listed in
[recall-pipeline.md](recall-pipeline.md#tuning-knobs).

## Tests

```sh
make test           # Go + Rust
go test ./...       # root module only
(cd _chapterhouse/ch-server && go test ./...)
(cd extension && cargo test)   # dormant extension
```

Integration tests live under `test/` (cross-binary, acceptance-criteria
flavor) and per-package `*_test.go` files.

## Layout reference

See [layout.md](layout.md) for the monorepo map.
