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

	"github.com/logan-broit/ghola/internal/core"
	ghttp "github.com/logan-broit/ghola/internal/http"
	"github.com/logan-broit/ghola/internal/sietch"
)

// Fakes kept trivial — the server is intentionally a dumb JSON shim;
// anything interesting belongs in Core (internal/core/core_test.go
// covers it). These tests prove the wire surface, not behavior.

type noopChapterhouse struct{}

func (noopChapterhouse) IngestEpisodic(context.Context, core.Session, []core.Event) (int, int, error) {
	return 0, 0, nil
}
func (noopChapterhouse) QueryEpisodic(context.Context, core.EpisodicQuery) ([]core.RecallHit, error) {
	return nil, nil
}
func (noopChapterhouse) ShareEpisodic(context.Context, core.ShareInput) (string, error) {
	return "share-id", nil
}
func (noopChapterhouse) ForgetEpisodic(context.Context, string, []string) (int, error) {
	return 0, nil
}
func (noopChapterhouse) QuerySemantic(context.Context, core.SemanticQuery) ([]core.RecallHit, error) {
	return nil, nil
}
func (noopChapterhouse) FeedbackSemantic(context.Context, string, float64) (float64, error) {
	return 0.88, nil
}

type fixedEmbedder struct{}

func (fixedEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3, 0.4}, nil
}

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	s, err := sietch.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	c := core.New(s, noopChapterhouse{}, fixedEmbedder{})
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
	resp, body := post(t, srv, "/v1/session_start",
		core.SessionStartInput{UserID: "u1"})
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
		SessionID: sessionID, UserID: "u1",
		Event: core.Event{Type: "user", Text: &text, RawEvent: rawEvt},
	})
	require.Equal(t, stdhttp.StatusOK, resp.StatusCode, "body=%s", body)
	var recResp struct {
		Event core.Event `json:"event"`
	}
	require.NoError(t, json.Unmarshal(body, &recResp))
	assert.Equal(t, []float32{0.1, 0.2, 0.3, 0.4}, recResp.Event.Embedding,
		"embedder output must be attached by Core.Record")

	// 3. /v1/recall — should surface the recorded event from sietch.
	resp, body = post(t, srv, "/v1/recall", core.RecallInput{
		SessionID: sessionID, UserID: "u1",
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
	resp, _ := post(t, srv, "/v1/record", core.RecordInput{UserID: "u1"})
	assert.Equal(t, stdhttp.StatusBadRequest, resp.StatusCode)

	// /v1/feedback with out-of-range evidence -> 400
	resp, _ = post(t, srv, "/v1/feedback",
		map[string]any{"mneme_id": "m1", "evidence": 1.5})
	assert.Equal(t, stdhttp.StatusBadRequest, resp.StatusCode)
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
