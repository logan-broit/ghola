package embedding

import (
	"context"
	"errors"
)

var (
	ErrProviderUnavailable = errors.New("embedding provider unavailable")
	ErrEmptyInput          = errors.New("empty input text")
	ErrBatchTooLarge       = errors.New("batch size exceeds limit")
)

// Provider defines the interface for embedding generation.
type Provider interface {
	// Embed generates an embedding for a single text.
	Embed(ctx context.Context, text string) ([]float32, error)

	// EmbedBatch generates embeddings for multiple texts.
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)

	// Dimensions returns the embedding dimension size.
	Dimensions() int

	// Name returns the provider name.
	Name() string
}

// Config holds common embedding provider configuration.
type Config struct {
	URL         string
	Model       string
	Dimensions  int
	Concurrency int
	MaxBatch    int
}
