package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/thinkwright/chapterhouse/ch-server/internal/auth"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/pkg/apierror"
)

// SemanticHandler services /v1/semantic/*. Wraps the ghola extension's
// semantic.recall and semantic.update_confidence SQL functions for the
// query + feedback paths; hits semantic.mnemes directly for list.
type SemanticHandler struct {
	repo *repository.Repository
}

func NewSemanticHandler(repo *repository.Repository) *SemanticHandler {
	return &SemanticHandler{repo: repo}
}

// ---------------------------------------------------------------------
// Query
// ---------------------------------------------------------------------

type semanticQueryRequest struct {
	WorkspaceID     uuid.UUID `json:"workspace_id"`
	QueryText       string    `json:"query_text"`
	QueryEmbedding  []float64 `json:"query_embedding"`
	Limit           int       `json:"limit"`
	MinConfidence   *float64  `json:"min_confidence,omitempty"`
	MemoryType      *string   `json:"memory_type,omitempty"`
	Tags            []string  `json:"tags,omitempty"`
	FilterEntities  []string  `json:"filter_entities,omitempty"`
}

type semanticQueryResponse struct {
	Hits []mnemeHit `json:"hits"`
}

type mnemeHit struct {
	MnemeID       uuid.UUID `json:"mneme_id"`
	Score         float64   `json:"score"`
	ContentMatch  float64   `json:"content_match"`
	Activation    float64   `json:"activation"`
	HebbianBoost  float64   `json:"hebbian_boost"`
	Confidence    float64   `json:"confidence"`
	Concept       string    `json:"concept"`
	Content       string    `json:"content"`
	Tier          string    `json:"tier"`
}

func (h *SemanticHandler) Query(w http.ResponseWriter, r *http.Request) {
	if auth.UserIDFromContext(r.Context()) == uuid.Nil {
		apierror.Unauthorized("missing auth context").WriteJSON(w)
		return
	}

	var req semanticQueryRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.BadRequest("body decode: " + err.Error()).WriteJSON(w)
		return
	}
	if req.WorkspaceID == uuid.Nil {
		apierror.BadRequest("workspace_id is required").WriteJSON(w)
		return
	}

	params := repository.SemanticQueryParams{
		WorkspaceID:    req.WorkspaceID,
		QueryText:      req.QueryText,
		QueryEmbedding: req.QueryEmbedding,
		Limit:          req.Limit,
	}
	if req.MinConfidence != nil {
		params.MinConfidence = *req.MinConfidence
	}

	hits, err := h.repo.SemanticRecall(r.Context(), params)
	if err != nil {
		slog.Error("semantic recall failed", "err", err.Error())
		apierror.InternalError("query failed").WithError(err).WriteJSON(w)
		return
	}

	out := semanticQueryResponse{Hits: make([]mnemeHit, 0, len(hits))}
	for _, h := range hits {
		out.Hits = append(out.Hits, mnemeHit{
			MnemeID:      h.MnemeID,
			Score:        h.Score,
			ContentMatch: h.ContentMatch,
			Activation:   h.Activation,
			HebbianBoost: h.HebbianBoost,
			Confidence:   h.Confidence,
			Concept:      h.Concept,
			Content:      h.Content,
			Tier:         "semantic",
		})
	}
	OK(w, out)
}

// ---------------------------------------------------------------------
// Feedback
// ---------------------------------------------------------------------

type feedbackRequest struct {
	MnemeID  uuid.UUID `json:"mneme_id"`
	Evidence float64   `json:"evidence"`
}

type feedbackResponse struct {
	MnemeID    uuid.UUID `json:"mneme_id"`
	Confidence float64   `json:"confidence"`
}

func (h *SemanticHandler) Feedback(w http.ResponseWriter, r *http.Request) {
	if auth.UserIDFromContext(r.Context()) == uuid.Nil {
		apierror.Unauthorized("missing auth context").WriteJSON(w)
		return
	}

	var req feedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.BadRequest("body decode: " + err.Error()).WriteJSON(w)
		return
	}
	if req.MnemeID == uuid.Nil {
		apierror.BadRequest("mneme_id is required").WriteJSON(w)
		return
	}
	if req.Evidence < 0 || req.Evidence > 1 {
		apierror.BadRequest("evidence must be in [0,1]").WriteJSON(w)
		return
	}

	conf, err := h.repo.SemanticUpdateConfidence(r.Context(), req.MnemeID, req.Evidence)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			apierror.NotFound("mneme").WriteJSON(w)
			return
		}
		slog.Error("semantic feedback failed", "err", err.Error())
		apierror.InternalError("feedback failed").WithError(err).WriteJSON(w)
		return
	}

	OK(w, feedbackResponse{MnemeID: req.MnemeID, Confidence: conf})
}

// ---------------------------------------------------------------------
// List
// ---------------------------------------------------------------------

type semanticListRequest struct {
	WorkspaceID uuid.UUID                `json:"workspace_id"`
	Limit       int                      `json:"limit"`
	Cursor      *string                  `json:"cursor,omitempty"`
	Filters     *repository.MnemeFilters `json:"filters,omitempty"`
}

type semanticListResponse struct {
	Mnemes     []repository.Mneme `json:"mnemes"`
	NextCursor *string            `json:"next_cursor,omitempty"`
}

func (h *SemanticHandler) List(w http.ResponseWriter, r *http.Request) {
	if auth.UserIDFromContext(r.Context()) == uuid.Nil {
		apierror.Unauthorized("missing auth context").WriteJSON(w)
		return
	}

	var req semanticListRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.BadRequest("body decode: " + err.Error()).WriteJSON(w)
		return
	}
	if req.WorkspaceID == uuid.Nil {
		apierror.BadRequest("workspace_id is required").WriteJSON(w)
		return
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}

	params := repository.SemanticListParams{
		WorkspaceID: req.WorkspaceID,
		Limit:       limit,
		Cursor:      req.Cursor,
	}
	if req.Filters != nil {
		params.Filters = *req.Filters
	}

	mnemes, next, err := h.repo.SemanticList(r.Context(), params)
	if err != nil {
		slog.Error("semantic list failed", "err", err.Error())
		apierror.InternalError("list failed").WithError(err).WriteJSON(w)
		return
	}

	OK(w, semanticListResponse{Mnemes: mnemes, NextCursor: next})
}
