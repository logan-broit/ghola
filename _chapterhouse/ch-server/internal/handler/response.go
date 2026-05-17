package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/thinkwright/chapterhouse/ch-server/internal/middleware"
	"github.com/thinkwright/chapterhouse/ch-server/pkg/apierror"
)

// Response helpers for consistent JSON responses.

// JSON writes a JSON response with the given status code.
func JSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if data != nil {
		_ = json.NewEncoder(w).Encode(data)
	}
}

// OK writes a 200 OK response with JSON data.
func OK(w http.ResponseWriter, data any) {
	JSON(w, http.StatusOK, data)
}

// Created writes a 201 Created response with JSON data.
func Created(w http.ResponseWriter, data any) {
	JSON(w, http.StatusCreated, data)
}

// NoContent writes a 204 No Content response.
func NoContent(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNoContent)
}

// Error writes an error response. Logs the underlying error (for
// debugging) at the level appropriate to the status — 5xx Error, 4xx
// Warn — so validation chatter doesn't page anyone. Pulls request_id
// from r's context so middleware-attached attrs flow into the log line.
//
// Callers pass the original *http.Request so the request_id surfaces
// in the JSON-log line alongside the access-log entry from
// middleware.LoggingMiddleware. Pass r=nil only from non-request paths
// (workers, startup), and the request_id field is omitted.
func Error(w http.ResponseWriter, r *http.Request, err error) {
	apiErr := apierror.FromError(err)
	if apiErr.Err != nil {
		attrs := []slog.Attr{
			slog.String("code", apiErr.Code),
			slog.String("message", apiErr.Message),
			slog.String("error", apiErr.Err.Error()),
		}
		if r != nil {
			if id := middleware.RequestID(r.Context()); id != "" {
				attrs = append(attrs, slog.String("request_id", id))
			}
		}
		lvl := slog.LevelError
		if apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 {
			lvl = slog.LevelWarn
		}
		var ctx context.Context = context.Background()
		if r != nil {
			ctx = r.Context()
		}
		slog.LogAttrs(ctx, lvl, "API error", attrs...)
	}
	apiErr.WriteJSON(w)
}

// DecodeJSON decodes JSON from the request body into the given struct.
func DecodeJSON(r *http.Request, v any) error {
	if r.Body == nil {
		return apierror.BadRequest("Request body is required")
	}

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(v); err != nil {
		return apierror.BadRequest("Invalid JSON: " + err.Error())
	}

	return nil
}

// dbError converts a database error into an appropriate API error.
// If the error is pgx.ErrNoRows, returns a NotFound error for the given resource.
// Otherwise wraps it as an InternalError with the given message.
func dbError(err error, resource, action string) *apierror.Error {
	if errors.Is(err, pgx.ErrNoRows) {
		return apierror.NotFound(resource)
	}
	return apierror.InternalError("Failed to " + action).WithError(err)
}

// PaginatedResponse wraps paginated data with metadata.
type PaginatedResponse[T any] struct {
	Data       []T `json:"data"`
	Pagination struct {
		Offset int `json:"offset"`
		Limit  int `json:"limit"`
		Total  int `json:"total,omitempty"`
	} `json:"pagination"`
}

// NewPaginatedResponse creates a paginated response.
func NewPaginatedResponse[T any](data []T, offset, limit, total int) PaginatedResponse[T] {
	resp := PaginatedResponse[T]{
		Data: data,
	}
	resp.Pagination.Offset = offset
	resp.Pagination.Limit = limit
	resp.Pagination.Total = total
	return resp
}
