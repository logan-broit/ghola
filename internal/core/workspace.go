package core

import "github.com/google/uuid"

// NS_workspace is the namespace UUID for cwd-derived workspace IDs.
// Distinct from any other namespace in the project (e.g. the bench's
// per-question namespace in longmemeval-ghola/backends/ghola_v2.py).
// Different namespaces, different inputs, never collide.
//
// This constant is load-bearing for "same cwd everywhere -> same
// workspace_id." Changing it divorces every live session from its
// historical workspace; treat as immutable.
var NS_workspace = uuid.MustParse("8e3a4c2d-1b5f-4d7a-9c8e-0f1a2b3c4d5e")

// WorkspaceForCwd derives the workspace UUID for a working directory.
// Used at session_start when the caller doesn't pass workspace_id
// explicitly but session metadata carries cwd. uuid5 (SHA-1 over
// namespace + name) gives reproducibility — the same cwd always maps
// to the same UUID across machines and runs, which is what the bench's
// pre-ingest backfill relies on.
func WorkspaceForCwd(cwd string) uuid.UUID {
	return uuid.NewSHA1(NS_workspace, []byte(cwd))
}
