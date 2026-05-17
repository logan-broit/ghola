package core

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

// TestWorkspaceForCwd_Deterministic pins the load-bearing property:
// same cwd in -> same UUID out, every run, every machine. Without
// this, the bench backfill (which mints workspaces ahead of ingest
// via the same algorithm) would produce divergent IDs from live
// session_start.
func TestWorkspaceForCwd_Deterministic(t *testing.T) {
	a := WorkspaceForCwd("/path/to/project")
	b := WorkspaceForCwd("/path/to/project")
	assert.Equal(t, a, b, "same cwd must produce same workspace UUID")

	c := WorkspaceForCwd("/path/to/other-project")
	assert.NotEqual(t, a, c, "different cwd must produce different UUID")
}

// TestWorkspaceForCwd_StableValue pins the actual UUID one cwd
// produces. This is a regression gate: changing NS_workspace, the
// uuid version, or the input encoding shifts every derived ID and
// silently divorces live sessions from any backfill that already
// landed. The expected value below was computed once via:
//
//   uuid5(NS_workspace, "/path/to/project")
//
// using NS_workspace = "8e3a4c2d-1b5f-4d7a-9c8e-0f1a2b3c4d5e".
func TestWorkspaceForCwd_StableValue(t *testing.T) {
	got := WorkspaceForCwd("/path/to/project")
	want := uuid.MustParse("59a79454-1d87-5840-a389-61ae16c382bc")
	assert.Equal(t, want, got)
}
