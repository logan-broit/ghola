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
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
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

// importLogsBin is the absolute path of the import-logs binary built
// once in TestMain and reused across every runImport call. Building
// once instead of `go run`-ing per invocation drops ~2s of cold-build
// cost off each test, which adds up when this test moves to CI.
var importLogsBin string

func TestMain(m *testing.M) {
	tmpdir, err := os.MkdirTemp("", "import-logs-bin-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: tempdir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(tmpdir)

	importLogsBin = filepath.Join(tmpdir, "import-logs")
	build := exec.Command("go", "build", "-o", importLogsBin, ".")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "TestMain: go build: %v\n", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

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
//
// workspace_id is required by validateIngest (see
// _chapterhouse/ch-server/internal/handler/episodic.go); the ping
// uses a fixed sentinel UUID so this canary row is upserted-once and
// doesn't accumulate per workspace. Real test runs use per-run
// workspace UUIDs separately.
func pingChapterhouse(baseURL, apiKey string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	body := map[string]any{
		"session": map[string]any{
			"id":           "00000000-0000-0000-0000-0000000000ff",
			"user_id":      userID,
			"workspace_id": "00000000-0000-0000-0000-0000000000fe",
			"started_at":   time.Now().UTC().Format(time.RFC3339),
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

	cmd := exec.CommandContext(ctx, importLogsBin, args...)
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

// TestImport_GithubAdapter_E2E imports a github-bundle fixture through
// the real binary and verifies the data round-tripped to chapterhouse:
//
//  1. import-logs reports imported>0 / failed==0 (the binary's own
//     summary already proves the workspace_id-bearing ingest path
//     succeeded; without the fix from the prior commit this would 400
//     and surface as failed).
//
//  2. POST /v1/episodic/query_keyword for a distinctive substring
//     present in exactly one fixture event returns at least one hit,
//     and that hit's raw_event JSONB carries the github adapter's
//     stamped tag (e.g. "era:v14", "type:issue"). The github adapter
//     packs tags into NormalizedEvent.Metadata["tags"]; the ingestor
//     marshals metadata into core.Event.RawEvent, which chapterhouse
//     persists as events.raw_event. Asserting the tag survived the
//     full path proves both the adapter's tag-stamping and the
//     ingestor's metadata-passthrough are wired through to the DB.
//
// The query uses /v1/episodic/query_keyword (FTS-only): no embedding
// needed, deterministic ranking, and the fixture content includes a
// proper-noun-style token ("manticore-route") that is exactly the kind
// of literal-phrase match keyword search exists for.
func TestImport_GithubAdapter_E2E(t *testing.T) {
	chapterhouseURL := os.Getenv("CHAPTERHOUSE_URL")
	if chapterhouseURL == "" {
		chapterhouseURL = defaultChapterhouseURL
	}
	apiKey := os.Getenv("CHAPTERHOUSE_API_KEY")
	if apiKey == "" {
		apiKey = defaultAPIKey
	}

	if err := pingChapterhouse(chapterhouseURL, apiKey); err != nil {
		t.Skipf("compose stack unreachable at %s: %v", chapterhouseURL, err)
	}

	t.Setenv("CHAPTERHOUSE_API_KEY", apiKey)
	t.Setenv("CHAPTERHOUSE_URL", chapterhouseURL)

	// Wire the guild client through the import-logs binary so events get
	// embedded at ingest time. Default to the compose-stack guild port;
	// honor an operator override so the test runs against a non-dev stack.
	// Skip the embedding-required E2E path with a clear message if guild
	// is unreachable rather than failing on a network error during the
	// real import — that surfaces "guild down" instead of "embedding bug".
	embedderURL := envOr("EMBEDDING_URL", "http://localhost:8082")
	if !pingGuild(embedderURL) {
		t.Skipf("guild unreachable at %s; embedding population path can't be exercised", embedderURL)
	}
	t.Setenv("EMBEDDING_URL", embedderURL)
	t.Setenv("EMBEDDING_MODEL", envOr("EMBEDDING_MODEL", "qwen3-embedding"))

	corpusRoot, err := filepath.Abs("testdata/integration-github")
	require.NoError(t, err, "resolve github corpus root")

	// Per-run workspace UUID isolates this test's data from prior runs.
	// Same trade-off as TestImport_BootstrapCorpus: dev DB accumulates
	// rows because chapterhouse exposes no session-delete endpoint.
	workspace := uuid.New().String()

	resumeState := filepath.Join(t.TempDir(), "imported.txt")

	binArgs := []string{
		"--workspace=" + workspace,
		"--user=" + userID,
		"--source=github:" + corpusRoot,
		"--resume-state=" + resumeState,
		"--batch-size=8",
	}

	got := runImport(t, binArgs)
	require.Equal(t, 0, got.failed,
		"github import: failed should be zero, got summary %+v", got)
	require.Greater(t, got.imported, 0,
		"github import: every bundle record should produce an imported session")

	// Bundle holds 4 thread records on 4 lines of one .jsonl file. The
	// github adapter walks per-record (not per-file), so imported counts
	// records, not files — make the expectation explicit.
	require.Equal(t, 4, got.imported,
		"github import: 4 thread records in fixture -> 4 imported sessions")

	// Query keyword for a distinctive token from one specific fixture
	// thread (#1001, era:v14, type:issue). The token is unique across
	// the bundle so the FTS hit can only come from this thread's events.
	hits := queryEpisodicKeyword(t, chapterhouseURL, apiKey,
		userID, workspace, "manticore-route")
	require.NotEmpty(t, hits,
		"keyword query for distinctive fixture token must hit at least once")

	// Drill into the first hit's raw_event JSONB. The github adapter
	// stamps tags into NormalizedEvent.Metadata["tags"] as a JSON-
	// encoded string; the ingestor marshals Metadata into raw_event's
	// metadata field. Confirming era:v14 + type:issue is present here
	// is what proves the adapter's tag stamping survived round-trip.
	hit := hits[0]
	tagsForHit := extractTagsFromRawEvent(t, hit)
	assert.Contains(t, tagsForHit, "era:v14",
		"expected era:v14 tag (stamped by github adapter on issue#1001) "+
			"to round-trip through ingest into events.raw_event.metadata.tags; "+
			"hit raw_event=%s", string(hit.RawEvent))
	assert.Contains(t, tagsForHit, "type:issue",
		"expected type:issue tag to round-trip through ingest")

	// Top-level tags lift: episodic.events.tags is a text[] with a gin
	// index (episodic_events_tags_gin). The github adapter populates
	// NormalizedEvent.Tags; the ingestor copies onto core.Event.Tags;
	// chapterhouse persists to the top-level column. Asserting these
	// here proves the gin-index path is live — WHERE tags @> ARRAY[...]
	// queries will be index-served, not full-scan over raw_event jsonb.
	require.NotEmpty(t, hit.Tags,
		"top-level events.tags must be populated (gin-indexed); empty hit.Tags means the lift never happened. raw_event=%s",
		string(hit.RawEvent))
	assert.Contains(t, hit.Tags, "era:v14",
		"expected era:v14 on top-level hit.tags so gin-indexed era filters work")
	assert.Contains(t, hit.Tags, "type:issue",
		"expected type:issue on top-level hit.tags")

	// Parity: top-level tags must equal metadata-decoded tags. Same
	// stamp source, two storage locations. Divergence here would mean
	// the adapter's two writes drifted — caught at integration time
	// rather than after a real recall returns the wrong filter slice.
	assert.ElementsMatch(t, tagsForHit, hit.Tags,
		"top-level events.tags must match raw_event.metadata.tags (both written from same stamp pass)")

	// Embedding population: pin the bug-fix contract end-to-end. The
	// import path used to POST events without an embedding (bug surfaced
	// by seeding-eval — 266/266 NULL-embedding events on the bulk-import
	// workspace, breaking the 5-tier hybrid recall pipeline). With the
	// fix wired, EMBEDDING_URL must be set in the test env so import-logs
	// runs the guild client, and the resulting events.embedding column
	// must be non-NULL on every imported row.
	//
	// We probe the DB directly via psql (the chapterhouse server exposes
	// no endpoint that surfaces the raw vector — and adding one just for
	// this test would balloon the surface area). The compose stack ships
	// the postgres container under a known name; if the operator reshapes
	// the stack the assertion skips with a clear message rather than a
	// confusing failure.
	if pgContainer := os.Getenv("CHAPTERHOUSE_PG_CONTAINER"); pgContainer != "" || haveDockerContainer(t, "docker-compose-postgres-1") {
		if pgContainer == "" {
			pgContainer = "docker-compose-postgres-1"
		}
		assertEmbeddingsPopulated(t, pgContainer, workspace, got.imported)
	} else {
		t.Logf("postgres container not reachable — skipping embedding-NOT-NULL DB probe " +
			"(set CHAPTERHOUSE_PG_CONTAINER to override)")
	}
}

// envOr returns the env var or the fallback. Local helper so the test
// file doesn't depend on the binary's package-internal helper.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// pingGuild returns true iff the guild embedder responds on /v1/embeddings
// — used to skip the embedding-required E2E with a clear message rather
// than failing on a network error mid-import.
func pingGuild(baseURL string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body := bytes.NewReader([]byte(`{"model":"qwen3-embedding","input":"ping"}`))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(baseURL, "/")+"/v1/embeddings", body)
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// haveDockerContainer reports whether `docker exec <name> true` succeeds
// — i.e. the named container is up and reachable. Used to gate the
// embedding-population probe so the test still skips cleanly when the
// compose stack uses a different container name.
func haveDockerContainer(t *testing.T, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "docker", "exec", name, "true").Run() == nil
}

// assertEmbeddingsPopulated runs psql in the postgres container and
// asserts every event in `workspace` has a non-NULL embedding. Sample
// values: see internal/importlogs/ingestor.go's Embedder plumbing.
//
// The query joins episodic.events to episodic.session_workspaces so
// only this test's per-run rows are counted — without that join the
// assertion would scan the global table and fail-loose on any prior
// run that left NULL rows behind (which the bug fix expressly does not
// retroactively repair).
func assertEmbeddingsPopulated(t *testing.T, pgContainer, workspace string, expectedTotal int) {
	t.Helper()

	query := `
SELECT
  COUNT(*) AS total,
  COUNT(*) FILTER (WHERE e.embedding IS NOT NULL) AS populated,
  COUNT(*) FILTER (WHERE e.embedding IS NULL) AS empty
FROM episodic.events e
JOIN episodic.session_workspaces sw ON e.session_id = sw.session_id
WHERE sw.workspace_id = '` + workspace + `';
`
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "docker", "exec", pgContainer,
		"psql", "-U", "memory_api", "-d", "memories",
		"-A", "-t", "-F", ",", "-c", query)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		t.Fatalf("psql probe failed: %v\nstdout=%s\nstderr=%s",
			err, stdout.String(), stderr.String())
	}

	line := strings.TrimSpace(stdout.String())
	parts := strings.Split(line, ",")
	require.Len(t, parts, 3,
		"unexpected psql output (want total,populated,empty): %q", line)

	total := mustAtoi(t, parts[0])
	populated := mustAtoi(t, parts[1])
	empty := mustAtoi(t, parts[2])

	t.Logf("embedding population: total=%d populated=%d empty=%d (workspace=%s)",
		total, populated, empty, workspace)

	require.Greater(t, total, 0,
		"no events landed in workspace=%s — ingest path is broken", workspace)
	assert.Equal(t, 0, empty,
		"workspace=%s has %d events with embedding IS NULL — the import-logs "+
			"embedding plumbing regressed; events should be embedded at ingest time",
		workspace, empty)
	assert.Equal(t, total, populated,
		"every event should have a populated embedding; got %d/%d populated",
		populated, total)

	// Sanity: number of rows in the workspace should not be wildly less
	// than the binary's imported count. Allow >= because imported counts
	// sessions, while events fanned out per-thread can exceed that —
	// what we want is "non-trivial coverage", not exact parity.
	require.GreaterOrEqual(t, total, expectedTotal,
		"workspace event count (%d) below imported-session count (%d) — "+
			"some sessions never landed events", total, expectedTotal)
}

// episodicHit is the narrow projection of the chapterhouse keyword
// query response we care about: id + raw_event (the JSONB column the
// ingestor packs metadata into). text and tags (top-level array) are
// surfaced for diagnostics on assertion failure.
type episodicHit struct {
	ID       string          `json:"id"`
	Text     *string         `json:"text"`
	Tags     []string        `json:"tags"`
	RawEvent json.RawMessage `json:"raw_event"`
}

// queryEpisodicKeyword issues POST /v1/episodic/query in multi-ranking
// mode (rankings=["fts"]) and returns the FTS sub-list projected onto
// the local episodicHit shape. Post-A8 the legacy /v1/episodic/query_keyword
// path is gone — the only entry point is the multi-ranking handler.
//
// The multi-ranking response carries the event_id+text on each hit
// (via the MultiRankingHit shape) but NOT the full event surface
// (raw_event, top-level tags). This integration test asserts on those
// fields, so this helper does a second hop: for each FTS hit it queries
// chapterhouse's Postgres directly via psql to source the full event
// row. Skipped silently if the postgres container isn't reachable —
// same posture as assertEmbeddingsPopulated.
func queryEpisodicKeyword(t *testing.T, baseURL, apiKey, userID, workspace, queryText string) []episodicHit {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	body := map[string]any{
		"user_id":      userID,
		"workspace_id": workspace,
		"query_text":   queryText,
		"limit":        10,
		"rankings":     []string{"fts"},
	}
	buf, err := json.Marshal(body)
	require.NoError(t, err)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+"/v1/episodic/query", bytes.NewReader(buf))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "POST /v1/episodic/query (rankings=fts)")
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("/v1/episodic/query returned %d: %s", resp.StatusCode, string(buf))
	}

	// Multi-ranking response shape: parallel sub-lists keyed by tier.
	// Decode the fts sub-list; each MultiRankingHit carries event_id +
	// text, but raw_event / top-level tags require a second hop via
	// psql to fill in.
	var multiResp struct {
		FTS []struct {
			EventID *string `json:"event_id"`
			Text    *string `json:"text"`
		} `json:"fts"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&multiResp))

	hits := make([]episodicHit, 0, len(multiResp.FTS))
	for _, h := range multiResp.FTS {
		if h.EventID == nil {
			continue
		}
		hit := episodicHit{ID: *h.EventID, Text: h.Text}
		hydrateEpisodicHitFromPG(t, &hit)
		hits = append(hits, hit)
	}
	return hits
}

// hydrateEpisodicHitFromPG fills hit.RawEvent + hit.Tags from a direct
// psql probe against the chapterhouse postgres container. Multi-
// ranking hits carry only event_id + text; the integration assertions
// need the full event surface (raw_event JSONB, top-level tags array).
//
// Mirrors assertEmbeddingsPopulated's container-discovery convention:
// honors CHAPTERHOUSE_PG_CONTAINER, falls back to docker-compose-postgres-1,
// skips with a clear log line if neither resolves.
func hydrateEpisodicHitFromPG(t *testing.T, hit *episodicHit) {
	t.Helper()
	pgContainer := os.Getenv("CHAPTERHOUSE_PG_CONTAINER")
	if pgContainer == "" && haveDockerContainer(t, "docker-compose-postgres-1") {
		pgContainer = "docker-compose-postgres-1"
	}
	if pgContainer == "" {
		t.Logf("postgres container not reachable — hit %s carries only id+text from "+
			"multi-ranking response (raw_event/tags assertions will fail unless "+
			"CHAPTERHOUSE_PG_CONTAINER is set)", hit.ID)
		return
	}

	// Single-row probe: jsonb raw_event and text[] tags. Query is
	// scoped by event id; the per-user ACL is enforced upstream by
	// the chapterhouse query that surfaced the id, so a direct probe
	// here is safe in the integration env.
	cmd := exec.Command("docker", "exec", pgContainer,
		"psql", "-U", "memory_api", "-d", "memories", "-tA", "-F", "\x1f",
		"-c", "SELECT raw_event::text, COALESCE(array_to_json(tags)::text, '[]') "+
			"FROM episodic.events WHERE id = '"+hit.ID+"'")
	out, err := cmd.Output()
	require.NoError(t, err, "psql probe for event %s failed", hit.ID)

	line := strings.TrimSpace(string(out))
	if line == "" {
		t.Fatalf("event %s not found in episodic.events", hit.ID)
	}
	parts := strings.SplitN(line, "\x1f", 2)
	require.Len(t, parts, 2,
		"psql output had %d fields (expected 2): %q", len(parts), line)

	hit.RawEvent = json.RawMessage(parts[0])
	require.NoError(t, json.Unmarshal([]byte(parts[1]), &hit.Tags),
		"decode tags JSON %q", parts[1])
}

// extractTagsFromRawEvent walks the raw_event JSONB into the github
// adapter's metadata.tags field. The adapter writes tags as a JSON-
// encoded string under metadata["tags"] (see
// internal/importlogs/adapters/github/adapter.go around line 228) — so
// raw_event.metadata.tags is the literal JSON array text, and a second
// Unmarshal recovers the []string.
func extractTagsFromRawEvent(t *testing.T, hit episodicHit) []string {
	t.Helper()
	var raw struct {
		SourceTool string            `json:"source_tool"`
		Metadata   map[string]string `json:"metadata"`
	}
	require.NoError(t, json.Unmarshal(hit.RawEvent, &raw),
		"decode raw_event: %s", string(hit.RawEvent))

	tagsJSON, ok := raw.Metadata["tags"]
	require.True(t, ok,
		"raw_event.metadata.tags missing; metadata=%+v raw=%s",
		raw.Metadata, string(hit.RawEvent))

	var tags []string
	require.NoError(t, json.Unmarshal([]byte(tagsJSON), &tags),
		"decode tags JSON %q", tagsJSON)
	return tags
}
