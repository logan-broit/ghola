package handler

import (
	"encoding/json"
	"errors"
	"fmt"
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

func (h *EpisodicHandler) Query(w http.ResponseWriter, r *http.Request) {
	apierror.InternalError("/v1/episodic/query lands in Phase 3.5").WriteJSON(w)
}

func (h *EpisodicHandler) Share(w http.ResponseWriter, r *http.Request) {
	apierror.InternalError("/v1/episodic/share lands in Phase 3.6").WriteJSON(w)
}

func (h *EpisodicHandler) Forget(w http.ResponseWriter, r *http.Request) {
	apierror.InternalError("/v1/episodic/forget lands in Phase 3.6").WriteJSON(w)
}
