package handler

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/thinkwright/chapterhouse/ch-server/internal/auth"
	"github.com/thinkwright/chapterhouse/ch-server/internal/mentat"
	"github.com/thinkwright/chapterhouse/ch-server/internal/semantic"
	"github.com/thinkwright/chapterhouse/ch-server/pkg/apierror"
)

// SemanticHandler services /v1/semantic/query and the manual
// consolidation trigger /v1/semantic/consolidate. The v0.2 surface
// (feedback + list) is gone — the v0.3 read path is just cosine over
// pooled embeddings, and the dropped text columns mean list/feedback
// have no shape that makes sense yet. PR7 introduces a new
// dogfooding-tags feedback mechanism.
type SemanticHandler struct {
	q              *semantic.Querier
	runConsolidate consolidateRunner
}

// NewSemanticHandler wraps the concrete Querier. Pass nil-tolerant
// querier (constructed with a nil *mentat.Client) when the deployment
// runs without mentat — Recall will short-circuit to zero hits.
func NewSemanticHandler(q *semantic.Querier) *SemanticHandler {
	return &SemanticHandler{q: q}
}

// ---------------------------------------------------------------------
// /v1/semantic/query
// ---------------------------------------------------------------------

// semanticQueryRequest is the wire shape for POST /v1/semantic/query.
//
// Field-name compatibility with v0.2: workspace_id, query_embedding,
// and limit are unchanged. query_text is accepted but unused (the
// embedding now drives recall end-to-end). recent_context is new and
// optional — callers without it just get a single-event pool.
//
// RecentContext piggybacks mentat.Event directly — the JSON tags
// (`type`, `embedding`) match what mentat's pool API consumes, so the
// handler hands the slice through to Querier.Recall verbatim.
type semanticQueryRequest struct {
	WorkspaceID    uuid.UUID      `json:"workspace_id"`
	QueryText      string         `json:"query_text,omitempty"`
	QueryEmbedding []float32      `json:"query_embedding"`
	RecentContext  []mentat.Event `json:"recent_context,omitempty"`
	Limit          int            `json:"limit"`
}

// semanticQueryResponse mirrors the v0.2 envelope (`hits: [...]`) so
// existing ghola-mcp clients keep deserializing — only the per-hit
// shape narrows. Clients that read concept/content/confidence from a
// hit will see those fields absent and need to upgrade.
type semanticQueryResponse struct {
	Hits []mnemeHit `json:"hits"`
}

type mnemeHit struct {
	MnemeID uuid.UUID `json:"mneme_id"`
	Score   float64   `json:"score"`
	Level   int       `json:"level"`
	Tier    string    `json:"tier"`
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
	if len(req.QueryEmbedding) == 0 {
		apierror.BadRequest("query_embedding is required").WriteJSON(w)
		return
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}

	hits, err := h.q.Recall(r.Context(), semantic.RecallRequest{
		WorkspaceID:    req.WorkspaceID,
		QueryEmbedding: req.QueryEmbedding,
		RecentContext:  req.RecentContext,
		Limit:          limit,
	})
	if err != nil {
		apierror.InternalError("query failed").WithError(err).WriteJSON(w)
		return
	}

	out := semanticQueryResponse{Hits: make([]mnemeHit, 0, len(hits))}
	for _, hit := range hits {
		out.Hits = append(out.Hits, mnemeHit{
			MnemeID: hit.ID,
			Score:   hit.Score,
			Level:   hit.Level,
			Tier:    "semantic",
		})
	}
	OK(w, out)
}

// ---------------------------------------------------------------------
// /v1/semantic/consolidate
// ---------------------------------------------------------------------
//
// Manual trigger for the episodic->semantic consolidation batch (cluster
// closed sessions, enrich, optionally label/digest). Distinct from
// ghola's own POST /v1/consolidate {session_id} + MCP `consolidate` verb,
// which flush one session's pending sietch events to episodic — a
// different pipeline entirely. This endpoint and the chapterhouse
// worker's nightly schedule both call consolidation.RunWorkspace, so
// there is exactly one code path for the job (plan seam decision 1).

// consolidateRunner runs the episodic->semantic batch for a workspace.
// Defaulted in cmd/api to a closure over consolidation.RunWorkspace +
// the API's deps; tests inject a fake. nil => 500 (not configured).
type consolidateRunner func(ctx context.Context, workspaceID uuid.UUID) error

// WithConsolidateRunner attaches the manual-trigger runner. Fluent.
func (h *SemanticHandler) WithConsolidateRunner(fn consolidateRunner) *SemanticHandler {
	h.runConsolidate = fn
	return h
}

// consolidateRequest is the wire shape for POST /v1/semantic/consolidate.
type consolidateRequest struct {
	Workspace uuid.UUID `json:"workspace"`
}

// Consolidate handles POST /v1/semantic/consolidate. Synchronous: runs
// the full workspace pipeline and returns when done. This is the manual
// counterpart to the worker's nightly schedule — same code path
// (consolidation.RunWorkspace). The job is idempotent (unchanged-
// membership reinforcements are skipped, ArchivePriorDigest+
// InsertDigestMneme converge on re-run), so a manual trigger racing the
// nightly tick converges rather than corrupting state.
func (h *SemanticHandler) Consolidate(w http.ResponseWriter, r *http.Request) {
	if auth.UserIDFromContext(r.Context()) == uuid.Nil {
		apierror.Unauthorized("authentication required").WriteJSON(w)
		return
	}
	var req consolidateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		apierror.BadRequest("invalid JSON body").WriteJSON(w)
		return
	}
	if req.Workspace == uuid.Nil {
		apierror.BadRequest("workspace is required").WriteJSON(w)
		return
	}
	if h.runConsolidate == nil {
		apierror.InternalError("consolidation not configured").WriteJSON(w)
		return
	}
	if err := h.runConsolidate(r.Context(), req.Workspace); err != nil {
		apierror.InternalError("consolidation failed").WithError(err).WriteJSON(w)
		return
	}
	OK(w, map[string]string{"status": "ok"})
}
