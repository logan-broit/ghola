package importlogs

// Wire-format tests for the ingestor's POST body. Uses httptest.NewServer
// (a real HTTP server, not a mock) so the assertion actually reads the
// bytes the production client emits — same posture as
// internal/chapterhouse/client_test.go.
//
// Internal-package test (package importlogs, not _test) so we can call
// the unexported ingestSession directly. The function is small and
// pure-by-construction; there's no value in routing through Run() and
// fabricating an Adapter just to land on the same POST.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/logan-broit/ghola/internal/chapterhouse"
)

// TestIngestSession_PlumbsWorkspaceID asserts the POST body to
// /v1/episodic/ingest carries session.workspace_id matching cfg.Workspace.
// Without this, chapterhouse rejects the request with 400
// "session.workspace_id is required" (see
// _chapterhouse/ch-server/internal/handler/episodic.go validateIngest).
func TestIngestSession_PlumbsWorkspaceID(t *testing.T) {
	workspace := uuid.MustParse("aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee")
	user := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	sessionID := uuid.MustParse("99999999-9999-4999-8999-999999999999")

	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/episodic/ingest", r.URL.Path,
			"only ingest endpoint should be hit")
		buf, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var body map[string]any
		require.NoError(t, json.Unmarshal(buf, &body), "decode ingest body")
		bodies = append(bodies, body)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"session_id":"` + sessionID.String() +
			`","inserted":1,"updated":0}`))
	}))
	t.Cleanup(srv.Close)

	client := chapterhouse.New(srv.URL, "test-key").WithHTTPClient(srv.Client())

	cfg := Config{
		User:      user,
		Workspace: workspace,
		BatchSize: 8,
	}
	ns := &NormalizedSession{
		SourceTool: "github",
		SessionID:  sessionID,
		UserID:     user,
		StartedAt:  time.Date(2025, 1, 5, 16, 0, 0, 0, time.UTC),
		AgentKind:  "github",
		Events: []NormalizedEvent{
			{
				Type:      "user",
				Text:      "issue body",
				Timestamp: time.Date(2025, 1, 5, 16, 0, 0, 0, time.UTC),
				Metadata:  map[string]string{"tags": `["era:v15"]`},
			},
		},
	}

	require.NoError(t, ingestSession(context.Background(), client, cfg, ns))

	require.Len(t, bodies, 1, "exactly one ingest call expected")
	sess, ok := bodies[0]["session"].(map[string]any)
	require.True(t, ok, "session field should be an object: %+v", bodies[0])
	assert.Equal(t, workspace.String(), sess["workspace_id"],
		"session.workspace_id must be plumbed onto every ingest POST; "+
			"chapterhouse 400s without it")
}

// TestIngestSession_LiftsTagsAndEntitiesToTopLevel pins the contract that
// NormalizedEvent.Tags + Entities land on the per-event top-level
// `tags`/`entities` arrays in the ingest POST body — not just inside
// `raw_event.metadata`. This is what enables chapterhouse's
// episodic_events_tags_gin / entities gin index to serve
// `WHERE tags @> ARRAY[...]` filters at recall time. Without this lift
// the tags only round-trip through events.raw_event jsonb (still
// queryable, but no gin index — full scan).
func TestIngestSession_LiftsTagsAndEntitiesToTopLevel(t *testing.T) {
	workspace := uuid.MustParse("dddddddd-eeee-4fff-8aaa-bbbbbbbbbbbb")
	user := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	sessionID := uuid.MustParse("77777777-7777-4777-8777-777777777777")

	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var body map[string]any
		require.NoError(t, json.Unmarshal(buf, &body))
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"session_id":"` + sessionID.String() +
			`","inserted":1,"updated":0}`))
	}))
	t.Cleanup(srv.Close)

	client := chapterhouse.New(srv.URL, "test-key").WithHTTPClient(srv.Client())

	wantTags := []string{"era:v15", "type:issue", "repo:vercel/next.js"}
	wantEntities := []string{"alice", "bob"}

	cfg := Config{User: user, Workspace: workspace, BatchSize: 8}
	ns := &NormalizedSession{
		SourceTool: "github",
		SessionID:  sessionID,
		UserID:     user,
		StartedAt:  time.Date(2025, 1, 5, 16, 0, 0, 0, time.UTC),
		AgentKind:  "github",
		Events: []NormalizedEvent{
			{
				Type:      "user",
				Text:      "issue body",
				Timestamp: time.Date(2025, 1, 5, 16, 0, 0, 0, time.UTC),
				Tags:      wantTags,
				Entities:  wantEntities,
				// Metadata round-trip envelope stays intact alongside top-level
				// — readers that decode raw_event.metadata.tags must still work.
				Metadata: map[string]string{
					"tags":     `["era:v15","type:issue","repo:vercel/next.js"]`,
					"entities": `["alice","bob"]`,
				},
			},
		},
	}

	require.NoError(t, ingestSession(context.Background(), client, cfg, ns))

	require.Len(t, bodies, 1)
	events, ok := bodies[0]["events"].([]any)
	require.True(t, ok, "events array should be present: %+v", bodies[0])
	require.Len(t, events, 1)

	ev, ok := events[0].(map[string]any)
	require.True(t, ok, "event should be object: %+v", events[0])

	// Top-level tags: must be a JSON array, must equal NormalizedEvent.Tags
	// in order. Order matters because era:vN -> type:X -> repo:Y is the
	// adapter's stamping convention; flipping it would silently bury
	// regressions in the convention.
	gotTagsAny, ok := ev["tags"].([]any)
	require.True(t, ok, "event.tags must be a top-level JSON array (gin index target); got %T", ev["tags"])
	gotTags := make([]string, len(gotTagsAny))
	for i, t := range gotTagsAny {
		gotTags[i], _ = t.(string)
	}
	assert.Equal(t, wantTags, gotTags,
		"event.tags must equal NormalizedEvent.Tags so episodic_events_tags_gin can serve @> queries")

	gotEntsAny, ok := ev["entities"].([]any)
	require.True(t, ok, "event.entities must be a top-level JSON array; got %T", ev["entities"])
	gotEnts := make([]string, len(gotEntsAny))
	for i, t := range gotEntsAny {
		gotEnts[i], _ = t.(string)
	}
	assert.Equal(t, wantEntities, gotEnts,
		"event.entities must equal NormalizedEvent.Entities for top-level gin lookups")

	// raw_event.metadata.tags / .entities must STILL be present — the lift
	// adds top-level fields, doesn't replace the round-trip envelope.
	rawJSON, ok := ev["raw_event"].(string)
	if ok {
		// chapterhouse decodes raw_event as json.RawMessage; client.Post
		// emits it as a JSON-encoded string when the byte slice is set.
		// Either shape is acceptable; what matters is that metadata.tags
		// survived. Decode and assert.
		var raw struct {
			Metadata map[string]string `json:"metadata"`
		}
		require.NoError(t, json.Unmarshal([]byte(rawJSON), &raw))
		assert.Equal(t, `["era:v15","type:issue","repo:vercel/next.js"]`, raw.Metadata["tags"],
			"raw_event.metadata.tags must survive — round-trip envelope is preserved alongside top-level lift")
	} else {
		// Object shape.
		rawObj, ok := ev["raw_event"].(map[string]any)
		require.True(t, ok, "raw_event must be string or object; got %T", ev["raw_event"])
		md, _ := rawObj["metadata"].(map[string]any)
		assert.Equal(t, `["era:v15","type:issue","repo:vercel/next.js"]`, md["tags"],
			"raw_event.metadata.tags must survive — round-trip envelope is preserved alongside top-level lift")
	}
}

// TestIngestSession_HonorsSuppliedEventID pins the contract that when a
// NormalizedEvent carries a non-empty ID, the ingestor uses it verbatim
// as the per-event id in the chapterhouse POST body — NOT the derived
// (session, ordinal) UUID. This is what makes adapter-side stable IDs
// (e.g. github bundle's uuid5(NS_EVENT, "vercel/next.js/issue/93146"))
// survive ingest. Without this lift the eval harness's Python-side
// ground-truth IDs never match what chapterhouse stores, so H2/recall
// scores read 0 across all variants.
func TestIngestSession_HonorsSuppliedEventID(t *testing.T) {
	workspace := uuid.MustParse("eeeeeeee-ffff-4aaa-8bbb-cccccccccccc")
	user := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	sessionID := uuid.MustParse("66666666-6666-4666-8666-666666666666")

	wantIDs := []string{
		"00000000-0000-0000-0000-0000000000aa",
		"00000000-0000-0000-0000-0000000000bb",
		"00000000-0000-0000-0000-0000000000cc",
	}

	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var body map[string]any
		require.NoError(t, json.Unmarshal(buf, &body))
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"session_id":"` + sessionID.String() +
			`","inserted":` + "3" + `,"updated":0}`))
	}))
	t.Cleanup(srv.Close)

	client := chapterhouse.New(srv.URL, "test-key").WithHTTPClient(srv.Client())

	cfg := Config{User: user, Workspace: workspace, BatchSize: 8}
	ns := &NormalizedSession{
		SourceTool: "github",
		SessionID:  sessionID,
		UserID:     user,
		StartedAt:  time.Date(2025, 1, 5, 16, 0, 0, 0, time.UTC),
		AgentKind:  "github",
		Events: []NormalizedEvent{
			{
				ID:        wantIDs[0],
				Type:      "user",
				Text:      "issue body",
				Timestamp: time.Date(2025, 1, 5, 16, 0, 0, 0, time.UTC),
				Metadata:  map[string]string{},
			},
			{
				ID:        wantIDs[1],
				Type:      "user",
				Text:      "pr body",
				Timestamp: time.Date(2025, 1, 6, 16, 0, 0, 0, time.UTC),
				Metadata:  map[string]string{},
			},
			{
				ID:        wantIDs[2],
				Type:      "user",
				Text:      "commit",
				Timestamp: time.Date(2025, 1, 7, 16, 0, 0, 0, time.UTC),
				Metadata:  map[string]string{},
			},
		},
	}

	require.NoError(t, ingestSession(context.Background(), client, cfg, ns))

	require.Len(t, bodies, 1, "exactly one ingest call expected")
	events, ok := bodies[0]["events"].([]any)
	require.True(t, ok, "events array should be present: %+v", bodies[0])
	require.Len(t, events, len(wantIDs))

	for i, raw := range events {
		ev, ok := raw.(map[string]any)
		require.True(t, ok, "events[%d] should be object: %+v", i, raw)
		assert.Equal(t, wantIDs[i], ev["id"],
			"events[%d].id must equal NormalizedEvent.ID (verbatim, not derived); "+
				"adapter-supplied stable IDs are load-bearing for ground-truth recall", i)
	}
}

// TestIngestSession_DerivesEventIDWhenUnsupplied pins the fall-through
// path: when NormalizedEvent.ID is empty, the ingestor MUST derive a
// stable id from (session, ordinal) so re-runs of jsonl-family adapters
// (which don't supply IDs) land on the same row and chapterhouse's
// upsert dedupes. Without this fall-through TestImport_BootstrapCorpus
// breaks.
func TestIngestSession_DerivesEventIDWhenUnsupplied(t *testing.T) {
	workspace := uuid.MustParse("ffffffff-aaaa-4bbb-8ccc-dddddddddddd")
	user := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	sessionID := uuid.MustParse("44444444-4444-4444-8444-444444444444")

	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		var body map[string]any
		require.NoError(t, json.Unmarshal(buf, &body))
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"session_id":"` + sessionID.String() +
			`","inserted":2,"updated":0}`))
	}))
	t.Cleanup(srv.Close)

	client := chapterhouse.New(srv.URL, "test-key").WithHTTPClient(srv.Client())

	cfg := Config{User: user, Workspace: workspace, BatchSize: 8}
	ns := &NormalizedSession{
		SourceTool: "jsonl-family",
		SessionID:  sessionID,
		UserID:     user,
		StartedAt:  time.Date(2025, 1, 5, 16, 0, 0, 0, time.UTC),
		AgentKind:  "claude-code",
		Events: []NormalizedEvent{
			// ID intentionally unset — adapter has no upstream id.
			{
				Type:      "user",
				Text:      "ev0",
				Timestamp: time.Date(2025, 1, 5, 16, 0, 0, 0, time.UTC),
				Metadata:  map[string]string{},
			},
			{
				Type:      "assistant",
				Text:      "ev1",
				Timestamp: time.Date(2025, 1, 5, 16, 0, 1, 0, time.UTC),
				Metadata:  map[string]string{},
			},
		},
	}

	require.NoError(t, ingestSession(context.Background(), client, cfg, ns))

	require.Len(t, bodies, 1)
	events, ok := bodies[0]["events"].([]any)
	require.True(t, ok)
	require.Len(t, events, 2)

	want0 := deriveEventID(sessionID, 0).String()
	want1 := deriveEventID(sessionID, 1).String()

	ev0 := events[0].(map[string]any)
	ev1 := events[1].(map[string]any)
	assert.Equal(t, want0, ev0["id"],
		"empty NormalizedEvent.ID must fall through to deriveEventID(session, 0); "+
			"jsonl-family adapters depend on this for re-run idempotency")
	assert.Equal(t, want1, ev1["id"],
		"empty NormalizedEvent.ID must fall through to deriveEventID(session, 1)")

	// Sanity: the derived id is a parseable UUID.
	_, err := uuid.Parse(ev0["id"].(string))
	require.NoError(t, err, "derived id must be a valid UUID")
}

// stubEmbedder is a deterministic implementer of the Embedder interface
// (NOT a mock). It returns the same canned vector for any text. Used to
// pin the wire-format contract that the ingestor calls .Embed() per
// event and lifts the result onto the per-event JSON `embedding` field
// the chapterhouse server reads. Same pattern as
// internal/encoding/worker_test.go's nullEmbedder.
type stubEmbedder struct {
	vec []float32
	// errOn, when non-empty, makes Embed return an error if text == errOn.
	// Used by the error-propagation test.
	errOn string
}

func (s stubEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	if s.errOn != "" && text == s.errOn {
		return nil, fmt.Errorf("stub embed: refused %q", text)
	}
	return append([]float32(nil), s.vec...), nil
}

// makeVec builds a `dim`-element []float32 with every element = fill.
// The eval evidence at hand says the production embedder is qwen3
// (1024-dim); using 1024 here pins the shape that landed end-to-end in
// the bug report.
func makeVec(dim int, fill float32) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = fill
	}
	return v
}

// TestIngestSession_PopulatesEventEmbedding pins the contract this fix
// adds: when cfg.Embedder is non-nil the ingestor MUST call .Embed() per
// event and emit a top-level `embedding` array on each per-event JSON
// object in the POST body. Without this contract chapterhouse stores
// embedding as NULL and the 5-tier hybrid recall pipeline degrades to
// FTS-only on the imported corpus — exactly the bug the seeding-eval
// harness surfaced (266/266 NULL embeddings on the bulk-import workspace).
func TestIngestSession_PopulatesEventEmbedding(t *testing.T) {
	workspace := uuid.MustParse("11111111-aaaa-4bbb-8ccc-dddddddddddd")
	user := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	sessionID := uuid.MustParse("22222222-aaaa-4bbb-8ccc-dddddddddddd")

	const dim = 1024
	const fill = float32(0.125)

	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var body map[string]any
		require.NoError(t, json.Unmarshal(buf, &body))
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"session_id":"` + sessionID.String() +
			`","inserted":3,"updated":0}`))
	}))
	t.Cleanup(srv.Close)

	client := chapterhouse.New(srv.URL, "test-key").WithHTTPClient(srv.Client())

	cfg := Config{
		User:      user,
		Workspace: workspace,
		BatchSize: 8,
		Embedder:  stubEmbedder{vec: makeVec(dim, fill)},
	}
	ns := &NormalizedSession{
		SourceTool: "github",
		SessionID:  sessionID,
		UserID:     user,
		StartedAt:  time.Date(2025, 1, 5, 16, 0, 0, 0, time.UTC),
		AgentKind:  "github",
		Events: []NormalizedEvent{
			{Type: "user", Text: "issue body", Timestamp: time.Date(2025, 1, 5, 16, 0, 0, 0, time.UTC)},
			{Type: "user", Text: "pr body", Timestamp: time.Date(2025, 1, 6, 16, 0, 0, 0, time.UTC)},
			{Type: "user", Text: "commit", Timestamp: time.Date(2025, 1, 7, 16, 0, 0, 0, time.UTC)},
		},
	}

	require.NoError(t, ingestSession(context.Background(), client, cfg, ns))

	require.Len(t, bodies, 1, "exactly one ingest call expected")
	events, ok := bodies[0]["events"].([]any)
	require.True(t, ok, "events array should be present: %+v", bodies[0])
	require.Len(t, events, 3)

	for i, raw := range events {
		ev, ok := raw.(map[string]any)
		require.True(t, ok, "events[%d] should be object", i)

		embAny, ok := ev["embedding"].([]any)
		require.True(t, ok,
			"events[%d].embedding must be a top-level JSON array (chapterhouse "+
				"persists this verbatim into episodic.events.embedding); got %T = %v",
			i, ev["embedding"], ev["embedding"])
		require.Len(t, embAny, dim,
			"events[%d].embedding must have %d elements (qwen3-embedding shape)", i, dim)
		// Spot-check first + last element so a partial truncation to a
		// shorter slice still trips the assertion even if the length is
		// somehow padded downstream.
		first, _ := embAny[0].(float64)
		last, _ := embAny[dim-1].(float64)
		assert.InDelta(t, float64(fill), first, 1e-6,
			"events[%d].embedding[0] must be the stub vector's value", i)
		assert.InDelta(t, float64(fill), last, 1e-6,
			"events[%d].embedding[last] must be the stub vector's value", i)
	}
}

// TestIngestSession_NilEmbedder_OmitsEmbedding pins the fall-through
// path: when cfg.Embedder is nil (the dry-run / no-guild case) the POST
// body MUST NOT carry an `embedding` field per event. This keeps the
// existing TestImport_BootstrapCorpus integration test running without a
// guild reachable, and matches chapterhouse's omitempty contract — a
// missing field stays NULL rather than carrying a zero-length array.
func TestIngestSession_NilEmbedder_OmitsEmbedding(t *testing.T) {
	workspace := uuid.MustParse("33333333-aaaa-4bbb-8ccc-dddddddddddd")
	user := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	sessionID := uuid.MustParse("44444444-aaaa-4bbb-8ccc-dddddddddddd")

	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		var body map[string]any
		require.NoError(t, json.Unmarshal(buf, &body))
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"session_id":"` + sessionID.String() +
			`","inserted":1,"updated":0}`))
	}))
	t.Cleanup(srv.Close)

	client := chapterhouse.New(srv.URL, "test-key").WithHTTPClient(srv.Client())

	cfg := Config{User: user, Workspace: workspace, BatchSize: 8} // Embedder nil
	ns := &NormalizedSession{
		SourceTool: "jsonl-family",
		SessionID:  sessionID,
		UserID:     user,
		StartedAt:  time.Date(2025, 1, 5, 16, 0, 0, 0, time.UTC),
		AgentKind:  "claude-code",
		Events: []NormalizedEvent{
			{Type: "user", Text: "ev0", Timestamp: time.Date(2025, 1, 5, 16, 0, 0, 0, time.UTC)},
		},
	}

	require.NoError(t, ingestSession(context.Background(), client, cfg, ns))
	require.Len(t, bodies, 1)
	events, ok := bodies[0]["events"].([]any)
	require.True(t, ok)
	require.Len(t, events, 1)
	ev := events[0].(map[string]any)
	_, present := ev["embedding"]
	assert.False(t, present,
		"events[0].embedding must be absent when cfg.Embedder is nil "+
			"(omitempty preserves chapterhouse NULL semantics); got %v", ev["embedding"])
}

// TestIngestSession_EmbedderErrorBubbles asserts an embedder failure
// aborts the session ingest with a wrapped error. Without bubbling the
// error the ingestor would silently send events with empty embeddings —
// a quieter but identical regression to the bug this commit fixes.
func TestIngestSession_EmbedderErrorBubbles(t *testing.T) {
	workspace := uuid.MustParse("55555555-aaaa-4bbb-8ccc-dddddddddddd")
	user := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	sessionID := uuid.MustParse("66666666-aaaa-4bbb-8ccc-dddddddddddd")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Should never be reached — error must abort before POST.
		t.Errorf("ingestor POSTed despite embedder error")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	client := chapterhouse.New(srv.URL, "test-key").WithHTTPClient(srv.Client())

	cfg := Config{
		User:      user,
		Workspace: workspace,
		BatchSize: 8,
		Embedder:  stubEmbedder{vec: makeVec(8, 0.1), errOn: "boom"},
	}
	ns := &NormalizedSession{
		SourceTool: "github",
		SessionID:  sessionID,
		UserID:     user,
		StartedAt:  time.Date(2025, 1, 5, 16, 0, 0, 0, time.UTC),
		Events: []NormalizedEvent{
			{Type: "user", Text: "ok", Timestamp: time.Date(2025, 1, 5, 16, 0, 0, 0, time.UTC)},
			{Type: "user", Text: "boom", Timestamp: time.Date(2025, 1, 5, 16, 0, 1, 0, time.UTC)},
		},
	}

	err := ingestSession(context.Background(), client, cfg, ns)
	require.Error(t, err, "embedder error must bubble up, not be swallowed")
	assert.Contains(t, err.Error(), "embed",
		"error must mention the embed step so operators can diagnose; got %v", err)
}

// TestIngestSession_PlumbsWorkspaceID_Batched asserts every batch in a
// multi-batch session carries workspace_id, not just the first.
func TestIngestSession_PlumbsWorkspaceID_Batched(t *testing.T) {
	workspace := uuid.MustParse("cccccccc-dddd-4eee-8fff-aaaaaaaaaaaa")
	user := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	sessionID := uuid.MustParse("88888888-8888-4888-8888-888888888888")

	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		var body map[string]any
		require.NoError(t, json.Unmarshal(buf, &body))
		bodies = append(bodies, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"session_id":"` + sessionID.String() +
			`","inserted":0,"updated":0}`))
	}))
	t.Cleanup(srv.Close)

	client := chapterhouse.New(srv.URL, "test-key").WithHTTPClient(srv.Client())

	// 5 events with batch size 2 -> 3 batches.
	events := make([]NormalizedEvent, 5)
	for i := range events {
		events[i] = NormalizedEvent{
			Type:      "user",
			Text:      "event",
			Timestamp: time.Now().UTC(),
			Metadata:  map[string]string{},
		}
	}
	cfg := Config{User: user, Workspace: workspace, BatchSize: 2}
	ns := &NormalizedSession{
		SourceTool: "github",
		SessionID:  sessionID,
		UserID:     user,
		StartedAt:  time.Now().UTC(),
		Events:     events,
	}

	require.NoError(t, ingestSession(context.Background(), client, cfg, ns))

	require.Len(t, bodies, 3, "5 events / batch size 2 -> 3 POSTs")
	for i, body := range bodies {
		sess, ok := body["session"].(map[string]any)
		require.True(t, ok, "batch %d: session should be object", i)
		assert.Equal(t, workspace.String(), sess["workspace_id"],
			"batch %d: workspace_id must be set on every batch", i)
	}
}
