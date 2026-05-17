package embedding_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/embedding"
)

// TestOpenAIProvider_EmbedBatch pins the wire format chapterhouse expects
// from any OpenAI-compat embeddings endpoint (guild, vLLM, OpenAI, etc.).
//
// The contract:
//   - request body has {"model": "...", "input": [<string>...]}
//   - response is {"data": [{"index": N, "embedding": [...]}, ...]}
//   - the provider returns vectors in input-order regardless of response order.
func TestOpenAIProvider_EmbedBatch(t *testing.T) {
	var capturedBody []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/v1/embeddings", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))

		var err error
		capturedBody, err = io.ReadAll(r.Body)
		require.NoError(t, err)

		var req struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		require.NoError(t, json.Unmarshal(capturedBody, &req))
		require.Equal(t, "qwen3-embedding", req.Model)
		require.Len(t, req.Input, 3)

		// Return vectors in REVERSE order to verify the provider re-sorts
		// by Index. A real server can return out-of-order entries.
		out := map[string]any{
			"data": []map[string]any{
				{"index": 2, "embedding": fillVec(1024, 0.3)},
				{"index": 1, "embedding": fillVec(1024, 0.2)},
				{"index": 0, "embedding": fillVec(1024, 0.1)},
			},
			"model": "qwen3-embedding",
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(out))
	}))
	defer srv.Close()

	p := embedding.NewOpenAIProvider(embedding.Config{
		URL:        srv.URL,
		Model:      "qwen3-embedding",
		Dimensions: 1024,
	}, "")

	vecs, err := p.EmbedBatch(context.Background(), []string{"alpha", "beta", "gamma"})
	require.NoError(t, err)
	require.Len(t, vecs, 3)

	// Each returned vector is 1024 wide and matches the by-Index re-sort.
	require.Len(t, vecs[0], 1024)
	require.Len(t, vecs[1], 1024)
	require.Len(t, vecs[2], 1024)
	assert.InDelta(t, 0.1, vecs[0][0], 1e-6)
	assert.InDelta(t, 0.2, vecs[1][0], 1e-6)
	assert.InDelta(t, 0.3, vecs[2][0], 1e-6)

	// Wire shape check: input is a JSON array of strings, not a single string.
	assert.True(t, strings.Contains(string(capturedBody), `"input":["alpha","beta","gamma"]`),
		"input must be a JSON array of strings, got: %s", string(capturedBody))
}

// TestOpenAIProvider_EmbedBatch_EmptyInput pins that empty input is a
// short-circuit, NOT a wasted HTTP round-trip. Some callers (e.g. the
// episodic ingest backstop after filtering for nil-embedding events)
// will legitimately call this with [] when nothing needs embedding.
func TestOpenAIProvider_EmbedBatch_EmptyInput(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p := embedding.NewOpenAIProvider(embedding.Config{
		URL:        srv.URL,
		Model:      "qwen3-embedding",
		Dimensions: 1024,
	}, "")

	vecs, err := p.EmbedBatch(context.Background(), nil)
	require.NoError(t, err)
	assert.Nil(t, vecs)
	assert.False(t, called, "empty input must not call the upstream")

	vecs, err = p.EmbedBatch(context.Background(), []string{})
	require.NoError(t, err)
	assert.Nil(t, vecs)
	assert.False(t, called, "empty slice must not call the upstream")
}

// TestOpenAIProvider_EmbedBatch_HTTPError pins the all-or-nothing
// contract: a non-200 from the embeddings server bubbles as an error,
// no partial success. The episodic-ingest backstop relies on this — if
// the embedder errors, the entire ingest must fail (no NULL-embedding
// rows persisted).
func TestOpenAIProvider_EmbedBatch_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("upstream is on fire"))
	}))
	defer srv.Close()

	p := embedding.NewOpenAIProvider(embedding.Config{
		URL:        srv.URL,
		Model:      "qwen3-embedding",
		Dimensions: 1024,
	}, "")

	vecs, err := p.EmbedBatch(context.Background(), []string{"a", "b"})
	require.Error(t, err)
	assert.Nil(t, vecs)
	assert.Contains(t, err.Error(), "500")
}

// TestOpenAIProvider_Embed_DelegatesToBatch verifies the single-text
// Embed path goes through EmbedBatch (one network code path is simpler
// to reason about + fewer wire-format surprises).
func TestOpenAIProvider_Embed_DelegatesToBatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		require.Len(t, req.Input, 1)
		require.Equal(t, "lonely text", req.Input[0])

		out := map[string]any{
			"data": []map[string]any{
				{"index": 0, "embedding": fillVec(1024, 0.5)},
			},
		}
		require.NoError(t, json.NewEncoder(w).Encode(out))
	}))
	defer srv.Close()

	p := embedding.NewOpenAIProvider(embedding.Config{
		URL:        srv.URL,
		Model:      "qwen3-embedding",
		Dimensions: 1024,
	}, "")

	vec, err := p.Embed(context.Background(), "lonely text")
	require.NoError(t, err)
	require.Len(t, vec, 1024)
	assert.InDelta(t, 0.5, vec[0], 1e-6)
}

// TestOpenAIProvider_Embed_EmptyText pins that empty text returns
// ErrEmptyInput without a network round-trip. Episodic events without
// text bodies (e.g. tool calls) are caught upstream by the backstop's
// nil-text filter; this test pins the contract for direct callers.
func TestOpenAIProvider_Embed_EmptyText(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	p := embedding.NewOpenAIProvider(embedding.Config{
		URL:        srv.URL,
		Model:      "qwen3-embedding",
		Dimensions: 1024,
	}, "")

	_, err := p.Embed(context.Background(), "")
	require.Error(t, err)
	assert.True(t, errors.Is(err, embedding.ErrEmptyInput))
	assert.False(t, called)
}

// fillVec returns a 1024-element vector filled with v, used as a
// deterministic stand-in for the upstream's response.
func fillVec(n int, v float32) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = v
	}
	return out
}
