package embedding_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/embedding"
)

// These tests pin the OpenAIProvider ADAPTER semantics: the ch-server
// caller contracts (ErrEmptyInput, ErrBatchTooLarge, empty-input
// short-circuit, single-text-through-batch, Name/Dimensions). The
// transport-level wire format, retry/backoff, and by-index re-sort are
// owned by github.com/logan-broit/ghola/pkg/embedding and tested there;
// the duplicate transport test (formerly TestOpenAIProvider_EmbedBatch,
// which re-asserted the index re-sort + JSON-array wire shape) was
// removed when the provider became a thin adapter over that client.

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

// TestOpenAIProvider_EmbedBatch_TooLarge pins the adapter's hard batch
// cap: a request larger than MaxBatch is rejected with ErrBatchTooLarge
// before any HTTP round-trip. The shared client does not impose this
// cap (it chunks instead); the cap is a ch-server caller contract.
func TestOpenAIProvider_EmbedBatch_TooLarge(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	p := embedding.NewOpenAIProvider(embedding.Config{
		URL:      srv.URL,
		Model:    "qwen3-embedding",
		MaxBatch: 2,
	}, "")

	_, err := p.EmbedBatch(context.Background(), []string{"a", "b", "c"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, embedding.ErrBatchTooLarge))
	assert.Contains(t, err.Error(), "got 3, max 2")
	assert.False(t, called, "oversize batch must be rejected before the upstream")
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

// TestOpenAIProvider_NameAndDimensions pins the advertised metadata the
// Provider interface exposes. Name stays "openai" so existing config /
// log expectations are unchanged by the adapter rewrite.
func TestOpenAIProvider_NameAndDimensions(t *testing.T) {
	p := embedding.NewOpenAIProvider(embedding.Config{
		URL:        "http://unused",
		Model:      "qwen3-embedding",
		Dimensions: 1024,
	}, "")

	assert.Equal(t, "openai", p.Name())
	assert.Equal(t, 1024, p.Dimensions())
	require.NoError(t, p.Close())
}

// fillVec returns an n-element vector filled with v, used as a
// deterministic stand-in for the upstream's response.
func fillVec(n int, v float32) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = v
	}
	return out
}
