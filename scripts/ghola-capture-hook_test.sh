#!/usr/bin/env bash
# Fixture-driven test for ghola-capture-hook.sh. Stands up a one-shot
# python capture server, points the hook at it via GHOLA_CAPTURE_URL,
# and asserts the POSTed JSON body. Needs python3, jq, curl. No ghola
# daemon required.
set -u
here="$(cd "$(dirname "$0")" && pwd)"
hook="$here/ghola-capture-hook.sh"
port=17431
url="http://127.0.0.1:$port/v1/record"
fails=0

capture() { # $1=outfile ; serves exactly one request then exits
  python3 - "$1" "$port" <<'PY' &
import sys, http.server
out, port = sys.argv[1], int(sys.argv[2])
class H(http.server.BaseHTTPRequestHandler):
    def do_POST(self):
        n = int(self.headers.get('Content-Length', '0'))
        open(out, 'wb').write(self.rfile.read(n))
        self.send_response(200); self.end_headers()
    def log_message(self, *a): pass
srv = http.server.HTTPServer(('127.0.0.1', port), H)
srv.handle_request()
PY
  server_pid=$!
  # Wait for the port to start listening. This is a one-shot server
  # (handle_request() serves exactly one request then exits), so the
  # probe must NOT itself make an HTTP connection -- a real connect
  # would consume the single request slot and the hook's actual POST
  # would then hit a server that already exited. Check the listening
  # socket table instead (ss), which never touches the socket.
  for _ in $(seq 1 50); do
    if ss -ltn 2>/dev/null | awk -v p=":$port" '$4 ~ p"$"{f=1} END{exit !f}'; then break; fi
    sleep 0.1
  done
}

check() { # $1=desc $2=expected $3=actual
  if [ "$2" = "$3" ]; then
    echo "ok   - $1"
  else
    echo "FAIL - $1: expected [$2] got [$3]"; fails=$((fails+1))
  fi
}

# --- UserPromptSubmit ---
out="$(mktemp)"; capture "$out"
GHOLA_CAPTURE_URL="$url" "$hook" UserPromptSubmit < "$here/testdata/user_prompt_submit.json"
wait "$server_pid" 2>/dev/null
check "user type"   "user"  "$(jq -r '.event.type' "$out")"
check "user text"   "how do I segment the session" "$(jq -r '.event.text' "$out")"
check "user cwd"    "/tmp/proj" "$(jq -r '.cwd' "$out")"

# --- Stop (transcript-derived) ---
out="$(mktemp)"; capture "$out"
jq -n --arg t "$here/testdata/transcript.jsonl" --arg c /tmp/proj \
  '{hook_event_name:"Stop", cwd:$c, transcript_path:$t}' \
  | GHOLA_CAPTURE_URL="$url" "$hook" Stop
wait "$server_pid" 2>/dev/null
check "stop type"   "assistant" "$(jq -r '.event.type' "$out")"
check "stop text"   "final answer" "$(jq -r '.event.text' "$out")"

# --- 16KB truncation ---
out="$(mktemp)"; capture "$out"
big="$(head -c 40000 /dev/zero | tr '\0' 'x')"
jq -n --arg c /tmp/proj --arg p "$big" '{hook_event_name:"UserPromptSubmit", cwd:$c, prompt:$p}' \
  | GHOLA_CAPTURE_URL="$url" "$hook" UserPromptSubmit
wait "$server_pid" 2>/dev/null
len="$(jq -r '.event.text' "$out" | tr -d '\n' | wc -c)"
if [ "$len" -le 16384 ]; then echo "ok   - truncated to <=16384 ($len)"; else echo "FAIL - not truncated ($len)"; fails=$((fails+1)); fi

# --- Stop on a large transcript (bounded read, must stay fast) ---
big_transcript="$(mktemp)"
for i in $(seq 1 4996); do
  printf '{"type":"user","message":{"role":"user","content":[{"type":"text","text":"filler %d"}]}}\n' "$i"
done >"$big_transcript"
cat "$here/testdata/transcript.jsonl" >>"$big_transcript"
wc -l "$big_transcript" | awk '{if ($1 < 5000) { print "FAIL - fixture too small (" $1 " lines)"; exit 1 }}'

out="$(mktemp)"; capture "$out"
start=$(date +%s)
jq -n --arg t "$big_transcript" --arg c /tmp/proj \
  '{hook_event_name:"Stop", cwd:$c, transcript_path:$t}' \
  | GHOLA_CAPTURE_URL="$url" "$hook" Stop
wait "$server_pid" 2>/dev/null
elapsed=$(( $(date +%s) - start ))
check "large-transcript stop text" "final answer" "$(jq -r '.event.text' "$out")"
if [ "$elapsed" -lt 5 ]; then echo "ok   - large transcript handled fast (${elapsed}s)"; else echo "FAIL - large transcript too slow (${elapsed}s)"; fails=$((fails+1)); fi
rm -f "$big_transcript"

echo "---"
if [ "$fails" -eq 0 ]; then echo "ALL PASS"; exit 0; else echo "$fails FAILURE(S)"; exit 1; fi
