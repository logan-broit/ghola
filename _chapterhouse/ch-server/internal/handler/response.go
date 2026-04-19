package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5"
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

// Error writes an error response.
func Error(w http.ResponseWriter, err error) {
	apiErr := apierror.FromError(err)
	// Log the underlying error for debugging
	if apiErr.Err != nil {
		slog.Error("API error",
			slog.String("code", apiErr.Code),
			slog.String("message", apiErr.Message),
			slog.String("error", apiErr.Err.Error()),
		)
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
