// Package http wraps core.Core as HTTP/JSON on localhost:7421. The
// same Core is also wrapped as MCP in cmd/ghola-mcp — two protocol
// skins over one behavioral surface.
package http

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"github.com/google/uuid"

	"github.com/logan-broit/ghola/internal/apierror"
	"github.com/logan-broit/ghola/internal/chapterhouse"
	"github.com/logan-broit/ghola/internal/core"
)

// Server exposes Core's 12 canonical operations over HTTP/JSON.
type Server struct {
	core   *core.Core
	mux    *http.ServeMux
	logger *slog.Logger
	// LoopbackOnly rejects non-127.0.0.1 / ::1 connections when true.
	// Tests override via NewTestServer.
	LoopbackOnly bool
	// defaultUserID is applied when a request omits user_id. Sourced
	// from AUTH_DEFAULT_USER at startup; empty means "no fallback —
	// every request must supply user_id."
	defaultUserID string
}

// SetDefaultUserID configures the fallback applied when a request's
// user_id field is empty. The argument must parse as a UUID; pass
// empty to disable the fallback. The typical caller reads
// AUTH_DEFAULT_USER from env at startup and forwards it here, which
// mirrors chapterhouse's auth-default config so the whole stack
// shares one variable.
func (s *Server) SetDefaultUserID(id string) error {
	if id != "" {
		if _, err := uuid.Parse(id); err != nil {
			return fmt.Errorf("default user_id must be a UUID: %w", err)
		}
	}
	s.defaultUserID = id
	return nil
}

// HTTP-layer validation sentinels. Wrap core.ErrValidation so the
// boundary classifier (handleErr) maps them to 400 via errors.Is.
var (
	errInvalidUserID  = fmt.Errorf("%w: user_id must be a UUID; omit to use AUTH_DEFAULT_USER", core.ErrValidation)
	errMissingUserIDHTTP = fmt.Errorf("%w: user_id required", core.ErrValidation)
)

// resolveUserID enforces the user_id ingress contract: a valid UUID
// is left in place, an empty value falls back to defaultUserID (or
// errors if no default is set), and a non-UUID value is rejected. The
// resolved value is written back through the pointer so handlers
// pass a real UUID downstream — chapterhouse rejects non-UUIDs at
// /v1/episodic/query and we want the 400 to surface here, where the
// caller can act on it, not as an opaque later failure.
func (s *Server) resolveUserID(id *string) error {
	if *id == "" {
		if s.defaultUserID == "" {
			return errMissingUserIDHTTP
		}
		*id = s.defaultUserID
		return nil
	}
	if _, err := uuid.Parse(*id); err != nil {
		return errInvalidUserID
	}
	return nil
}

// NewServer builds the server with a fresh mux. Handler() returns the
// net/http.Handler ready to Listen.
func NewServer(c *core.Core, logger *slog.Logger) *Server {
	s := &Server{
		core:         c,
		mux:          http.NewServeMux(),
		logger:       logger,
		LoopbackOnly: true,
	}
	s.routes()
	return s
}

// Handler returns the mux wrapped in middleware. Order (outermost
// first): request_id -> access_log -> recover -> loopback -> mux.
// request_id runs first so every downstream log line + response
// header carries the same id; access_log wraps next so even panics
// (handled in recover) are observed at the boundary.
func (s *Server) Handler() http.Handler {
	return s.requestID(s.accessLog(s.recover(s.loopback(s.mux))))
}

func (s *Server) routes() {
	s.mux.HandleFunc("POST /v1/session_start", s.sessionStart)
	s.mux.HandleFunc("POST /v1/session_end", s.sessionEnd)
	s.mux.HandleFunc("POST /v1/list_sessions", s.listSessions)
	s.mux.HandleFunc("POST /v1/record", s.record)
	s.mux.HandleFunc("POST /v1/branch", s.branch)
	s.mux.HandleFunc("POST /v1/bookmark", s.bookmark)
	s.mux.HandleFunc("POST /v1/navigate", s.navigate)
	s.mux.HandleFunc("POST /v1/recall", s.recall)
	s.mux.HandleFunc("POST /v1/forget", s.forget)
	s.mux.HandleFunc("POST /v1/share", s.share)
	s.mux.HandleFunc("POST /v1/session_workspace", s.sessionWorkspace)
	s.mux.HandleFunc("POST /v1/consolidate", s.consolidate)
	s.mux.HandleFunc("POST /v1/semantic/consolidate", s.consolidateWorkspace)
	s.mux.HandleFunc("GET /health", s.health)
}

// ---------------------------------------------------------------------
// Middleware
// ---------------------------------------------------------------------

func (s *Server) loopback(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.LoopbackOnly {
			next.ServeHTTP(w, r)
			return
		}
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		if host == "127.0.0.1" || host == "::1" || host == "localhost" {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "ghola listens on loopback only", http.StatusForbidden)
	})
}

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic", "path", r.URL.Path, "recover", rec)
				writeError(w, http.StatusInternalServerError, "panic")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------
// Request / response helpers
// ---------------------------------------------------------------------

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError emits the apierror JSON shape ({code, message, details})
// with the given status. Status-only callers (e.g. validation messages
// constructed inline at the HTTP layer) use BAD_REQUEST as the code.
func writeError(w http.ResponseWriter, status int, msg string) {
	apiErr := &apierror.Error{
		Code:       codeForStatus(status),
		Message:    msg,
		StatusCode: status,
	}
	apiErr.WriteJSON(w)
}

// codeForStatus maps an HTTP status to the apierror code string used
// in JSON responses. Mirrors the constructors in package apierror so
// the wire shape stays identical whether the error came from a sentinel
// or an inline writeError(w, 4xx, msg) call.
func codeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "BAD_REQUEST"
	case http.StatusUnauthorized:
		return "UNAUTHORIZED"
	case http.StatusForbidden:
		return "FORBIDDEN"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusConflict:
		return "CONFLICT"
	case http.StatusUnprocessableEntity:
		return "UNPROCESSABLE_ENTITY"
	case http.StatusTooManyRequests:
		return "TOO_MANY_REQUESTS"
	case http.StatusServiceUnavailable:
		return "SERVICE_UNAVAILABLE"
	default:
		return "INTERNAL_ERROR"
	}
}

func (s *Server) handleErr(w http.ResponseWriter, r *http.Request, err error) {
	if err == nil {
		return
	}
	// Classify in priority order: chapterhouse 4xx propagates verbatim
	// (caller corrects the input), then core.ErrValidation -> 400, else
	// 500. errors.Is replaces the prior strings.Contains heuristic that
	// silently misclassified any internal error containing "must" or
	// "required" as a 400.
	status := http.StatusInternalServerError
	var se *chapterhouse.StatusError
	switch {
	case errors.As(err, &se) && se.Status >= 400 && se.Status < 500:
		status = se.Status
	case errors.Is(err, core.ErrValidation):
		status = http.StatusBadRequest
	}
	s.logger.Warn("request failed",
		slog.String("path", r.URL.Path),
		slog.Int("status", status),
		slog.String("error", err.Error()),
	)
	writeError(w, status, err.Error())
}

// ---------------------------------------------------------------------
// Handlers — one per Core method. Keep each body tiny: decode, call,
// encode. Any logic belongs in Core.
// ---------------------------------------------------------------------

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) sessionStart(w http.ResponseWriter, r *http.Request) {
	var in core.SessionStartInput
	if err := decode(r, &in); err != nil {
		s.handleErr(w, r, err)
		return
	}
	if err := s.resolveUserID(&in.UserID); err != nil {
		s.handleErr(w, r, err)
		return
	}
	sess, err := s.core.SessionStart(r.Context(), in)
	if err != nil {
		s.handleErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session": sess})
}

func (s *Server) sessionEnd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := decode(r, &req); err != nil {
		s.handleErr(w, r, err)
		return
	}
	if err := s.core.SessionEnd(r.Context(), req.SessionID); err != nil {
		s.handleErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID string `json:"user_id"`
	}
	if err := decode(r, &req); err != nil {
		s.handleErr(w, r, err)
		return
	}
	if err := s.resolveUserID(&req.UserID); err != nil {
		s.handleErr(w, r, err)
		return
	}
	sessions, err := s.core.ListSessions(r.Context(), req.UserID)
	if err != nil {
		s.handleErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

func (s *Server) record(w http.ResponseWriter, r *http.Request) {
	var in core.RecordInput
	if err := decode(r, &in); err != nil {
		s.handleErr(w, r, err)
		return
	}
	if err := s.resolveUserID(&in.UserID); err != nil {
		s.handleErr(w, r, err)
		return
	}
	// Validate event.type at the HTTP boundary so an unrecognised type
	// surfaces as 400 with the offending value named, not as an opaque
	// 500 from SQLite's CHECK constraint.
	if err := core.ValidateEventType(in.Event.Type); err != nil {
		s.handleErr(w, r, err)
		return
	}
	ev, err := s.core.Record(r.Context(), in)
	if err != nil {
		s.handleErr(w, r, err)
		return
	}
	// Slim the response: the caller supplied the text; the embedding is
	// a server-side artifact and ~20 KB of noise per call when echoed
	// back. Zero it on the response copy — the full event (with
	// embedding) lives in sietch and is returned by recall as needed.
	ev.Embedding = nil
	writeJSON(w, http.StatusOK, map[string]any{"event": ev})
}

func (s *Server) branch(w http.ResponseWriter, r *http.Request) {
	var in core.RecordInput
	if err := decode(r, &in); err != nil {
		s.handleErr(w, r, err)
		return
	}
	if err := s.resolveUserID(&in.UserID); err != nil {
		s.handleErr(w, r, err)
		return
	}
	ev, err := s.core.Branch(r.Context(), in)
	if err != nil {
		s.handleErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": ev})
}

func (s *Server) bookmark(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
		EventID   string `json:"event_id"`
		Label     string `json:"label"`
	}
	if err := decode(r, &req); err != nil {
		s.handleErr(w, r, err)
		return
	}
	if err := s.core.Bookmark(r.Context(), req.SessionID, req.EventID, req.Label); err != nil {
		s.handleErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) navigate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
		EventID   string `json:"event_id"`
	}
	if err := decode(r, &req); err != nil {
		s.handleErr(w, r, err)
		return
	}
	if err := s.core.Navigate(r.Context(), req.SessionID, req.EventID); err != nil {
		s.handleErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) recall(w http.ResponseWriter, r *http.Request) {
	var in core.RecallInput
	if err := decode(r, &in); err != nil {
		s.handleErr(w, r, err)
		return
	}
	if err := s.resolveUserID(&in.UserID); err != nil {
		s.handleErr(w, r, err)
		return
	}
	out, err := s.core.Recall(r.Context(), in)
	if err != nil {
		s.handleErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) forget(w http.ResponseWriter, r *http.Request) {
	var in core.ForgetInput
	if err := decode(r, &in); err != nil {
		s.handleErr(w, r, err)
		return
	}
	if err := s.resolveUserID(&in.UserID); err != nil {
		s.handleErr(w, r, err)
		return
	}
	n, err := s.core.Forget(r.Context(), in)
	if err != nil {
		s.handleErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"forgotten": n})
}

func (s *Server) share(w http.ResponseWriter, r *http.Request) {
	var in core.ShareInput
	if err := decode(r, &in); err != nil {
		s.handleErr(w, r, err)
		return
	}
	if err := s.resolveUserID(&in.UserID); err != nil {
		s.handleErr(w, r, err)
		return
	}
	id, err := s.core.Share(r.Context(), in)
	if err != nil {
		s.handleErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
}

// sessionWorkspace tags an existing session into an additional
// workspace. The 409 from chapterhouse (session not yet consolidated)
// surfaces here as 409 via the *StatusError pass-through in
// handleErr — without that, agents would see opaque 500s and have no
// way to know "consolidate first" is the fix.
func (s *Server) sessionWorkspace(w http.ResponseWriter, r *http.Request) {
	var in core.AddSessionWorkspaceInput
	if err := decode(r, &in); err != nil {
		s.handleErr(w, r, err)
		return
	}
	if err := s.resolveUserID(&in.UserID); err != nil {
		s.handleErr(w, r, err)
		return
	}
	added, err := s.core.ExpandSessionWorkspace(r.Context(), in)
	if err != nil {
		s.handleErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"added": added})
}

func (s *Server) consolidate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := decode(r, &req); err != nil {
		s.handleErr(w, r, err)
		return
	}
	n, err := s.core.Consolidate(r.Context(), req.SessionID)
	if err != nil {
		s.handleErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"flushed": n})
}

// consolidateWorkspace handles the manual trigger for chapterhouse's
// episodic->semantic consolidation batch (cluster + enrich +
// label/digest). Distinct from consolidate above, which flushes one
// session's pending sietch events to episodic — see
// core.ConsolidateWorkspace's doc comment for the seam rationale.
// Synchronous: chapterhouse runs the batch in-process, so this
// request blocks for the run's full duration.
func (s *Server) consolidateWorkspace(w http.ResponseWriter, r *http.Request) {
	var req core.ConsolidateWorkspaceInput
	if err := decode(r, &req); err != nil {
		s.handleErr(w, r, err)
		return
	}
	if err := s.core.ConsolidateWorkspace(r.Context(), req); err != nil {
		s.handleErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// Errors is exported for tests that want to assert canonical error
// propagation.
var ErrUnimplemented = errors.New("unimplemented")
