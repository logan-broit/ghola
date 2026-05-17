package truthsayer_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/logan-broit/ghola/internal/truthsayer"
)

// TestClient_RerankWireShape pins the request body the client sends and
// the response body it expects, against a httptest.Server. If either
// side of the wire changes shape, this test catches it before recall
// integration in PR-C breaks.
func TestClient_RerankWireShape(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/rerank", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		b, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(b, &gotBody))

		// Server emits desc-by-score order (truthsayer's contract).
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scores":[
			{"id":"a","score":0.92},
			{"id":"c","score":0.41},
			{"id":"b","score":0.05}
		]}`))
	}))
	defer srv.Close()

	c := truthsayer.New(srv.URL).WithHTTPClient(srv.Client())
	scores, err := c.Rerank(context.Background(), "what time is the meeting",
		[]truthsayer.Candidate{
			{ID: "a", Text: "the meeting starts at 3pm"},
			{ID: "b", Text: "i had pizza for lunch"},
			{ID: "c", Text: "meeting room is on floor 4"},
		}, 5)
	require.NoError(t, err)

	// Request body shape: query + candidates [{id, text}] + top_k.
	assert.Equal(t, "what time is the meeting", gotBody["query"])
	cands, ok := gotBody["candidates"].([]any)
	require.True(t, ok, "candidates is array")
	require.Len(t, cands, 3)
	first := cands[0].(map[string]any)
	assert.Equal(t, "a", first["id"])
	assert.Equal(t, "the meeting starts at 3pm", first["text"])
	assert.EqualValues(t, 5, gotBody["top_k"])

	// Response decoded; order preserved (descending by Score, server-side).
	require.Len(t, scores, 3)
	assert.Equal(t, "a", scores[0].ID)
	assert.InDelta(t, 0.92, scores[0].Score, 1e-9)
	assert.Equal(t, "c", scores[1].ID)
	assert.Equal(t, "b", scores[2].ID)
	assert.GreaterOrEqual(t, scores[0].Score, scores[1].Score)
	assert.GreaterOrEqual(t, scores[1].Score, scores[2].Score)
}

// TestClient_RerankOmitsTopKWhenZero confirms top_k is dropped from the
// request body when caller passes 0 — letting the server return all.
func TestClient_RerankOmitsTopKWhenZero(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(b, &gotBody))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scores":[]}`))
	}))
	defer srv.Close()

	c := truthsayer.New(srv.URL).WithHTTPClient(srv.Client())
	_, err := c.Rerank(context.Background(), "q",
		[]truthsayer.Candidate{{ID: "a", Text: "x"}}, 0)
	require.NoError(t, err)

	_, hasTopK := gotBody["top_k"]
	assert.False(t, hasTopK, "top_k=0 should be omitted from request body")
}
