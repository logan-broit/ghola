# Workspaces

A *workspace* is the per-query scoping primitive. Each session belongs
to one or more workspaces (N:N via `episodic.session_workspaces`).
Recall queries scope to a single workspace; the candidate pool shrinks
from "everything this user ever stored" to "everything in this
workspace."

## Resolution at `session_start`

1. Caller passes `workspace_id` explicitly → use it.
2. Caller passes `cwd` → derive `uuid5(NS_workspace, cwd)`.
3. Neither → `400 BAD_REQUEST`.

See `internal/core/workspace.go` for the resolver and
`internal/core/workspace_test.go` for the uuid5 derivation contract.

## Adding a session to another workspace

To tag an existing session into an additional workspace mid-conversation
(when topic drift makes it relevant elsewhere), call
`expand_session_workspace`. Idempotent. Returns `409 CONFLICT` if
called against a session that has already been consolidated — workspace
membership is fixed once consolidation runs.
