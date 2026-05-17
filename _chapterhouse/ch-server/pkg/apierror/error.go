package apierror

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

// Error represents a structured API error.
type Error struct {
	Code       string            `json:"code"`
	Message    string            `json:"message"`
	Details    map[string]string `json:"details,omitempty"`
	StatusCode int               `json:"-"`
	Err        error             `json:"-"`
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s (%v)", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error {
	return e.Err
}

// WithDetail adds a detail to the error.
func (e *Error) WithDetail(key, value string) *Error {
	if e.Details == nil {
		e.Details = make(map[string]string)
	}
	e.Details[key] = value
	return e
}

// WithError wraps an underlying error.
func (e *Error) WithError(err error) *Error {
	e.Err = err
	return e
}

// MarshalJSON implements json.Marshaler.
func (e *Error) MarshalJSON() ([]byte, error) {
	type alias Error
	return json.Marshal(&struct {
		*alias
	}{
		alias: (*alias)(e),
	})
}

// WriteJSON writes the error as JSON to the response.
func (e *Error) WriteJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.StatusCode)
	_ = json.NewEncoder(w).Encode(e)
}

// Common error constructors

// BadRequest creates a 400 Bad Request error.
func BadRequest(message string) *Error {
	return &Error{
		Code:       "BAD_REQUEST",
		Message:    message,
		StatusCode: http.StatusBadRequest,
	}
}

// Unauthorized creates a 401 Unauthorized error.
func Unauthorized(message string) *Error {
	return &Error{
		Code:       "UNAUTHORIZED",
		Message:    message,
		StatusCode: http.StatusUnauthorized,
	}
}

// Forbidden creates a 403 Forbidden error.
func Forbidden(message string) *Error {
	return &Error{
		Code:       "FORBIDDEN",
		Message:    message,
		StatusCode: http.StatusForbidden,
	}
}

// NotFound creates a 404 Not Found error.
func NotFound(resource string) *Error {
	return &Error{
		Code:       "NOT_FOUND",
		Message:    fmt.Sprintf("%s not found", resource),
		StatusCode: http.StatusNotFound,
	}
}

// Conflict creates a 409 Conflict error.
func Conflict(message string) *Error {
	return &Error{
		Code:       "CONFLICT",
		Message:    message,
		StatusCode: http.StatusConflict,
	}
}

// UnprocessableEntity creates a 422 Unprocessable Entity error.
func UnprocessableEntity(message string) *Error {
	return &Error{
		Code:       "UNPROCESSABLE_ENTITY",
		Message:    message,
		StatusCode: http.StatusUnprocessableEntity,
	}
}

// TooManyRequests creates a 429 Too Many Requests error.
func TooManyRequests(message string) *Error {
	return &Error{
		Code:       "TOO_MANY_REQUESTS",
		Message:    message,
		StatusCode: http.StatusTooManyRequests,
	}
}

// InternalError creates a 500 Internal Server Error.
func InternalError(message string) *Error {
	return &Error{
		Code:       "INTERNAL_ERROR",
		Message:    message,
		StatusCode: http.StatusInternalServerError,
	}
}

// ServiceUnavailable creates a 503 Service Unavailable error.
func ServiceUnavailable(message string) *Error {
	return &Error{
		Code:       "SERVICE_UNAVAILABLE",
		Message:    message,
		StatusCode: http.StatusServiceUnavailable,
	}
}

// FromError attempts to convert an error to an *Error.
// If the error is already an *Error, it returns it directly.
// Otherwise, it wraps it in an InternalError.
func FromError(err error) *Error {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return InternalError("An unexpected error occurred").WithError(err)
}

// IsNotFound returns true if the error is a NotFound error.
func IsNotFound(err error) bool {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusNotFound
	}
	return false
}
