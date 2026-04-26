//go:build integration

package main_test

// TestImport_BootstrapCorpus runs the real import-logs binary against
// the compose chapterhouse stack and a hermetic fixture corpus on disk.
// It is the ship-gate for PR2: PR2 ships only when this passes.
//
// The test exercises the CLI surface (os/exec) rather than reaching
// into the importlogs package directly, per the project's
// "tests through the production API" rule. Two binary invocations
// against the same fixture root + the same resume-state file confirm:
//
//  1. First run: imported == fileCount, failed == 0.
//     fileCount counts every *.jsonl under the corpus root recursively
//     so this assertion proves the recursive walk + the subagent
//     session-derivation path both fired.
//  2. Re-run: imported == 0, skipped == fileCount, failed == 0.
//     This proves --resume reads the state file written on the first
//     run; a regression that broke resume-state would surface here.
//
// The test skips (does not fail) if the compose stack is unreachable,
// so a developer without the stack up locally can still run
// `go test ./...` without integration build tag and not be blocked.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const (
	defaultChapterhouseURL = "http://localhost:8080"
	defaultAPIKey          = "dev-token" // compose stack runs AUTH_PROVIDER=default
	// userID is a fixed dev UUID — chapterhouse rejects non-UUID user
	// fields. The fixed value keeps test output predictable when
	// inspecting the stack manually.
	userID = "00000000-0000-0000-0000-000000000001"
)

func TestImport_BootstrapCorpus(t *testing.T) {
	chapterhouseURL := os.Getenv("CHAPTERHOUSE_URL")
	if chapterhouseURL == "" {
		chapterhouseURL = defaultChapterhouseURL
	}

	// Honor any operator-set CHAPTERHOUSE_API_KEY so this test works
	// against a non-dev stack (where AUTH_PROVIDER=default would 401 on
	// the literal "dev-token"). The subsequent t.Setenv re-asserts the
	// resolved value so the binary's env lookup sees exactly what the
	// ping used — preserving the operator's choice while keeping the
	// binary's auth scoped to this test.
	apiKey := os.Getenv("CHAPTERHOUSE_API_KEY")
	if apiKey == "" {
		apiKey = defaultAPIKey
	}

	if err := pingChapterhouse(chapterhouseURL, apiKey); err != nil {
		t.Skipf("compose stack unreachable at %s: %v", chapterhouseURL, err)
	}

	t.Setenv("CHAPTERHOUSE_API_KEY", apiKey)
	t.Setenv("CHAPTERHOUSE_URL", chapterhouseURL)

	corpusRoot, err := filepath.Abs("testdata/integration")
	require.NoError(t, err, "resolve corpus root")

	fileCount := countJSONL(t, corpusRoot)
	require.GreaterOrEqual(t, fileCount, 3, "fixture corpus should contain at least 3 .jsonl files")

	// Fresh resume-state file per test run: otherwise a previous run
	// would pre-skip everything and the imported assertion would fail.
	resumeState := filepath.Join(t.TempDir(), "imported.txt")

	// Per-run workspace UUID isolates this test's data from prior runs
	// in the same compose stack DB. We don't reset the DB; chapterhouse
	// upserts by id and the binary's `imported` counter increments per
	// successful POST regardless of insert/update outcome, so re-imports
	// into a fresh workspace are still counted as imports.
	//
	// Trade-off: the dev DB accumulates ~fileCount session rows per run.
	// Chapterhouse exposes no session-delete endpoint today (only
	// /v1/episodic/forget, which soft-deletes events, not sessions), so
	// there's no clean t.Cleanup we can wire up here. When this test
	// moves to CI, the path forward is one of: (a) chapterhouse grows a
	// session-delete API, (b) CI provisions a per-test database, or
	// (c) the test switches to a fixed workspace UUID and relies on
	// chapterhouse's idempotent upsert semantics. Documenting the
	// choice rather than papering over the limitation is intentional.
	workspace := uuid.New().String()

	binArgs := []string{
		"run", ".",
		"--workspace=" + workspace,
		"--user=" + userID,
		"--source=jsonl-family:" + corpusRoot,
		"--resume-state=" + resumeState,
		"--batch-size=8",
	}

	// First run: every fixture file should be imported.
	first := runImport(t, binArgs)
	require.Equal(t, 0, first.failed,
		"first run: failed should be zero, got summary %+v", first)
	require.Equal(t, fileCount, first.imported,
		"first run: every fixture .jsonl should produce an imported session")

	// Re-run with the same args + same resume-state: idempotency check.
	second := runImport(t, binArgs)
	require.Equal(t, 0, second.failed,
		"re-run: failed should be zero, got summary %+v", second)
	require.Equal(t, 0, second.imported,
		"re-run: nothing should be imported a second time")
	require.Equal(t, fileCount, second.skipped,
		"re-run: every previously-imported session should be skipped via resume-state")
}

// pingChapterhouse confirms the compose stack is up. There is no
// /healthz endpoint on chapterhouse, but POST /v1/episodic/ingest with
// an empty events array is a cheap upsert-only round-trip the server
// answers with 200 — same shape the binary uses.
//
// The session ID is intentionally fixed at "...000000ff" rather than
// per-run unique. Chapterhouse upserts on session ID, so this single
// row exists exactly once across all test runs in this workspace and
// never accumulates. We deliberately exercise the DB write path
// instead of using a GET-only probe: a GET would only confirm the
// HTTP listener is alive, missing partial outages where the API
// answers but the DB write path is broken — exactly the failure mode
// that would later produce confusing test failures rather than a
// clean skip.
func pingChapterhouse(baseURL, apiKey string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	body := map[string]any{
		"session": map[string]any{
			"id":         "00000000-0000-0000-0000-0000000000ff",
			"user_id":    userID,
			"started_at": time.Now().UTC().Format(time.RFC3339),
		},
		"events": []any{},
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+"/v1/episodic/ingest", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("ping returned %d", resp.StatusCode)
	}
	return nil
}

// summary captures the binary's final stdout line.
type summary struct {
	imported, skipped, failed, total int
}

var summaryRE = regexp.MustCompile(`imported=(\d+) skipped=(\d+) failed=(\d+) total=(\d+)`)

func runImport(t *testing.T, args []string) summary {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "go", args...)
	// Run from the cmd/import-logs package dir so `go run .` resolves
	// to the binary under test.
	cmd.Dir = mustModuleRelativeDir(t)
	cmd.Env = os.Environ()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("import-logs run failed: %v\nstdout:\n%s\nstderr:\n%s",
			err, stdout.String(), stderr.String())
	}

	m := summaryRE.FindStringSubmatch(stdout.String())
	if m == nil {
		t.Fatalf("could not find summary line in stdout:\n%s\nstderr:\n%s",
			stdout.String(), stderr.String())
	}
	s := summary{
		imported: mustAtoi(t, m[1]),
		skipped:  mustAtoi(t, m[2]),
		failed:   mustAtoi(t, m[3]),
		total:    mustAtoi(t, m[4]),
	}
	t.Logf("import-logs summary: %+v", s)
	return s
}

// mustModuleRelativeDir returns the absolute path of cmd/import-logs.
// The test file lives there, so the working directory at test time IS
// that path — we just resolve it absolutely so go-run is unambiguous
// even if a future test runner cd's elsewhere first.
func mustModuleRelativeDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	require.NoError(t, err)
	return wd
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	require.NoError(t, err, "parse %q as int", s)
	return n
}

// countJSONL walks root and counts files ending in .jsonl. The adapter
// under test uses the same recursive pattern; matching the count here
// makes the assertion "every fixture file produces a session" load-
// bearing on the recursive walk.
func countJSONL(t *testing.T, root string) int {
	t.Helper()
	var n int
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if filepath.Ext(path) == ".jsonl" {
			n++
		}
		return nil
	})
	require.NoError(t, err, "walk corpus root")
	return n
}
