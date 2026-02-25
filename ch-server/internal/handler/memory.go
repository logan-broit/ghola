package handler

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/thinkwright/chapterhouse/ch-server/internal/auth"
	"github.com/thinkwright/chapterhouse/ch-server/internal/model"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository/sqlc"
	"github.com/thinkwright/chapterhouse/ch-server/pkg/apierror"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// MemoryHandler handles memory block operations.
type MemoryHandler struct {
	queries *sqlc.Queries
}

// NewMemoryHandler creates a new memory handler.
func NewMemoryHandler(queries *sqlc.Queries) *MemoryHandler {
	return &MemoryHandler{queries: queries}
}

// GetContext handles GET /api/v1/memories/context
// Returns all current memory blocks organized by tier.
func (h *MemoryHandler) GetContext(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := auth.UserIDFromContext(ctx)

	blocks, err := h.queries.GetMemoryContext(ctx, userID)
	if err != nil {
		Error(w, apierror.InternalError("Failed to load memory context").WithError(err))
		return
	}

	// Organize by tier
	memCtx := model.MemoryContext{
		Core:  make([]model.MemoryBlock, 0),
		Index: make([]model.MemoryBlock, 0),
		State: make([]model.MemoryBlock, 0),
	}

	for _, b := range blocks {
		block := toMemoryBlock(b)
		switch block.Tier {
		case model.TierCore:
			memCtx.Core = append(memCtx.Core, block)
		case model.TierIndex:
			memCtx.Index = append(memCtx.Index, block)
		case model.TierState:
			memCtx.State = append(memCtx.State, block)
		}
	}

	OK(w, memCtx)
}

// ListBlocks handles GET /api/v1/memories/blocks
func (h *MemoryHandler) ListBlocks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := auth.UserIDFromContext(ctx)

	tier := r.URL.Query().Get("tier")

	var blocks []sqlc.CurrentMemoryBlock
	var err error

	if tier != "" && model.MemoryTier(tier).Valid() {
		blocks, err = h.queries.GetCurrentMemoryBlocksByTier(ctx, sqlc.GetCurrentMemoryBlocksByTierParams{
			UserID: userID,
			Tier:   tier,
		})
	} else {
		blocks, err = h.queries.GetCurrentMemoryBlocks(ctx, userID)
	}

	if err != nil {
		Error(w, apierror.InternalError("Failed to list memory blocks").WithError(err))
		return
	}

	result := make([]model.MemoryBlock, len(blocks))
	for i, b := range blocks {
		result[i] = toMemoryBlock(b)
	}

	OK(w, result)
}

// GetBlock handles GET /api/v1/memories/blocks/{name}
func (h *MemoryHandler) GetBlock(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := auth.UserIDFromContext(ctx)
	name := chi.URLParam(r, "name")

	block, err := h.queries.GetCurrentMemoryBlockByName(ctx, sqlc.GetCurrentMemoryBlockByNameParams{
		UserID: userID,
		Name:   name,
	})
	if err != nil {
		Error(w, apierror.NotFound("Memory block"))
		return
	}

	OK(w, toMemoryBlock(block))
}

// CreateOrUpdateBlockRequest represents the request body for creating/updating a block.
type CreateOrUpdateBlockRequest struct {
	Tier       string `json:"tier"`
	Value      string `json:"value"`
	SortOrder  *int   `json:"sort_order,omitempty"`
	MemoryType string `json:"memory_type,omitempty"`
	Scope      string `json:"scope,omitempty"`
}

// CreateOrUpdateBlock handles PUT /api/v1/memories/blocks/{name}
func (h *MemoryHandler) CreateOrUpdateBlock(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := auth.UserIDFromContext(ctx)
	name := chi.URLParam(r, "name")

	var req CreateOrUpdateBlockRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, err)
		return
	}

	tier := model.MemoryTier(req.Tier)
	if !tier.Valid() {
		Error(w, apierror.BadRequest("Invalid tier: must be core, index, or state"))
		return
	}

	sortOrder := 0
	if req.SortOrder != nil {
		sortOrder = *req.SortOrder
	}

	// Get the next version number for this block
	nextVersion, err := h.queries.GetNextMemoryBlockVersion(ctx, sqlc.GetNextMemoryBlockVersionParams{
		UserID: userID,
		Name:   name,
	})
	if err != nil {
		Error(w, apierror.InternalError("Failed to get next version").WithError(err))
		return
	}

	memoryType := req.MemoryType
	if memoryType == "" {
		memoryType = "factual"
	}
	scope := req.Scope
	if scope == "" {
		scope = "personal"
	}

	block, err := h.queries.CreateMemoryBlock(ctx, sqlc.CreateMemoryBlockParams{
		UserID: userID,
		Name:   name,
		Tier:   req.Tier,
		Value: pgtype.Text{
			String: req.Value,
			Valid:  true,
		},
		Version:    nextVersion,
		SortOrder:  int32(sortOrder),
		MemoryType: memoryType,
		Scope:      scope,
	})
	if err != nil {
		Error(w, apierror.InternalError("Failed to save memory block").WithError(err))
		return
	}

	Created(w, toMemoryBlockFromFull(block))
}

// DeleteBlock handles DELETE /api/v1/memories/blocks/{name}
func (h *MemoryHandler) DeleteBlock(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := auth.UserIDFromContext(ctx)
	name := chi.URLParam(r, "name")

	err := h.queries.DeleteMemoryBlock(ctx, sqlc.DeleteMemoryBlockParams{
		UserID: userID,
		Name:   name,
	})
	if err != nil {
		Error(w, apierror.InternalError("Failed to delete memory block").WithError(err))
		return
	}

	NoContent(w)
}

// GetBlockHistory handles GET /api/v1/memories/blocks/{name}/history
func (h *MemoryHandler) GetBlockHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := auth.UserIDFromContext(ctx)
	name := chi.URLParam(r, "name")

	limit := 20
	offset := 0
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
	}
	if o := r.URL.Query().Get("offset"); o != "" {
		if parsed, err := strconv.Atoi(o); err == nil && parsed >= 0 {
			offset = parsed
		}
	}

	history, err := h.queries.GetMemoryBlockHistory(ctx, sqlc.GetMemoryBlockHistoryParams{
		UserID: userID,
		Name:   name,
		Limit:  int32(limit),
		Offset: int32(offset),
	})
	if err != nil {
		Error(w, apierror.InternalError("Failed to get block history").WithError(err))
		return
	}

	result := make([]model.MemoryBlock, len(history))
	for i, b := range history {
		result[i] = toMemoryBlockFromFull(b)
	}

	OK(w, result)
}

func toMemoryBlock(b sqlc.CurrentMemoryBlock) model.MemoryBlock {
	value := ""
	if b.Value.Valid {
		value = b.Value.String
	}
	return model.MemoryBlock{
		ID:         b.ID,
		GUID:       b.Guid,
		UserID:     b.UserID,
		Name:       b.Name,
		Tier:       model.MemoryTier(b.Tier),
		Value:      value,
		Version:    int(b.Version),
		SortOrder:  int(b.SortOrder),
		CreatedAt:  b.CreatedAt,
		ModifiedAt: b.ModifiedAt,
	}
}

func toMemoryBlockFromFull(b sqlc.MemoryBlock) model.MemoryBlock {
	value := ""
	if b.Value.Valid {
		value = b.Value.String
	}
	return model.MemoryBlock{
		ID:         b.ID,
		GUID:       b.Guid,
		UserID:     b.UserID,
		Name:       b.Name,
		Tier:       model.MemoryTier(b.Tier),
		Value:      value,
		Version:    int(b.Version),
		SortOrder:  int(b.SortOrder),
		CreatedAt:  b.CreatedAt,
		ModifiedAt: b.ModifiedAt,
	}
}

// SearchRequest represents a semantic search request.
type SearchRequest struct {
	Query string   `json:"query"`
	Limit int      `json:"limit,omitempty"`
	Types []string `json:"types,omitempty"`
}

// Search handles POST /api/v1/memories/search
func (h *MemoryHandler) Search(w http.ResponseWriter, r *http.Request) {
	var req SearchRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, err)
		return
	}

	if req.Query == "" {
		Error(w, apierror.BadRequest("Query is required"))
		return
	}

	ctx := r.Context()
	userID := auth.UserIDFromContext(ctx)

	limit := int32(20)
	if req.Limit > 0 && req.Limit <= 100 {
		limit = int32(req.Limit)
	}

	blocks, err := h.queries.SearchAccessibleMemoryBlocks(ctx, sqlc.SearchAccessibleMemoryBlocksParams{
		UserID:      userID,
		Query:       pgtype.Text{String: req.Query, Valid: true},
		SearchLimit: limit,
	})
	if err != nil {
		Error(w, apierror.InternalError("Search failed").WithError(err))
		return
	}

	results := make([]model.SearchResult, 0, len(blocks))
	for _, b := range blocks {
		content := ""
		if b.Value.Valid {
			content = b.Value.String
		}
		results = append(results, model.SearchResult{
			ID:        b.Guid,
			Type:      "memory_block",
			EntryType: b.MemoryType,
			Content:   content,
			CreatedAt: b.CreatedAt,
			Metadata: map[string]any{
				"name":  b.Name,
				"scope": b.Scope,
				"tags":  b.Tags,
			},
		})
	}

	OK(w, results)
}

// StatusResponse represents the /api/v1/status response.
type StatusResponse struct {
	UserID       string `json:"user_id"`
	MemoryBlocks int    `json:"memory_blocks"`
	Environment  string `json:"environment"`
}

// Status handles GET /api/v1/status
func (h *MemoryHandler) Status(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := auth.UserIDFromContext(ctx)

	count, err := h.queries.CountMemoryBlocksByUser(ctx, userID)
	if err != nil {
		count = 0
	}

	resp := StatusResponse{
		UserID:       userID.String(),
		MemoryBlocks: int(count),
		Environment:  "development",
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
