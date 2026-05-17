package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// OpenAIProvider generates embeddings using any OpenAI-compatible API.
// Works with Together.ai, vLLM, OpenAI, and other compatible services.
type OpenAIProvider struct {
	baseURL    string
	model      string
	apiKey     string
	dimensions int
	maxBatch   int
	client     *http.Client
	name       string
}

type openaiEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type openaiEmbedResponse struct {
	Object string `json:"object"`
	Data   []struct {
		Object    string    `json:"object"`
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// NewOpenAIProvider creates a new OpenAI-compatible embedding provider.
func NewOpenAIProvider(cfg Config, apiKey string) *OpenAIProvider {
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = 32
	}

	return &OpenAIProvider{
		baseURL:    cfg.URL,
		model:      cfg.Model,
		apiKey:     apiKey,
		dimensions: cfg.Dimensions,
		maxBatch:   cfg.MaxBatch,
		name:       "openai",
		client: &http.Client{
			Timeout: 60 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
			},
		},
	}
}

// Embed generates an embedding for a single text.
func (p *OpenAIProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	if text == "" {
		return nil, ErrEmptyInput
	}

	results, err := p.EmbedBatch(ctx, []string{text})
	if err != nil {
		return nil, err
	}

	if len(results) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}

	return results[0], nil
}

// EmbedBatch generates embeddings for multiple texts.
func (p *OpenAIProvider) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	if len(texts) > p.maxBatch {
		return nil, fmt.Errorf("%w: got %d, max %d", ErrBatchTooLarge, len(texts), p.maxBatch)
	}

	reqBody := openaiEmbedRequest{
		Model: p.model,
		Input: texts,
	}

	jsonBody, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.baseURL+"/v1/embeddings", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProviderUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("embedding API returned status %d: %s", resp.StatusCode, string(body))
	}

	var result openaiEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}

	// Ensure embeddings are in correct order
	embeddings := make([][]float32, len(texts))
	for _, item := range result.Data {
		if item.Index < len(embeddings) {
			embeddings[item.Index] = item.Embedding
		}
	}

	// Verify all embeddings were received
	for i, emb := range embeddings {
		if emb == nil {
			return nil, fmt.Errorf("missing embedding at index %d", i)
		}
	}

	return embeddings, nil
}

// Dimensions returns the embedding dimension size.
func (p *OpenAIProvider) Dimensions() int {
	return p.dimensions
}

// Name returns the provider name.
func (p *OpenAIProvider) Name() string {
	return p.name
}

// Close is a no-op for the OpenAI provider.
func (p *OpenAIProvider) Close() error {
	return nil
}
