package github_test

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/logan-broit/ghola/internal/importlogs"
	"github.com/logan-broit/ghola/internal/importlogs/adapters/github"
)

func TestNew_NameMatches(t *testing.T) {
	a := github.New()
	require.Equal(t, "github", a.Name())
}

func TestWalk_EmptyRootYieldsNothing(t *testing.T) {
	a := github.New()
	count := 0
	for range a.Walk(t.TempDir()) {
		count++
	}
	require.Equal(t, 0, count)
}

func TestNew_SatisfiesAdapterInterface(t *testing.T) {
	var _ importlogs.Adapter = github.New()
}

// walkOne returns the single SessionFile yielded by walking the
// fixture testdata. Tests call this rather than re-iterating in
// each so the failure mode (zero or many records yielded) is
// explicit at one site.
func walkOne(t *testing.T, a *github.Adapter, root string) importlogs.SessionFile {
	t.Helper()
	sfs := walkAll(t, a, root)
	require.Len(t, sfs, 1, "expected exactly one record under %s", root)
	return sfs[0]
}

// walkAll drains the Walk iterator into a slice. Test-only — we
// want every yielded SessionFile so multi-record / malformed
// fixtures can assert on full ordering and per-line outcomes.
func walkAll(t *testing.T, a *github.Adapter, root string) []importlogs.SessionFile {
	t.Helper()
	var sfs []importlogs.SessionFile
	for sf := range a.Walk(root) {
		sfs = append(sfs, sf)
	}
	return sfs
}

func TestParse_SingleRecord_ParsesAllEvents(t *testing.T) {
	a := github.New()
	sf := walkOne(t, a, "testdata/single")
	ns, err := a.Parse(sf)
	require.NoError(t, err)
	require.NotNil(t, ns)

	require.Equal(t, "github", ns.SourceTool)
	require.Equal(t, "github", ns.AgentKind)
	require.Equal(t, "7f3a9b2c-5e44-5a8d-9c12-8f1b4d2a6e90", ns.SessionID.String())
	require.NotNil(t, ns.Cwd)
	require.Equal(t, "vercel/next.js", *ns.Cwd)
	// git_branch is JSON null in the fixture; should remain unset.
	require.Nil(t, ns.GitBranch)

	// 4 events in fixture order: issue, pr, commit, commit. Type
	// is mapped to "user" (the canonical chapterhouse role for
	// externally-authored content). Bundle kind is preserved on
	// Metadata.tags via the type:<kind> tag (asserted in
	// TestParse_SingleRecord_StampsTags).
	require.Len(t, ns.Events, 4)
	for i, ev := range ns.Events {
		require.Equal(t, "user", ev.Type,
			"events[%d].Type must be canonical \"user\" (chapterhouse only accepts user|assistant|tool_result|system)", i)
	}

	// Content survives intact (sanity).
	require.Contains(t, ns.Events[0].Text, "Hydration mismatch")
	require.Contains(t, ns.Events[1].Text, "suppressHydrationWarning")
}

// TestParse_SingleRecord_PreservesEventIDs pins the contract that the
// github adapter sets NormalizedEvent.ID from the bundle's events[i].id
// verbatim. Without this lift the ingestor falls through to a derived
// UUID and the seeding-eval harness's Python-side ground-truth IDs
// (computed from the same upstream keys) never match what chapterhouse
// stores — so H2/recall scores read 0 across all variants by
// construction.
//
// The fixture's bundle event IDs are read from the JSONL itself rather
// than hard-coded so this test stays in sync if the fixture is
// regenerated.
func TestParse_SingleRecord_PreservesEventIDs(t *testing.T) {
	a := github.New()
	sf := walkOne(t, a, "testdata/single")

	// Pull the expected event IDs straight from the fixture so this test
	// can't drift from the JSONL. Decode lazily — only the ids are
	// load-bearing for this assertion.
	type bundleEvent struct {
		ID string `json:"id"`
	}
	type bundleRecord struct {
		Events []bundleEvent `json:"events"`
	}
	raw, err := os.ReadFile(sf.Path)
	require.NoError(t, err)
	var rec bundleRecord
	require.NoError(t, json.Unmarshal(raw, &rec))
	require.NotEmpty(t, rec.Events, "fixture must have events to pin against")

	wantIDs := make([]string, len(rec.Events))
	for i, ev := range rec.Events {
		require.NotEmpty(t, ev.ID, "fixture events[%d].id must be non-empty (test pins ID survival)", i)
		wantIDs[i] = ev.ID
	}

	ns, err := a.Parse(sf)
	require.NoError(t, err)
	require.Len(t, ns.Events, len(wantIDs))

	for i, ev := range ns.Events {
		assert.Equal(t, wantIDs[i], ev.ID,
			"events[%d].ID must equal the bundle's events[%d].id verbatim — "+
				"this is what makes Python-side ground-truth recompute match "+
				"chapterhouse's stored ids at recall time", i, i)
	}
}

func TestParse_SingleRecord_PreservesTimestamps(t *testing.T) {
	a := github.New()
	sf := walkOne(t, a, "testdata/single")
	ns, err := a.Parse(sf)
	require.NoError(t, err)

	// Real GitHub timestamps must round-trip — Ebbinghaus decay
	// depends on it. Sub-second precision must survive the
	// JSON-decode round-trip; UTC must be preserved.
	wantStart := time.Date(2024, 1, 15, 14, 33, 1, 123456789, time.UTC)
	wantEnd := time.Date(2024, 1, 22, 8, 11, 44, 0, time.UTC)
	wantE0 := wantStart
	wantE1 := time.Date(2024, 1, 18, 9, 21, 55, 0, time.UTC)
	wantE2 := time.Date(2024, 1, 19, 16, 44, 12, 0, time.UTC)
	wantE3 := wantEnd

	require.True(t, ns.StartedAt.Equal(wantStart),
		"StartedAt: got %s want %s", ns.StartedAt, wantStart)
	require.Equal(t, wantStart.UnixNano(), ns.StartedAt.UnixNano(),
		"sub-second precision lost on StartedAt")
	require.NotNil(t, ns.EndedAt)
	require.True(t, ns.EndedAt.Equal(wantEnd),
		"EndedAt: got %s want %s", ns.EndedAt, wantEnd)

	require.True(t, ns.Events[0].Timestamp.Equal(wantE0))
	require.Equal(t, wantE0.UnixNano(), ns.Events[0].Timestamp.UnixNano())
	require.True(t, ns.Events[1].Timestamp.Equal(wantE1))
	require.True(t, ns.Events[2].Timestamp.Equal(wantE2))
	require.True(t, ns.Events[3].Timestamp.Equal(wantE3))
}

func TestParse_SingleRecord_StampsTags(t *testing.T) {
	a := github.New()
	sf := walkOne(t, a, "testdata/single")
	ns, err := a.Parse(sf)
	require.NoError(t, err)

	// Tags + entities are stamped onto NormalizedEvent.Metadata as
	// JSON-encoded array strings. The adapter's contract: preserve
	// fixture order EXACTLY (no extras, no re-sorting). We compare
	// slice equality, not "contains", so unintended tag injection
	// would fail the test.
	wantIssue := []string{
		"era:v14", "repo:vercel/next.js", "type:issue",
		"module:packages/next/src/client", "author:rauchg",
	}
	wantPR := []string{
		"era:v14", "repo:vercel/next.js", "type:pr",
		"module:packages/next/src/client",
		"author:timneutkens", "reviewer:rauchg",
	}
	wantCommit1 := []string{
		"era:v14", "repo:vercel/next.js", "type:commit",
		"module:packages/next/src/client", "author:timneutkens",
	}
	wantCommit2 := wantCommit1

	require.Equal(t, wantIssue, decodeMetaList(t, ns.Events[0], "tags"))
	require.Equal(t, wantPR, decodeMetaList(t, ns.Events[1], "tags"))
	require.Equal(t, wantCommit1, decodeMetaList(t, ns.Events[2], "tags"))
	require.Equal(t, wantCommit2, decodeMetaList(t, ns.Events[3], "tags"))

	// Entities flow through the same encoding so downstream
	// consumers can recover them by symmetric decode.
	require.Equal(t, []string{"rauchg"}, decodeMetaList(t, ns.Events[0], "entities"))
	require.Equal(t, []string{"timneutkens", "rauchg"}, decodeMetaList(t, ns.Events[1], "entities"))
}

// TestParse_SingleRecord_StampsTopLevelTags pins the contract that the
// github adapter populates NormalizedEvent.Tags (top-level, []string)
// in addition to Metadata["tags"]. The top-level field is what the
// ingestor copies onto core.Event.Tags, which chapterhouse persists
// into episodic.events.tags — a text[] with a gin index
// (episodic_events_tags_gin) that serves WHERE tags @> ARRAY[...]
// without a full scan. Reading the same list out of
// raw_event.metadata.tags works but loses the index.
//
// Parity assertion: top-level Tags MUST equal the JSON-decoded
// metadata["tags"] list, slice-for-slice. The metadata envelope stays
// for round-trip fidelity; the lift is additive, never replacing.
func TestParse_SingleRecord_StampsTopLevelTags(t *testing.T) {
	a := github.New()
	sf := walkOne(t, a, "testdata/single")
	ns, err := a.Parse(sf)
	require.NoError(t, err)

	for i, ev := range ns.Events {
		// 1. Top-level Tags is non-empty.
		require.NotEmpty(t, ev.Tags,
			"events[%d].Tags must be populated at top-level so the gin index can serve era/type/repo filters", i)
		// 2. Top-level matches metadata-encoded list exactly (parity).
		fromMeta := decodeMetaList(t, ev, "tags")
		require.Equal(t, fromMeta, ev.Tags,
			"events[%d].Tags must equal JSON-decoded Metadata[\"tags\"] — top-level is a lift, not a divergent re-stamp", i)
	}

	// Spot-check a few load-bearing tags appear directly on Tags
	// (without going through metadata decode) — proves the gin-index
	// path can answer era/type queries directly.
	assert.Contains(t, ns.Events[0].Tags, "era:v14")
	assert.Contains(t, ns.Events[0].Tags, "type:issue")
	assert.Contains(t, ns.Events[1].Tags, "type:pr")
	assert.Contains(t, ns.Events[2].Tags, "type:commit")
}

// TestParse_SingleRecord_StampsTopLevelEntities mirrors the tags lift
// for Entities — chapterhouse persists into episodic.events.entities
// with the same gin-index treatment.
func TestParse_SingleRecord_StampsTopLevelEntities(t *testing.T) {
	a := github.New()
	sf := walkOne(t, a, "testdata/single")
	ns, err := a.Parse(sf)
	require.NoError(t, err)

	// Issue + PR carry entities in fixture; commits do not. Assert
	// parity for the events that have them, and that the empty
	// shape (nil or empty slice) matches metadata for the ones that
	// don't.
	for i, ev := range ns.Events {
		fromMeta := decodeMetaList(t, ev, "entities")
		// Top-level field must be the same list.
		require.Equal(t, fromMeta, ev.Entities,
			"events[%d].Entities must equal JSON-decoded Metadata[\"entities\"] — parity contract", i)
	}

	// Spot-check the issue + PR have actual entities populated at
	// top-level (proving the lift fired for non-empty cases).
	assert.Equal(t, []string{"rauchg"}, ns.Events[0].Entities)
	assert.Equal(t, []string{"timneutkens", "rauchg"}, ns.Events[1].Entities)
}

// TestParse_SingleRecord_PreservesKindInTags pins the contract that
// when Type is collapsed to canonical "user", the bundle's per-event
// kind discrimination still survives via the type:<kind> tag.
// Downstream queries can recover issue/pr/commit by tag filter.
func TestParse_SingleRecord_PreservesKindInTags(t *testing.T) {
	a := github.New()
	sf := walkOne(t, a, "testdata/single")
	ns, err := a.Parse(sf)
	require.NoError(t, err)

	wantKindTag := []string{"type:issue", "type:pr", "type:commit", "type:commit"}
	require.Len(t, ns.Events, len(wantKindTag))
	for i, ev := range ns.Events {
		tags := decodeMetaList(t, ev, "tags")
		require.Contains(t, tags, wantKindTag[i],
			"events[%d] must keep %q on Metadata.tags so kind discrimination survives Type collapse to \"user\"", i, wantKindTag[i])
	}
}

func decodeMetaList(t *testing.T, ev importlogs.NormalizedEvent, key string) []string {
	t.Helper()
	raw, ok := ev.Metadata[key]
	require.True(t, ok, "event metadata missing key %q", key)
	var out []string
	require.NoError(t, json.Unmarshal([]byte(raw), &out))
	return out
}

// TestParse_MultiRecord_AllSessionsParsed pins the contract that
// Walk yields one SessionFile per JSONL record (per-line, not
// per-file) and Parse returns a NormalizedSession for each. The
// fixture has 3 distinct issue threads with different issue
// numbers, eras, and event counts so we can assert content, not
// just count, and detect any silent reordering.
func TestParse_MultiRecord_AllSessionsParsed(t *testing.T) {
	a := github.New()
	sfs := walkAll(t, a, "testdata/multi")
	require.Len(t, sfs, 3, "multi.jsonl has 3 records; Walk must yield one SessionFile per line")

	// LineNum cursor must be 1-indexed and monotonically increasing
	// in input order — Parse() depends on this to seek to the right
	// record. If a future refactor lets WalkDir reorder, this
	// catches it.
	for i, sf := range sfs {
		require.Equal(t, i+1, sf.LineNum, "sfs[%d] LineNum should be %d", i, i+1)
	}

	want := []struct {
		sessionID  string
		startedAt  time.Time
		eventCount int
		firstTag   string // era tag on events[0] — distinguishes the records
		issueText  string // substring on events[0].Text — content survival
	}{
		{
			sessionID:  "11111111-1111-5111-9111-111111111111",
			startedAt:  time.Date(2022, 6, 1, 10, 0, 0, 0, time.UTC),
			eventCount: 3,
			firstTag:   "era:v12",
			issueText:  "App Router routing regression",
		},
		{
			sessionID:  "22222222-2222-5222-9222-222222222222",
			startedAt:  time.Date(2024, 3, 10, 8, 0, 0, 0, time.UTC),
			eventCount: 5,
			firstTag:   "era:v14",
			issueText:  "Turbopack HMR flakiness",
		},
		{
			sessionID:  "33333333-3333-5333-9333-333333333333",
			startedAt:  time.Date(2025, 1, 5, 16, 0, 0, 0, time.UTC),
			eventCount: 2,
			firstTag:   "era:v15",
			issueText:  "Server Actions swallow errors",
		},
	}

	for i, sf := range sfs {
		ns, err := a.Parse(sf)
		require.NoError(t, err, "sfs[%d] parse", i)
		require.NotNil(t, ns)
		require.Equal(t, want[i].sessionID, ns.SessionID.String(),
			"sfs[%d] session id (records must come out in input order)", i)
		require.True(t, ns.StartedAt.Equal(want[i].startedAt),
			"sfs[%d] StartedAt: got %s want %s", i, ns.StartedAt, want[i].startedAt)
		require.Len(t, ns.Events, want[i].eventCount,
			"sfs[%d] event count (issue#%d in fixture)", i, (i+1)*100)
		require.Contains(t, decodeMetaList(t, ns.Events[0], "tags"), want[i].firstTag,
			"sfs[%d] events[0] should carry %q tag (era stamping)", i, want[i].firstTag)
		require.Contains(t, ns.Events[0].Text, want[i].issueText,
			"sfs[%d] events[0].Text should contain %q (content survival)", i, want[i].issueText)
	}
}

// TestParse_MalformedLine_FailsLoud pins the contract that a
// corrupt JSONL line produces a *loud* error, not a silent skip.
// The fixture has 2 lines: line 1 valid, line 2 corrupt
// (truncated JSON, unclosed string + unbalanced braces). Walk
// yields both SessionFiles (it doesn't pre-validate JSON); Parse
// returns ok for line 1 and an error wrapping path + line number
// for line 2. Without this, downstream consumers would silently
// drop records and the seeding-eval harness would under-count
// without warning.
func TestParse_MalformedLine_FailsLoud(t *testing.T) {
	a := github.New()
	sfs := walkAll(t, a, "testdata/malformed")
	require.Len(t, sfs, 2, "malformed.jsonl has 2 lines; Walk must yield both (it does not pre-validate JSON)")

	// Line 1 is a valid record — must parse cleanly. This proves
	// the malformed line on line 2 is the only failure source.
	ns1, err := a.Parse(sfs[0])
	require.NoError(t, err, "line 1 is a valid record and must parse")
	require.NotNil(t, ns1)
	require.Equal(t, "99999999-9999-5999-9999-999999999999", ns1.SessionID.String())
	require.NotEmpty(t, ns1.Events, "valid record must produce events, not a zero-event session")

	// Line 2 is corrupt. The error must be:
	//   - non-nil (no silent skip, no nil-nil return)
	//   - identify the file path (so an operator can find the bad bundle)
	//   - identify the line number (so they can find the bad record)
	ns2, err := a.Parse(sfs[1])
	require.Error(t, err, "malformed line must surface as a non-nil error, not a silent skip")
	require.Nil(t, ns2, "malformed line must NOT produce a partial NormalizedSession (caller must see error and skip)")

	msg := err.Error()
	require.Contains(t, msg, sfs[1].Path,
		"error must include the bundle path so operators can locate the bad file; got %q", msg)
	require.Contains(t, msg, "line 2",
		"error must include the 1-indexed line number so operators can locate the bad record; got %q", msg)
}
