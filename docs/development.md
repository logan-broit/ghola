# Development

## Build

The root `Makefile` orchestrates per-component:

```sh
make server        # go build for _chapterhouse/ch-server
make service       # go build for cmd/ghola + cmd/ghola-mcp
make all           # everything
make test          # run Go tests
make dev-up        # docker compose up on deploy/docker-compose
make dev-down      # tear it down
make smoke-predictive       # isolated smoke stack on alternate ports
make smoke-predictive-down  # tear it down
```

The original Rust extension is retired to `attic/extension/` (algorithm
reference, not in the build graph). Build it by hand with `cargo` if you
need to mine it.

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

The as-shipped dev stack expects a **CUDA GPU** for the embedder
service (guild = vLLM serving `Qwen3-Embedding-0.6B`); vLLM is
CUDA-only and doesn't support Metal or CPU. The reranker (truthsayer)
defaults to CUDA fp16 but accepts other devices via
`TRUTHSAYER_DEVICE`. Mentat is pure CPU (sklearn HDBSCAN), no GPU
concern.

| Component | CUDA | Apple Silicon (Metal) | CPU |
|---|---|---|---|
| guild (embedder) | vLLM as shipped | swap the service | swap the service |
| truthsayer (reranker) | default, fp16 | `TRUTHSAYER_DEVICE=mps` | `TRUTHSAYER_DEVICE=cpu` |
| mentat (clustering) | n/a | n/a | always CPU |

**Swapping the embedder service.** The models themselves run on
anything (Qwen3-0.6B at ~2-3s/embed on a modern x86 CPU, similar on
M-series; bge-reranker-v2-m3 at ~50ms/pair on CPU). Replace the
`guild` compose service with:

- **Apple Silicon**: Ollama (Metal-native, exposes
  `/v1/embeddings` OpenAI-compatible) or llama.cpp's `--embedding`
  server, both backed by a GGUF Qwen3-Embedding build.
- **CPU**:
  [HuggingFace TEI](https://github.com/huggingface/text-embeddings-inference)
  CPU image (`ghcr.io/huggingface/text-embeddings-inference:cpu-1.5`),
  pointed at the same model, or a `sentence-transformers` FastAPI
  wrapper (~30 lines).

Keep `EMBEDDING_URL`, `EMBEDDING_MODEL`, `EMBEDDING_DIM` env vars
unchanged across the swap so ghola + chapterhouse don't need to know.

The `guild-stub` profile (`docker compose --profile stub up -d`) swaps
in a deterministic hash stub for tests where you don't care about
embedding quality.

**fp16 caveat.** Truthsayer's `dtype` knob only applies on CUDA
(see `truthsayer/truthsayer/scorer.py`). On MPS / CPU the model
runs in fp32 — by design, since MPS fp16 has historically been
flaky and CPU fp16 inference isn't worth it.

## Env vars

ghola env knobs are documented at the top of `cmd/ghola/main.go`. The
recall-tuning ones are listed in
[recall-pipeline.md](recall-pipeline.md#tuning-knobs).

## Tests

```sh
make test           # Go
go test ./...       # root module only
(cd _chapterhouse/ch-server && go test ./...)
(cd attic/extension && cargo test)   # retired extension, not in the build graph
```

Integration tests live under `test/` (cross-binary, acceptance-criteria
flavor) and per-package `*_test.go` files.

## Layout reference

See [layout.md](layout.md) for the monorepo map.
