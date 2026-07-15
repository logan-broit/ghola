#!/usr/bin/env bash
# OPERATOR-GATED: brings up the compose stack and runs the consolidation
# integration smoke (internal/consolidation/smoke_integration_test.go,
# behind `-tags integration_smoke`). Requires Docker + the stack build.
# Do NOT run without operator approval — it starts services and writes
# rows into a fresh, random scratch workspace (see the test file for why
# that's safe against a live/running stack).
#
#   ./scripts/consolidation-smoke.sh
#
# DATABASE_URL / MENTAT_URL / EMBEDDING_DIM may be overridden; the
# defaults below match deploy/docker-compose/docker-compose.yml's published
# ports (POSTGRES_PORT default 5432, MENTAT_PORT default 8084) and the
# stack's default embedding dimension (1024, Qwen3-Embedding-0.6B).

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE_DIR="$SCRIPT_DIR/../deploy/docker-compose"
CH_SERVER_DIR="$SCRIPT_DIR/../_chapterhouse/ch-server"

cd "$COMPOSE_DIR"
echo "==> docker compose up -d postgres ch-init mentat guild"
docker compose up -d postgres ch-init mentat guild

echo "==> waiting for health"

wait_http() {
  local name="$1" url="$2" deadline=$((SECONDS + 180))
  while (( SECONDS < deadline )); do
    if curl -fsS "$url" >/dev/null 2>&1; then
      echo "    $name ok"
      return 0
    fi
    sleep 2
  done
  echo "    $name FAILED (url=$url)" >&2
  docker compose logs "$name" | tail -40 >&2 || true
  return 1
}

wait_service_completed() {
  local name="$1" deadline=$((SECONDS + 60))
  while (( SECONDS < deadline )); do
    if [[ "$(docker compose ps -q "$name" 2>/dev/null)" == "" ]]; then
      echo "    $name completed"
      return 0
    fi
    sleep 1
  done
  echo "    $name FAILED to complete" >&2
  docker compose logs "$name" | tail -40 >&2 || true
  return 1
}

: "${POSTGRES_PORT:=5432}"
: "${MENTAT_PORT:=8084}"
: "${EMBEDDING_PORT:=8082}"

wait_http guild "http://localhost:${EMBEDDING_PORT}/health"
wait_service_completed ch-init
wait_http mentat "http://localhost:${MENTAT_PORT}/v1/health"

export DATABASE_URL="${DATABASE_URL:-postgres://memory_api:dev@localhost:${POSTGRES_PORT}/memories?sslmode=disable}"
export MENTAT_URL="${MENTAT_URL:-http://localhost:${MENTAT_PORT}}"
export EMBEDDING_DIM="${EMBEDDING_DIM:-1024}"

echo "==> running consolidation smoke (DATABASE_URL=$DATABASE_URL MENTAT_URL=$MENTAT_URL EMBEDDING_DIM=$EMBEDDING_DIM)"
cd "$CH_SERVER_DIR"
go test -tags integration_smoke ./internal/consolidation/ -run TestConsolidationSmoke -count=1 -v
