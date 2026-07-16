#!/usr/bin/env bash
# ghola-capture-hook.sh -- Claude Code UserPromptSubmit + Stop hook.
#
# Reads the hook JSON on stdin, extracts cwd + the turn text, and POSTs
# {event:{type,text}, cwd} to the ghola daemon's /v1/record. The daemon
# fills user_id from AUTH_DEFAULT_USER.
#
# Fire-and-forget: a down or slow ghola must never block or noise Claude
# Code, so every failure is swallowed and the script ALWAYS exits 0.
#
# Mode: $1 if given, else the payload's .hook_event_name.
#   UserPromptSubmit -> type "user",      text = .prompt
#   Stop             -> type "assistant", text = last assistant message
#                       in the transcript (.transcript_path JSONL)
#
# Endpoint override (tests): GHOLA_CAPTURE_URL.

url="${GHOLA_CAPTURE_URL:-http://localhost:7421/v1/record}"
max_bytes=16384

command -v jq >/dev/null 2>&1 || exit 0
payload="$(cat)"

mode="${1:-}"
if [ -z "$mode" ]; then
  mode="$(printf '%s' "$payload" | jq -r '.hook_event_name // empty' 2>/dev/null)"
fi
cwd="$(printf '%s' "$payload" | jq -r '.cwd // empty' 2>/dev/null)"

type=""
text=""
case "$mode" in
  UserPromptSubmit)
    type="user"
    text="$(printf '%s' "$payload" | jq -r '.prompt // empty' 2>/dev/null)"
    ;;
  Stop)
    type="assistant"
    transcript="$(printf '%s' "$payload" | jq -r '.transcript_path // empty' 2>/dev/null)"
    [ -n "$transcript" ] && [ -f "$transcript" ] || exit 0
    # Last assistant line's text blocks, joined. null-safe: a missing
    # last/content yields "" and we exit below.
    #
    # Bound the read: Claude Code waits on this hook, and the full
    # transcript can be huge. The last assistant message always lives
    # at the tail, so only the last 400 lines are ever considered.
    text="$(tail -n 400 "$transcript" | jq -rs '
      map(select(.type == "assistant"))
      | last
      | (.message.content // [])
      | map(select(.type == "text") | .text)
      | join("\n")
    ' 2>/dev/null)"
    ;;
  *)
    exit 0
    ;;
esac

[ -n "$text" ] || exit 0

# Byte-cap the captured text. This is a raw byte cut, not rune-aware: a
# mid-rune split makes the trailing partial UTF-8 sequence invalid, so
# the re-encoding jq below either drops it (jq -n fails on invalid
# UTF-8 input) or emits U+FFFD for it, depending on where the cut lands.
text="$(printf '%s' "$text" | head -c "$max_bytes")"

body="$(jq -n --arg type "$type" --arg text "$text" --arg cwd "$cwd" \
  '{event: {type: $type, text: $text}, cwd: $cwd}' 2>/dev/null)" || exit 0

printf '%s' "$body" | curl -s --max-time 1 -X POST \
  -H 'Content-Type: application/json' --data-binary @- "$url" >/dev/null 2>&1 || true

exit 0
