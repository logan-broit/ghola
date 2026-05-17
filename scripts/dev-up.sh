#!/usr/bin/env bash
# Bring up the local dev stack and assert each service is healthy.
# Returns non-zero on any failure. Designed for Phase 9 Gate 9.
#
#   ./scripts/dev-up.sh              # core stack only
#   ./scripts/dev-up.sh --with-ghola # include the ghola daemon
#
# Budget: must print all-green in under 30 seconds on a warm cache.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE_DIR="$SCRIPT_DIR/../deploy/docker-compose"
cd "$COMPOSE_DIR"

PROFILE_ARG=()
INCLUDE_GHOLA=0
for arg in "$@"; do
  case "$arg" in
    --with-ghola)
      PROFILE_ARG=(--profile all)
      INCLUDE_GHOLA=1
      ;;
    *)
      echo "unknown arg: $arg" >&2
      exit 2
      ;;
  esac
done

echo "==> docker compose up -d"
docker compose "${PROFILE_ARG[@]}" up -d --build

echo "==> waiting for health"

wait_http() {
  local name="$1" url="$2" deadline=$((SECONDS + 60))
  while (( SECONDS < deadline )); do
    if curl -fsS "$url" >/dev/null 2>&1; then
      echo "    $name ok"
      return 0
    fi
    sleep 1
  done
  echo "    $name FAILED (url=$url)" >&2
  docker compose logs "$name" | tail -40 >&2 || true
  return 1
}

: "${CHAPTERHOUSE_PORT:=8080}"
: "${GHOLA_PORT:=7421}"

wait_http chapterhouse "http://localhost:${CHAPTERHOUSE_PORT}/health"

if (( INCLUDE_GHOLA )); then
  wait_http ghola "http://localhost:${GHOLA_PORT}/health"
fi

echo "==> all green"
