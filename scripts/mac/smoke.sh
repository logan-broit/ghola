#!/usr/bin/env bash
# End-to-end smoke test against a running ghola daemon (default localhost:7421).
# Records an event, recalls it, asserts the recall is non-degraded and the
# recorded text is retrievable. Run AFTER the stack + native services are up.
#
# Wire shapes (internal/core/types.go):
#   record:  {"cwd","event":{"type","role","text"}}   -> server fills id/embedding/session
#   recall:  {"cwd","query_text","limit"}             -> {"hits":[{"content",...}],"degraded":[...]}
# user_id is omitted: the daemon's AUTH_DEFAULT_USER fallback supplies it.
set -euo pipefail

GHOLA="${GHOLA_URL:-http://localhost:7421}"
CWD="${GHOLA_SMOKE_CWD:-/tmp/ghola-smoke}"
MARKER="smoke-$(date +%s)-the-mauve-database-runs-on-port-9931"

echo "== record =="
curl -fsS -X POST "$GHOLA/v1/record" -H 'content-type: application/json' -d @- <<JSON | tee /tmp/ghola-smoke-record.json
{"cwd": "$CWD", "event": {"type": "user_message", "role": "user", "text": "$MARKER"}}
JSON
echo

echo "== recall =="
curl -fsS -X POST "$GHOLA/v1/recall" -H 'content-type: application/json' -d @- <<JSON | tee /tmp/ghola-smoke-recall.json
{"cwd": "$CWD", "query_text": "what port does the mauve database run on", "limit": 5}
JSON
echo

echo "== assertions =="
python3 - <<'PY'
import json
r = json.load(open('/tmp/ghola-smoke-recall.json'))
deg = r.get('degraded') or []
assert not deg, f"degraded tiers: {deg}"
blob = json.dumps(r)
assert "mauve-database" in blob, "recorded marker not retrievable"
print("SMOKE OK: non-degraded, marker retrievable")
PY
