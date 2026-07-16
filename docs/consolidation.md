# Consolidation

The episodic→semantic consolidation pipeline
(`_chapterhouse/ch-server/internal/consolidation`, `RunWorkspace`) turns a
workspace's closed sessions into semantic-tier mnemes: reconcile any
session missing an L1 embedding, cluster via mentat's HDBSCAN, match each
cluster against existing mnemes and insert/reinforce
(overlap-match/apply), enrich the result with selection-first
representative excerpts (MMR over per-session events), and — when an LLM
is configured — attach a per-cluster label and roll the labels up into a
level-2 workspace digest paragraph. See [layout.md](layout.md) and
[recall-pipeline.md](recall-pipeline.md) for how the output feeds recall.

The same `RunWorkspace` call backs both the nightly worker schedule and
the manual HTTP trigger, so both paths behave identically and can't race
each other (below).

## Env vars

| Env var | Default | What |
|---|---|---|
| `CONSOLIDATE_WORKSPACES` | (unset) | CSV of workspace UUIDs to consolidate. Empty is the kill-switch — consolidation does not run at all. |
| `CONSOLIDATE_HOUR` | `2` | Local hour (0-23) the nightly worker run fires at. Out-of-range values are clamped into `[0,23]` (a warning is logged) rather than silently rolling to an adjacent day the way `time.Date` normalization would. |
| `CONSOLIDATE_LLM_URL` | (unset) | OpenAI-compatible chat-completions URL for per-cluster labels + the workspace digest. Unset means selection-only mnemes — content and excerpts still get written, just no label or digest paragraph. |
| `CONSOLIDATE_LLM_MODEL` | `local-model` | Chat model id. |
| `CONSOLIDATE_LLM_API_KEY` | (unset) | Bearer token for the chat endpoint, if it needs one. |

`MENTAT_URL` is also required — both the nightly worker and the manual
trigger disable consolidation entirely when it's unset (clustering has
nothing to do without mentat). The digest embedder reuses the server's
regular `EMBEDDING_*` config (`EMBEDDING_URL`, `EMBEDDING_MODEL`,
`EMBEDDING_DIMENSIONS`); an unset `EMBEDDING_URL` skips the digest
write the same way an unset LLM skips the label.

## Manual trigger

`POST /v1/semantic/consolidate {"workspace": "<uuid>"}` runs
`RunWorkspace` synchronously and returns when the batch completes.
Concurrency is enforced with a per-workspace Postgres advisory lock held
for the run's full duration: a second call for the same workspace
(another manual trigger, or a nightly tick landing mid-run) gets `409
Conflict` immediately rather than racing the first run. On a multi-minute
run the HTTP response itself can be lost to a client-side timeout or
disconnect — the run is detached from the request context and keeps
running server-side regardless, so a lost response is not a failed run.
Retrying the call while it's still in flight returns `409` until the lock
releases; a retry after it releases either finds fresh output (first call
actually finished) or starts a new run.

## MCP verb

`consolidate_workspace` (agent-facing, unlike the hidden session-level
`consolidate` op) proxies to the same manual-trigger endpoint. It exists
for the case where an agent is about to have its own context cleared or
compacted and wants the semantic tier to have fresh, readable content
before that happens. It's synchronous from the agent's point of view too,
with a 10-minute proxy timeout headroom above the HTTP client's normal
30s default.

## Nightly schedule

`cmd/worker` reads `CONSOLIDATE_WORKSPACES`; if it's non-empty (and
`MENTAT_URL` is set) it starts a loop that sleeps until the next
`CONSOLIDATE_HOUR:00` local time, runs `RunWorkspace` for each configured
workspace in turn, and repeats. A workspace whose lock is already held
(e.g. a manual trigger caught it mid-run) is logged and skipped for that
tick rather than treated as an error; the next night retries. Other
per-workspace failures (e.g. mentat down) are logged and don't abort the
rest of the batch.

A brand-new, single-topic workspace can legitimately consolidate to zero
mnemes: mentat's HDBSCAN runs with `allow_single_cluster=False` (the
library default), which never emits a cluster when the *entire* corpus is
one dense blob — there's no density contrast for anything to be a
cluster relative to, so every point comes back labeled noise (`-1`) and
`RunWorkspace` writes nothing. This is expected behavior, not a failure:
it resolves itself once the workspace's session history diversifies
enough for HDBSCAN to see more than one direction in the embedding
space.

## Session hygiene

Nothing on the MCP path calls `session_end`, so without hygiene a
`(user, workspace)` reuses one session forever: L1 pooling and
episodic->semantic consolidation never fire, and orphaned second-open
sessions idle indefinitely. Two seams, sharing one idle threshold, fix
this.

- **Idle threshold** — `GHOLA_SESSION_IDLE_HOURS` (whole hours, default
  `4`). Read once at daemon start into `Core.SessionIdleTimeout`. Set
  `0` to disable BOTH seams (the kill-switch); negative is rejected at
  startup.
- **Record-time staleness** — `findOpenSession` refuses to reuse a
  session whose last activity (its `last_event_at`, or `started_at`
  when it has no events) is older than the threshold. The record path
  mints a fresh session instead, so consolidation sees real episode
  boundaries. The stale session is left open (the hot path stays
  write-free); the sweep closes it. Recall's working-tier scoping uses
  the same helper, so a stale session also stops being the working-tier
  target -- correct, since its content is already drained to episodic.
- **Idle sweep** — the encoding worker's tick calls
  `Core.SweepIdleSession` for every session it walks. An open session
  idle past the threshold gets the full `SessionEnd` sequence
  (`MarkEnded` -> `Consolidate` flush -> `CloseSession`). This is the
  guaranteed-closure mechanism and also reaps orphaned second-open
  sessions. Sietch GC (existing, 7d after close) then removes the file.

Rollback: `GHOLA_SESSION_IDLE_HOURS=0` (redeploy) disables staleness +
sweep with no schema change.

### Auto-capture hooks (Claude Code)

`scripts/ghola-capture-hook.sh` captures user + assistant turns into
ghola. Wire it into the user-global `~/.claude/settings.json` (operator
applies; merge into any existing `hooks` block). The daemon must run
with `AUTH_DEFAULT_USER` set (the docker-compose stack does) so the
hook can omit `user_id`:

**[operator-applies]** merge into `~/.claude/settings.json`:

```json
{
  "hooks": {
    "UserPromptSubmit": [
      {
        "hooks": [
          { "type": "command", "command": "/home/loganb/ghola/scripts/ghola-capture-hook.sh UserPromptSubmit" }
        ]
      }
    ],
    "Stop": [
      {
        "hooks": [
          { "type": "command", "command": "/home/loganb/ghola/scripts/ghola-capture-hook.sh Stop" }
        ]
      }
    ]
  }
}
```

Removing these two entries fully disables capture. The hook is
fire-and-forget (`curl --max-time 1`, always exits 0), so a down ghola
never blocks Claude Code.

> The `command` path above points at the operator's canonical checkout
> (`/home/loganb/ghola/scripts/...`). If the operator runs the
> daemon/hooks from a different clone, adjust the path when applying.

## Rollback

Disable future runs by clearing `CONSOLIDATE_WORKSPACES` (redeploy the
worker with it empty — the kill-switch). To undo output already written,
archive the workspace's mnemes:

```sql
UPDATE semantic.mnemes SET state = 'archived' WHERE workspace_id = $1;
```

Archived mnemes are excluded from recall's semantic tier but stay in the
table (nothing is deleted), so this is reversible by flipping `state`
back to `'active'` if needed.
