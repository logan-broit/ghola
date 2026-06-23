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

## The path identity gotcha

`WorkspaceForCwd` is `uuid5(SHA-1 over cwd)` — deterministic on the
*string*. This means the same project reached by a different path gets
a different workspace and **cannot see its own memory**. The most common
case: **git worktrees**. Every `git worktree add` creates a new
directory (e.g. `~/repo-feature-branch`), so each worktree is a distinct
cwd = distinct workspace = independent memory.

For the intended use case (agent memory scoped to a codebase) this is
mostly fine — a feature branch worktree is arguably a different working
context. But it also means:

- **Symlinks and relocations fork memory.** Moving a project directory
  (or accessing it through a symlink) gives a new workspace id. The old
  memory is still in Postgres under the old id — it is not lost, just
  unreachable from the new path.
- **The amnesia is silent.** Recall returns zero hits with no error.
  The agent doesn't know it has memory under a different path; it just
  behaves as if it has never seen the project before.

This is a **doc-level gotcha, not a bug** — the uuid5 mapping is
intentional and has a stable contract (`workspace_test.go`). A
structural fix (path normalization via `realpath`, or workspace
aliases) would re-key all existing memories, so it is a design decision
for a future iteration, not a patch.

**Practical guidance:** if you use worktrees or symlinks, pass
`workspace_id` explicitly to `session_start` and `recall` to share
memory across paths that refer to the same project. Or accept the
per-path isolation as a feature.

## Adding a session to another workspace

To tag an existing session into an additional workspace mid-conversation
(when topic drift makes it relevant elsewhere), call
`expand_session_workspace`. Idempotent. Returns `409 CONFLICT` if
called against a session that has already been consolidated — workspace
membership is fixed once consolidation runs.
