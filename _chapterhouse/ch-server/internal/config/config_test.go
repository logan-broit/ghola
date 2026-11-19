package config

import "testing"

func TestLoadExplicitEmptyEmbeddingURL(t *testing.T) {
	t.Setenv("AUTH_PROVIDER", "default")
	t.Setenv("EMBEDDING_URL", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Embedding.URL != "" {
		t.Fatalf("embedding URL = %q; explicit empty must disable the provider", cfg.Embedding.URL)
	}
}
