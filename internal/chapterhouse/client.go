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

	"github.com/google/uuid"

	"github.com/logan-broit/ghola/internal/core"
)

// StatusError is returned when chapterhouse responds with a non-2xx
// status. Callers can errors.As to recover the original status code —
// the ghola HTTP layer uses this to propagate 4xx as 4xx instead of
// escalating every chapterhouse error to a 500.
type StatusError struct {
	Status  int
	Path    string
	Message string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: %d %s: %s", e.Path, e.Status,
		http.StatusText(e.Status), e.Message)
}

// StatusCode lets callers read the wire status without an errors.As
// dance when they already know the error type.
func (e *StatusError) StatusCode() int { return e.Status }

// Client POSTs JSON to chapterhouse's /v1/* endpoints with a Bearer
// API key. All methods return wire errors unwrapped; callers layer
// their own context.
type Client struct {
	baseURL string // trailing slash stripped
	apiKey  string
	http    *http.Client
}

const (
	// defaultRequestTimeout bounds a normal chapterhouse call end-to-end.
	// Applied per-request via context in do() (not as a fixed
	// http.Client.Timeout) so a single route can be lifted without a
	// separate client.
	defaultRequestTimeout = 30 * time.Second
	// consolidateTimeout bounds the manual consolidation trigger, which is
	// synchronous and blocks for the full episodic->semantic batch (tens of
	// seconds to minutes) — far past the default. See ConsolidateWorkspace.
	consolidateTimeout = 10 * time.Minute
)

// requestTimeout returns the per-request deadline for a chapterhouse path.
// The consolidate route legitimately runs for minutes; everything else uses
// the default. do() consults this single knob, so lifting a route is a
// one-line change here rather than a bespoke client per method.
func requestTimeout(path string) time.Duration {
	if path == "/v1/semantic/consolidate" {
		return consolidateTimeout
	}
	return defaultRequestTimeout
}

// New builds a Client. `baseURL` is chapterhouse's root URL
// (e.g. "http://localhost:8080" or
// "https://chapterhouse.example.com"). apiKey is the per-user
// bearer token the deployment issued.
func New(baseURL, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		// No client-level Timeout: per-request deadlines are applied in do()
		// via requestTimeout(path). A fixed http.Client.Timeout would clip
		// every request uniformly and cap the long-running consolidate call
		// below its legitimate multi-minute duration.
		http: &http.Client{},
	}
}

// WithHTTPClient lets tests (and custom callers) replace the
// underlying client — e.g. httptest's Client().
func (c *Client) WithHTTPClient(h *http.Client) *Client {
	c.http = h
	return c
}

func (c *Client) do(ctx context.Context, path string, body, out any) error {
	// Per-request deadline: bounds normal calls at defaultRequestTimeout and
	// lifts the long-running consolidate call to consolidateTimeout. Layered
	// on ctx (WithTimeout takes the min) so an earlier caller deadline still
	// wins. Replaces the former fixed 30s http.Client.Timeout, which would
	// have aborted consolidate mid-batch.
	ctx, cancel := context.WithTimeout(ctx, requestTimeout(path))
	defer cancel()

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
		return &StatusError{
			Status:  resp.StatusCode,
			Path:    path,
			Message: strings.TrimSpace(string(payload)),
		}
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

// ---------------------------------------------------------------------
// Multi-ranking request / response.
//
// Mirrors chapterhouse's MultiRankingRequest / MultiRankingResponse /
// MultiRankingHit / ScoreBreakdown shapes (see
// _chapterhouse/ch-server/internal/handler/episodic.go) one-for-one so
// the ghola client can talk to /v1/episodic/query without importing
// across the multi-module worktree boundary. Field-by-field with
// matching JSON tags; deviations would silently drop fields on the
// wire.
// ---------------------------------------------------------------------

// QueryEpisodicMultiRequest is the wire shape for /v1/episodic/query.
// A non-empty Rankings list is required — the chapterhouse handler
// rejects an absent/empty list with 400. Mirrors chapterhouse's
// MultiRankingRequest. Exported so internal/core/ can pass these
// values through Recall.
type QueryEpisodicMultiRequest struct {
	UserID         string    `json:"user_id"`
	WorkspaceID    string    `json:"workspace_id"`
	QueryText      string    `json:"query_text,omitempty"`
	QueryEmbedding []float64 `json:"query_embedding,omitempty"`
	Limit          int       `json:"limit,omitempty"`
	IncludeShared  *bool     `json:"include_shared,omitempty"`
	TagsAny        []string  `json:"tags_any,omitempty"`
	// Rankings names the tiers the caller wants ranked separately
	// ("vector", "fts", "session_vector"). Required; omitempty so a
	// zero-value request encodes as an empty body (the server will
	// reject it cleanly) rather than emitting a `"rankings":null`
	// that would deserialize ambiguously.
	Rankings []string `json:"rankings,omitempty"`
	// Primitives, when true, asks the chapterhouse handler to compute
	// a fourth Hebbian-boosted sub-list (D1) over the union of the
	// per-tier candidates and return it under the `primitives` key.
	// omitempty so a zero-value request stays off-the-wire — the
	// server's default-false bool semantics keep the legacy path
	// untouched for callers that don't opt in.
	Primitives bool `json:"primitives,omitempty"`
	// Settle opts the response into the P4 recurrent-settle expansion
	// sub-list. Pointer + omitempty so a settle-off request never
	// serializes the block (byte-identical to the pre-P4 wire), and the
	// chapterhouse handler treats the field's presence + enabled:true as
	// the discriminator. Mirrors chapterhouse's MultiRankingRequest.Settle.
	Settle *settleReq `json:"settle,omitempty"`
}

// settleReq mirrors chapterhouse's SettleRequest — the optional settle
// configuration block. Presence enables the expansion sub-list; numeric
// params are omitempty so zero values fall back to the server's
// DefaultSettleParams() rather than forcing a client-side default.
type settleReq struct {
	Enabled  bool    `json:"enabled"`
	Lambda   float64 `json:"lambda,omitempty"`
	HopCap   int     `json:"hop_cap,omitempty"`
	NodeCap  int     `json:"node_cap,omitempty"`
	TopM     int     `json:"top_m,omitempty"`
	Eps      float64 `json:"eps,omitempty"`
	MaxIters int     `json:"max_iters,omitempty"`
}

// MultiRankingScore mirrors chapterhouse's ScoreBreakdown: the per-
// tier raw scores (Semantic, FTS) + Merged, the tier's single sort
// key. Decoded by value on each MultiRankingHit; for tiers that
// produce only one of the two raw legs (e.g. session_vector has no
// FTS leg) the unused field stays at its zero value.
type MultiRankingScore struct {
	Semantic float64 `json:"semantic"`
	FTS      float64 `json:"fts"`
	Merged   float64 `json:"merged"`
}

// QueryEpisodicMultiHit is the shared hit shape across all per-tier
// sub-lists in QueryEpisodicMultiResponse. Mirrors chapterhouse's
// MultiRankingHit: event_id and session_id are pointers so a tier
// that doesn't produce one of them (session_vector has no per-event
// id) decodes as nil rather than the zero UUID. Score uses ,omitzero
// (Go 1.24+) for parity with the server — encoding/json's ,omitempty
// is a no-op for non-pointer structs, so a zero-value Score would
// always emit `"score":{...all zero...}` without it.
type QueryEpisodicMultiHit struct {
	EventID          *uuid.UUID        `json:"event_id,omitempty"`
	SessionID        *uuid.UUID        `json:"session_id,omitempty"`
	Tier             string            `json:"tier,omitempty"`
	Score            MultiRankingScore `json:"score,omitzero"`
	Text             *string           `json:"text,omitempty"`
	SessionChunkText string            `json:"session_chunk_text,omitempty"`
}

// QueryEpisodicMultiResponse carries one ranked sub-list per tier the
// caller named in Rankings. Mirrors chapterhouse's
// MultiRankingResponse: sub-list keys are the snake-case tier names
// ghola.Recall consumes. A tier that wasn't requested decodes as nil
// (omitempty on the wire); a requested-but-empty tier decodes as a
// non-nil empty slice so callers can iterate without nil-checking.
//
// Primitives uses a pointer-to-slice to mirror chapterhouse's 3-state
// wire shape (D1):
//   - nil → the `primitives` key is absent on the wire (flag was off,
//     OR flag was on but the chapterhouse-side association lookup
//     failed and the handler dropped the field as best-effort degrade).
//   - non-nil pointer to empty slice → flag was on and the server
//     emitted `"primitives":[]` (no in-set Hebbian boosts surfaced).
//   - non-nil pointer to populated slice → primitives ranking is live.
//
// Round-tripping the distinction matters because future consumers
// (D3+) need to tell "primitives never ran" from "primitives ran and
// found nothing" — the latter is a real signal about the seeded set.
type QueryEpisodicMultiResponse struct {
	Vector        []QueryEpisodicMultiHit  `json:"vector,omitempty"`
	FTS           []QueryEpisodicMultiHit  `json:"fts,omitempty"`
	SessionVector []QueryEpisodicMultiHit  `json:"session_vector,omitempty"`
	Primitives    *[]QueryEpisodicMultiHit `json:"primitives,omitempty"`
	// Expansion is the P4 recurrent-settle sub-list. Absent (omitempty on
	// the server) → nil here; a present but empty settle run → empty
	// slice. NOT a tier list — it is a separate output surface consumed
	// by core.Recall's rerank-pool expansion (config A/B). Mirrors
	// chapterhouse's MultiRankingResponse.Expansion.
	Expansion *[]expansionHitWire `json:"expansion,omitempty"`
}

// expansionHitWire mirrors chapterhouse's ExpansionHit: event_id +
// activation are always present; text is a pointer + omitempty so an
// entry whose text column is NULL / ACL-denied (Task 4 documented
// finding) decodes with a nil pointer, which the client projects to an
// empty Text string rather than dropping the row (core.Recall owns the
// drop decision).
type expansionHitWire struct {
	EventID    uuid.UUID `json:"event_id"`
	Activation float64   `json:"activation"`
	Text       *string   `json:"text,omitempty"`
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

type addSessionWorkspaceReq struct {
	UserID      string `json:"user_id"`
	SessionID   string `json:"session_id"`
	WorkspaceID string `json:"workspace_id"`
}

type addSessionWorkspaceResp struct {
	Added bool `json:"added"`
}

type semanticQueryReq struct {
	WorkspaceID    string    `json:"workspace_id"`
	QueryText      string    `json:"query_text"`
	QueryEmbedding []float32 `json:"query_embedding"`
	Limit          int       `json:"limit"`
}

// consolidateWorkspaceReq is the wire shape for the manual
// consolidation trigger, mirroring chapterhouse's consolidateRequest
// (_chapterhouse/ch-server/internal/handler/semantic.go).
type consolidateWorkspaceReq struct {
	Workspace string `json:"workspace"`
}

type semanticHitsResp struct {
	Hits []semanticHitRow `json:"hits"`
}

// semanticHitRow mirrors the v0.3 chapterhouse mnemeHit shape (see
// _chapterhouse/ch-server/internal/handler/semantic.go: mnemeHit). The
// v0.2 fields content_match, activation, hebbian_boost, confidence,
// concept, and content were dropped along with the schema columns
// they projected; PR7's dogfooding-tags work will reintroduce a
// content-bearing surface. Keep the struct narrow so the JSON decoder
// can't silently zero-fill fields the server no longer emits.
type semanticHitRow struct {
	MnemeID string  `json:"mneme_id"`
	Score   float64 `json:"score"`
	Level   int     `json:"level"`
	Tier    string  `json:"tier"`
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

// QueryEpisodicMulti POSTs to /v1/episodic/query, the multi-ranking
// endpoint that fans the requested subset of event-grain rankings
// inside chapterhouse instead of one HTTP call per tier.
//
// Caller owns Rankings — typical ghola.Recall use sends
// {"vector","fts","session_vector"} so the chapterhouse handler runs
// the same fan-out three separate endpoints used to do, but in one
// HTTP round trip. core.Recall trims the list per query gate (no
// embedding → drop vector+session_vector; no query_text → drop fts).
//
// Tier-string mapping: chapterhouse's multi-ranking response tags
// hits with the per-ranking tier name ("vector", "fts",
// "session_vector"). core.Recall's downstream RRF + dedup-by-grain
// logic keys on the legacy tier strings ("episodic", "keyword",
// "session_vector"), so this method maps "vector" → "episodic" and
// "fts" → "keyword" before returning. session_vector passes through
// — same string both sides. Without this remap, hitKey() would not
// recognize session-grain hits and the dedup-grain bug from prior
// commits would re-surface.
func (c *Client) QueryEpisodicMulti(ctx context.Context, q core.EpisodicMultiQuery) (core.EpisodicMultiResult, error) {
	emb := make([]float64, len(q.QueryEmbedding))
	for i, v := range q.QueryEmbedding {
		emb[i] = float64(v)
	}
	var includeShared *bool
	if q.IncludeShared {
		t := true
		includeShared = &t
	}
	// Settle: marshal a block only when the caller opted in, so a
	// settle-off request stays byte-identical to the pre-P4 wire. Zero
	// params serialize as omitted (omitempty) so chapterhouse applies
	// DefaultSettleParams — the client never injects its own defaults.
	var settle *settleReq
	if q.Settle {
		settle = &settleReq{
			Enabled:  true,
			Lambda:   q.SettleParams.Lambda,
			HopCap:   q.SettleParams.HopCap,
			NodeCap:  q.SettleParams.NodeCap,
			TopM:     q.SettleParams.TopM,
			Eps:      q.SettleParams.Eps,
			MaxIters: q.SettleParams.MaxIters,
		}
	}
	body := QueryEpisodicMultiRequest{
		UserID:         q.UserID,
		WorkspaceID:    q.WorkspaceID,
		QueryText:      q.QueryText,
		QueryEmbedding: emb,
		Limit:          q.Limit,
		IncludeShared:  includeShared,
		TagsAny:        q.TagsAny,
		Rankings:       q.Rankings,
		Primitives:     q.Primitives,
		Settle:         settle,
	}

	var r QueryEpisodicMultiResponse
	if err := c.do(ctx, "/v1/episodic/query", body, &r); err != nil {
		return core.EpisodicMultiResult{}, err
	}

	// Whether a tier was requested decides nil-vs-empty — server uses
	// omitempty for tiers it didn't run, so an empty-but-requested
	// tier decodes as nil here too. Allocate explicitly for the
	// requested set so downstream callers can iterate without nil
	// checks; tiers the caller didn't ask for stay nil.
	requested := make(map[string]struct{}, len(q.Rankings))
	for _, name := range q.Rankings {
		requested[name] = struct{}{}
	}

	out := core.EpisodicMultiResult{}
	if _, ok := requested["vector"]; ok {
		out.Vector = make([]core.RecallHit, 0, len(r.Vector))
		for _, h := range r.Vector {
			out.Vector = append(out.Vector, multiHitToRecallHit(h, "episodic"))
		}
	}
	if _, ok := requested["fts"]; ok {
		out.FTS = make([]core.RecallHit, 0, len(r.FTS))
		for _, h := range r.FTS {
			out.FTS = append(out.FTS, multiHitToRecallHit(h, "keyword"))
		}
	}
	if _, ok := requested["session_vector"]; ok {
		out.SessionVector = make([]core.RecallHit, 0, len(r.SessionVector))
		for _, h := range r.SessionVector {
			out.SessionVector = append(out.SessionVector, multiHitToRecallHit(h, "session_vector"))
		}
	}
	// Primitives: wire shape is pointer-to-slice for 3-state semantics
	// (nil = absent / flag-off, &[] = flag-on but no in-set boosts,
	// &[hits] = ranking present). Mirror that on the core side: when
	// the server omitted the field, leave out.Primitives nil; when it
	// was present, allocate the slice (even if empty) so callers can
	// distinguish "primitives ran and found nothing" from "primitives
	// never ran". Tier string passes through unchanged ("primitives")
	// — distinct from the legacy vector→episodic / fts→keyword remap,
	// since "primitives" is a new tier with no legacy alias to honor.
	if r.Primitives != nil {
		prims := make([]core.RecallHit, 0, len(*r.Primitives))
		for _, h := range *r.Primitives {
			prims = append(prims, multiHitToRecallHit(h, "primitives"))
		}
		out.Primitives = &prims
	}
	// Expansion: the P4 recurrent-settle sub-list. Absent (nil pointer)
	// leaves out.Expansion nil — no expansion applied. A present pointer
	// (even to an empty slice) means settle ran; project each wire entry
	// onto core.ExpansionHit. A nil Text pointer (event text NULL or
	// ACL-denied — Task 4 finding) projects to an empty Text string; the
	// entry is retained here and dropped later by core.Recall, keeping
	// the drop policy in one place.
	if r.Expansion != nil {
		exp := make([]core.ExpansionHit, 0, len(*r.Expansion))
		for _, h := range *r.Expansion {
			eh := core.ExpansionHit{
				ID:         h.EventID.String(),
				Activation: h.Activation,
			}
			if h.Text != nil {
				eh.Text = *h.Text
			}
			exp = append(exp, eh)
		}
		out.Expansion = exp
	}
	return out, nil
}

// multiHitToRecallHit projects a wire-level multi-ranking hit onto
// core.RecallHit. The caller chooses the downstream tier string:
// legacy event-grain tiers get remapped (server "vector"→"episodic",
// "fts"→"keyword") because the rest of core (hitKey, dedup,
// TierCounts) keys on those strings; "session_vector" passes through
// unchanged. The D2 "primitives" tier also passes through verbatim —
// it's a new tier with no legacy alias, and the grain prefix in
// hitKey() ("event:") is correct for it because primitives hits have
// EventID populated.
//
// ID resolution: event_id wins when present (vector / fts /
// primitives tiers); session_vector hits have no event_id and surface
// session_id as the hit id, mirroring the legacy
// QueryEpisodicSessionVector behavior so the grain-aware dedup key
// ("session:" + h.ID) collapses correctly.
func multiHitToRecallHit(h QueryEpisodicMultiHit, tier string) core.RecallHit {
	hit := core.RecallHit{
		Tier:             tier,
		Score:            h.Score.Merged,
		SessionChunkText: h.SessionChunkText,
	}
	if h.EventID != nil {
		hit.ID = h.EventID.String()
	} else if h.SessionID != nil {
		hit.ID = h.SessionID.String()
	}
	if h.SessionID != nil {
		s := h.SessionID.String()
		hit.SessionID = &s
	}
	if h.Text != nil {
		hit.Content = *h.Text
	}
	return hit
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

// AddSessionWorkspace POSTs to /v1/episodic/session_workspace. The 409
// pre-consolidate path is preserved by c.do's wrapping of non-2xx as
// *StatusError — the ghola HTTP layer does errors.As to re-emit the
// status to its caller (so MCP agents receive the "consolidate first"
// guidance verbatim).
func (c *Client) AddSessionWorkspace(ctx context.Context, in core.AddSessionWorkspaceInput) (bool, error) {
	var r addSessionWorkspaceResp
	body := addSessionWorkspaceReq{
		UserID:      in.UserID,
		SessionID:   in.SessionID,
		WorkspaceID: in.WorkspaceID,
	}
	if err := c.do(ctx, "/v1/episodic/session_workspace", body, &r); err != nil {
		return false, err
	}
	return r.Added, nil
}

// ConsolidateWorkspace POSTs to chapterhouse's
// /v1/semantic/consolidate, the manual trigger for the episodic->
// semantic consolidation batch (cluster closed sessions, enrich with
// excerpts, optionally label/digest). The call is synchronous —
// chapterhouse runs consolidation.RunWorkspace in-process and the HTTP
// response doesn't return until the batch completes, so this method
// blocks for the run's full duration. do() therefore grants this path the
// generous consolidateTimeout (10m) instead of the shared 30s — a batch of
// tens of seconds to minutes must not be aborted mid-run. Distinct from
// ghola's own Core.Consolidate (sietch->episodic session flush) — see
// internal/core/core.go's ConsolidateWorkspace doc comment for the
// seam rationale.
func (c *Client) ConsolidateWorkspace(ctx context.Context, workspaceID string) error {
	return c.do(ctx, "/v1/semantic/consolidate", consolidateWorkspaceReq{Workspace: workspaceID}, nil)
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
		// v0.3 semantic hits carry only id/score/tier — the underlying
		// content column was dropped in PR1.1, leaving Content at "".
		// PR7 will reintroduce a content-bearing field via the
		// dogfooding-tags mechanism.
		out = append(out, core.RecallHit{
			Tier:  h.Tier,
			ID:    h.MnemeID,
			Score: h.Score,
		})
	}
	return out, nil
}
