package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	stdhttp "net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/logan-broit/ghola/internal/chapterhouse"
	"github.com/logan-broit/ghola/internal/core"
	ghttp "github.com/logan-broit/ghola/internal/http"
	"github.com/logan-broit/ghola/internal/sietch"
)

// Fakes kept trivial — the server is intentionally a dumb JSON shim;
// anything interesting belongs in Core (internal/core/core_test.go
// covers it). These tests prove the wire surface, not behavior.

// testUserID is a fixed valid UUID — the HTTP layer rejects non-UUID
// user_id values, so tests need a real one. Using one constant keeps
// failure messages legible across the suite.
const testUserID = "00000000-0000-0000-0000-0000000000a1"

type noopChapterhouse struct{}

func (noopChapterhouse) IngestEpisodic(context.Context, core.Session, []core.Event) (int, int, error) {
	return 0, 0, nil
}
func (noopChapterhouse) QueryEpisodicMulti(context.Context, core.EpisodicMultiQuery) (core.EpisodicMultiResult, error) {
	return core.EpisodicMultiResult{}, nil
}
func (noopChapterhouse) ShareEpisodic(context.Context, core.ShareInput) (string, error) {
	return "share-id", nil
}
func (noopChapterhouse) ForgetEpisodic(context.Context, string, []string) (int, error) {
	return 0, nil
}
func (noopChapterhouse) AddSessionWorkspace(context.Context, core.AddSessionWorkspaceInput) (bool, error) {
	return true, nil
}
func (noopChapterhouse) QuerySemantic(context.Context, core.SemanticQuery) ([]core.RecallHit, error) {
	return nil, nil
}
func (noopChapterhouse) ConsolidateWorkspace(context.Context, string) error {
	return nil
}

type fixedEmbedder struct{}

func (fixedEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3, 0.4}, nil
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newTestServerWithChapterhouse(t, noopChapterhouse{})
}

// newTestServerWithChapterhouse parametrizes the chapterhouse fake so
// tests can inject a custom one (e.g. to assert pass-through of a 409
// from the wire as a 409 to the caller).
func newTestServerWithChapterhouse(t *testing.T, ch core.ChapterhouseClient) *httptest.Server {
	t.Helper()

	s, err := sietch.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	c := core.New(s, ch, fixedEmbedder{})
	srv := ghttp.NewServer(c, slogNoop())
	// Accept connections regardless of origin so httptest's
	// 127.0.0.1 dialer works without special-casing.
	srv.LoopbackOnly = false

	return httptest.NewServer(srv.Handler())
}

// ---------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------

func post(t *testing.T, srv *httptest.Server, path string, body any) (*stdhttp.Response, []byte) {
	t.Helper()
	buf := new(bytes.Buffer)
	if body != nil {
		require.NoError(t, json.NewEncoder(buf).Encode(body))
	}
	resp, err := srv.Client().Post(srv.URL+path, "application/json", buf)
	require.NoError(t, err)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, b
}

// ---------------------------------------------------------------------
// Happy-path smoke across the endpoint surface. One test exercises a
// realistic session: start -> record -> recall -> consolidate -> end.
// ---------------------------------------------------------------------

func TestServer_EndToEndLoop(t *testing.T) {
	srv := newTestServer(t)

	// 1. /v1/session_start
	cwd := "/test"
	resp, body := post(t, srv, "/v1/session_start",
		core.SessionStartInput{UserID: testUserID, Cwd: &cwd})
	require.Equal(t, stdhttp.StatusOK, resp.StatusCode, "body=%s", body)
	var startResp struct {
		Session core.Session `json:"session"`
	}
	require.NoError(t, json.Unmarshal(body, &startResp))
	sessionID := startResp.Session.ID
	require.NotEmpty(t, sessionID)

	// 2. /v1/record
	text := "hello world"
	rawEvt := []byte(`{"t":"hello"}`)
	resp, body = post(t, srv, "/v1/record", core.RecordInput{
		SessionID: sessionID, UserID: testUserID,
		Event: core.Event{Type: "user", Text: &text, RawEvent: rawEvt},
	})
	require.Equal(t, stdhttp.StatusOK, resp.StatusCode, "body=%s", body)
	var recResp struct {
		Event core.Event `json:"event"`
	}
	require.NoError(t, json.Unmarshal(body, &recResp))
	assert.Empty(t, recResp.Event.Embedding,
		"/v1/record response must omit the embedding (server-side artifact; ~20 KB noise per call)")
	assert.NotEmpty(t, recResp.Event.ID,
		"record response must include the assigned event id")

	// 3. /v1/recall — should surface the recorded event from sietch.
	resp, body = post(t, srv, "/v1/recall", core.RecallInput{
		SessionID:     sessionID,
		UserID:        testUserID,
		Workspace:     "00000000-0000-0000-0000-0000000000ff",
		QueryText:     "hello",
		IncludeSietch: true,
	})
	require.Equal(t, stdhttp.StatusOK, resp.StatusCode, "body=%s", body)
	var recallResp core.RecallResult
	require.NoError(t, json.Unmarshal(body, &recallResp))
	assert.NotEmpty(t, recallResp.Hits, "recall should see the just-recorded event")

	// 4. /v1/consolidate — flush to the (no-op) chapterhouse.
	resp, body = post(t, srv, "/v1/consolidate",
		map[string]string{"session_id": sessionID})
	require.Equal(t, stdhttp.StatusOK, resp.StatusCode, "body=%s", body)

	// 5. /v1/session_end
	resp, _ = post(t, srv, "/v1/session_end",
		map[string]string{"session_id": sessionID})
	require.Equal(t, stdhttp.StatusOK, resp.StatusCode)
}

func TestServer_InputValidation_400(t *testing.T) {
	srv := newTestServer(t)

	// /v1/record without session_id -> 400
	resp, _ := post(t, srv, "/v1/record", core.RecordInput{UserID: testUserID})
	assert.Equal(t, stdhttp.StatusBadRequest, resp.StatusCode)
}

// A malformed workspace_id is a client error, not a server error. The
// HTTP error mapper classifies by substring (`required`/`must`/`evidence`),
// so the core wrap message must contain one of those tokens to surface
// as 400 instead of falling through to 500. This test pins the wire-
// level contract so a future error-message refactor can't silently
// regress it back to 500.
func TestServer_SessionStart_RejectsInvalidWorkspaceID(t *testing.T) {
	srv := newTestServer(t)
	resp, body := post(t, srv, "/v1/session_start", core.SessionStartInput{
		UserID:      testUserID,
		WorkspaceID: "not-a-uuid",
	})
	assert.Equal(t, stdhttp.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, string(body), "workspace_id must be a valid UUID")
}

// resolveUserID enforces three branches; one test per branch documents
// each contract violation as a 4xx, plus the success path that
// AUTH_DEFAULT_USER backs into when user_id is omitted.

func TestServer_UserID_RejectsNonUUID(t *testing.T) {
	srv := newTestServer(t)

	// A friendly handle is the canonical non-UUID failure mode — it's
	// the case that motivated this validation in the first place.
	resp, body := post(t, srv, "/v1/session_start",
		core.SessionStartInput{UserID: "alice"})
	assert.Equal(t, stdhttp.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, string(body), "must be a UUID")
}

func TestServer_UserID_RequiredWhenNoDefault(t *testing.T) {
	srv := newTestServer(t)

	resp, body := post(t, srv, "/v1/session_start",
		core.SessionStartInput{}) // user_id omitted, no default configured
	assert.Equal(t, stdhttp.StatusBadRequest, resp.StatusCode)
	assert.Contains(t, string(body), "user_id required")
}

func TestServer_UserID_FallsBackToDefault(t *testing.T) {
	// Wire a server with a default user — same path that
	// cmd/ghola/main.go uses when AUTH_DEFAULT_USER is set in env.
	s, err := sietch.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	c := core.New(s, noopChapterhouse{}, fixedEmbedder{})
	srv := ghttp.NewServer(c, slogNoop())
	srv.LoopbackOnly = false
	require.NoError(t, srv.SetDefaultUserID(testUserID))

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	// Omit user_id; the server should fall back to the configured
	// default and the session_start should succeed. Cwd is required
	// post-Task-3 to satisfy the workspace-or-cwd precondition; this
	// test asserts the user-id fallback, so workspace presence is
	// orthogonal — pass cwd so the session reaches Core.SessionStart's
	// happy path.
	cwd := "/test"
	resp, body := post(t, ts, "/v1/session_start",
		core.SessionStartInput{Cwd: &cwd}) // user_id intentionally empty
	require.Equal(t, stdhttp.StatusOK, resp.StatusCode, "body=%s", body)

	var startResp struct {
		Session core.Session `json:"session"`
	}
	require.NoError(t, json.Unmarshal(body, &startResp))
	assert.Equal(t, testUserID, startResp.Session.UserID,
		"session should carry the resolved default user")
}

func TestServer_Health(t *testing.T) {
	srv := newTestServer(t)
	resp, err := srv.Client().Get(srv.URL + "/health")
	require.NoError(t, err)
	defer resp.Body.Close()
	assert.Equal(t, stdhttp.StatusOK, resp.StatusCode)
}

func TestServer_LoopbackGuardRejectsRemoteOrigin(t *testing.T) {
	s, err := sietch.Open(t.TempDir())
	require.NoError(t, err)
	defer s.Close()

	c := core.New(s, noopChapterhouse{}, fixedEmbedder{})
	srv := ghttp.NewServer(c, slogNoop())
	srv.LoopbackOnly = true // explicit

	// Synthesize a request from a non-loopback origin.
	req := httptest.NewRequest(stdhttp.MethodGet, "http://example.com/health", nil)
	req.RemoteAddr = "10.0.0.5:54321"

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, stdhttp.StatusForbidden, rec.Code)
}

// TestServer_ExpandSessionWorkspace_Happy pins the wire shape of
// /v1/session_workspace: 200 OK + {"added": true} when the underlying
// chapterhouse call succeeds.
func TestServer_ExpandSessionWorkspace_Happy(t *testing.T) {
	srv := newTestServer(t)

	// First start a session (the noopChapterhouse stub returns
	// (true, nil) regardless of state, so we don't need
	// session-existence simulation).
	cwd := "/test"
	resp, body := post(t, srv, "/v1/session_start",
		core.SessionStartInput{
			UserID: testUserID,
			Cwd:    &cwd,
		})
	require.Equal(t, stdhttp.StatusOK, resp.StatusCode, "body=%s", body)

	resp, body = post(t, srv, "/v1/session_workspace", core.AddSessionWorkspaceInput{
		UserID:      testUserID,
		SessionID:   "sess-1",
		WorkspaceID: "22222222-3333-4444-5555-666666666666",
	})
	require.Equal(t, stdhttp.StatusOK, resp.StatusCode, "body=%s", body)
	var out struct {
		Added bool `json:"added"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	assert.True(t, out.Added)
}

// recordingChapterhouse captures the multi-ranking query args so the
// HTTP layer's tags_any plumbing can be asserted end-to-end without
// going to a real chapterhouse server.
type recordingChapterhouse struct {
	noopChapterhouse
	multiQueries          []core.EpisodicMultiQuery
	semQueries            []core.SemanticQuery
	consolidateWorkspaces []string
}

func (r *recordingChapterhouse) QueryEpisodicMulti(_ context.Context, q core.EpisodicMultiQuery) (core.EpisodicMultiResult, error) {
	r.multiQueries = append(r.multiQueries, q)
	return core.EpisodicMultiResult{}, nil
}
func (r *recordingChapterhouse) QuerySemantic(_ context.Context, q core.SemanticQuery) ([]core.RecallHit, error) {
	r.semQueries = append(r.semQueries, q)
	return nil, nil
}
func (r *recordingChapterhouse) ConsolidateWorkspace(_ context.Context, workspaceID string) error {
	r.consolidateWorkspaces = append(r.consolidateWorkspaces, workspaceID)
	return nil
}

// TestServer_Recall_TagsAnyReachesEventGrainTiers — H3.c wire-level pin.
// POSTing /v1/recall with {"tags_any":[...]} must propagate the filter
// through the HTTP DTO into RecallInput and out to the chapterhouse
// client's multi-ranking request. Pinning the DTO field name protects
// future renames from silently breaking the eval harness.
func TestServer_Recall_TagsAnyReachesEventGrainTiers(t *testing.T) {
	rch := &recordingChapterhouse{}
	srv := newTestServerWithChapterhouse(t, rch)

	resp, body := post(t, srv, "/v1/recall", map[string]any{
		"user_id":    testUserID,
		"workspace":  "00000000-0000-0000-0000-0000000000ff",
		"query_text": "kubernetes",
		"tags_any":   []string{"era:v15"},
	})
	require.Equal(t, stdhttp.StatusOK, resp.StatusCode, "body=%s", body)

	require.Len(t, rch.multiQueries, 1, "single multi-ranking call")
	assert.Equal(t, []string{"era:v15"}, rch.multiQueries[0].TagsAny,
		"multi-ranking request must receive tags_any from /v1/recall body — "+
			"chapterhouse forwards it to the event-grain tiers server-side")
	assert.Contains(t, rch.multiQueries[0].Rankings, "vector",
		"vector tier requested for query_text+tags_any recall")
	assert.Contains(t, rch.multiQueries[0].Rankings, "fts",
		"fts tier requested when query_text is non-empty")
}

// TestServer_Recall_PrimitivesFlagReachesMultiQuery — D3 wire-level
// pin. POSTing /v1/recall with {"primitives":true} must propagate the
// flag through the HTTP DTO into RecallInput and out to the
// chapterhouse multi-ranking request. Pins the JSON DTO field name
// ("primitives") so future renames don't silently break the eval
// harness CLI flag (D4).
func TestServer_Recall_PrimitivesFlagReachesMultiQuery(t *testing.T) {
	rch := &recordingChapterhouse{}
	srv := newTestServerWithChapterhouse(t, rch)

	resp, body := post(t, srv, "/v1/recall", map[string]any{
		"user_id":    testUserID,
		"workspace":  "00000000-0000-0000-0000-0000000000ff",
		"query_text": "kubernetes",
		"primitives": true,
	})
	require.Equal(t, stdhttp.StatusOK, resp.StatusCode, "body=%s", body)

	require.Len(t, rch.multiQueries, 1, "single multi-ranking call")
	assert.True(t, rch.multiQueries[0].Primitives,
		"multi-ranking request must receive primitives=true from /v1/recall body")
}

// TestServer_Recall_PrimitivesDefaultsOff — without the field in the
// body, the multi-query must carry Primitives=false. Pins the
// "opt-in only, never on by default" property end-to-end.
func TestServer_Recall_PrimitivesDefaultsOff(t *testing.T) {
	rch := &recordingChapterhouse{}
	srv := newTestServerWithChapterhouse(t, rch)

	resp, body := post(t, srv, "/v1/recall", map[string]any{
		"user_id":    testUserID,
		"workspace":  "00000000-0000-0000-0000-0000000000ff",
		"query_text": "kubernetes",
	})
	require.Equal(t, stdhttp.StatusOK, resp.StatusCode, "body=%s", body)

	require.Len(t, rch.multiQueries, 1)
	assert.False(t, rch.multiQueries[0].Primitives,
		"primitives must default to false when absent from the request body")
}

// TestServer_Recall_SettleOnByDefault pins the post-flip contract at the
// wire level: omitting the settle field applies the server default
// (channel), so the chapterhouse multi-ranking request carries Settle=true.
// This replaces the pre-flip "unset → off" wire pin — see the LongMemEval
// settle gate in docs/benchmarks.md.
func TestServer_Recall_SettleOnByDefault(t *testing.T) {
	rch := &recordingChapterhouse{}
	srv := newTestServerWithChapterhouse(t, rch)

	resp, body := post(t, srv, "/v1/recall", map[string]any{
		"user_id":    testUserID,
		"workspace":  "00000000-0000-0000-0000-0000000000ff",
		"query_text": "kubernetes",
	})
	require.Equal(t, stdhttp.StatusOK, resp.StatusCode, "body=%s", body)

	require.Len(t, rch.multiQueries, 1, "single multi-ranking call")
	assert.True(t, rch.multiQueries[0].Settle,
		"settle must be true when omitted — the server default (channel) applies")
}

// TestServer_Recall_SettleExplicitOff pins the explicit opt-out at the wire
// level: POSTing {"settle":"off"} maps to EpisodicMultiQuery.Settle=false so
// chapterhouse never runs spreading activation — byte-identical to the pre-P4
// pipeline. This is the pre-flip default behavior, now reachable only by
// asking for it explicitly.
func TestServer_Recall_SettleExplicitOff(t *testing.T) {
	rch := &recordingChapterhouse{}
	srv := newTestServerWithChapterhouse(t, rch)

	resp, body := post(t, srv, "/v1/recall", map[string]any{
		"user_id":    testUserID,
		"workspace":  "00000000-0000-0000-0000-0000000000ff",
		"query_text": "kubernetes",
		"settle":     "off",
	})
	require.Equal(t, stdhttp.StatusOK, resp.StatusCode, "body=%s", body)

	require.Len(t, rch.multiQueries, 1, "single multi-ranking call")
	assert.False(t, rch.multiQueries[0].Settle,
		"settle=off must leave the multi-query settle flag false — pre-P4 byte-identical path")
}

// TestServer_Recall_SettleExpandReachesMultiQuery — T7 wire-level pin.
// POSTing /v1/recall with {"settle":"expand"} must propagate through the
// HTTP DTO into RecallInput and out to the chapterhouse multi-ranking
// request as Settle=true. Config A: spreading activation only; activation
// is not folded into score fusion.
func TestServer_Recall_SettleExpandReachesMultiQuery(t *testing.T) {
	rch := &recordingChapterhouse{}
	srv := newTestServerWithChapterhouse(t, rch)

	resp, body := post(t, srv, "/v1/recall", map[string]any{
		"user_id":    testUserID,
		"workspace":  "00000000-0000-0000-0000-0000000000ff",
		"query_text": "kubernetes",
		"settle":     "expand",
	})
	require.Equal(t, stdhttp.StatusOK, resp.StatusCode, "body=%s", body)

	require.Len(t, rch.multiQueries, 1, "single multi-ranking call")
	assert.True(t, rch.multiQueries[0].Settle,
		"multi-ranking request must receive Settle=true when settle=expand is requested")
}

// TestServer_Recall_SettleChannelWithWeightReachesMultiQuery — T7 wire-level pin.
// POSTing /v1/recall with {"settle":"channel","activation_weight":0.2} must
// reach the chapterhouse multi-ranking request with Settle=true. Activation
// weight is consumed by ghola-side score fusion, not forwarded to chapterhouse.
func TestServer_Recall_SettleChannelWithWeightReachesMultiQuery(t *testing.T) {
	rch := &recordingChapterhouse{}
	srv := newTestServerWithChapterhouse(t, rch)

	resp, body := post(t, srv, "/v1/recall", map[string]any{
		"user_id":           testUserID,
		"workspace":         "00000000-0000-0000-0000-0000000000ff",
		"query_text":        "kubernetes",
		"settle":            "channel",
		"activation_weight": 0.2,
	})
	require.Equal(t, stdhttp.StatusOK, resp.StatusCode, "body=%s", body)

	require.Len(t, rch.multiQueries, 1, "single multi-ranking call")
	assert.True(t, rch.multiQueries[0].Settle,
		"multi-ranking request must receive Settle=true when settle=channel is requested")
}

// TestServer_Recall_SettleBogusMode400 pins that an unknown settle mode is
// rejected at the Recall boundary with a 400. core.Recall validates the mode
// and returns ErrValidation, which the HTTP layer maps to 400.
func TestServer_Recall_SettleBogusMode400(t *testing.T) {
	srv := newTestServer(t)

	resp, body := post(t, srv, "/v1/recall", map[string]any{
		"user_id":   testUserID,
		"workspace": "00000000-0000-0000-0000-0000000000ff",
		"settle":    "fly",
	})
	require.Equal(t, stdhttp.StatusBadRequest, resp.StatusCode,
		"unknown settle mode must return 400; body=%s", body)
}

// TestServer_Recall_SettleChannelWeightSumExceeds400 pins the coupling: when
// settle=channel is requested with activation_weight=0.6 and the server's
// default RerankWeight is 0.5, their sum (1.1) exceeds 1 and Recall rejects
// the request with a 400. The error message must mention both weights so the
// caller knows which constraint was violated.
func TestServer_Recall_SettleChannelWeightSumExceeds400(t *testing.T) {
	srv := newTestServer(t) // default Core: RerankWeight=0.5

	resp, body := post(t, srv, "/v1/recall", map[string]any{
		"user_id":           testUserID,
		"workspace":         "00000000-0000-0000-0000-0000000000ff",
		"settle":            "channel",
		"activation_weight": 0.6,
	})
	require.Equal(t, stdhttp.StatusBadRequest, resp.StatusCode,
		"rerank_weight+activation_weight > 1 must return 400; body=%s", body)
	assert.Contains(t, string(body), "rerank_weight",
		"error message must mention rerank_weight so the caller knows the constraint")
}

// TestServer_Recall_SettleParamsOutOfRange400 pins that an out-of-range
// settle_params knob is rejected at the Recall boundary with a 400 rather
// than being silently absorbed by chapterhouse's DefaultSettleParams. The
// error message must name the offending field so the caller can correct it.
func TestServer_Recall_SettleParamsOutOfRange400(t *testing.T) {
	srv := newTestServer(t)

	resp, body := post(t, srv, "/v1/recall", map[string]any{
		"user_id":       testUserID,
		"workspace":     "00000000-0000-0000-0000-0000000000ff",
		"settle":        "expand",
		"settle_params": map[string]any{"lambda": -0.7},
	})
	require.Equal(t, stdhttp.StatusBadRequest, resp.StatusCode,
		"out-of-range settle_params.lambda must return 400; body=%s", body)
	assert.Contains(t, string(body), "lambda",
		"error message must name the offending settle_params field")
}

// conflict409Chapterhouse is a test fake that satisfies
// ChapterhouseClient and returns *StatusError{409} from
// AddSessionWorkspace — proves the HTTP handler maps 409 from
// chapterhouse to 409 to the caller.
type conflict409Chapterhouse struct{ noopChapterhouse }

func (conflict409Chapterhouse) AddSessionWorkspace(context.Context, core.AddSessionWorkspaceInput) (bool, error) {
	return false, &chapterhouse.StatusError{
		Status:  stdhttp.StatusConflict,
		Path:    "/v1/episodic/session_workspace",
		Message: "session not yet persisted",
	}
}

// TestServer_ExpandSessionWorkspace_PassesThrough409 is load-bearing:
// without 409 -> 409 mapping, an agent calling expand mid-conversation
// would see a bare 500 and have no way to know "consolidate first" is
// the fix.
func TestServer_ExpandSessionWorkspace_PassesThrough409(t *testing.T) {
	srv := newTestServerWithChapterhouse(t, conflict409Chapterhouse{})

	resp, _ := post(t, srv, "/v1/session_workspace", core.AddSessionWorkspaceInput{
		UserID:      testUserID,
		SessionID:   "sess-x",
		WorkspaceID: "11111111-2222-3333-4444-555555555555",
	})
	assert.Equal(t, stdhttp.StatusConflict, resp.StatusCode)
}

// ---------------------------------------------------------------------
// /v1/semantic/consolidate — manual trigger for chapterhouse's
// episodic->semantic consolidation batch. Distinct from /v1/consolidate
// above (sietch->episodic session flush, Pipeline A).
// ---------------------------------------------------------------------

func TestServer_ConsolidateWorkspace_Happy(t *testing.T) {
	rch := &recordingChapterhouse{}
	srv := newTestServerWithChapterhouse(t, rch)

	ws := "11111111-2222-3333-4444-555555555555"
	resp, body := post(t, srv, "/v1/semantic/consolidate", map[string]any{
		"workspace": ws,
	})
	require.Equal(t, stdhttp.StatusOK, resp.StatusCode, "body=%s", body)

	var out struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	assert.Equal(t, "ok", out.Status)

	require.Len(t, rch.consolidateWorkspaces, 1)
	assert.Equal(t, ws, rch.consolidateWorkspaces[0])
}

// TestServer_ConsolidateWorkspace_DerivesFromCwd mirrors recall's cwd
// ergonomics at the wire level: an agent that only knows cwd must still
// be able to trigger consolidation.
func TestServer_ConsolidateWorkspace_DerivesFromCwd(t *testing.T) {
	rch := &recordingChapterhouse{}
	srv := newTestServerWithChapterhouse(t, rch)

	cwd := "/home/loganb/ghola"
	resp, body := post(t, srv, "/v1/semantic/consolidate", map[string]any{
		"cwd": cwd,
	})
	require.Equal(t, stdhttp.StatusOK, resp.StatusCode, "body=%s", body)

	require.Len(t, rch.consolidateWorkspaces, 1)
	assert.Equal(t, core.WorkspaceForCwd(cwd).String(), rch.consolidateWorkspaces[0])
}

func TestServer_ConsolidateWorkspace_MissingWorkspaceAndCwd_Returns400(t *testing.T) {
	rch := &recordingChapterhouse{}
	srv := newTestServerWithChapterhouse(t, rch)

	resp, body := post(t, srv, "/v1/semantic/consolidate", map[string]any{})
	assert.Equal(t, stdhttp.StatusBadRequest, resp.StatusCode, "body=%s", body)
	assert.Empty(t, rch.consolidateWorkspaces)
}

// consolidateFail500Chapterhouse simulates a mentat-down loud-fail from
// consolidation.RunWorkspace: chapterhouse's own endpoint doesn't wrap
// this as a *StatusError (it's a 500 INTERNAL_ERROR on the chapterhouse
// side per Task 17), so the ghola client surfaces it as a plain error
// and the HTTP layer's default classification (handleErr) must map it
// to 500 — not silently swallow it as 200.
type consolidateFail500Chapterhouse struct{ noopChapterhouse }

func (consolidateFail500Chapterhouse) ConsolidateWorkspace(context.Context, string) error {
	return &chapterhouse.StatusError{
		Status:  stdhttp.StatusInternalServerError,
		Path:    "/v1/semantic/consolidate",
		Message: "consolidation failed: mentat cluster (loud-fail): connection refused",
	}
}

func TestServer_ConsolidateWorkspace_PropagatesChapterhouseFailure(t *testing.T) {
	srv := newTestServerWithChapterhouse(t, consolidateFail500Chapterhouse{})

	resp, body := post(t, srv, "/v1/semantic/consolidate", map[string]any{
		"workspace": "11111111-2222-3333-4444-555555555555",
	})
	assert.Equal(t, stdhttp.StatusInternalServerError, resp.StatusCode, "body=%s", body)
}

// ---------------------------------------------------------------------
// Bug fixes: event.type validation (Bug 1) + embedding omission (Bug 2)
// ---------------------------------------------------------------------

// TestServer_Record_InvalidEventType_Returns400 pins Bug 1: an event
// whose type is not in the allowed set must return 400 (not 500) with
// a message that names the offending value and the allowed set.
func TestServer_Record_InvalidEventType_Returns400(t *testing.T) {
	srv := newTestServer(t)

	// Start a session so we have a valid session_id.
	cwd := "/test"
	resp, body := post(t, srv, "/v1/session_start",
		core.SessionStartInput{UserID: testUserID, Cwd: &cwd})
	require.Equal(t, stdhttp.StatusOK, resp.StatusCode, "setup: session_start body=%s", body)
	var startResp struct {
		Session core.Session `json:"session"`
	}
	require.NoError(t, json.Unmarshal(body, &startResp))
	sessionID := startResp.Session.ID

	text := "this is a decision"
	resp, body = post(t, srv, "/v1/record", core.RecordInput{
		SessionID: sessionID,
		UserID:    testUserID,
		Event:     core.Event{Type: "decision", Text: &text, RawEvent: []byte(`{}`)},
	})
	assert.Equal(t, stdhttp.StatusBadRequest, resp.StatusCode,
		"invalid event.type must be 400, not 500; body=%s", body)
	bodyStr := string(body)
	assert.Contains(t, bodyStr, "decision",
		"error message must name the offending type value")
	assert.Contains(t, bodyStr, "user",
		"error message must list allowed types")
	assert.Contains(t, bodyStr, "assistant",
		"error message must list allowed types")
}

// TestServer_Record_ValidEventTypes_Return200 ensures none of the
// four allowed types are accidentally rejected by the new validation.
func TestServer_Record_ValidEventTypes_Return200(t *testing.T) {
	for _, evType := range []string{"user", "assistant", "tool_result", "system"} {
		evType := evType
		t.Run(evType, func(t *testing.T) {
			srv := newTestServer(t)
			cwd := "/test"
			resp, body := post(t, srv, "/v1/session_start",
				core.SessionStartInput{UserID: testUserID, Cwd: &cwd})
			require.Equal(t, stdhttp.StatusOK, resp.StatusCode, "setup body=%s", body)
			var startResp struct {
				Session core.Session `json:"session"`
			}
			require.NoError(t, json.Unmarshal(body, &startResp))

			text := "some text"
			resp, body = post(t, srv, "/v1/record", core.RecordInput{
				SessionID: startResp.Session.ID,
				UserID:    testUserID,
				Event:     core.Event{Type: evType, Text: &text, RawEvent: []byte(`{}`)},
			})
			assert.Equal(t, stdhttp.StatusOK, resp.StatusCode,
				"type=%q must be accepted; body=%s", evType, body)
		})
	}
}

// TestServer_Record_ResponseOmitsEmbedding pins Bug 2: the /v1/record
// response JSON must not contain an "embedding" key. Callers sent the
// text; the embedding is a server-side artifact and ~20KB of noise per
// call when echoed back.
func TestServer_Record_ResponseOmitsEmbedding(t *testing.T) {
	srv := newTestServer(t)

	cwd := "/test"
	resp, body := post(t, srv, "/v1/session_start",
		core.SessionStartInput{UserID: testUserID, Cwd: &cwd})
	require.Equal(t, stdhttp.StatusOK, resp.StatusCode, "setup body=%s", body)
	var startResp struct {
		Session core.Session `json:"session"`
	}
	require.NoError(t, json.Unmarshal(body, &startResp))

	text := "hello"
	resp, body = post(t, srv, "/v1/record", core.RecordInput{
		SessionID: startResp.Session.ID,
		UserID:    testUserID,
		Event:     core.Event{Type: "user", Text: &text, RawEvent: []byte(`{}`)},
	})
	require.Equal(t, stdhttp.StatusOK, resp.StatusCode, "body=%s", body)

	// Unmarshal into a raw map so we can check key presence directly —
	// a typed struct would silently swallow missing fields.
	var raw map[string]any
	require.NoError(t, json.Unmarshal(body, &raw))
	eventRaw, ok := raw["event"].(map[string]any)
	require.True(t, ok, "response must have an 'event' object; got %s", body)
	_, hasEmbedding := eventRaw["embedding"]
	assert.False(t, hasEmbedding,
		"/v1/record response must not include the 'embedding' field; body=%s", body)
}
