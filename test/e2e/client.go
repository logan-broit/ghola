//go:build e2e

// Package e2e drives a running ghola docker-compose stack through the
// same HTTP surface agents use. Skipped unless GHOLA_E2E_URL is set.
//
//	GHOLA_E2E_URL=http://localhost:7421 go test -tags e2e ./test/e2e/...
package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// client is a tiny JSON-over-HTTP wrapper specific to the ghola
// local-service API. Each call shares the same http.Client so the
// connection pool stays warm across a test run.
type client struct {
	base string
	http *http.Client
	t    *testing.T
}

func newClient(t *testing.T) *client {
	t.Helper()
	base := os.Getenv("GHOLA_E2E_URL")
	if base == "" {
		t.Skip("GHOLA_E2E_URL not set; start `./scripts/dev-up.sh --with-ghola` and re-run")
	}
	return &client{
		base: base,
		http: &http.Client{Timeout: 10 * time.Second},
		t:    t,
	}
}

func (c *client) post(path string, body any, out any) {
	c.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		c.t.Fatalf("marshal %s: %v", path, err)
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		c.t.Fatalf("build %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatalf("%s: %v", path, err)
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.t.Fatalf("%s: %d %s", path, resp.StatusCode, string(payload))
	}
	if out == nil {
		return
	}
	if err := json.Unmarshal(payload, out); err != nil {
		c.t.Fatalf("decode %s: %v body=%s", path, err, string(payload))
	}
}

// waitHealthy polls /health until it returns 200 or the budget is
// exhausted. Buys test robustness when the stack is still warming up.
func (c *client) waitHealthy(budget time.Duration) {
	c.t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		resp, err := c.http.Get(c.base + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	c.t.Fatalf("%s/health never returned 200 in %s", c.base, budget)
}

// ---------------------------------------------------------------------
// Typed request / response shapes — mirror internal/core/types.go.
// We duplicate them instead of importing so the e2e suite only needs
// to match the wire contract, not Go internals.
// ---------------------------------------------------------------------

type sessionDTO struct {
	ID        string `json:"id"`
	UserID    string `json:"user_id"`
	StartedAt string `json:"started_at"`
}

type sessionStartResp struct {
	Session sessionDTO `json:"session"`
}

type eventDTO struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	UserID    string `json:"user_id"`
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	RawEvent  any    `json:"raw_event"`
}

type recordReq struct {
	SessionID string   `json:"session_id"`
	UserID    string   `json:"user_id"`
	Event     eventDTO `json:"event"`
}

type recordResp struct {
	Event eventDTO `json:"event"`
}

type recallReq struct {
	SessionID      string `json:"session_id,omitempty"`
	UserID         string `json:"user_id"`
	QueryText      string `json:"query_text"`
	Limit          int    `json:"limit,omitempty"`
	IncludeSietch  bool   `json:"include_sietch"`
	IncludeEpisode bool   `json:"include_episode"`
	IncludeSemant  bool   `json:"include_semant"`
}

type recallHit struct {
	Tier      string  `json:"tier"`
	ID        string  `json:"id"`
	Score     float64 `json:"score"`
	Content   string  `json:"content"`
	SessionID string  `json:"session_id,omitempty"`
}

type recallResp struct {
	Hits       []recallHit    `json:"hits"`
	TierCounts map[string]int `json:"tier_counts"`
}

// helper: recordText is the common "record a single user turn" path.
func (c *client) recordText(sessionID, userID, text string) eventDTO {
	c.t.Helper()
	var out recordResp
	c.post("/v1/record", recordReq{
		SessionID: sessionID,
		UserID:    userID,
		Event: eventDTO{
			SessionID: sessionID,
			UserID:    userID,
			Type:      "user",
			Text:      text,
			RawEvent:  map[string]string{"kind": "user"},
		},
	}, &out)
	if out.Event.ID == "" {
		c.t.Fatalf("record returned empty event id: %+v", out)
	}
	return out.Event
}

func (c *client) startSession(userID, agentKind string) sessionDTO {
	c.t.Helper()
	var out sessionStartResp
	c.post("/v1/session_start", map[string]any{
		"user_id":    userID,
		"agent_kind": agentKind,
	}, &out)
	return out.Session
}

func (c *client) consolidate(sessionID string) {
	c.t.Helper()
	c.post("/v1/consolidate", map[string]any{"session_id": sessionID}, nil)
}

// recall runs a query with a budget — Pipeline A flushes are
// asynchronous, so episodic hits can lag momentarily even after
// consolidate returns. The helper retries until an expected event id
// appears or the budget expires.
func (c *client) recallAwait(userID, query string, wantID string, budget time.Duration) recallResp {
	c.t.Helper()
	deadline := time.Now().Add(budget)
	var last recallResp
	for time.Now().Before(deadline) {
		var out recallResp
		c.post("/v1/recall", recallReq{
			UserID:         userID,
			QueryText:      query,
			Limit:          10,
			IncludeSietch:  true,
			IncludeEpisode: true,
			IncludeSemant:  false,
		}, &out)
		last = out
		if wantID == "" || containsHit(out.Hits, wantID) {
			return out
		}
		time.Sleep(300 * time.Millisecond)
	}
	c.t.Fatalf("recall for %q never surfaced event %s in %s; last hits=%s",
		query, wantID, budget, formatHits(last.Hits))
	return last
}

func containsHit(hits []recallHit, id string) bool {
	for _, h := range hits {
		if h.ID == id {
			return true
		}
	}
	return false
}

func formatHits(hits []recallHit) string {
	if len(hits) == 0 {
		return "[]"
	}
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, fmt.Sprintf("%s/%.4f/%s", h.Tier, h.Score, h.ID))
	}
	return "[" + fmt.Sprint(out) + "]"
}
