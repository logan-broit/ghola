package chapterhouse_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/logan-broit/ghola/internal/chapterhouse"
	"github.com/logan-broit/ghola/internal/core"
)

// newServer wires a chi-less mux that dispatches on path. Each test
// installs the handler(s) it cares about; unexpected paths fail the
// test so typo bugs surface immediately.
func newServer(t *testing.T, handlers map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for path, h := range handlers {
		mux.HandleFunc(path, h)
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected path %s", r.URL.Path)
		http.NotFound(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newClient(t *testing.T, srv *httptest.Server) *chapterhouse.Client {
	t.Helper()
	return chapterhouse.New(srv.URL, "test-key").WithHTTPClient(srv.Client())
}

// assertAuthHeader is the shared assertion: every client call must
// carry Bearer <apikey>.
func assertAuthHeader(t *testing.T, r *http.Request) {
	t.Helper()
	assert.Equal(t, "Bearer test-key", r.Header.Get("Authorization"))
}

// ---------------------------------------------------------------------
// episodic: ingest / query / share / forget
// ---------------------------------------------------------------------

func TestClient_IngestEpisodic(t *testing.T) {
	var gotBody map[string]any
	srv := newServer(t, map[string]http.HandlerFunc{
		"/v1/episodic/ingest": func(w http.ResponseWriter, r *http.Request) {
			assertAuthHeader(t, r)
			b, _ := io.ReadAll(r.Body)
			require.NoError(t, json.Unmarshal(b, &gotBody))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"session_id":"s1","inserted":2,"updated":1}`))
		},
	})
	c := newClient(t, srv)

	ins, upd, err := c.IngestEpisodic(context.Background(),
		core.Session{ID: "s1", UserID: "u1"},
		[]core.Event{{ID: "e1"}, {ID: "e2"}, {ID: "e3"}},
	)
	require.NoError(t, err)
	assert.Equal(t, 2, ins)
	assert.Equal(t, 1, upd)
	require.NotNil(t, gotBody)
	assert.Contains(t, gotBody, "session")
	assert.Contains(t, gotBody, "events")
}

func TestClient_ShareEpisodic(t *testing.T) {
	srv := newServer(t, map[string]http.HandlerFunc{
		"/v1/episodic/share": func(w http.ResponseWriter, r *http.Request) {
			assertAuthHeader(t, r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"share-42"}`))
		},
	})
	c := newClient(t, srv)

	id, err := c.ShareEpisodic(context.Background(), core.ShareInput{
		UserID: "u1", Target: "team", ScopeType: "session", ScopeID: "s1",
	})
	require.NoError(t, err)
	assert.Equal(t, "share-42", id)
}

func TestClient_ForgetEpisodic(t *testing.T) {
	srv := newServer(t, map[string]http.HandlerFunc{
		"/v1/episodic/forget": func(w http.ResponseWriter, r *http.Request) {
			assertAuthHeader(t, r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"forgotten":3}`))
		},
	})
	c := newClient(t, srv)

	n, err := c.ForgetEpisodic(context.Background(), "u1", []string{"e1", "e2", "e3"})
	require.NoError(t, err)
	assert.Equal(t, 3, n)
}

// ---------------------------------------------------------------------
// episodic: query (multi-ranking mode)
// ---------------------------------------------------------------------

// TestClient_QueryEpisodicMulti pins the wire contract for the
// multi-ranking flavor of /v1/episodic/query: the request must carry
// a `rankings` array naming each tier the caller wants ranked
// independently, and the response carries one parallel sub-list per
// requested tier (vector, fts, session_vector). A6 will migrate
// core.Recall to call this single endpoint instead of the three
// per-tier methods; until then both surfaces co-exist.
func TestClient_QueryEpisodicMulti(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := newServer(t, map[string]http.HandlerFunc{
		"/v1/episodic/query": func(w http.ResponseWriter, r *http.Request) {
			assertAuthHeader(t, r)
			gotPath = r.URL.Path
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"vector":[
					{"event_id":"11111111-1111-1111-1111-111111111111",
					 "session_id":"aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
					 "tier":"vector","text":"vec hit",
					 "score":{"semantic":0.9,"fts":0,"merged":0.9}}
				],
				"fts":[
					{"event_id":"22222222-2222-2222-2222-222222222222",
					 "session_id":"bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
					 "tier":"fts","text":"fts hit",
					 "score":{"semantic":0,"fts":0.7,"merged":0.7}}
				],
				"session_vector":[
					{"session_id":"cccccccc-cccc-cccc-cccc-cccccccccccc",
					 "tier":"session_vector",
					 "session_chunk_text":"pooled session text",
					 "score":{"semantic":0.85,"fts":0,"merged":0.85}}
				]
			}`))
		},
	})
	c := newClient(t, srv)

	resp, err := c.QueryEpisodicMulti(context.Background(), core.EpisodicMultiQuery{
		UserID:         "00000000-0000-0000-0000-000000000001",
		WorkspaceID:    "00000000-0000-0000-0000-000000000002",
		QueryText:      "k8s",
		QueryEmbedding: []float32{0.1, 0.2, 0.3},
		Limit:          10,
		Rankings:       []string{"vector", "fts", "session_vector"},
	})
	require.NoError(t, err)

	// Wire path: still /v1/episodic/query — the discriminator is the
	// `rankings` field, not the URL.
	assert.Equal(t, "/v1/episodic/query", gotPath)

	// Request body: rankings field is present and ordered as sent;
	// chapterhouse's handler keys off this list to decide which
	// per-tier rankers to fan out to.
	require.NotNil(t, gotBody)
	rankings, ok := gotBody["rankings"].([]any)
	require.True(t, ok, "rankings must serialize as a JSON array, got %T", gotBody["rankings"])
	assert.Equal(t, []any{"vector", "fts", "session_vector"}, rankings)

	// Response projection onto core.RecallHit: each requested tier
	// comes back as its own sub-list with the downstream tier strings
	// core.Recall's RRF + dedup logic keys on ("episodic" for vector,
	// "keyword" for fts, "session_vector" passes through).
	require.Len(t, resp.Vector, 1)
	require.Len(t, resp.FTS, 1)
	require.Len(t, resp.SessionVector, 1)

	// Vector hit → "episodic" tier, event_id surfaced as RecallHit.ID,
	// session_id mirrored as *string.
	assert.Equal(t, "episodic", resp.Vector[0].Tier)
	assert.Equal(t, "11111111-1111-1111-1111-111111111111", resp.Vector[0].ID)
	require.NotNil(t, resp.Vector[0].SessionID)
	assert.Equal(t, "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", *resp.Vector[0].SessionID)
	assert.Equal(t, "vec hit", resp.Vector[0].Content)
	assert.InDelta(t, 0.9, resp.Vector[0].Score, 1e-9)

	// FTS hit → "keyword" tier, score is the merged tier score.
	assert.Equal(t, "keyword", resp.FTS[0].Tier)
	assert.Equal(t, "22222222-2222-2222-2222-222222222222", resp.FTS[0].ID)
	assert.Equal(t, "fts hit", resp.FTS[0].Content)
	assert.InDelta(t, 0.7, resp.FTS[0].Score, 1e-9)

	// session_vector hit: no event_id, so session_id surfaces as the
	// RecallHit.ID — matches legacy QueryEpisodicSessionVector
	// behavior and lets hitKey() ("session:" + ID) collapse session-
	// grain dedup correctly.
	assert.Equal(t, "session_vector", resp.SessionVector[0].Tier)
	assert.Equal(t, "cccccccc-cccc-cccc-cccc-cccccccccccc", resp.SessionVector[0].ID)
	require.NotNil(t, resp.SessionVector[0].SessionID)
	assert.Equal(t, "cccccccc-cccc-cccc-cccc-cccccccccccc", *resp.SessionVector[0].SessionID)
	assert.Equal(t, "pooled session text", resp.SessionVector[0].SessionChunkText)
	assert.InDelta(t, 0.85, resp.SessionVector[0].Score, 1e-9)
}

// TestClient_QueryEpisodicMulti_PrimitivesFlag pins the request-side
// wire contract for D2's `primitives` flag: when the caller sets
// EpisodicMultiQuery.Primitives = true, the marshaled body must carry
// `"primitives": true`. The chapterhouse handler discriminates on this
// flag (D1) to decide whether to compute the Hebbian-boosted fourth
// sub-list, so failing to thread it through here would silently turn
// the feature off for every ghola caller.
func TestClient_QueryEpisodicMulti_PrimitivesFlag(t *testing.T) {
	var gotBody map[string]any
	srv := newServer(t, map[string]http.HandlerFunc{
		"/v1/episodic/query": func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			require.NoError(t, json.Unmarshal(b, &gotBody))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		},
	})
	c := newClient(t, srv)

	_, err := c.QueryEpisodicMulti(context.Background(), core.EpisodicMultiQuery{
		UserID:      "00000000-0000-0000-0000-000000000001",
		WorkspaceID: "00000000-0000-0000-0000-000000000002",
		QueryText:   "k8s",
		Limit:       10,
		Rankings:    []string{"vector", "fts"},
		Primitives:  true,
	})
	require.NoError(t, err)

	require.NotNil(t, gotBody)
	prim, ok := gotBody["primitives"].(bool)
	require.True(t, ok, "primitives must serialize as a JSON bool, got %T", gotBody["primitives"])
	assert.True(t, prim, "primitives must be true on the wire when the caller opted in")
}

// TestClient_QueryEpisodicMulti_PrimitivesFlagOmittedWhenFalse pins the
// omitempty contract on the `primitives` request field: a zero-value
// (false) flag must not serialize on the wire. The chapterhouse handler
// reads the field as a plain bool with default-false semantics, so the
// distinction here is purely "don't add a key the server doesn't need
// to see" rather than 3-state semantics — but we still pin it so a
// future field-tag change doesn't silently shift the default behavior.
func TestClient_QueryEpisodicMulti_PrimitivesFlagOmittedWhenFalse(t *testing.T) {
	var gotBody map[string]any
	srv := newServer(t, map[string]http.HandlerFunc{
		"/v1/episodic/query": func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			require.NoError(t, json.Unmarshal(b, &gotBody))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		},
	})
	c := newClient(t, srv)

	_, err := c.QueryEpisodicMulti(context.Background(), core.EpisodicMultiQuery{
		UserID:      "00000000-0000-0000-0000-000000000001",
		WorkspaceID: "00000000-0000-0000-0000-000000000002",
		QueryText:   "k8s",
		Limit:       10,
		Rankings:    []string{"vector"},
	})
	require.NoError(t, err)

	_, present := gotBody["primitives"]
	assert.False(t, present, "primitives must be omitted when the flag is false")
}

// TestClient_QueryEpisodicMulti_PrimitivesResponseDecode pins the
// response-side wire contract. The server emits a `primitives` sub-list
// with the same hit shape as the other tiers (event_id, session_id,
// tier="primitives", score with merged carrying the Hebbian boost).
// The client must decode it and project onto core.RecallHit, with the
// tier string passed through unchanged ("primitives") so downstream
// dedup (hitKey() prefixes "event:" for non-session_vector tiers) and
// telemetry (TierCounts keyed by tier name) can tell where the ranking
// came from.
//
// Decision logged at D2: tier string is *not* remapped server→client.
// The legacy remap (vector→episodic, fts→keyword) exists only because
// the older core.Recall fan-out used those strings as cache/tier keys
// before A6; "primitives" is a new tier with no legacy alias to
// preserve, so the simplest contract is end-to-end pass-through.
func TestClient_QueryEpisodicMulti_PrimitivesResponseDecode(t *testing.T) {
	srv := newServer(t, map[string]http.HandlerFunc{
		"/v1/episodic/query": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"vector":[],
				"primitives":[
					{"event_id":"33333333-3333-3333-3333-333333333333",
					 "session_id":"dddddddd-dddd-dddd-dddd-dddddddddddd",
					 "tier":"primitives","text":"hebbian-boosted hit",
					 "score":{"semantic":0,"fts":0,"merged":0.42}}
				]
			}`))
		},
	})
	c := newClient(t, srv)

	resp, err := c.QueryEpisodicMulti(context.Background(), core.EpisodicMultiQuery{
		UserID:      "00000000-0000-0000-0000-000000000001",
		WorkspaceID: "00000000-0000-0000-0000-000000000002",
		QueryText:   "k8s",
		Limit:       10,
		Rankings:    []string{"vector"},
		Primitives:  true,
	})
	require.NoError(t, err)

	require.NotNil(t, resp.Primitives,
		"primitives pointer must be non-nil when the server returned a populated sub-list")
	prims := *resp.Primitives
	require.Len(t, prims, 1)
	assert.Equal(t, "primitives", prims[0].Tier,
		"tier string passes through end-to-end (no server→client remap)")
	assert.Equal(t, "33333333-3333-3333-3333-333333333333", prims[0].ID)
	require.NotNil(t, prims[0].SessionID)
	assert.Equal(t, "dddddddd-dddd-dddd-dddd-dddddddddddd", *prims[0].SessionID)
	assert.Equal(t, "hebbian-boosted hit", prims[0].Content)
	assert.InDelta(t, 0.42, prims[0].Score, 1e-9)
}

// TestClient_QueryEpisodicMulti_PrimitivesEmptyArrayDecode pins the
// 3-state pointer-to-slice contract on the response side: a wire
// `"primitives":[]` must decode to a *non-nil* pointer to an empty
// slice, distinct from absent/nil. The server uses the same 3-state
// shape (D1) to distinguish "flag was off / lookup failed" (absent →
// nil) from "flag was on, no in-set neighbors" (present, empty), and
// the client side must round-trip that distinction so future consumers
// (D3+) can react to the difference if they need to.
func TestClient_QueryEpisodicMulti_PrimitivesEmptyArrayDecode(t *testing.T) {
	srv := newServer(t, map[string]http.HandlerFunc{
		"/v1/episodic/query": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"vector":[],"primitives":[]}`))
		},
	})
	c := newClient(t, srv)

	resp, err := c.QueryEpisodicMulti(context.Background(), core.EpisodicMultiQuery{
		UserID:      "00000000-0000-0000-0000-000000000001",
		WorkspaceID: "00000000-0000-0000-0000-000000000002",
		Rankings:    []string{"vector"},
		Primitives:  true,
	})
	require.NoError(t, err)

	require.NotNil(t, resp.Primitives,
		"empty `primitives:[]` on the wire must decode as a non-nil pointer to an empty slice (3-state contract: distinct from absent)")
	assert.Empty(t, *resp.Primitives,
		"the underlying slice itself is empty when the server reported zero in-set neighbors")
}

// TestClient_QueryEpisodicMulti_PrimitivesAbsentDecode pins the third
// state of the pointer-to-slice contract: when the server omits the
// `primitives` key entirely (flag was false, OR flag was on but the
// association lookup failed and the handler dropped the field), the
// decoded pointer must be nil. Distinguishes "no signal" from "empty
// signal" so future consumers can tell whether primitives was queried
// at all.
func TestClient_QueryEpisodicMulti_PrimitivesAbsentDecode(t *testing.T) {
	srv := newServer(t, map[string]http.HandlerFunc{
		"/v1/episodic/query": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"vector":[]}`))
		},
	})
	c := newClient(t, srv)

	resp, err := c.QueryEpisodicMulti(context.Background(), core.EpisodicMultiQuery{
		UserID:      "00000000-0000-0000-0000-000000000001",
		WorkspaceID: "00000000-0000-0000-0000-000000000002",
		Rankings:    []string{"vector"},
	})
	require.NoError(t, err)

	assert.Nil(t, resp.Primitives,
		"absent `primitives` on the wire must decode as a nil pointer (3-state contract: distinct from empty slice)")
}

// TestClient_QueryEpisodicMulti_RankingsOmittedWhenEmpty pins the
// omitempty contract on the `rankings` field: a request without any
// rankings (callers using QueryEpisodicMulti with an empty slice) must
// NOT serialize a `rankings` key on the wire, otherwise the server's
// discriminator (presence of the field, not just non-empty) would be
// tripped and route the request through the multi-ranking path with
// zero tiers requested. omitempty keeps the legacy semantics intact
// for callers that pass a zero-value request.
func TestClient_QueryEpisodicMulti_RankingsOmittedWhenEmpty(t *testing.T) {
	var gotBody map[string]any
	srv := newServer(t, map[string]http.HandlerFunc{
		"/v1/episodic/query": func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			require.NoError(t, json.Unmarshal(b, &gotBody))
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		},
	})
	c := newClient(t, srv)
	_, err := c.QueryEpisodicMulti(context.Background(), core.EpisodicMultiQuery{
		UserID:      "00000000-0000-0000-0000-000000000001",
		WorkspaceID: "00000000-0000-0000-0000-000000000002",
		QueryText:   "hi",
		Limit:       5,
	})
	require.NoError(t, err)
	_, present := gotBody["rankings"]
	assert.False(t, present, "rankings must be omitted when nil/empty")
}

// ---------------------------------------------------------------------
// semantic: query
// ---------------------------------------------------------------------

func TestClient_QuerySemantic(t *testing.T) {
	// v0.3 mnemeHit shape: only mneme_id/score/level/tier. The dropped
	// content column means Content stays "" on the resulting RecallHit.
	srv := newServer(t, map[string]http.HandlerFunc{
		"/v1/semantic/query": func(w http.ResponseWriter, r *http.Request) {
			assertAuthHeader(t, r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"hits":[
				{"mneme_id":"m1","score":0.92,"level":1,"tier":"semantic"}
			]}`))
		},
	})
	c := newClient(t, srv)

	hits, err := c.QuerySemantic(context.Background(), core.SemanticQuery{
		Workspace: "ws", QueryText: "k8s",
		QueryEmbedding: []float32{0.1}, Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "m1", hits[0].ID)
	assert.Equal(t, "semantic", hits[0].Tier)
	assert.InDelta(t, 0.92, hits[0].Score, 1e-9)
	assert.Equal(t, "", hits[0].Content, "Content must be empty post-v0.3 (field dropped)")
}

// ---------------------------------------------------------------------
// error path: non-2xx surfaces the status + body
// ---------------------------------------------------------------------

func TestClient_NonSuccessSurfacesError(t *testing.T) {
	srv := newServer(t, map[string]http.HandlerFunc{
		"/v1/episodic/ingest": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":"UNAUTHORIZED","message":"bad key"}`))
		},
	})
	c := newClient(t, srv)

	_, _, err := c.IngestEpisodic(context.Background(),
		core.Session{ID: "s1", UserID: "u1"}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "401")
	assert.Contains(t, err.Error(), "bad key")
}

// ---------------------------------------------------------------------
// session_workspace: tag an existing session into an additional
// workspace
// ---------------------------------------------------------------------

func TestAddSessionWorkspace_Happy(t *testing.T) {
	srv := newServer(t, map[string]http.HandlerFunc{
		"/v1/episodic/session_workspace": func(w http.ResponseWriter, r *http.Request) {
			assertAuthHeader(t, r)
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, "u1", body["user_id"])
			assert.Equal(t, "s1", body["session_id"])
			assert.Equal(t, "w1", body["workspace_id"])
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"added":true}`))
		},
	})
	c := newClient(t, srv)

	added, err := c.AddSessionWorkspace(context.Background(),
		core.AddSessionWorkspaceInput{UserID: "u1", SessionID: "s1", WorkspaceID: "w1"})
	require.NoError(t, err)
	assert.True(t, added)
}

// 409 from chapterhouse — "session not yet persisted" — must surface
// as *chapterhouse.StatusError so the ghola HTTP layer can re-emit
// the status verbatim. Pinning the contract here prevents a future
// refactor from silently swallowing the 409 into a generic error and
// stealing the "consolidate first" guidance from the agent caller.
func TestAddSessionWorkspace_Surface409(t *testing.T) {
	srv := newServer(t, map[string]http.HandlerFunc{
		"/v1/episodic/session_workspace": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"CONFLICT","message":"session not yet persisted"}`))
		},
	})
	c := newClient(t, srv)

	_, err := c.AddSessionWorkspace(context.Background(),
		core.AddSessionWorkspaceInput{UserID: "u1", SessionID: "s1", WorkspaceID: "w1"})
	require.Error(t, err)

	var statusErr *chapterhouse.StatusError
	require.ErrorAs(t, err, &statusErr,
		"caller-facing 409 must be a typed StatusError so the ghola HTTP layer can pass it through")
	assert.Equal(t, http.StatusConflict, statusErr.StatusCode())
}

// 4xx must surface as a *StatusError carrying the wire status, so the
// ghola HTTP layer can propagate the status to its caller instead of
// escalating every chapterhouse error to 500.
func TestClient_StatusErrorPreservesWireStatus(t *testing.T) {
	srv := newServer(t, map[string]http.HandlerFunc{
		"/v1/episodic/query": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":"BAD_REQUEST","message":"invalid UUID length: 6"}`))
		},
	})
	c := newClient(t, srv)

	_, err := c.QueryEpisodicMulti(context.Background(), core.EpisodicMultiQuery{
		UserID:   "alice",
		Rankings: []string{"vector"},
	})
	require.Error(t, err)
	var se *chapterhouse.StatusError
	require.True(t, errors.As(err, &se), "client error should be *StatusError")
	assert.Equal(t, http.StatusBadRequest, se.Status)
	assert.Equal(t, http.StatusBadRequest, se.StatusCode())
	assert.Contains(t, se.Message, "invalid UUID length")
}
