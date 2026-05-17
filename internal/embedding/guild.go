// Package embedding implements core.Embedder against the Guild
// service — an OpenAI-compatible embeddings endpoint
// (POST /v1/embeddings). The default deployment is vLLM or TEI
// hosting Qwen3-Embedding, but any OpenAI-shaped server works.
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"time"
)

// Guild is the embedding client. Retries 5xx responses with
// exponential backoff (100ms, 500ms, 2s) up to three attempts so a
// flaky embedding server doesn't kill an agent's recall path.
type Guild struct {
	baseURL string // trailing slash stripped
	model   string
	http    *http.Client
	retries int
}

// New builds a Guild client. `baseURL` points at the service root
// (e.g. "http://localhost:8082"); model is the name the server uses
// (e.g. "qwen3-embedding").
func New(baseURL, model string) *Guild {
	return &Guild{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		http:    &http.Client{Timeout: 15 * time.Second},
		retries: 3,
	}
}

// WithHTTPClient swaps the underlying client — used by tests.
func (m *Guild) WithHTTPClient(h *http.Client) *Guild {
	m.http = h
	return m
}

// WithRetries overrides the default retry budget (3).
func (m *Guild) WithRetries(n int) *Guild {
	m.retries = n
	return m
}

// Embed returns the embedding vector for `text`. Errors are returned
// after the retry budget is exhausted.
func (m *Guild) Embed(ctx context.Context, text string) ([]float32, error) {
	body, err := json.Marshal(map[string]any{"model": m.model, "input": text})
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < m.retries; attempt++ {
		if attempt > 0 {
			if err := sleepWithContext(ctx, backoff(attempt)); err != nil {
				return nil, err
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			m.baseURL+"/v1/embeddings", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("build: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := m.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("embed: %w", err)
			continue // transport error — retry
		}
		vec, err := decodeEmbedding(resp)
		resp.Body.Close()
		if err == nil {
			return vec, nil
		}
		lastErr = err
		if !isRetryable(resp.StatusCode) {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

// isRetryable flags the HTTP status codes worth a retry budget:
// 5xx (server wobble) and 408 (request timeout). Everything else is
// treated as a definitive client error.
func isRetryable(status int) bool {
	return status == http.StatusRequestTimeout || (status >= 500 && status < 600)
}

// backoff returns the sleep duration before attempt `n` (1-indexed).
// Exponential + small jitter so coordinated clients don't thundering-
// herd a recovering server.
func backoff(n int) time.Duration {
	base := time.Duration(100) * time.Millisecond
	for i := 1; i < n; i++ {
		base *= 5
	}
	jitter := time.Duration(rand.Int63n(int64(base / 4)))
	return base + jitter
}

func sleepWithContext(ctx context.Context, d time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// ---------------------------------------------------------------------
// Response decode — OpenAI-compatible wire shape
// ---------------------------------------------------------------------

type embeddingsResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

func decodeEmbedding(resp *http.Response) ([]float32, error) {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("embed: %d %s: %s",
			resp.StatusCode, http.StatusText(resp.StatusCode),
			strings.TrimSpace(string(payload)))
	}
	var r embeddingsResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if r.Error != nil {
		return nil, fmt.Errorf("embed: %s", r.Error.Message)
	}
	if len(r.Data) == 0 || len(r.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embed: server returned no embedding")
	}
	return r.Data[0].Embedding, nil
}
