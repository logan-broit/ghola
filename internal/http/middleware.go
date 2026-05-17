package http

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"time"

	"log/slog"

	"github.com/google/uuid"
)

// Request-scoped context keys. The empty-struct trick avoids string
// collisions across packages and is the canonical Go pattern.
type requestIDKey struct{}

// RequestIDFromContext returns the request_id attached by the
// request-id middleware. Empty string if no middleware ran.
func RequestIDFromContext(ctx context.Context) string {
	if id, ok := ctx.Value(requestIDKey{}).(string); ok {
		return id
	}
	return ""
}

// requestID is middleware: read X-Request-ID from the caller, else
// mint a UUID. Attach to context and echo on the response so the
// caller can correlate.
func (s *Server) requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = uuid.New().String()
		}
		ctx := context.WithValue(r.Context(), requestIDKey{}, id)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// accessLog is middleware: one structured line per request with
// method, path, status, duration, and request_id. Level rides the
// status — 5xx is Error, 4xx Warn, else Info. Mirrors the chapterhouse
// LoggingMiddleware so operators see a single shape across services.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)
		duration := time.Since(start)

		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", rw.status),
			slog.Duration("duration", duration),
			slog.Int("size", rw.size),
			slog.String("remote", r.RemoteAddr),
			slog.String("request_id", RequestIDFromContext(r.Context())),
		}

		lvl := slog.LevelInfo
		switch {
		case rw.status >= 500:
			lvl = slog.LevelError
		case rw.status >= 400:
			lvl = slog.LevelWarn
		}
		s.logger.LogAttrs(r.Context(), lvl, "http_request", attrs...)
	})
}

// responseWriter wraps http.ResponseWriter to capture status + size
// for the access log. Implements Flusher (SSE) and Hijacker
// (WebSocket) so it stays transparent for any handler that needs them.
type responseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	size        int
}

func (rw *responseWriter) WriteHeader(code int) {
	if rw.wroteHeader {
		return
	}
	rw.status = code
	rw.wroteHeader = true
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *responseWriter) Write(b []byte) (int, error) {
	if !rw.wroteHeader {
		rw.WriteHeader(http.StatusOK)
	}
	n, err := rw.ResponseWriter.Write(b)
	rw.size += n
	return n, err
}

func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := rw.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}
