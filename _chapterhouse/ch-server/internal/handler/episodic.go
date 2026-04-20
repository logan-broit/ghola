package handler

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/thinkwright/chapterhouse/ch-server/internal/auth"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/pkg/apierror"
)

// EpisodicHandler services /v1/episodic/* — the per-user raw event
// store called only by the ghola local service (Pipeline A writes;
// recall reads). Not agent-facing; auth is per-user API key and is
// expected to be set on the request context by middleware (Phase
// 3.8); tests inject directly via auth.WithContext.
type EpisodicHandler struct {
	repo *repository.Repository
}

func NewEpisodicHandler(repo *repository.Repository) *EpisodicHandler {
	return &EpisodicHandler{repo: repo}
}

// ---------------------------------------------------------------------
// DTOs (mirror docs/api/v1-chapterhouse.yaml)
// ---------------------------------------------------------------------

type ingestRequest struct {
	Session repository.EpisodicSession `json:"session"`
	Events  []repository.EpisodicEvent `json:"events"`
}

type ingestResponse struct {
	SessionID uuid.UUID `json:"session_id"`
	Inserted  int       `json:"inserted"`
	Updated   int       `json:"updated"`
}

// ---------------------------------------------------------------------
// Ingest
// ---------------------------------------------------------------------

// Ingest upserts one session and its events. Idempotent — same event
// id POSTed twice is counted as Updated, not a duplicate. Pipeline A's
// at-least-once delivery depends on this.
func (h *EpisodicHandler) Ingest(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	if userID == uuid.Nil {
		apierror.Unauthorized("missing auth context").WriteJSON(w)
		return
	}

	var req ingestRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.BadRequest("body decode: " + err.Error()).WriteJSON(w)
		return
	}

	if err := validateIngest(&req, userID); err != nil {
		apierror.BadRequest(err.Error()).WriteJSON(w)
		return
	}

	// Default times.
	if req.Session.StartedAt.IsZero() {
		req.Session.StartedAt = time.Now().UTC()
	}
	for i := range req.Events {
		if req.Events[i].CreatedAt.IsZero() {
			req.Events[i].CreatedAt = time.Now().UTC()
		}
	}

	ctx := r.Context()
	inserted, updated, err := h.repo.IngestEpisodicBatch(ctx, &req.Session, req.Events)
	if err != nil {
		apierror.InternalError("ingest failed").WithError(err).WriteJSON(w)
		return
	}

	OK(w, ingestResponse{
		SessionID: req.Session.ID,
		Inserted:  inserted,
		Updated:   updated,
	})
}

func validateIngest(req *ingestRequest, caller uuid.UUID) error {
	if req.Session.ID == uuid.Nil {
		return errors.New("session.id is required")
	}
	if req.Session.UserID == uuid.Nil {
		return errors.New("session.user_id is required")
	}
	if req.Session.UserID != caller {
		return errors.New("session.user_id must match caller")
	}
	for i, ev := range req.Events {
		if ev.ID == uuid.Nil {
			return fmt.Errorf("events[%d].id is required", i)
		}
		if ev.SessionID != req.Session.ID {
			return fmt.Errorf("events[%d].session_id must match session.id", i)
		}
		if ev.UserID != caller {
			return fmt.Errorf("events[%d].user_id must match caller", i)
		}
		switch ev.Type {
		case "user", "assistant", "tool_result", "system":
		default:
			return fmt.Errorf("events[%d].type must be user|assistant|tool_result|system", i)
		}
	}
	return nil
}

// ---------------------------------------------------------------------
// Query / Share / Forget — implemented in Tasks 3.5 + 3.6.
// ---------------------------------------------------------------------

// queryRequest mirrors the OpenAPI EpisodicQueryRequest.
type queryRequest struct {
	UserID         uuid.UUID               `json:"user_id"`
	QueryText      string                  `json:"query_text"`
	QueryEmbedding []float64               `json:"query_embedding"`
	Limit          int                     `json:"limit"`
	IncludeShared  *bool                   `json:"include_shared,omitempty"`
	WSemantic      *float64                `json:"w_semantic,omitempty"`
	WFTS           *float64                `json:"w_fts,omitempty"`
	Filters        *repository.QueryFilters `json:"filters,omitempty"`
}

type queryResponse struct {
	Hits []eventHit `json:"hits"`
}

type eventHit struct {
	repository.EpisodicEvent
	Score scoreBreakdown `json:"score"`
	Tier  string         `json:"tier"`
}

type scoreBreakdown struct {
	Semantic float64 `json:"semantic"`
	FTS      float64 `json:"fts"`
	Merged   float64 `json:"merged"`
}

func (h *EpisodicHandler) Query(w http.ResponseWriter, r *http.Request) {
	userID := auth.UserIDFromContext(r.Context())
	if userID == uuid.Nil {
		apierror.Unauthorized("missing auth context").WriteJSON(w)
		return
	}

	var req queryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.BadRequest("body decode: " + err.Error()).WriteJSON(w)
		return
	}
	if req.UserID == uuid.Nil {
		apierror.BadRequest("user_id is required").WriteJSON(w)
		return
	}
	if req.UserID != userID {
		// Agents may only query as themselves. (A future admin
		// endpoint could lift this; not v1a.)
		apierror.Forbidden("user_id must match caller").WriteJSON(w)
		return
	}

	params := repository.EpisodicQueryParams{
		UserID:         userID,
		QueryText:      req.QueryText,
		QueryEmbedding: req.QueryEmbedding,
		Limit:          req.Limit,
		IncludeShared:  true,
		WSemantic:      0.6,
		WFTS:           0.4,
	}
	if req.Limit <= 0 {
		params.Limit = 10
	}
	if req.IncludeShared != nil {
		params.IncludeShared = *req.IncludeShared
	}
	if req.WSemantic != nil {
		params.WSemantic = *req.WSemantic
	}
	if req.WFTS != nil {
		params.WFTS = *req.WFTS
	}
	if req.Filters != nil {
		params.Filters = *req.Filters
	}

	hits, err := h.repo.QueryEpisodicEvents(r.Context(), params)
	if err != nil {
		slog.Error("episodic query failed", "err", err.Error())
		apierror.InternalError("query failed").WithError(err).WriteJSON(w)
		return
	}

	out := queryResponse{Hits: make([]eventHit, 0, len(hits))}
	for _, h := range hits {
		out.Hits = append(out.Hits, eventHit{
			EpisodicEvent: h.Event,
			Score: scoreBreakdown{
				Semantic: h.Semantic,
				FTS:      h.FTS,
				Merged:   h.Merged,
			},
			Tier: "episodic",
		})
	}
	OK(w, out)
}

// ---------------------------------------------------------------------
// Share
// ---------------------------------------------------------------------

type shareRequest struct {
	OwnerUserID uuid.UUID  `json:"owner_user_id"`
	Target      string     `json:"target"`
	TargetID    *uuid.UUID `json:"target_id,omitempty"`
	ScopeType   string     `json:"scope_type"`
	ScopeID     uuid.UUID  `json:"scope_id"`
}

type shareResponse struct {
	ID uuid.UUID `json:"id"`
}

func (h *EpisodicHandler) Share(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserIDFromContext(r.Context())
	if caller == uuid.Nil {
		apierror.Unauthorized("missing auth context").WriteJSON(w)
		return
	}

	var req shareRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.BadRequest("body decode: " + err.Error()).WriteJSON(w)
		return
	}
	if req.OwnerUserID != caller {
		apierror.Forbidden("owner_user_id must match caller").WriteJSON(w)
		return
	}
	switch req.Target {
	case "team", "user":
	default:
		apierror.BadRequest("target must be 'team' or 'user'").WriteJSON(w)
		return
	}
	if req.Target == "user" && (req.TargetID == nil || *req.TargetID == uuid.Nil) {
		apierror.BadRequest("target_id is required when target='user'").WriteJSON(w)
		return
	}
	switch req.ScopeType {
	case "session", "branch", "event":
	default:
		apierror.BadRequest("scope_type must be 'session' | 'branch' | 'event'").WriteJSON(w)
		return
	}
	if req.ScopeID == uuid.Nil {
		apierror.BadRequest("scope_id is required").WriteJSON(w)
		return
	}

	id, err := h.repo.CreateEpisodicShare(r.Context(), repository.CreateShareParams{
		Caller:    caller,
		Target:    req.Target,
		TargetID:  req.TargetID,
		ScopeType: req.ScopeType,
		ScopeID:   req.ScopeID,
	})
	if err != nil {
		if errors.Is(err, repository.ErrShareNotOwned) {
			apierror.Forbidden("caller does not own the referenced scope").WriteJSON(w)
			return
		}
		slog.Error("episodic share failed", "err", err.Error())
		apierror.InternalError("share failed").WithError(err).WriteJSON(w)
		return
	}

	OK(w, shareResponse{ID: id})
}

// ---------------------------------------------------------------------
// Forget
// ---------------------------------------------------------------------

type forgetRequest struct {
	UserID   uuid.UUID   `json:"user_id"`
	EventIDs []uuid.UUID `json:"event_ids"`
}

type forgetResponse struct {
	Forgotten int `json:"forgotten"`
}

func (h *EpisodicHandler) Forget(w http.ResponseWriter, r *http.Request) {
	caller := auth.UserIDFromContext(r.Context())
	if caller == uuid.Nil {
		apierror.Unauthorized("missing auth context").WriteJSON(w)
		return
	}

	var req forgetRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.BadRequest("body decode: " + err.Error()).WriteJSON(w)
		return
	}
	if req.UserID != uuid.Nil && req.UserID != caller {
		apierror.Forbidden("user_id must match caller").WriteJSON(w)
		return
	}
	if len(req.EventIDs) == 0 {
		apierror.BadRequest("event_ids must be non-empty").WriteJSON(w)
		return
	}

	forgotten, err := h.repo.SoftDeleteEpisodicEvents(r.Context(), caller, req.EventIDs)
	if err != nil {
		slog.Error("episodic forget failed", "err", err.Error())
		apierror.InternalError("forget failed").WithError(err).WriteJSON(w)
		return
	}

	OK(w, forgetResponse{Forgotten: forgotten})
}
