package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// HealthHandler provides health and readiness endpoints.
type HealthHandler struct {
	db         *pgxpool.Pool
	qdrantURL  string
	httpClient *http.Client

	mu    sync.RWMutex
	ready bool
}

// NewHealthHandler creates a new health handler.
func NewHealthHandler(db *pgxpool.Pool, qdrantURL string) *HealthHandler {
	return &HealthHandler{
		db:        db,
		qdrantURL: qdrantURL,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		ready: false,
	}
}

// SetReady marks the service as ready to receive traffic.
func (h *HealthHandler) SetReady(ready bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ready = ready
}

// IsReady returns whether the service is ready.
func (h *HealthHandler) IsReady() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.ready
}

// HealthResponse represents the health check response.
type HealthResponse struct {
	Status    string            `json:"status"`
	Timestamp string            `json:"timestamp"`
	Checks    map[string]string `json:"checks,omitempty"`
}

// Health handles GET /health - basic liveness check.
func (h *HealthHandler) Health(w http.ResponseWriter, r *http.Request) {
	resp := HealthResponse{
		Status:    "ok",
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// Ready handles GET /ready - readiness check with dependency verification.
func (h *HealthHandler) Ready(w http.ResponseWriter, r *http.Request) {
	if !h.IsReady() {
		http.Error(w, "Service not ready", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	checks := make(map[string]string)
	allHealthy := true

	// Check database
	if err := h.db.Ping(ctx); err != nil {
		checks["database"] = "unhealthy: " + err.Error()
		allHealthy = false
	} else {
		checks["database"] = "healthy"
	}

	// Check Qdrant if configured
	if h.qdrantURL != "" {
		if err := h.checkQdrant(ctx); err != nil {
			checks["qdrant"] = "unhealthy: " + err.Error()
			allHealthy = false
		} else {
			checks["qdrant"] = "healthy"
		}
	}

	status := "ok"
	statusCode := http.StatusOK
	if !allHealthy {
		status = "degraded"
		statusCode = http.StatusServiceUnavailable
	}

	resp := HealthResponse{
		Status:    status,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Checks:    checks,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *HealthHandler) checkQdrant(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.qdrantURL+"/", nil)
	if err != nil {
		return err
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return http.ErrNotSupported
	}

	return nil
}
