package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/thinkwright/chapterhouse/ch-server/internal/auth"
	"github.com/thinkwright/chapterhouse/ch-server/internal/consolidation"
	"github.com/thinkwright/chapterhouse/ch-server/internal/mentat"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
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
	// Label + Content are the consolidation-era content surface. Both are
	// omitempty so a content-less mneme (all pre-consolidation rows) emits
	// exactly the v0.3 shape — the wire stays byte-identical until a mneme
	// actually carries text. Content is what recall consumes for rerank.
	Label   string `json:"label,omitempty"`
	Content string `json:"content,omitempty"`
}

// semanticContentCap bounds the text a semantic hit carries onto the
// wire. Defensive: label is a short LLM cluster label (level-1) or a
// digest paragraph (level-2), and the excerpt is already ≤500 bytes, so
// this mainly guards an unexpectedly long label. Trims on a rune
// boundary so a truncated value is never invalid UTF-8.
const semanticContentCap = 800

// semanticHitContent joins a mneme's label and top excerpt into the
// readable `content` the recall path reranks against. When both are
// present they are newline-joined (label first); otherwise whichever is
// non-empty is used verbatim. A level-2 digest carries its paragraph in
// label with no excerpt, so it surfaces as the paragraph alone.
func semanticHitContent(label, excerpt string) string {
	var content string
	switch {
	case label != "" && excerpt != "":
		content = label + "\n" + excerpt
	case label != "":
		content = label
	default:
		content = excerpt
	}
	// Shared rune-safe truncator (repository.TruncateRuneSafe) — see that
	// function's doc comment for why repository is the shared spot.
	return repository.TruncateRuneSafe(content, semanticContentCap)
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
			Label:   hit.Label,
			Content: semanticHitContent(hit.Label, hit.TopExcerpt),
		})
	}
	OK(w, out)

	// HOLA weak-label stream: bump access_count + last_access for the
	// mnemes we just returned. Fired AFTER the response is written and on a
	// detached context (WithoutCancel keeps request-scoped values but drops
	// cancellation — same discipline as Consolidate, commit d06392c) so a
	// client disconnect can't abort it and it adds ZERO latency to the
	// recall response path. Bounded + logged, never surfaced: an access-
	// tracking failure must not perturb recall.
	//
	// Deliberately untouched by level: this also bumps level-2 workspace
	// digest mnemes when one is returned. Excluding the digest at write time
	// would destroy information — its access data is a legitimate signal
	// about whether the digest paragraph itself was useful, and it's
	// trivially filterable downstream. Any HOLA weak-label analysis over
	// access_count/last_access MUST filter to level=1 (the digest is
	// synthetic — a workspace rollup, not a clustered representative — so
	// mixing it into the level-1 signal would skew the label).
	if len(hits) > 0 {
		ids := make([]uuid.UUID, len(hits))
		for i, hit := range hits {
			ids[i] = hit.ID
		}
		touchCtx := context.WithoutCancel(r.Context())
		go func() {
			bg, cancel := context.WithTimeout(touchCtx, 5*time.Second)
			defer cancel()
			if err := h.q.TouchMnemes(bg, ids); err != nil {
				slog.Warn("semantic: touch mnemes failed", "err", err.Error())
			}
		}()
	}
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
// (consolidation.RunWorkspace).
//
// Concurrency safety is now ENFORCED, not merely tolerated: a per-workspace
// advisory lock inside RunWorkspace serializes runs, so a manual trigger
// racing the nightly tick (or a second manual trigger) sees
// ErrConsolidationBusy and this handler returns 409 Conflict rather than
// letting two runs race. Sequential re-runs still converge idempotently
// (unchanged-membership reinforcements are skipped; ArchivePriorDigest+
// InsertDigestMneme replace the single active digest), which is what makes
// the 409-then-retry safe.
//
// Detached run context: the pipeline runs on context.WithoutCancel(r.Context())
// so a client disconnect or a client-side timeout does NOT abort a run
// already in flight — consolidation legitimately takes tens of seconds to
// minutes. The run completes server-side; if the response was lost, an
// idempotent re-trigger converges. The detached context still carries
// request-scoped values (auth) so downstream reads stay authorized.
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
		// The feature is unconfigured (deployment ran without mentat), not a
		// server fault — 503 lets callers distinguish a deploy-time gap from
		// a genuine 500.
		apierror.ServiceUnavailable("consolidation not configured").WriteJSON(w)
		return
	}
	// Detach from the request context so a client disconnect/timeout can't
	// abort a run mid-flight (Finding 1a). WithoutCancel preserves values.
	runCtx := context.WithoutCancel(r.Context())
	if err := h.runConsolidate(runCtx, req.Workspace); err != nil {
		if errors.Is(err, consolidation.ErrConsolidationBusy) {
			apierror.Conflict("consolidation already running for this workspace").WriteJSON(w)
			return
		}
		apierror.InternalError("consolidation failed").WithError(err).WriteJSON(w)
		return
	}
	OK(w, map[string]string{"status": "ok"})
}
