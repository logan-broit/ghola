package vector

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

func TestConfig(t *testing.T) {
	cfg := Config{
		Host:       "localhost",
		GRPCPort:   6334,
		APIKey:     "secret",
		Collection: "memories",
		Dimensions: 384,
	}

	assert.Equal(t, "localhost", cfg.Host)
	assert.Equal(t, 6334, cfg.GRPCPort)
	assert.Equal(t, "secret", cfg.APIKey)
	assert.Equal(t, "memories", cfg.Collection)
	assert.Equal(t, 384, cfg.Dimensions)
}

func TestPoint(t *testing.T) {
	userID := uuid.New()
	pointID := uuid.New().String()

	point := Point{
		ID:      pointID,
		UserID:  userID,
		BlockID: 42,
		Text:    "test memory",
		Vector:  make([]float32, 384),
	}

	assert.Equal(t, pointID, point.ID)
	assert.Equal(t, userID, point.UserID)
	assert.Equal(t, int64(42), point.BlockID)
	assert.Equal(t, "test memory", point.Text)
	assert.Len(t, point.Vector, 384)
}

func TestSearchResult(t *testing.T) {
	result := SearchResult{
		BlockID: 123,
		Score:   0.95,
		Text:    "matched memory",
	}

	assert.Equal(t, int64(123), result.BlockID)
	assert.Equal(t, float32(0.95), result.Score)
	assert.Equal(t, "matched memory", result.Text)
}

func TestNewClient_InvalidHost(t *testing.T) {
	// Test with completely invalid address (will fail on gRPC dial)
	// Note: NewClient uses grpc.NewClient which may not immediately fail
	// but will fail when trying to actually connect
	cfg := Config{
		Host:       "invalid-host-that-does-not-exist",
		GRPCPort:   6334,
		Collection: "test",
		Dimensions: 384,
	}

	client, err := NewClient(cfg)
	// grpc.NewClient returns a connection that may be in connecting state
	// It won't fail until we try to use it
	if err == nil {
		assert.NotNil(t, client)
		client.Close()
	}
}

func TestClient_Close(t *testing.T) {
	// Test that Close doesn't panic on a valid client
	// We can't easily test without a real Qdrant server
	// This is more of a smoke test
	cfg := Config{
		Host:       "localhost",
		GRPCPort:   6334,
		Collection: "test",
		Dimensions: 384,
	}

	client, err := NewClient(cfg)
	if err != nil {
		t.Skip("Skipping: could not create client (Qdrant may not be running)")
	}

	// Close should not panic
	err = client.Close()
	assert.NoError(t, err)
}

func TestPointVectorDimensions(t *testing.T) {
	tests := []struct {
		name       string
		dimensions int
	}{
		{"small", 128},
		{"medium", 384},
		{"large", 1536},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			point := Point{
				ID:      uuid.New().String(),
				UserID:  uuid.New(),
				BlockID: 1,
				Text:    "test",
				Vector:  make([]float32, tc.dimensions),
			}
			assert.Len(t, point.Vector, tc.dimensions)
		})
	}
}

func TestSearchResultOrdering(t *testing.T) {
	results := []SearchResult{
		{BlockID: 1, Score: 0.95, Text: "best match"},
		{BlockID: 2, Score: 0.85, Text: "good match"},
		{BlockID: 3, Score: 0.75, Text: "okay match"},
	}

	// Verify results are in descending score order
	for i := 1; i < len(results); i++ {
		assert.GreaterOrEqual(t, results[i-1].Score, results[i].Score)
	}
}

func TestPointWithMetadata(t *testing.T) {
	userID := uuid.New()

	point := Point{
		ID:      uuid.New().String(),
		UserID:  userID,
		BlockID: 100,
		Text:    "[kubernetes,deployment] Use rolling updates for zero-downtime deployments",
		Vector:  make([]float32, 384),
	}

	// Verify text with tags is preserved
	assert.Contains(t, point.Text, "[kubernetes,deployment]")
	assert.Contains(t, point.Text, "rolling updates")
}

func TestEmptyVector(t *testing.T) {
	point := Point{
		ID:      uuid.New().String(),
		UserID:  uuid.New(),
		BlockID: 1,
		Text:    "test",
		Vector:  []float32{},
	}

	assert.Empty(t, point.Vector)
}

func TestSearchResultWithZeroScore(t *testing.T) {
	result := SearchResult{
		BlockID: 1,
		Score:   0.0,
		Text:    "no similarity",
	}

	assert.Equal(t, float32(0.0), result.Score)
}

func TestSearchResultWithPerfectScore(t *testing.T) {
	result := SearchResult{
		BlockID: 1,
		Score:   1.0,
		Text:    "exact match",
	}

	assert.Equal(t, float32(1.0), result.Score)
}
