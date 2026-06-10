package embedding

import (
	"context"
	"fmt"
	"time"

	ghola "github.com/logan-broit/ghola/pkg/embedding"
)

// OpenAIProvider generates embeddings using any OpenAI-compatible API.
// Works with Together.ai, vLLM, OpenAI, and other compatible services.
//
// It is a thin adapter over the canonical github.com/logan-broit/ghola
// pkg/embedding client (shared with the ghola service). The adapter
// owns ch-server's caller contracts — the ErrEmptyInput / ErrBatchTooLarge
// sentinels and the maxBatch hard cap — which the shared client
// deliberately does not impose; everything below those guards delegates
// to the shared client.
//
// The old provider's custom transport tunings (MaxIdleConnsPerHost,
// IdleConnTimeout, dial timeouts) were dropped in favor of the shared
// client's default transport; revisit via a Config transport hook if pool
// tuning becomes necessary.
type OpenAIProvider struct {
	client     *ghola.Client
	dimensions int
	maxBatch   int
	name       string
}

// NewOpenAIProvider creates a new OpenAI-compatible embedding provider.
func NewOpenAIProvider(cfg Config, apiKey string) *OpenAIProvider {
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = 32
	}

	return &OpenAIProvider{
		// Timeout 60s + Retries 1 preserve this provider's prior effective
		// behavior: it hard-coded a 60s http.Client and made exactly one
		// request (EmbeddingConfig.Timeout never reached it, and there was
		// no retry loop). MaxBatch is mirrored into the shared client's
		// chunk size, but the adapter's own maxBatch cap below rejects
		// oversize batches before any chunking can occur — so the shared
		// client sees only single-chunk requests.
		client: ghola.New(ghola.Config{
			BaseURL:    cfg.URL,
			Model:      cfg.Model,
			APIKey:     apiKey,
			Dimensions: cfg.Dimensions,
			MaxBatch:   cfg.MaxBatch,
			Timeout:    60 * time.Second,
			Retries:    1,
		}),
		dimensions: cfg.Dimensions,
		maxBatch:   cfg.MaxBatch,
		name:       "openai",
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

	return p.client.EmbedBatch(ctx, texts)
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
