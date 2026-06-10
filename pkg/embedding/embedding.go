// Package embedding is the canonical OpenAI-compatible embeddings
// client shared by the ghola service and ch-server (which imports it
// via a replace directive — keep this package stdlib-only).
//
// It is the union of the two clients it replaced: the retry/backoff
// behavior from ghola's Guild client (exponential backoff, context-
// aware sleep, retry on 429/5xx + transport errors) applied to both
// single and batch calls, plus the batch chunking + by-index order
// preservation from ch-server's OpenAIProvider.
//
// Validation policy: this client is deliberately permissive. It does
// NOT reject empty input or oversize batches — those are caller
// contracts that live in the ch-server adapter (which owns the
// ErrEmptyInput / ErrBatchTooLarge sentinels). The MaxBatch field here
// governs internal chunking of large EmbedBatch calls, not a hard cap.
package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
)

// Default values applied by New when a Config field is zero. These
// match the historical hard-coded values of the clients this package
// replaced: Timeout 30s, Retries 3 (from Guild's retry budget),
// MaxBatch 64 (chunk size).
const (
	defaultTimeout  = 30 * time.Second
	defaultRetries  = 3
	defaultMaxBatch = 64
)

// Config configures a Client. Zero-value fields fall back to defaults
// (Timeout 30s, Retries 3, MaxBatch 64); Dimensions 0 means unknown.
type Config struct {
	BaseURL    string        // e.g. http://guild:8082
	Model      string        //
	APIKey     string        // optional; sent as Bearer when set
	Timeout    time.Duration // per-request; default 30s
	Retries    int           // on 429/5xx/transport errors; default 3
	MaxBatch   int           // EmbedBatch chunk size; default 64
	Dimensions int           // advertised by Dimensions(); 0 = unknown
}

// Client is an OpenAI-compatible embeddings client.
type Client struct {
	baseURL    string // trailing slash stripped
	model      string
	apiKey     string
	retries    int
	maxBatch   int
	dimensions int
	http       *http.Client
}

// New builds a Client from cfg, applying defaults for zero-value
// Timeout/Retries/MaxBatch. baseURL points at the service root (e.g.
// "http://localhost:8082"); the client posts to baseURL + /v1/embeddings.
func New(cfg Config) *Client {
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	if cfg.Retries <= 0 {
		cfg.Retries = defaultRetries
	}
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = defaultMaxBatch
	}
	return &Client{
		baseURL:    strings.TrimRight(cfg.BaseURL, "/"),
		model:      cfg.Model,
		apiKey:     cfg.APIKey,
		retries:    cfg.Retries,
		maxBatch:   cfg.MaxBatch,
		dimensions: cfg.Dimensions,
		http:       &http.Client{Timeout: cfg.Timeout},
	}
}

// Embed returns the embedding vector for text. Errors are returned
// after the retry budget is exhausted.
func (c *Client) Embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := c.embedOnce(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("embed: server returned no embedding")
	}
	return vecs[0], nil
}

// EmbedBatch returns embeddings for texts in input order. Inputs larger
// than MaxBatch are split into chunks; each chunk is a retried request
// and results are concatenated back in order. An empty input returns
// (nil, nil) without any HTTP round-trip.
func (c *Client) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += c.maxBatch {
		end := start + c.maxBatch
		if end > len(texts) {
			end = len(texts)
		}
		chunk, err := c.embedOnce(ctx, texts[start:end])
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
	}
	return out, nil
}

// embedOnce posts a single embeddings request for inputs (already
// chunk-sized by the caller) and applies the retry/backoff budget. It
// returns vectors re-sorted into inputs order regardless of the
// response's reported index ordering.
func (c *Client) embedOnce(ctx context.Context, inputs []string) ([][]float32, error) {
	body, err := json.Marshal(openaiEmbedRequest{Model: c.model, Input: inputs})
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < c.retries; attempt++ {
		if attempt > 0 {
			if err := sleepWithContext(ctx, backoff(attempt)); err != nil {
				return nil, err
			}
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			c.baseURL+"/v1/embeddings", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("build: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.apiKey)
		}

		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("embed: %w", err)
			continue // transport error — retry
		}
		vecs, derr := decodeBatch(resp, len(inputs))
		if derr == nil {
			resp.Body.Close()
			return vecs, nil
		}
		// Error outcome: decodeBatch read only a capped slice of the body
		// (4xx/5xx) or stopped at the first decode error. Drain to EOF
		// before Close so the keep-alive connection is reusable on the
		// next retry — connection reuse requires both EOF and Close.
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		lastErr = derr
		if !isRetryable(resp.StatusCode) {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

// Dimensions returns the advertised embedding dimension (0 = unknown).
func (c *Client) Dimensions() int { return c.dimensions }

// Name returns the provider name. Preserved as "openai" for parity with
// the ch-server provider this client backs.
func (c *Client) Name() string { return "openai" }

// isRetryable flags the HTTP status codes worth a retry budget: 429
// (rate-limited), 408 (request timeout), and 5xx (server wobble).
// Everything else is treated as a definitive client error.
func isRetryable(status int) bool {
	return status == http.StatusTooManyRequests ||
		status == http.StatusRequestTimeout ||
		(status >= 500 && status < 600)
}

// backoff returns the sleep duration before attempt n (1-indexed).
// Exponential (100ms, 500ms, 2.5s, ...) + small jitter so coordinated
// clients don't thundering-herd a recovering server.
func backoff(n int) time.Duration {
	base := time.Duration(100) * time.Millisecond
	for i := 1; i < n; i++ {
		base *= 5
	}
	jitter := time.Duration(rand.Int64N(int64(base / 4)))
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
// Wire shapes — OpenAI-compatible
// ---------------------------------------------------------------------

type openaiEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type openaiEmbedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error,omitempty"`
}

// decodeBatch reads an embeddings response and returns the vectors
// re-sorted into the request's input order (the server may return
// out-of-order entries). want is the number of inputs sent, used to
// size the output and verify completeness.
func decodeBatch(resp *http.Response, want int) ([][]float32, error) {
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("embed: %d %s: %s",
			resp.StatusCode, http.StatusText(resp.StatusCode),
			strings.TrimSpace(string(payload)))
	}
	var r openaiEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if r.Error != nil {
		return nil, fmt.Errorf("embed: %s", r.Error.Message)
	}

	out := make([][]float32, want)
	for _, item := range r.Data {
		if item.Index >= 0 && item.Index < want {
			out[item.Index] = item.Embedding
		}
	}
	for i, v := range out {
		if len(v) == 0 {
			return nil, fmt.Errorf("embed: missing embedding at index %d", i)
		}
	}
	return out, nil
}
