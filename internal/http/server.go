// Package http wraps core.Core as HTTP/JSON on localhost:7421. The
// same Core is also wrapped as MCP in cmd/ghola-mcp — two protocol
// skins over one behavioral surface.
package http

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"

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

// Handler returns the mux wrapped in middleware — loopback guard,
// recover, access log.
func (s *Server) Handler() http.Handler {
	return s.recover(s.loopback(s.mux))
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
	s.mux.HandleFunc("POST /v1/consolidate", s.consolidate)
	s.mux.HandleFunc("POST /v1/feedback", s.feedback)
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

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) handleErr(w http.ResponseWriter, r *http.Request, err error) {
	if err == nil {
		return
	}
	// Input-validation errors from core carry "required" / "must" in
	// their text. Everything else is an internal error.
	status := http.StatusInternalServerError
	msg := err.Error()
	if strings.Contains(msg, "required") ||
		strings.Contains(msg, "must") ||
		strings.Contains(msg, "evidence") {
		status = http.StatusBadRequest
	}
	s.logger.Warn("request failed",
		"path", r.URL.Path,
		"status", status,
		"err", msg,
	)
	writeError(w, status, msg)
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
	ev, err := s.core.Record(r.Context(), in)
	if err != nil {
		s.handleErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"event": ev})
}

func (s *Server) branch(w http.ResponseWriter, r *http.Request) {
	var in core.RecordInput
	if err := decode(r, &in); err != nil {
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
	id, err := s.core.Share(r.Context(), in)
	if err != nil {
		s.handleErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": id})
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

func (s *Server) feedback(w http.ResponseWriter, r *http.Request) {
	var req struct {
		MnemeID  string  `json:"mneme_id"`
		Evidence float64 `json:"evidence"`
	}
	if err := decode(r, &req); err != nil {
		s.handleErr(w, r, err)
		return
	}
	conf, err := s.core.Feedback(r.Context(), req.MnemeID, req.Evidence)
	if err != nil {
		s.handleErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mneme_id":   req.MnemeID,
		"confidence": conf,
	})
}

// Errors is exported for tests that want to assert canonical error
// propagation.
var ErrUnimplemented = errors.New("unimplemented")
