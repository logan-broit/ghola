package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/thinkwright/chapterhouse/ch-server/internal/auth"
	"github.com/thinkwright/chapterhouse/ch-server/pkg/apierror"

	"github.com/google/uuid"
)

// StdioHandler handles MCP over stdio (for direct process invocation).
type StdioHandler struct {
	server  *Server
	authCtx *auth.Context
}

func NewStdioHandler(server *Server, authCtx *auth.Context) *StdioHandler {
	authCtx.SessionID = uuid.New() // process-lifetime session
	return &StdioHandler{
		server:  server,
		authCtx: authCtx,
	}
}

func (h *StdioHandler) Run(reader io.Reader, writer io.Writer) {
	scanner := bufio.NewScanner(reader)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		var req Request
		if err := json.Unmarshal([]byte(line), &req); err != nil {
			resp := Response{
				JSONRPC: "2.0",
				ID:      nil,
				Error:   &Error{Code: -32700, Message: "Parse error"},
			}
			data, _ := json.Marshal(resp)
			fmt.Fprintln(writer, string(data))
			continue
		}

		resp := h.server.HandleRequest(h.authCtx, req)

		// Empty response means notification (no response needed)
		if resp.ID == nil && resp.Result == nil && resp.Error == nil {
			continue
		}

		data, _ := json.Marshal(resp)
		fmt.Fprintln(writer, string(data))
	}
}

// StreamableHTTPHandler implements the MCP Streamable HTTP transport (2025-06-18 spec).
// Manages per-session state with automatic cleanup.
type StreamableHTTPHandler struct {
	server       *Server
	authProvider auth.Provider
	logger       *slog.Logger
	sessions     map[string]*httpSession
	mu           sync.RWMutex
	done         chan struct{}
}

type httpSession struct {
	authCtx   *auth.Context
	createdAt time.Time
}

func NewStreamableHTTPHandler(server *Server, authProvider auth.Provider, logger *slog.Logger) *StreamableHTTPHandler {
	h := &StreamableHTTPHandler{
		server:       server,
		authProvider: authProvider,
		logger:       logger,
		sessions:     make(map[string]*httpSession),
		done:         make(chan struct{}),
	}
	go h.cleanupSessions()
	return h
}

// Close stops the session cleanup goroutine. Call during graceful shutdown.
func (h *StreamableHTTPHandler) Close() {
	close(h.done)
}

func (h *StreamableHTTPHandler) cleanupSessions() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-h.done:
			return
		case <-ticker.C:
			h.mu.Lock()
			now := time.Now()
			for id, sess := range h.sessions {
				if now.Sub(sess.createdAt) > 30*time.Minute {
					delete(h.sessions, id)
				}
			}
			h.mu.Unlock()
		}
	}
}

func (h *StreamableHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS is handled by the router-level middleware; no hardcoded headers here.
	switch r.Method {
	case http.MethodPost:
		h.handlePost(w, r)
	case http.MethodGet:
		h.handleGet(w, r)
	case http.MethodDelete:
		h.handleDelete(w, r)
	default:
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *StreamableHTTPHandler) handlePost(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendJSONError(w, -32700, "Parse error", nil)
		return
	}

	if req.Method == "initialize" {
		h.handleInitialize(w, r, req)
		return
	}

	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		h.sendJSONError(w, -32600, "Missing Mcp-Session-Id header", req.ID)
		return
	}

	h.mu.RLock()
	session, exists := h.sessions[sessionID]
	h.mu.RUnlock()

	if !exists {
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}

	resp := h.server.HandleRequest(session.authCtx, req)

	if resp.ID == nil && resp.Result == nil && resp.Error == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *StreamableHTTPHandler) handleInitialize(w http.ResponseWriter, r *http.Request, req Request) {
	authCtx, err := h.authProvider.Authenticate(r)
	if err != nil {
		apiErr := apierror.Unauthorized("API key authentication required. Provide a valid API key in the Authorization header: 'Bearer ch_k1_...'").
			WithDetail("hint", "Create an API key via the admin console at /admin/keys")
		apiErr.WriteJSON(w)
		return
	}

	authCtx.IPAddress = r.RemoteAddr
	authCtx.UserAgent = r.Header.Get("User-Agent")

	sessionUUID := uuid.New()
	authCtx.SessionID = sessionUUID
	sessionID := sessionUUID.String()
	h.mu.Lock()
	h.sessions[sessionID] = &httpSession{
		authCtx:   authCtx,
		createdAt: time.Now(),
	}
	h.mu.Unlock()

	h.logger.Info("HTTP session created",
		slog.String("session_prefix", sessionID[:8]),
		slog.String("user", authCtx.UserID.String()),
	)

	resp := h.server.HandleRequest(authCtx, req)

	w.Header().Set("Mcp-Session-Id", sessionID)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *StreamableHTTPHandler) handleGet(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		http.Error(w, "Missing Mcp-Session-Id header", http.StatusBadRequest)
		return
	}

	h.mu.RLock()
	session, exists := h.sessions[sessionID]
	h.mu.RUnlock()

	if !exists {
		http.Error(w, "Session not found", http.StatusNotFound)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	blocks, err := h.server.queries.GetCurrentMemoryBlocks(auth.WithContext(ctx, session.authCtx), session.authCtx.UserID)
	if err == nil && len(blocks) > 0 {
		limit := 20
		if len(blocks) < limit {
			limit = len(blocks)
		}

		var contextLines []string
		for i := 0; i < limit; i++ {
			if blocks[i].Value.Valid {
				contextLines = append(contextLines, fmt.Sprintf("- %s", blocks[i].Value.String))
			}
		}

		contextData := map[string]any{
			"type":     "session_context",
			"message":  "Previously stored memories for this user:",
			"memories": contextLines,
			"count":    len(blocks),
		}

		data, _ := json.Marshal(contextData)
		fmt.Fprintf(w, "event: context\ndata: %s\n\n", string(data))
		flusher.Flush()
	}

	select {
	case <-r.Context().Done():
	case <-time.After(30 * time.Second):
	}
}

func (h *StreamableHTTPHandler) handleDelete(w http.ResponseWriter, r *http.Request) {
	sessionID := r.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		http.Error(w, "Missing Mcp-Session-Id header", http.StatusBadRequest)
		return
	}

	// Authenticate the delete request to prevent unauthorized session termination.
	authCtx, err := h.authProvider.Authenticate(r)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	h.mu.Lock()
	session, exists := h.sessions[sessionID]
	if exists && session.authCtx.UserID != authCtx.UserID {
		h.mu.Unlock()
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	delete(h.sessions, sessionID)
	h.mu.Unlock()

	h.logger.Info("HTTP session deleted",
		slog.String("session_prefix", sessionID[:8]),
		slog.String("user", authCtx.UserID.String()),
	)
	w.WriteHeader(http.StatusOK)
}

func (h *StreamableHTTPHandler) sendJSONError(w http.ResponseWriter, code int, message string, id any) {
	resp := Response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &Error{Code: code, Message: message},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(resp)
}

// StatelessHTTPHandler authenticates on every request without session management.
// Compatible with Claude Code's `-t http` transport.
type StatelessHTTPHandler struct {
	server       *Server
	authProvider auth.Provider
	logger       *slog.Logger
}

func NewStatelessHTTPHandler(server *Server, authProvider auth.Provider, logger *slog.Logger) *StatelessHTTPHandler {
	return &StatelessHTTPHandler{
		server:       server,
		authProvider: authProvider,
		logger:       logger,
	}
}

func (h *StatelessHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// CORS is handled by the router-level middleware; no hardcoded headers here.
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	authCtx, err := h.authProvider.Authenticate(r)
	if err != nil {
		apiErr := apierror.Unauthorized("API key authentication required. Provide a valid API key in the Authorization header: 'Bearer ch_k1_...'").
			WithDetail("hint", "Create an API key via the admin console at /admin/keys")
		apiErr.WriteJSON(w)
		return
	}

	authCtx.IPAddress = r.RemoteAddr
	authCtx.UserAgent = r.Header.Get("User-Agent")

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendJSONError(w, -32700, "Parse error", nil)
		return
	}

	h.logger.Debug("MCP request received",
		slog.String("method", req.Method),
		slog.String("user", authCtx.UserID.String()),
	)

	resp := h.server.HandleRequest(authCtx, req)

	if resp.ID == nil && resp.Result == nil && resp.Error == nil {
		w.WriteHeader(http.StatusAccepted)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (h *StatelessHTTPHandler) sendJSONError(w http.ResponseWriter, code int, message string, id any) {
	resp := Response{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &Error{Code: code, Message: message},
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	json.NewEncoder(w).Encode(resp)
}
