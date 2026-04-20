package handler

import (
	"net/http"

	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/pkg/apierror"
)

// SemanticHandler services /v1/semantic/*. Real implementation lands
// in Phase 3.7; this stub exists so the handler package compiles with
// its tests.
type SemanticHandler struct {
	repo *repository.Repository
}

func NewSemanticHandler(repo *repository.Repository) *SemanticHandler {
	return &SemanticHandler{repo: repo}
}

func (h *SemanticHandler) Query(w http.ResponseWriter, r *http.Request) {
	apierror.InternalError("/v1/semantic/query lands in Phase 3.7").WriteJSON(w)
}

func (h *SemanticHandler) Feedback(w http.ResponseWriter, r *http.Request) {
	apierror.InternalError("/v1/semantic/feedback lands in Phase 3.7").WriteJSON(w)
}

func (h *SemanticHandler) List(w http.ResponseWriter, r *http.Request) {
	apierror.InternalError("/v1/semantic/list lands in Phase 3.7").WriteJSON(w)
}
