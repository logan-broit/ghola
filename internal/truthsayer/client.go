// Package truthsayer is the ghola HTTP client for the truthsayer
// cross-encoder reranker service. The service exposes:
//
//   GET  /v1/health    -> { status, model, device }
//   POST /v1/rerank    -> { scores: [{id, score}] }, ordered desc
//
// The score scale is whatever the underlying CrossEncoder produces
// (sigmoid-ish for bge-reranker; raw logit for some others). Callers
// normalize before fusion.
package truthsayer

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/logan-broit/ghola/internal/httpx"
)

// Client talks to a truthsayer service over HTTP/JSON.
type Client struct {
	baseURL string
	http    *http.Client
}

// Candidate is one document presented to the reranker.
type Candidate struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

// Score is the reranker's verdict on one candidate. Returned in the
// order the server emits, which is descending by Score.
type Score struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
}

// New builds a Client. baseURL points at the service root
// (e.g. "http://localhost:8085" or "http://truthsayer:8085").
func New(baseURL string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// WithHTTPClient swaps the underlying client — used by tests.
func (c *Client) WithHTTPClient(h *http.Client) *Client {
	c.http = h
	return c
}

// Health probes GET /v1/health. Returns nil if the service responds 2xx.
// Callers (main.go at startup, recall fallback in PR-C) decide what to
// do on error — typically log a warning and disable rerank.
func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("truthsayer health: %s", resp.Status)
	}
	return nil
}

// Rerank scores candidates against query and returns them sorted
// best-first (server-guaranteed). If topK > 0 the server truncates;
// pass 0 to get all candidates back.
//
// Wire shape + error model is shared with the chapterhouse client via
// internal/httpx — same encode/decode/status-check pipeline; truthsayer
// is auth-free so the bearer arg is empty.
func (c *Client) Rerank(ctx context.Context, query string, cand []Candidate, topK int) ([]Score, error) {
	payload := struct {
		Query      string      `json:"query"`
		Candidates []Candidate `json:"candidates"`
		TopK       *int        `json:"top_k,omitempty"`
	}{Query: query, Candidates: cand}
	if topK > 0 {
		payload.TopK = &topK
	}

	var r struct {
		Scores []Score `json:"scores"`
	}
	if err := httpx.PostJSON(ctx, c.http, c.baseURL+"/v1/rerank", "", "rerank", payload, &r); err != nil {
		return nil, err
	}
	return r.Scores, nil
}
