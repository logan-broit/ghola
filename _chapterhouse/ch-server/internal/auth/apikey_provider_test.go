package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockAPIKeyQuerier is a mock implementation of APIKeyQuerier
type mockAPIKeyQuerier struct {
	getAPIKeyByHashFn      func(ctx context.Context, keyHash string) (APIKeyResult, error)
	updateAPIKeyLastUsedFn func(ctx context.Context, id uuid.UUID) error
}

func (m *mockAPIKeyQuerier) GetAPIKeyByHash(ctx context.Context, keyHash string) (APIKeyResult, error) {
	if m.getAPIKeyByHashFn != nil {
		return m.getAPIKeyByHashFn(ctx, keyHash)
	}
	return APIKeyResult{}, pgx.ErrNoRows
}

func (m *mockAPIKeyQuerier) UpdateAPIKeyLastUsed(ctx context.Context, id uuid.UUID) error {
	if m.updateAPIKeyLastUsedFn != nil {
		return m.updateAPIKeyLastUsedFn(ctx, id)
	}
	return nil
}

func TestGenerateAPIKey(t *testing.T) {
	plaintext, hash, prefix, err := GenerateAPIKey()

	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(plaintext, APIKeyPrefix))
	assert.Len(t, hash, 64) // SHA-256 produces 64 hex chars
	assert.Len(t, prefix, 8)

	// Verify the hash is consistent
	assert.Equal(t, hash, HashAPIKey(plaintext))

	// Verify the prefix is extracted correctly
	randomPart := strings.TrimPrefix(plaintext, APIKeyPrefix)
	assert.Equal(t, prefix, randomPart[:8])
}

func TestGenerateAPIKey_Uniqueness(t *testing.T) {
	keys := make(map[string]bool)
	for i := 0; i < 100; i++ {
		plaintext, _, _, err := GenerateAPIKey()
		require.NoError(t, err)
		assert.False(t, keys[plaintext], "duplicate key generated")
		keys[plaintext] = true
	}
}

func TestValidateAPIKeyFormat(t *testing.T) {
	tests := []struct {
		name     string
		apiKey   string
		expected bool
	}{
		{
			name:     "valid key",
			apiKey:   "ch_k1_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			expected: true,
		},
		{
			name:     "wrong prefix",
			apiKey:   "ch_k2_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			expected: false,
		},
		{
			name:     "too short",
			apiKey:   "ch_k1_0123456789abcdef",
			expected: false,
		},
		{
			name:     "too long",
			apiKey:   "ch_k1_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef00",
			expected: false,
		},
		{
			name:     "invalid hex",
			apiKey:   "ch_k1_gggggggggggggggggggggggggggggggggggggggggggggggggggggggggggggggg",
			expected: false,
		},
		{
			name:     "empty",
			apiKey:   "",
			expected: false,
		},
		{
			name:     "no prefix",
			apiKey:   "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			expected: false,
		},
		{
			name:     "old prefix is rejected",
			apiKey:   "cnam_k1_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.expected, ValidateAPIKeyFormat(tc.apiKey))
		})
	}
}

func TestHashAPIKey(t *testing.T) {
	apiKey := "ch_k1_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	hash1 := HashAPIKey(apiKey)
	hash2 := HashAPIKey(apiKey)

	// Verify consistent hashing
	assert.Equal(t, hash1, hash2)
	assert.Len(t, hash1, 64) // SHA-256 produces 64 hex chars

	// Verify different keys produce different hashes
	differentKey := "ch_k1_fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
	differentHash := HashAPIKey(differentKey)
	assert.NotEqual(t, hash1, differentHash)
}

func TestAPIKeyProvider_Authenticate_Success(t *testing.T) {
	userID := uuid.New()
	keyID := uuid.New()

	querier := &mockAPIKeyQuerier{
		getAPIKeyByHashFn: func(ctx context.Context, keyHash string) (APIKeyResult, error) {
			return APIKeyResult{
				ID:       keyID,
				UserID:   userID,
				Username: "testuser",
				Email:    "test@example.com",
				IsAdmin:  false,
			}, nil
		},
	}

	provider := NewAPIKeyProvider(querier)

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	apiKey := "ch_k1_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	req.Header.Set("Authorization", "Bearer "+apiKey)

	ctx, err := provider.Authenticate(req)

	require.NoError(t, err)
	assert.Equal(t, userID, ctx.UserID)
	assert.Equal(t, "testuser", ctx.Username)
	assert.Equal(t, "test@example.com", ctx.Email)
	assert.Contains(t, ctx.Roles, "user")
	assert.NotContains(t, ctx.Roles, "admin")
	assert.Equal(t, "api_key", ctx.Claims["auth_method"])
	assert.Equal(t, keyID.String(), ctx.Claims["api_key_id"])
}

func TestAPIKeyProvider_Authenticate_AdminUser(t *testing.T) {
	userID := uuid.New()
	keyID := uuid.New()

	querier := &mockAPIKeyQuerier{
		getAPIKeyByHashFn: func(ctx context.Context, keyHash string) (APIKeyResult, error) {
			return APIKeyResult{
				ID:       keyID,
				UserID:   userID,
				Username: "adminuser",
				Email:    "admin@example.com",
				IsAdmin:  true,
			}, nil
		},
	}

	provider := NewAPIKeyProvider(querier)

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Authorization", "Bearer ch_k1_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	ctx, err := provider.Authenticate(req)

	require.NoError(t, err)
	assert.Contains(t, ctx.Roles, "user")
	assert.Contains(t, ctx.Roles, "admin")
	assert.True(t, ctx.IsAdmin())
}

func TestAPIKeyProvider_Authenticate_ApiKeyHeader(t *testing.T) {
	userID := uuid.New()

	querier := &mockAPIKeyQuerier{
		getAPIKeyByHashFn: func(ctx context.Context, keyHash string) (APIKeyResult, error) {
			return APIKeyResult{
				ID:       uuid.New(),
				UserID:   userID,
				Username: "testuser",
				Email:    "test@example.com",
				IsAdmin:  false,
			}, nil
		},
	}

	provider := NewAPIKeyProvider(querier)

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Authorization", "ApiKey ch_k1_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	ctx, err := provider.Authenticate(req)

	require.NoError(t, err)
	assert.Equal(t, userID, ctx.UserID)
}

func TestAPIKeyProvider_Authenticate_MissingToken(t *testing.T) {
	querier := &mockAPIKeyQuerier{}
	provider := NewAPIKeyProvider(querier)

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	// No Authorization header

	ctx, err := provider.Authenticate(req)

	assert.Nil(t, ctx)
	assert.ErrorIs(t, err, ErrMissingToken)
}

func TestAPIKeyProvider_Authenticate_NonAPIKeyBearer(t *testing.T) {
	querier := &mockAPIKeyQuerier{}
	provider := NewAPIKeyProvider(querier)

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Authorization", "Bearer eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9...")

	ctx, err := provider.Authenticate(req)

	assert.Nil(t, ctx)
	assert.ErrorIs(t, err, ErrMissingToken) // Returns missing token so composite can try next provider
}

func TestAPIKeyProvider_Authenticate_InvalidApiKeyFormat(t *testing.T) {
	querier := &mockAPIKeyQuerier{}
	provider := NewAPIKeyProvider(querier)

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Authorization", "ApiKey invalid_key")

	ctx, err := provider.Authenticate(req)

	assert.Nil(t, ctx)
	assert.ErrorIs(t, err, ErrInvalidAPIKey)
}

func TestAPIKeyProvider_Authenticate_KeyNotFound(t *testing.T) {
	querier := &mockAPIKeyQuerier{
		getAPIKeyByHashFn: func(ctx context.Context, keyHash string) (APIKeyResult, error) {
			return APIKeyResult{}, pgx.ErrNoRows
		},
	}

	provider := NewAPIKeyProvider(querier)

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Authorization", "Bearer ch_k1_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	ctx, err := provider.Authenticate(req)

	assert.Nil(t, ctx)
	assert.ErrorIs(t, err, ErrAPIKeyNotFound)
}

func TestAPIKeyProvider_Authenticate_DatabaseError(t *testing.T) {
	dbError := errors.New("database connection failed")
	querier := &mockAPIKeyQuerier{
		getAPIKeyByHashFn: func(ctx context.Context, keyHash string) (APIKeyResult, error) {
			return APIKeyResult{}, dbError
		},
	}

	provider := NewAPIKeyProvider(querier)

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Authorization", "Bearer ch_k1_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	ctx, err := provider.Authenticate(req)

	assert.Nil(t, ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to look up API key")
}

func TestAPIKeyProvider_Authenticate_UpdatesLastUsed(t *testing.T) {
	keyID := uuid.New()
	updateCalled := make(chan uuid.UUID, 1)

	querier := &mockAPIKeyQuerier{
		getAPIKeyByHashFn: func(ctx context.Context, keyHash string) (APIKeyResult, error) {
			return APIKeyResult{
				ID:       keyID,
				UserID:   uuid.New(),
				Username: "testuser",
				Email:    "test@example.com",
				IsAdmin:  false,
			}, nil
		},
		updateAPIKeyLastUsedFn: func(ctx context.Context, id uuid.UUID) error {
			updateCalled <- id
			return nil
		},
	}

	provider := NewAPIKeyProvider(querier)

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Authorization", "Bearer ch_k1_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	_, err := provider.Authenticate(req)
	require.NoError(t, err)

	// Wait for the async update
	select {
	case id := <-updateCalled:
		assert.Equal(t, keyID, id)
	}
}

func TestExtractAPIKey(t *testing.T) {
	tests := []struct {
		name          string
		authHeader    string
		expectedKey   string
		expectedError error
	}{
		{
			name:        "bearer with api key",
			authHeader:  "Bearer ch_k1_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			expectedKey: "ch_k1_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		{
			name:        "apikey header",
			authHeader:  "ApiKey ch_k1_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			expectedKey: "ch_k1_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
		{
			name:          "bearer with jwt",
			authHeader:    "Bearer eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9...",
			expectedError: ErrMissingToken,
		},
		{
			name:          "basic auth",
			authHeader:    "Basic dXNlcjpwYXNz",
			expectedError: ErrMissingToken,
		},
		{
			name:          "empty header",
			authHeader:    "",
			expectedError: ErrMissingToken,
		},
		{
			name:          "apikey with invalid format",
			authHeader:    "ApiKey invalid-key",
			expectedError: ErrInvalidAPIKey,
		},
		{
			name:          "bearer with old cnam_k1_ prefix is rejected",
			authHeader:    "Bearer cnam_k1_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			expectedError: ErrMissingToken,
		},
		{
			name:          "apikey header with old cnam_k1_ prefix is rejected",
			authHeader:    "ApiKey cnam_k1_0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			expectedError: ErrInvalidAPIKey,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}

			key, err := extractAPIKey(req)

			if tc.expectedError != nil {
				assert.ErrorIs(t, err, tc.expectedError)
				assert.Empty(t, key)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expectedKey, key)
			}
		})
	}
}
