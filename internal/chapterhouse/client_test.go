package chapterhouse_test

import (
	"context"
	"encoding/json"
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

func TestClient_QueryEpisodic(t *testing.T) {
	srv := newServer(t, map[string]http.HandlerFunc{
		"/v1/episodic/query": func(w http.ResponseWriter, r *http.Request) {
			assertAuthHeader(t, r)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"hits":[
				{"id":"e1","text":"hello","tier":"episodic","score":{"semantic":0.8,"fts":0.5,"merged":0.7}}
			]}`))
		},
	})
	c := newClient(t, srv)

	hits, err := c.QueryEpisodic(context.Background(), core.EpisodicQuery{
		UserID:         "u1",
		QueryText:      "hi",
		QueryEmbedding: []float32{0.1, 0.2},
		Limit:          5,
	})
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Equal(t, "e1", hits[0].ID)
	assert.Equal(t, "episodic", hits[0].Tier)
	assert.Equal(t, "hello", hits[0].Content)
	assert.InDelta(t, 0.7, hits[0].Score, 1e-9)
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
// semantic: query / feedback
// ---------------------------------------------------------------------

func TestClient_QuerySemantic(t *testing.T) {
	// v0.3 mnemeHit shape: only mneme_id/score/level/tier. The dropped
	// content/concept/confidence fields must NOT appear on the resulting
	// RecallHit (Concept/Confidence stay nil, Content stays "").
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
	assert.Nil(t, hits[0].Concept, "Concept must be nil post-v0.3 (field dropped)")
	assert.Nil(t, hits[0].Confidence, "Confidence must be nil post-v0.3 (field dropped)")
	assert.Equal(t, "", hits[0].Content, "Content must be empty post-v0.3 (field dropped)")
}

func TestClient_FeedbackSemantic_NoOpPostV03(t *testing.T) {
	// /v1/semantic/feedback was removed in chapterhouse v0.3; the client
	// short-circuits to a warn-log no-op until PR7 reintroduces feedback
	// via dogfooding-tags. The test asserts no HTTP call is made (the
	// catch-all "/" handler in newServer fails on any request) and that
	// the call returns (0, nil) cleanly.
	srv := newServer(t, map[string]http.HandlerFunc{})
	c := newClient(t, srv)

	conf, err := c.FeedbackSemantic(context.Background(), "m1", 0.95)
	require.NoError(t, err)
	assert.Equal(t, 0.0, conf)
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
