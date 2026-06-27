#!/usr/bin/env bash
# Native Metal reranker for the Mac ghola stack.
# Runs truthsayer (bge-reranker-v2-m3) on Apple MPS, :8085, /v1/rerank.
# Reached from Docker containers via host.docker.internal:8085.
#
# No truthsayer code change is needed: config.py reads TRUTHSAYER_DEVICE and
# scorer.py passes it straight to CrossEncoder(device=...); the cuda-only fp16
# dtype is gated on a "cuda" device prefix, so mps is left at its default dtype.
#
# Prereqs (one-time): brew install uv ; from repo root: cd truthsayer && uv sync
set -euo pipefail

cd "$(dirname "$0")/../../truthsayer"

export TRUTHSAYER_MODEL="${TRUTHSAYER_MODEL:-BAAI/bge-reranker-v2-m3}"
export TRUTHSAYER_DEVICE="${TRUTHSAYER_DEVICE:-mps}"
export TRUTHSAYER_MAX_LENGTH="${TRUTHSAYER_MAX_LENGTH:-8192}"
# Bind 127.0.0.1: reachable from containers via host.docker.internal on Docker
# Desktop. If containers cannot reach it, re-run with TRUTHSAYER_HOST=0.0.0.0.
HOST="${TRUTHSAYER_HOST:-127.0.0.1}"
PORT="${TRUTHSAYER_PORT:-8085}"

# app = FastAPI(...) lives in truthsayer/truthsayer/app.py → module truthsayer.app
exec uv run uvicorn truthsayer.app:app --host "$HOST" --port "$PORT"
