package embedding_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/logan-broit/ghola/pkg/embedding"
)

// fillVec returns an n-element vector filled with v — a deterministic
// stand-in for an upstream response body.
func fillVec(n int, v float32) []float32 {
	out := make([]float32, n)
	for i := range out {
		out[i] = v
	}
	return out
}

// TestEmbed_HappyPath pins single-text success: the request body carries
// the model and a one-element input array, and the vector is returned.
func TestEmbed_HappyPath(t *testing.T) {
	var gotBody struct {
		Model string   `json:"model"`
		Input []string `json:"input"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			t.Errorf("path = %q, want /v1/embeddings", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &gotBody); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2,0.3]}]}`))
	}))
	defer srv.Close()

	c := embedding.New(embedding.Config{BaseURL: srv.URL, Model: "qwen3-embedding"})
	vec, err := c.Embed(context.Background(), "hello world")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if want := []float32{0.1, 0.2, 0.3}; !floatsEqual(vec, want) {
		t.Errorf("vec = %v, want %v", vec, want)
	}
	if gotBody.Model != "qwen3-embedding" {
		t.Errorf("model = %q, want qwen3-embedding", gotBody.Model)
	}
	if len(gotBody.Input) != 1 || gotBody.Input[0] != "hello world" {
		t.Errorf("input = %v, want [hello world]", gotBody.Input)
	}
}

// TestEmbed_RetriesOn503 pins that a 503 is retried and a subsequent
// success is returned; the request count proves the retry happened.
func TestEmbed_RetriesOn503(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			http.Error(w, "wobble", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1.0]}]}`))
	}))
	defer srv.Close()

	c := embedding.New(embedding.Config{BaseURL: srv.URL, Model: "test", Retries: 3})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	vec, err := c.Embed(ctx, "x")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if !floatsEqual(vec, []float32{1.0}) {
		t.Errorf("vec = %v, want [1]", vec)
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3 (success on third try)", calls.Load())
	}
}

// TestEmbed_ExhaustsRetries pins that a persistent 503 fails after the
// retry budget is spent, surfacing the status in the error.
func TestEmbed_ExhaustsRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "still wobbling", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := embedding.New(embedding.Config{BaseURL: srv.URL, Model: "test", Retries: 2})
	_, err := c.Embed(context.Background(), "x")
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error = %q, want it to contain 503", err.Error())
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2 (retry budget)", calls.Load())
	}
}

// TestEmbed_NoRetryOn4xx pins that a 4xx (here 400) is definitive: one
// request, no retries.
func TestEmbed_NoRetryOn4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "bad input", http.StatusBadRequest)
	}))
	defer srv.Close()

	c := embedding.New(embedding.Config{BaseURL: srv.URL, Model: "test", Retries: 5})
	if _, err := c.Embed(context.Background(), "x"); err == nil {
		t.Fatal("expected error on 400")
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (4xx is definitive)", calls.Load())
	}
}

// TestEmbed_RetriesOn429 pins that rate-limit responses are retried —
// the union client adds 429 to the retryable set (Guild only had 5xx+408).
func TestEmbed_RetriesOn429(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 2 {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[2.0]}]}`))
	}))
	defer srv.Close()

	c := embedding.New(embedding.Config{BaseURL: srv.URL, Model: "test", Retries: 3})
	vec, err := c.Embed(context.Background(), "x")
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if !floatsEqual(vec, []float32{2.0}) {
		t.Errorf("vec = %v, want [2]", vec)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2", calls.Load())
	}
}

// TestEmbed_ContextCancelledMidBackoff pins that a context cancelled
// during the backoff sleep aborts promptly with the context error,
// rather than waiting out the full backoff. The server always 503s so
// the client is forced into a retry sleep.
func TestEmbed_ContextCancelledMidBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "wobble", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	// Cancel after 30ms — first request returns ~immediately (503), then
	// the client sleeps ~100ms for backoff; cancellation must interrupt it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	c := embedding.New(embedding.Config{BaseURL: srv.URL, Model: "test", Retries: 5})
	start := time.Now()
	_, err := c.Embed(ctx, "x")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected context error")
	}
	if !strings.Contains(err.Error(), context.DeadlineExceeded.Error()) {
		t.Errorf("error = %q, want context deadline exceeded", err.Error())
	}
	// With 5 retries and full backoff the call would take >100ms; a
	// prompt cancel returns well under the first backoff window.
	if elapsed > 90*time.Millisecond {
		t.Errorf("elapsed = %v, want prompt cancel (<90ms)", elapsed)
	}
}

// TestEmbed_BearerHeaderWhenAPIKeySet pins that the Authorization header
// is present iff APIKey is set.
func TestEmbed_BearerHeaderWhenAPIKeySet(t *testing.T) {
	t.Run("with key", func(t *testing.T) {
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1.0]}]}`))
		}))
		defer srv.Close()

		c := embedding.New(embedding.Config{BaseURL: srv.URL, Model: "test", APIKey: "sk-secret"})
		if _, err := c.Embed(context.Background(), "x"); err != nil {
			t.Fatalf("Embed: %v", err)
		}
		if gotAuth != "Bearer sk-secret" {
			t.Errorf("Authorization = %q, want Bearer sk-secret", gotAuth)
		}
	})

	t.Run("without key", func(t *testing.T) {
		var gotAuth string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1.0]}]}`))
		}))
		defer srv.Close()

		c := embedding.New(embedding.Config{BaseURL: srv.URL, Model: "test"})
		if _, err := c.Embed(context.Background(), "x"); err != nil {
			t.Fatalf("Embed: %v", err)
		}
		if gotAuth != "" {
			t.Errorf("Authorization = %q, want empty (no key)", gotAuth)
		}
	})
}

// TestEmbedBatch_OrderPreservedAcrossChunks pins both the chunking at
// MaxBatch and the by-index re-sort within each chunk. MaxBatch=2 over 5
// inputs forces 3 chunks (2,2,1); each chunk's server returns vectors in
// reverse order to prove the client re-sorts by index, and the chunks
// concatenate back into global input order.
func TestEmbedBatch_OrderPreservedAcrossChunks(t *testing.T) {
	var chunkSizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		chunkSizes = append(chunkSizes, len(req.Input))

		// Echo each input's trailing digit as the vector's first
		// element, emitting entries in REVERSE order to exercise the
		// by-index re-sort. Input strings are "t0".."t4".
		data := make([]map[string]any, 0, len(req.Input))
		for i := len(req.Input) - 1; i >= 0; i-- {
			digit := float32(req.Input[i][len(req.Input[i])-1] - '0')
			data = append(data, map[string]any{
				"index":     i,
				"embedding": []float32{digit},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()

	c := embedding.New(embedding.Config{BaseURL: srv.URL, Model: "test", MaxBatch: 2})
	inputs := []string{"t0", "t1", "t2", "t3", "t4"}
	vecs, err := c.EmbedBatch(context.Background(), inputs)
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if len(vecs) != 5 {
		t.Fatalf("got %d vectors, want 5", len(vecs))
	}
	for i, v := range vecs {
		if len(v) != 1 || int(v[0]) != i {
			t.Errorf("vecs[%d] = %v, want first element %d (global order preserved)", i, v, i)
		}
	}
	if want := []int{2, 2, 1}; !intsEqual(chunkSizes, want) {
		t.Errorf("chunk sizes = %v, want %v", chunkSizes, want)
	}
}

// TestEmbedBatch_IndexMapping pins the by-index re-sort in a single
// chunk: the server returns three entries in reverse and the client must
// return them in input order.
func TestEmbedBatch_IndexMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		out := map[string]any{
			"data": []map[string]any{
				{"index": 2, "embedding": fillVec(4, 0.3)},
				{"index": 0, "embedding": fillVec(4, 0.1)},
				{"index": 1, "embedding": fillVec(4, 0.2)},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	c := embedding.New(embedding.Config{BaseURL: srv.URL, Model: "test"})
	vecs, err := c.EmbedBatch(context.Background(), []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("EmbedBatch: %v", err)
	}
	if len(vecs) != 3 {
		t.Fatalf("got %d vectors, want 3", len(vecs))
	}
	if vecs[0][0] != 0.1 || vecs[1][0] != 0.2 || vecs[2][0] != 0.3 {
		t.Errorf("re-sort wrong: [%v %v %v], want first elements 0.1 0.2 0.3",
			vecs[0][0], vecs[1][0], vecs[2][0])
	}
}

// TestEmbedBatch_EmptyInputNoRoundTrip pins that empty input returns
// (nil, nil) without contacting the server.
func TestEmbedBatch_EmptyInputNoRoundTrip(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	c := embedding.New(embedding.Config{BaseURL: srv.URL, Model: "test"})
	for _, in := range [][]string{nil, {}} {
		vecs, err := c.EmbedBatch(context.Background(), in)
		if err != nil {
			t.Fatalf("EmbedBatch(%v): %v", in, err)
		}
		if vecs != nil {
			t.Errorf("EmbedBatch(%v) = %v, want nil", in, vecs)
		}
	}
	if called {
		t.Error("empty input must not contact the server")
	}
}

// TestEmbedBatch_MissingIndexIsError pins all-or-nothing: a response
// missing an entry for some input index is an error, no partial result.
func TestEmbedBatch_MissingIndexIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only return index 0 for a 2-input request.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1.0]}]}`))
	}))
	defer srv.Close()

	c := embedding.New(embedding.Config{BaseURL: srv.URL, Model: "test"})
	vecs, err := c.EmbedBatch(context.Background(), []string{"a", "b"})
	if err == nil {
		t.Fatal("expected error for missing index")
	}
	if vecs != nil {
		t.Errorf("vecs = %v, want nil on error", vecs)
	}
}

// TestDimensionsAndName pins the advertised metadata accessors.
func TestDimensionsAndName(t *testing.T) {
	c := embedding.New(embedding.Config{BaseURL: "http://x", Model: "m", Dimensions: 1024})
	if c.Dimensions() != 1024 {
		t.Errorf("Dimensions() = %d, want 1024", c.Dimensions())
	}
	if c.Name() != "openai" {
		t.Errorf("Name() = %q, want openai", c.Name())
	}
}

func floatsEqual(a, b []float32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func intsEqual(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
