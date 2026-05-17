// Package apierror provides a structured HTTP error type for the ghola
// daemon. Mirror of _chapterhouse/ch-server/pkg/apierror so both halves
// of the system speak the same JSON error shape: {code, message,
// details}. Both packages stay in lockstep — change one, change both
// (or extract to a shared module via go.work later).
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

func (e *Error) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %s (%v)", e.Code, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

func (e *Error) WithDetail(key, value string) *Error {
	if e.Details == nil {
		e.Details = make(map[string]string)
	}
	e.Details[key] = value
	return e
}

func (e *Error) WithError(err error) *Error {
	e.Err = err
	return e
}

func (e *Error) MarshalJSON() ([]byte, error) {
	type alias Error
	return json.Marshal(&struct{ *alias }{alias: (*alias)(e)})
}

func (e *Error) WriteJSON(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.StatusCode)
	_ = json.NewEncoder(w).Encode(e)
}

// Constructors.

func BadRequest(message string) *Error {
	return &Error{Code: "BAD_REQUEST", Message: message, StatusCode: http.StatusBadRequest}
}

func Unauthorized(message string) *Error {
	return &Error{Code: "UNAUTHORIZED", Message: message, StatusCode: http.StatusUnauthorized}
}

func Forbidden(message string) *Error {
	return &Error{Code: "FORBIDDEN", Message: message, StatusCode: http.StatusForbidden}
}

func NotFound(resource string) *Error {
	return &Error{Code: "NOT_FOUND", Message: fmt.Sprintf("%s not found", resource), StatusCode: http.StatusNotFound}
}

func Conflict(message string) *Error {
	return &Error{Code: "CONFLICT", Message: message, StatusCode: http.StatusConflict}
}

func UnprocessableEntity(message string) *Error {
	return &Error{Code: "UNPROCESSABLE_ENTITY", Message: message, StatusCode: http.StatusUnprocessableEntity}
}

func TooManyRequests(message string) *Error {
	return &Error{Code: "TOO_MANY_REQUESTS", Message: message, StatusCode: http.StatusTooManyRequests}
}

func InternalError(message string) *Error {
	return &Error{Code: "INTERNAL_ERROR", Message: message, StatusCode: http.StatusInternalServerError}
}

func ServiceUnavailable(message string) *Error {
	return &Error{Code: "SERVICE_UNAVAILABLE", Message: message, StatusCode: http.StatusServiceUnavailable}
}

// FromError unwraps to *Error if possible, else wraps as InternalError.
func FromError(err error) *Error {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		return apiErr
	}
	return InternalError("An unexpected error occurred").WithError(err)
}
