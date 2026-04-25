//go:build e2e

// Package e2e contains end-to-end tests that drive a real Compose stack.
//
// PR1.9 — predictive-replay vertical-slice smoke. This is the one e2e
// that earns its keep for PR1's plumbing: it brings up the full stack
// (postgres + melange-stub + mentat + chapterhouse + ghola) and walks
// every wire the predictive-replay v1a slice introduced — through
// ghola's HTTP API rather than poking chapterhouse directly.
//
//  1. ghola POST /v1/session_start
//  2. ghola POST /v1/record (xN) — embeddings sourced from melange-stub
//  3. ghola POST /v1/session_end — fires Consolidate which must
//     propagate ended_at to chapterhouse's episodic.sessions row
//  4. wait for chapterhouse's reconciler to pool the session into
//     l1_embedding (eligibility predicate `ended_at IS NOT NULL AND
//     l1_embedding IS NULL`)
//  5. ghola POST /v1/recall — confirm the wire works (semantic tier
//     hits not yet asserted; clustering lands in PR4)
//  6. confirm mentat reports cold-start
//
// The earlier shape of this test hand-set EndedAt in the chapterhouse
// /v1/episodic/ingest payload. That bypassed ghola entirely and
// silently masked a real bug: core.Consolidate rebuilt Session{} from
// the first pending event's fields, never sourced ended_at from
// sietch, and as a result the chapterhouse reconciler skipped every
// real ghola session forever. Driving the flow through ghola is the
// only way to catch this class of plumbing regression.
//
// Run via `make smoke-predictive` from the repo root, which brings up
// the isolated `predictive-smoke` Compose project on alternate host
// ports so it does not collide with any long-running dev stack.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// envOr returns the value of name, falling back to def when unset/empty.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func envOrInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		n, err := strconv.Atoi(v)
		if err == nil {
			return n
		}
	}
	return def
}

// testConfig collects the env-tunable knobs in one place. Defaults match
// the smoke-predictive Makefile target's port choices (offset by +10000
// from the dev compose defaults to leave room for a parallel dev stack).
type testConfig struct {
	gholaURL        string
	chapterhouseURL string
	mentatURL       string
	pgDSN           string
	userID          uuid.UUID
	embeddingDim    int
}

func loadConfig(t *testing.T) testConfig {
	t.Helper()
	uid, err := uuid.Parse(envOr("DEFAULT_USER_UUID",
		"00000000-0000-0000-0000-000000000001"))
	if err != nil {
		t.Fatalf("DEFAULT_USER_UUID parse: %v", err)
	}
	pgDSN := envOr("POSTGRES_DSN", fmt.Sprintf(
		"postgresql://%s:%s@%s:%s/%s?sslmode=disable",
		envOr("POSTGRES_USER", "memory_api"),
		envOr("POSTGRES_PASSWORD", "dev"),
		envOr("POSTGRES_HOST", "localhost"),
		envOr("POSTGRES_PORT", "15432"),
		envOr("POSTGRES_DB", "memories"),
	))
	return testConfig{
		gholaURL:        envOr("GHOLA_URL", "http://localhost:17421"),
		chapterhouseURL: envOr("CHAPTERHOUSE_URL", "http://localhost:18080"),
		mentatURL:       envOr("MENTAT_URL", "http://localhost:18084"),
		pgDSN:           pgDSN,
		userID:          uid,
		embeddingDim:    envOrInt("EMBEDDING_DIM", 1024),
	}
}

// httpJSON sends a request with optional JSON body + decodes the
// response into out. Bearer is set unconditionally; chapterhouse's
// default auth provider only checks presence, ghola accepts anything
// on loopback.
func httpJSON(ctx context.Context, t *testing.T, method, url string, body any, out any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal %s %s: %v", method, url, err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, url, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer e2e-smoke")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do %s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("%s %s: status %d body=%s", method, url, resp.StatusCode, string(respBody))
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			t.Fatalf("decode %s %s: %v body=%s", method, url, err, string(respBody))
		}
	}
}

// TestPredictiveReplayVerticalSlice exercises the full v1a wire path
// driven through ghola. Build tag `e2e` keeps it out of the default
// `go test ./...` run; `make smoke-predictive` sets the tag.
func TestPredictiveReplayVerticalSlice(t *testing.T) {
	cfg := loadConfig(t)

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, cfg.pgDSN)
	if err != nil {
		t.Fatalf("pgxpool.New: %v (dsn=%s)", err, cfg.pgDSN)
	}
	defer pool.Close()

	// Sanity: postgres reachable. Fails fast with a clear message if the
	// caller forgot to bring the smoke stack up.
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("postgres ping: %v (is the smoke stack up?)", err)
	}

	userID := cfg.userID.String()

	// 1. Start a session via ghola.
	type sessionStartResp struct {
		Session struct {
			ID     string `json:"id"`
			UserID string `json:"user_id"`
		} `json:"session"`
	}
	var ssr sessionStartResp
	httpJSON(ctx, t, http.MethodPost, cfg.gholaURL+"/v1/session_start",
		map[string]any{"user_id": userID}, &ssr)
	if ssr.Session.ID == "" {
		t.Fatal("session_start: empty session id")
	}
	t.Logf("session id = %s", ssr.Session.ID)

	// 2. Record a handful of events. Ghola will source embeddings from
	//    melange-stub and persist into sietch.
	corpus := []struct {
		Type string
		Text string
	}{
		{"user", "kubernetes deployment is failing in mediaserver namespace"},
		{"assistant", "checked sonarr logs and found db migration error"},
		{"user", "rolled back to previous version, kept config volume"},
		{"assistant", "need to investigate cnpg pg_recall extension wiring tomorrow"},
		{"user", "also re-check the chapterhouse reconciler tick interval"},
	}
	for i, ev := range corpus {
		httpJSON(ctx, t, http.MethodPost, cfg.gholaURL+"/v1/record",
			map[string]any{
				"session_id": ssr.Session.ID,
				"user_id":    userID,
				"event": map[string]any{
					"type":      ev.Type,
					"text":      ev.Text,
					"raw_event": map[string]any{"i": i},
				},
			}, nil)
	}

	// 3. End the session via ghola. This is the wire that surfaced the
	//    real PR1 bug: SessionEnd -> Consolidate -> /v1/episodic/ingest
	//    used to drop ended_at, leaving the chapterhouse reconciler's
	//    eligibility predicate (`ended_at IS NOT NULL AND l1_embedding
	//    IS NULL`) unsatisfiable for every real session.
	httpJSON(ctx, t, http.MethodPost, cfg.gholaURL+"/v1/session_end",
		map[string]any{"session_id": ssr.Session.ID}, nil)

	// 4a. Assert ended_at landed on the chapterhouse-side session row.
	//     This is the load-bearing assertion: the previous version of
	//     this test never checked it because it hand-set ended_at in
	//     the ingest payload. Now it has to come from Consolidate.
	var endedAtSet bool
	if err := pool.QueryRow(ctx,
		`SELECT ended_at IS NOT NULL FROM episodic.sessions WHERE id = $1`,
		ssr.Session.ID,
	).Scan(&endedAtSet); err != nil {
		t.Fatalf("ended_at lookup: %v", err)
	}
	if !endedAtSet {
		t.Fatalf("episodic.sessions.ended_at IS NULL for session %s — "+
			"core.Consolidate did not propagate ended_at (the PR1 bug)", ssr.Session.ID)
	}

	// 4b. Poll for the reconciler to populate l1_embedding. The
	//     reconciler ticks every 30s — give it 90s of slack so a tick
	//     that just fired can complete and the next tick (worst case)
	//     can run.
	deadline := time.Now().Add(90 * time.Second)
	var l1Set bool
	for time.Now().Before(deadline) {
		var present bool
		err := pool.QueryRow(ctx,
			`SELECT l1_embedding IS NOT NULL FROM episodic.sessions WHERE id = $1`,
			ssr.Session.ID,
		).Scan(&present)
		if err == nil && present {
			l1Set = true
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("ctx done while waiting for l1_embedding: %v", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
	if !l1Set {
		t.Fatalf("l1_embedding was not set within 90s — check chapterhouse logs " +
			"(reconciler tick) and mentat /v1/pool")
	}

	// 5. Hit ghola's /v1/recall and assert episodic tier returns hits.
	//    Semantic tier may be empty (Stage C clustering hasn't run);
	//    asserting the wire moves bytes is enough for the smoke.
	type recallResp struct {
		Hits []struct {
			Tier      string  `json:"tier"`
			ID        string  `json:"id"`
			Score     float64 `json:"score"`
			Content   string  `json:"content"`
			SessionID *string `json:"session_id,omitempty"`
		} `json:"hits"`
		TierCounts map[string]int `json:"tier_counts"`
	}
	var rr recallResp
	httpJSON(ctx, t, http.MethodPost, cfg.gholaURL+"/v1/recall",
		map[string]any{
			"user_id":         userID,
			"workspace":       userID,
			"query_text":      "kubernetes sonarr migration",
			"limit":           10,
			"include_episode": true,
			"include_semant":  true,
		}, &rr)
	if rr.TierCounts["episodic"] == 0 {
		t.Fatalf("recall: expected at least one episodic hit, got tier_counts=%v",
			rr.TierCounts)
	}

	// 6. Confirm mentat is in cold-start. No training has run; pooling
	//    works regardless. This is a cheap last-mile assertion that the
	//    /v1/health wire is intact.
	type healthResp struct {
		Status         string  `json:"status"`
		WeightsVersion *string `json:"weights_version"`
		ColdStart      bool    `json:"cold_start"`
		EmbeddingDim   int     `json:"embedding_dim"`
	}
	var hresp healthResp
	httpJSON(ctx, t, http.MethodGet, cfg.mentatURL+"/v1/health", nil, &hresp)
	if hresp.Status != "ok" {
		t.Fatalf("mentat health: status=%q", hresp.Status)
	}
	if !hresp.ColdStart {
		t.Fatalf("mentat health: expected cold_start=true (no training yet), got false")
	}
	if hresp.EmbeddingDim != cfg.embeddingDim {
		t.Fatalf("mentat health: embedding_dim=%d, want %d",
			hresp.EmbeddingDim, cfg.embeddingDim)
	}
}
