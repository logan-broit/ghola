package embedding_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/logan-broit/ghola/internal/embedding"
)

func TestMelange_HappyPath(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/embeddings", r.URL.Path)
		b, _ := io.ReadAll(r.Body)
		require.NoError(t, json.Unmarshal(b, &gotBody))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2,0.3]}]}`))
	}))
	defer srv.Close()

	m := embedding.New(srv.URL, "qwen3-embedding").WithHTTPClient(srv.Client())
	vec, err := m.Embed(context.Background(), "hello world")
	require.NoError(t, err)
	assert.Equal(t, []float32{0.1, 0.2, 0.3}, vec)
	assert.Equal(t, "qwen3-embedding", gotBody["model"])
	assert.Equal(t, "hello world", gotBody["input"])
}

func TestMelange_RetriesOn5xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			http.Error(w, "wobble", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[1.0]}]}`))
	}))
	defer srv.Close()

	m := embedding.New(srv.URL, "test").
		WithHTTPClient(srv.Client()).
		WithRetries(3)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	vec, err := m.Embed(ctx, "x")
	require.NoError(t, err)
	assert.Equal(t, []float32{1.0}, vec)
	assert.Equal(t, int32(3), calls.Load(), "should succeed on third try")
}

func TestMelange_GivesUpAfterRetryBudget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "still wobbling", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	m := embedding.New(srv.URL, "test").
		WithHTTPClient(srv.Client()).
		WithRetries(2)
	_, err := m.Embed(context.Background(), "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "503")
}

func TestMelange_NoRetryOn4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "bad input", http.StatusBadRequest)
	}))
	defer srv.Close()

	m := embedding.New(srv.URL, "test").
		WithHTTPClient(srv.Client()).
		WithRetries(5)
	_, err := m.Embed(context.Background(), "x")
	require.Error(t, err)
	assert.Equal(t, int32(1), calls.Load(), "4xx is definitive, no retry")
}

func TestMelange_EmptyResponseIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	m := embedding.New(srv.URL, "test").WithHTTPClient(srv.Client())
	_, err := m.Embed(context.Background(), "x")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no embedding")
}
