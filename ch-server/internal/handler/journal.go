package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/thinkwright/chapterhouse/ch-server/internal/auth"
	"github.com/thinkwright/chapterhouse/ch-server/internal/model"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository/sqlc"
	"github.com/thinkwright/chapterhouse/ch-server/pkg/apierror"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// JournalHandler handles journal entry operations.
type JournalHandler struct {
	queries *sqlc.Queries
}

// NewJournalHandler creates a new journal handler.
func NewJournalHandler(queries *sqlc.Queries) *JournalHandler {
	return &JournalHandler{queries: queries}
}

// ListEntries handles GET /api/v1/journal/entries
func (h *JournalHandler) ListEntries(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := auth.UserIDFromContext(ctx)

	entryType := r.URL.Query().Get("type")
	limit, offset := parsePagination(r)

	// Column2 corresponds to the optional entry_type filter
	entryTypeFilter := ""
	if entryType != "" && model.JournalEntryType(entryType).Valid() {
		entryTypeFilter = entryType
	}

	entries, err := h.queries.ListJournalEntries(ctx, sqlc.ListJournalEntriesParams{
		UserID:  userID,
		Column2: entryTypeFilter,
		Limit:   int32(limit),
		Offset:  int32(offset),
	})
	if err != nil {
		Error(w, apierror.InternalError("Failed to list journal entries").WithError(err))
		return
	}

	result := make([]model.JournalEntry, len(entries))
	for i, e := range entries {
		result[i] = toJournalEntry(e)
	}

	OK(w, result)
}

// GetEntry handles GET /api/v1/journal/entries/{id}
func (h *JournalHandler) GetEntry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := auth.UserIDFromContext(ctx)
	idStr := chi.URLParam(r, "id")

	guid, err := uuid.Parse(idStr)
	if err != nil {
		Error(w, apierror.BadRequest("Invalid entry ID"))
		return
	}

	entry, err := h.queries.GetJournalEntry(ctx, sqlc.GetJournalEntryParams{
		Guid:   guid,
		UserID: userID,
	})
	if err != nil {
		Error(w, apierror.NotFound("Journal entry"))
		return
	}

	OK(w, toJournalEntry(entry))
}

// CreateEntryRequest represents the request body for creating an entry.
type CreateEntryRequest struct {
	EntryType string         `json:"entry_type"`
	Content   string         `json:"content"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// CreateEntry handles POST /api/v1/journal/entries
func (h *JournalHandler) CreateEntry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := auth.UserIDFromContext(ctx)

	var req CreateEntryRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, err)
		return
	}

	entryType := model.JournalEntryType(req.EntryType)
	if !entryType.Valid() {
		Error(w, apierror.BadRequest("Invalid entry_type"))
		return
	}

	if req.Content == "" {
		Error(w, apierror.BadRequest("Content is required"))
		return
	}

	metadata, _ := marshalMetadata(req.Metadata)

	entry, err := h.queries.CreateJournalEntry(ctx, sqlc.CreateJournalEntryParams{
		UserID:    userID,
		EntryType: req.EntryType,
		Content:   req.Content,
		Metadata:  metadata,
		VectorID:  pgtype.UUID{}, // Empty UUID (not valid) - TODO: Generate embedding
	})
	if err != nil {
		Error(w, apierror.InternalError("Failed to create journal entry").WithError(err))
		return
	}

	Created(w, toJournalEntry(entry))
}

// UpdateEntryRequest represents the request body for updating an entry.
type UpdateEntryRequest struct {
	Content  string         `json:"content"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// UpdateEntry handles PUT /api/v1/journal/entries/{id}
func (h *JournalHandler) UpdateEntry(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := auth.UserIDFromContext(ctx)
	idStr := chi.URLParam(r, "id")

	guid, err := uuid.Parse(idStr)
	if err != nil {
		Error(w, apierror.BadRequest("Invalid entry ID"))
		return
	}

	var req UpdateEntryRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, err)
		return
	}

	if req.Content == "" {
		Error(w, apierror.BadRequest("Content is required"))
		return
	}

	metadata, _ := marshalMetadata(req.Metadata)

	entry, err := h.queries.UpdateJournalEntry(ctx, sqlc.UpdateJournalEntryParams{
		Guid:     guid,
		UserID:   userID,
		Content:  req.Content,
		Metadata: metadata,
	})
	if err != nil {
		Error(w, apierror.NotFound("Journal entry"))
		return
	}

	OK(w, toJournalEntry(entry))
}

// SearchEntriesRequest represents a full-text search request.
type SearchEntriesRequest struct {
	Query string `json:"query"`
	Limit int    `json:"limit,omitempty"`
}

// SearchEntries handles POST /api/v1/journal/search
func (h *JournalHandler) SearchEntries(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := auth.UserIDFromContext(ctx)

	var req SearchEntriesRequest
	if err := DecodeJSON(r, &req); err != nil {
		Error(w, err)
		return
	}

	if req.Query == "" {
		Error(w, apierror.BadRequest("Query is required"))
		return
	}

	limit := 20
	if req.Limit > 0 && req.Limit <= 100 {
		limit = req.Limit
	}

	entries, err := h.queries.SearchJournalFullText(ctx, sqlc.SearchJournalFullTextParams{
		UserID:         userID,
		PlaintoTsquery: req.Query,
		Limit:          int32(limit),
	})
	if err != nil {
		Error(w, apierror.InternalError("Failed to search journal entries").WithError(err))
		return
	}

	result := make([]model.JournalEntry, len(entries))
	for i, e := range entries {
		result[i] = toJournalEntryFromSearch(e)
	}

	OK(w, result)
}

// GetEntriesByType handles GET /api/v1/journal/types/{type}
func (h *JournalHandler) GetEntriesByType(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := auth.UserIDFromContext(ctx)
	entryType := chi.URLParam(r, "type")

	if !model.JournalEntryType(entryType).Valid() {
		Error(w, apierror.BadRequest("Invalid entry type"))
		return
	}

	limit, offset := parsePagination(r)

	entries, err := h.queries.GetJournalEntriesByType(ctx, sqlc.GetJournalEntriesByTypeParams{
		UserID:    userID,
		EntryType: entryType,
		Limit:     int32(limit),
		Offset:    int32(offset),
	})
	if err != nil {
		Error(w, apierror.InternalError("Failed to get journal entries").WithError(err))
		return
	}

	result := make([]model.JournalEntry, len(entries))
	for i, e := range entries {
		result[i] = toJournalEntry(e)
	}

	OK(w, result)
}

// GetRecentDecisions handles GET /api/v1/decisions
func (h *JournalHandler) GetRecentDecisions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := auth.UserIDFromContext(ctx)

	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
	}

	entries, err := h.queries.GetRecentDecisions(ctx, sqlc.GetRecentDecisionsParams{
		UserID: userID,
		Limit:  int32(limit),
	})
	if err != nil {
		Error(w, apierror.InternalError("Failed to get decisions").WithError(err))
		return
	}

	result := make([]model.JournalEntry, len(entries))
	for i, e := range entries {
		result[i] = toJournalEntry(e)
	}

	OK(w, result)
}

// GetRecentSolutions handles GET /api/v1/solutions
func (h *JournalHandler) GetRecentSolutions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	userID := auth.UserIDFromContext(ctx)

	limit := 20
	if l := r.URL.Query().Get("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
	}

	entries, err := h.queries.GetRecentSolutions(ctx, sqlc.GetRecentSolutionsParams{
		UserID: userID,
		Limit:  int32(limit),
	})
	if err != nil {
		Error(w, apierror.InternalError("Failed to get solutions").WithError(err))
		return
	}

	result := make([]model.JournalEntry, len(entries))
	for i, e := range entries {
		result[i] = toJournalEntry(e)
	}

	OK(w, result)
}

func toJournalEntry(e sqlc.Journal) model.JournalEntry {
	entry := model.JournalEntry{
		ID:         e.ID,
		GUID:       e.Guid,
		UserID:     e.UserID,
		EntryType:  model.JournalEntryType(e.EntryType),
		Content:    e.Content,
		CreatedAt:  e.CreatedAt,
		ModifiedAt: e.ModifiedAt,
	}

	// Convert pgtype.UUID to *uuid.UUID
	if e.VectorID.Valid {
		id := uuid.UUID(e.VectorID.Bytes)
		entry.VectorID = &id
	}
	if e.SupersededBy.Valid {
		id := uuid.UUID(e.SupersededBy.Bytes)
		entry.SupersededBy = &id
	}

	if e.Metadata != nil {
		_ = json.Unmarshal(e.Metadata, &entry.Metadata)
	}

	return entry
}

func toJournalEntryFromSearch(e sqlc.SearchJournalFullTextRow) model.JournalEntry {
	entry := model.JournalEntry{
		ID:         e.ID,
		GUID:       e.Guid,
		UserID:     e.UserID,
		EntryType:  model.JournalEntryType(e.EntryType),
		Content:    e.Content,
		CreatedAt:  e.CreatedAt,
		ModifiedAt: e.ModifiedAt,
	}

	// Convert pgtype.UUID to *uuid.UUID
	if e.VectorID.Valid {
		id := uuid.UUID(e.VectorID.Bytes)
		entry.VectorID = &id
	}
	if e.SupersededBy.Valid {
		id := uuid.UUID(e.SupersededBy.Bytes)
		entry.SupersededBy = &id
	}

	if e.Metadata != nil {
		_ = json.Unmarshal(e.Metadata, &entry.Metadata)
	}

	return entry
}

func parsePagination(r *http.Request) (limit, offset int) {
	limit = 20
	offset = 0

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

	return limit, offset
}

func marshalMetadata(m map[string]any) ([]byte, error) {
	if m == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(m)
}

// Helper for time parsing
func parseTime(s string) time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return t
}
