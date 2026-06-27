#!/usr/bin/env bash
# End-to-end smoke test against a running ghola daemon (default localhost:7421).
# Records an event, recalls it, asserts the recall is non-degraded and the
# recorded text is retrievable from an actual hit. Run AFTER the stack + native
# services are up.
#
# Wire shapes (internal/core/types.go):
#   record:  {"cwd","event":{"type","role","text"}}   -> server fills id/embedding/session
#   recall:  {"cwd","query_text","limit"}             -> {"hits":[{"content",...}],"degraded":[...]}
# user_id is omitted: the daemon's AUTH_DEFAULT_USER fallback supplies it.
set -euo pipefail

GHOLA="${GHOLA_URL:-http://localhost:7421}"
CWD="${GHOLA_SMOKE_CWD:-/tmp/ghola-smoke}"
MARKER="smoke-$(date +%s)-the-mauve-database-runs-on-port-9931"

# Build JSON bodies with json.dumps so CWD/MARKER are always safely encoded
# (no heredoc interpolation footgun if a value contains quotes/backslashes).
record_body=$(CWD="$CWD" MARKER="$MARKER" python3 -c 'import json,os; print(json.dumps({"cwd":os.environ["CWD"],"event":{"type":"user_message","role":"user","text":os.environ["MARKER"]}}))')
recall_body=$(CWD="$CWD" python3 -c 'import json,os; print(json.dumps({"cwd":os.environ["CWD"],"query_text":"what port does the mauve database run on","limit":5}))')

echo "== record =="
curl -fsS -X POST "$GHOLA/v1/record" -H 'content-type: application/json' -d "$record_body" | tee /tmp/ghola-smoke-record.json
echo

echo "== recall =="
curl -fsS -X POST "$GHOLA/v1/recall" -H 'content-type: application/json' -d "$recall_body" | tee /tmp/ghola-smoke-recall.json
echo

echo "== assertions =="
python3 - <<'PY'
import json
r = json.load(open('/tmp/ghola-smoke-recall.json'))
deg = r.get('degraded') or []
assert not deg, f"degraded tiers: {deg}"
hits = r.get('hits') or []
assert any("mauve-database" in (h.get("content") or "") for h in hits), \
    "recorded marker not present in any recall hit"
print("SMOKE OK: non-degraded, marker retrievable from a hit")
PY
