#!/usr/bin/env bash
# Native Metal embedder for the Mac ghola stack.
# Serves Qwen3-Embedding-0.6B (1024-d) on :8082 via llama.cpp-server, OpenAI
# /v1/embeddings. Reached from Docker containers via host.docker.internal:8082.
#
# Prereqs (one-time):
#   brew install llama.cpp
#   mkdir -p ~/.ghola-models && cd ~/.ghola-models
#   # Fetch a GGUF of Qwen3-Embedding-0.6B (e.g. from the Qwen or a community repo):
#   #   huggingface-cli download <repo>/Qwen3-Embedding-0.6B-GGUF <file>.gguf --local-dir .
set -euo pipefail

MODEL="${GHOLA_EMBED_GGUF:-$HOME/.ghola-models/Qwen3-Embedding-0.6B-Q8_0.gguf}"
PORT="${GHOLA_EMBED_PORT:-8082}"
# Bind 127.0.0.1: Docker Desktop's host.docker.internal reaches host loopback.
# If containers cannot reach it, re-run with GHOLA_EMBED_HOST=0.0.0.0.
HOST="${GHOLA_EMBED_HOST:-127.0.0.1}"

if [[ ! -f "$MODEL" ]]; then
  echo "embedder: GGUF not found at $MODEL" >&2
  echo "  set GHOLA_EMBED_GGUF=/path/to/Qwen3-Embedding-0.6B-*.gguf, or place the file there." >&2
  exit 1
fi

exec llama-server \
  -m "$MODEL" \
  --embeddings \
  --pooling last \
  --host "$HOST" \
  --port "$PORT" \
  --ctx-size 8192
