// Package chapterhouse is the ghola local service's HTTP client for
// chapterhouse's /v1/{episodic,semantic} surface (see
// docs/api/v1-chapterhouse.yaml). Implements core.ChapterhouseClient.
package chapterhouse

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/logan-broit/ghola/internal/core"
)

// Client POSTs JSON to chapterhouse's /v1/* endpoints with a Bearer
// API key. All methods return wire errors unwrapped; callers layer
// their own context.
type Client struct {
	baseURL string // trailing slash stripped
	apiKey  string
	http    *http.Client
}

// New builds a Client. `baseURL` is chapterhouse's root URL
// (e.g. "http://localhost:8080" or
// "https://chapterhouse.thesgc.internal"). apiKey is the per-user
// bearer token the deployment issued.
func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		http: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// WithHTTPClient lets tests (and custom callers) replace the
// underlying client — e.g. httptest's Client().
func (c *Client) WithHTTPClient(h *http.Client) *Client {
	c.http = h
	return c
}

func (c *Client) do(ctx context.Context, path string, body, out any) error {
	buf := new(bytes.Buffer)
	if body != nil {
		if err := json.NewEncoder(buf).Encode(body); err != nil {
			return fmt.Errorf("encode %s: %w", path, err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, buf)
	if err != nil {
		return fmt.Errorf("build %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s: %d %s: %s", path, resp.StatusCode,
			http.StatusText(resp.StatusCode), strings.TrimSpace(string(payload)))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// ---------------------------------------------------------------------
// Request / response wire types
// ---------------------------------------------------------------------

type ingestReq struct {
	Session core.Session `json:"session"`
	Events  []core.Event `json:"events"`
}

type ingestResp struct {
	SessionID string `json:"session_id"`
	Inserted  int    `json:"inserted"`
	Updated   int    `json:"updated"`
}

type episodicQueryReq struct {
	UserID         string    `json:"user_id"`
	QueryText      string    `json:"query_text"`
	QueryEmbedding []float32 `json:"query_embedding"`
	Limit          int       `json:"limit"`
	IncludeShared  bool      `json:"include_shared"`
}

type hitsResp struct {
	Hits []hitRow `json:"hits"`
}

// hitRow is a superset matching both episodic and semantic responses:
// - episodic hits flatten the Event fields + a score{} object + tier
// - semantic hits are flat fields + tier
// Optional pointer fields let the shape absorb both.
type hitRow struct {
	ID         string  `json:"id"`
	SessionID  *string `json:"session_id,omitempty"`
	Text       *string `json:"text,omitempty"`
	Content    string  `json:"content"`
	Concept    *string `json:"concept,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"`
	Tier       string  `json:"tier"`

	// Episodic only
	Score *struct {
		Semantic float64 `json:"semantic"`
		FTS      float64 `json:"fts"`
		Merged   float64 `json:"merged"`
	} `json:"score,omitempty"`

	// Semantic only — flat score field.
	SemanticScore *float64 `json:"score_semantic,omitempty"`
	MergedScore   *float64 `json:"merged,omitempty"`

	// Whichever field the server used for the ranking number:
	ScoreFlat *float64 `json:"score_flat,omitempty"`
}

type shareReq struct {
	OwnerUserID string  `json:"owner_user_id"`
	Target      string  `json:"target"`
	TargetID    *string `json:"target_id,omitempty"`
	ScopeType   string  `json:"scope_type"`
	ScopeID     string  `json:"scope_id"`
}

type shareResp struct {
	ID string `json:"id"`
}

type forgetReq struct {
	UserID   string   `json:"user_id"`
	EventIDs []string `json:"event_ids"`
}

type forgetResp struct {
	Forgotten int `json:"forgotten"`
}

type semanticQueryReq struct {
	WorkspaceID    string    `json:"workspace_id"`
	QueryText      string    `json:"query_text"`
	QueryEmbedding []float32 `json:"query_embedding"`
	Limit          int       `json:"limit"`
}

type semanticHitsResp struct {
	Hits []semanticHitRow `json:"hits"`
}

type semanticHitRow struct {
	MnemeID      string  `json:"mneme_id"`
	Score        float64 `json:"score"`
	ContentMatch float64 `json:"content_match"`
	Activation   float64 `json:"activation"`
	HebbianBoost float64 `json:"hebbian_boost"`
	Confidence   float64 `json:"confidence"`
	Concept      string  `json:"concept"`
	Content      string  `json:"content"`
	Tier         string  `json:"tier"`
}

type feedbackReq struct {
	MnemeID  string  `json:"mneme_id"`
	Evidence float64 `json:"evidence"`
}

type feedbackResp struct {
	MnemeID    string  `json:"mneme_id"`
	Confidence float64 `json:"confidence"`
}

// ---------------------------------------------------------------------
// core.ChapterhouseClient implementation
// ---------------------------------------------------------------------

func (c *Client) IngestEpisodic(ctx context.Context, s core.Session, events []core.Event) (int, int, error) {
	var r ingestResp
	if err := c.do(ctx, "/v1/episodic/ingest", ingestReq{Session: s, Events: events}, &r); err != nil {
		return 0, 0, err
	}
	return r.Inserted, r.Updated, nil
}

func (c *Client) QueryEpisodic(ctx context.Context, q core.EpisodicQuery) ([]core.RecallHit, error) {
	var r hitsResp
	body := episodicQueryReq{
		UserID:         q.UserID,
		QueryText:      q.QueryText,
		QueryEmbedding: q.QueryEmbedding,
		Limit:          q.Limit,
		IncludeShared:  q.IncludeShared,
	}
	if err := c.do(ctx, "/v1/episodic/query", body, &r); err != nil {
		return nil, err
	}
	out := make([]core.RecallHit, 0, len(r.Hits))
	for _, h := range r.Hits {
		hit := core.RecallHit{
			Tier:      h.Tier,
			ID:        h.ID,
			SessionID: h.SessionID,
		}
		if h.Text != nil {
			hit.Content = *h.Text
		} else if h.Content != "" {
			hit.Content = h.Content
		}
		if h.Score != nil {
			hit.Score = h.Score.Merged
		}
		out = append(out, hit)
	}
	return out, nil
}

func (c *Client) ShareEpisodic(ctx context.Context, in core.ShareInput) (string, error) {
	var r shareResp
	body := shareReq{
		OwnerUserID: in.UserID,
		Target:      in.Target,
		TargetID:    in.TargetID,
		ScopeType:   in.ScopeType,
		ScopeID:     in.ScopeID,
	}
	if err := c.do(ctx, "/v1/episodic/share", body, &r); err != nil {
		return "", err
	}
	return r.ID, nil
}

func (c *Client) ForgetEpisodic(ctx context.Context, userID string, eventIDs []string) (int, error) {
	var r forgetResp
	body := forgetReq{UserID: userID, EventIDs: eventIDs}
	if err := c.do(ctx, "/v1/episodic/forget", body, &r); err != nil {
		return 0, err
	}
	return r.Forgotten, nil
}

func (c *Client) QuerySemantic(ctx context.Context, q core.SemanticQuery) ([]core.RecallHit, error) {
	var r semanticHitsResp
	body := semanticQueryReq{
		WorkspaceID:    q.Workspace,
		QueryText:      q.QueryText,
		QueryEmbedding: q.QueryEmbedding,
		Limit:          q.Limit,
	}
	if err := c.do(ctx, "/v1/semantic/query", body, &r); err != nil {
		return nil, err
	}
	out := make([]core.RecallHit, 0, len(r.Hits))
	for _, h := range r.Hits {
		concept := h.Concept
		conf := h.Confidence
		out = append(out, core.RecallHit{
			Tier:       h.Tier,
			ID:         h.MnemeID,
			Score:      h.Score,
			Content:    h.Content,
			Concept:    &concept,
			Confidence: &conf,
		})
	}
	return out, nil
}

func (c *Client) FeedbackSemantic(ctx context.Context, mnemeID string, evidence float64) (float64, error) {
	var r feedbackResp
	body := feedbackReq{MnemeID: mnemeID, Evidence: evidence}
	if err := c.do(ctx, "/v1/semantic/feedback", body, &r); err != nil {
		return 0, err
	}
	return r.Confidence, nil
}
