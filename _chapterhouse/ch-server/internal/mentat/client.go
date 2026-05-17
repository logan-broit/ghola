// Package mentat is the Go HTTP client for the mentat service. It is
// intentionally small: one method per endpoint mentat actually serves.
// Train and Cluster land in PRs 5 and 4 respectively, when their mentat-
// side counterparts go live.
package mentat

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Event is a single workspace event fed to mentat for pooling.
type Event struct {
	Type      string    `json:"type"`
	Embedding []float32 `json:"embedding"`
}

// PoolRequest pools a batch of events into a single workspace embedding.
type PoolRequest struct {
	WorkspaceID uuid.UUID `json:"workspace_id"`
	Events      []Event   `json:"events"`
}

// PoolResponse is the pooled workspace embedding.
type PoolResponse struct {
	Embedding []float32 `json:"embedding"`
}

// PredictRequest predicts the next workspace embedding from a history of
// pooled embeddings.
type PredictRequest struct {
	WorkspaceID uuid.UUID   `json:"workspace_id"`
	History     [][]float32 `json:"history"`
}

// PredictResponse is the predicted next workspace embedding.
type PredictResponse struct {
	Embedding []float32 `json:"embedding"`
}

// HealthResponse describes mentat's runtime state, including which weights
// version is loaded and whether mentat is in cold-start mode.
type HealthResponse struct {
	Status         string  `json:"status"`
	WeightsVersion *string `json:"weights_version"`
	ColdStart      bool    `json:"cold_start"`
	EmbeddingDim   int     `json:"embedding_dim"`
}

// ClusterRequest triggers Stage C HDBSCAN clustering for a workspace.
type ClusterRequest struct {
	WorkspaceID     uuid.UUID `json:"workspace_id"`
	MinClusterSize  int       `json:"min_cluster_size,omitempty"`
}

// ClusterResponse reports the clustering outcome.
type ClusterResponse struct {
	WorkspaceID     uuid.UUID `json:"workspace_id"`
	NSessions       int       `json:"n_sessions"`
	NClusters       int       `json:"n_clusters"`
	NOutliers       int       `json:"n_outliers"`
	UpsertedMnemes  int       `json:"upserted_mnemes"`
}

// Client is an HTTP client for the mentat service.
type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient constructs a mentat client. If h is nil a default client with
// a tight 10s timeout is used; semantic recall calls Pool per-query, so a
// stuck mentat must not wedge user-visible recall.
func NewClient(baseURL string, h *http.Client) *Client {
	if h == nil {
		h = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{baseURL: baseURL, http: h}
}

// Pool calls POST /v1/pool to compress a batch of events into a single
// workspace embedding.
func (c *Client) Pool(ctx context.Context, req PoolRequest) (*PoolResponse, error) {
	if len(req.Events) == 0 {
		return nil, fmt.Errorf("mentat: pool requires at least one event")
	}
	var out PoolResponse
	if err := c.do(ctx, "/v1/pool", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Predict calls POST /v1/predict to forecast the next workspace embedding
// from a history of pooled embeddings.
func (c *Client) Predict(ctx context.Context, req PredictRequest) (*PredictResponse, error) {
	if len(req.History) == 0 {
		return nil, fmt.Errorf("mentat: predict requires non-empty history")
	}
	var out PredictResponse
	if err := c.do(ctx, "/v1/predict", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Cluster calls POST /v1/cluster to run HDBSCAN over the workspace's
// L1 embeddings and upsert mnemes. Returns counts (sessions seen,
// clusters found, outliers, mneme rows touched).
func (c *Client) Cluster(ctx context.Context, req ClusterRequest) (*ClusterResponse, error) {
	var out ClusterResponse
	if err := c.do(ctx, "/v1/cluster", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Health calls GET /v1/health.
func (c *Client) Health(ctx context.Context) (*HealthResponse, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/health", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("mentat: /v1/health: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("mentat: /v1/health: %d: %s", resp.StatusCode, string(buf))
	}
	var out HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("mentat: /v1/health: decode: %w", err)
	}
	return &out, nil
}

func (c *Client) do(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return fmt.Errorf("mentat: %s: marshal: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("mentat: %s: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("mentat: %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("mentat: %s: %d: %s", path, resp.StatusCode, string(buf))
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("mentat: %s: decode: %w", path, err)
	}
	return nil
}
