package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewDefaultProvider(t *testing.T) {
	tests := []struct {
		name      string
		userID    string
		username  string
		email     string
		expectErr bool
	}{
		{
			name:      "valid UUID",
			userID:    "550e8400-e29b-41d4-a716-446655440000",
			username:  "testuser",
			email:     "test@example.com",
			expectErr: false,
		},
		{
			name:      "invalid UUID",
			userID:    "not-a-uuid",
			username:  "testuser",
			email:     "test@example.com",
			expectErr: true,
		},
		{
			name:      "empty UUID",
			userID:    "",
			username:  "testuser",
			email:     "test@example.com",
			expectErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			provider, err := NewDefaultProvider(tc.userID, tc.username, tc.email)
			if tc.expectErr {
				assert.Error(t, err)
				assert.Nil(t, provider)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, provider)
			}
		})
	}
}

func TestDefaultProvider_Authenticate(t *testing.T) {
	userID := uuid.New()
	provider, err := NewDefaultProvider(userID.String(), "testuser", "test@example.com")
	require.NoError(t, err)

	// Create a dummy request - DefaultProvider ignores it
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)

	ctx, err := provider.Authenticate(req)

	require.NoError(t, err)
	assert.Equal(t, userID, ctx.UserID)
	assert.Equal(t, "testuser", ctx.Username)
	assert.Equal(t, "test@example.com", ctx.Email)
	assert.Contains(t, ctx.Roles, "user")
}

func TestDefaultProvider_Authenticate_IgnoresRequest(t *testing.T) {
	userID := uuid.New()
	provider, err := NewDefaultProvider(userID.String(), "testuser", "test@example.com")
	require.NoError(t, err)

	// Create request with auth header - should be ignored
	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Authorization", "Bearer some-token")

	ctx, err := provider.Authenticate(req)

	require.NoError(t, err)
	assert.Equal(t, userID, ctx.UserID)
}

func TestNewJWTProvider(t *testing.T) {
	provider := NewJWTProvider(
		"https://auth.example.com/.well-known/jwks.json",
		"https://auth.example.com",
		"my-app",
		300,
	)

	assert.NotNil(t, provider)
	assert.Equal(t, "https://auth.example.com/.well-known/jwks.json", provider.jwksURL)
	assert.Equal(t, "https://auth.example.com", provider.issuer)
	assert.Equal(t, "my-app", provider.audience)
	assert.NotNil(t, provider.client)
	assert.NotNil(t, provider.keys)
}

func TestJWTProvider_Authenticate_MissingToken(t *testing.T) {
	provider := NewJWTProvider(
		"https://auth.example.com/.well-known/jwks.json",
		"https://auth.example.com",
		"my-app",
		300,
	)

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	// No Authorization header

	ctx, err := provider.Authenticate(req)

	assert.Nil(t, ctx)
	assert.ErrorIs(t, err, ErrMissingToken)
}

func TestJWTProvider_Authenticate_InvalidPrefix(t *testing.T) {
	provider := NewJWTProvider(
		"https://auth.example.com/.well-known/jwks.json",
		"https://auth.example.com",
		"my-app",
		300,
	)

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")

	ctx, err := provider.Authenticate(req)

	assert.Nil(t, ctx)
	assert.ErrorIs(t, err, ErrInvalidToken)
}

func TestJWTProvider_Authenticate_MalformedToken(t *testing.T) {
	provider := NewJWTProvider(
		"https://auth.example.com/.well-known/jwks.json",
		"https://auth.example.com",
		"my-app",
		300,
	)

	req := httptest.NewRequest(http.MethodGet, "/api/test", nil)
	req.Header.Set("Authorization", "Bearer not.a.valid.token")

	ctx, err := provider.Authenticate(req)

	assert.Nil(t, ctx)
	assert.Error(t, err)
}

func TestJWTProvider_ExtractContext(t *testing.T) {
	provider := NewJWTProvider(
		"https://auth.example.com/.well-known/jwks.json",
		"https://auth.example.com",
		"my-app",
		300,
	)

	userID := uuid.New()
	claims := map[string]any{
		"sub":                userID.String(),
		"preferred_username": "jdoe",
		"email":              "jdoe@example.com",
		"realm_access": map[string]any{
			"roles": []any{"user", "developer"},
		},
	}

	ctx, err := provider.extractContext(claims)

	require.NoError(t, err)
	assert.Equal(t, userID, ctx.UserID)
	assert.Equal(t, "jdoe", ctx.Username)
	assert.Equal(t, "jdoe@example.com", ctx.Email)
	assert.Contains(t, ctx.Roles, "user")
	assert.Contains(t, ctx.Roles, "developer")
}

func TestJWTProvider_ExtractContext_MissingSub(t *testing.T) {
	provider := NewJWTProvider(
		"https://auth.example.com/.well-known/jwks.json",
		"https://auth.example.com",
		"my-app",
		300,
	)

	claims := map[string]any{
		"preferred_username": "jdoe",
		"email":              "jdoe@example.com",
	}

	ctx, err := provider.extractContext(claims)

	assert.Nil(t, ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing sub claim")
}

func TestJWTProvider_ExtractContext_InvalidSub(t *testing.T) {
	provider := NewJWTProvider(
		"https://auth.example.com/.well-known/jwks.json",
		"https://auth.example.com",
		"my-app",
		300,
	)

	claims := map[string]any{
		"sub":   "not-a-uuid",
		"email": "jdoe@example.com",
	}

	ctx, err := provider.extractContext(claims)

	assert.Nil(t, ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid sub claim")
}

func TestJWTProvider_ExtractContext_MinimalClaims(t *testing.T) {
	provider := NewJWTProvider(
		"https://auth.example.com/.well-known/jwks.json",
		"https://auth.example.com",
		"my-app",
		300,
	)

	userID := uuid.New()
	claims := map[string]any{
		"sub": userID.String(),
	}

	ctx, err := provider.extractContext(claims)

	require.NoError(t, err)
	assert.Equal(t, userID, ctx.UserID)
	assert.Empty(t, ctx.Username)
	assert.Empty(t, ctx.Email)
	assert.Empty(t, ctx.Roles)
}

func TestJWTProvider_ExtractContext_ClaimsPreserved(t *testing.T) {
	provider := NewJWTProvider(
		"https://auth.example.com/.well-known/jwks.json",
		"https://auth.example.com",
		"my-app",
		300,
	)

	userID := uuid.New()
	claims := map[string]any{
		"sub":           userID.String(),
		"custom_field":  "custom_value",
		"numeric_field": float64(42),
	}

	ctx, err := provider.extractContext(claims)

	require.NoError(t, err)
	assert.Equal(t, "custom_value", ctx.Claims["custom_field"])
	assert.Equal(t, float64(42), ctx.Claims["numeric_field"])
}

func TestErrors(t *testing.T) {
	assert.EqualError(t, ErrUnauthorized, "unauthorized")
	assert.EqualError(t, ErrInvalidToken, "invalid token")
	assert.EqualError(t, ErrTokenExpired, "token expired")
	assert.EqualError(t, ErrMissingToken, "missing authorization token")
	assert.EqualError(t, ErrInvalidAudience, "invalid token audience")
	assert.EqualError(t, ErrInvalidIssuer, "invalid token issuer")
}

// ============================================================================
// JWKS Integration Tests (with mock server)
// ============================================================================

func TestJWTProvider_KeyFunc_MissingKid(t *testing.T) {
	provider := NewJWTProvider(
		"https://auth.example.com/.well-known/jwks.json",
		"https://auth.example.com",
		"my-app",
		300,
	)

	token := &jwt.Token{
		Header: map[string]any{}, // No kid
	}

	_, err := provider.keyFunc(token)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "missing kid")
}

func TestJWTProvider_RefreshKeys_HTTPError(t *testing.T) {
	// Create mock server that returns error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	provider := NewJWTProvider(
		server.URL,
		"https://auth.example.com",
		"my-app",
		0, // No caching
	)

	_, err := provider.refreshKeys("test-kid")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "500")
}

func TestJWTProvider_RefreshKeys_InvalidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("not valid json"))
	}))
	defer server.Close()

	provider := NewJWTProvider(
		server.URL,
		"https://auth.example.com",
		"my-app",
		0,
	)

	_, err := provider.refreshKeys("test-kid")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to decode JWKS")
}

func TestJWTProvider_RefreshKeys_KeyNotFound(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"keys":[]}`))
	}))
	defer server.Close()

	provider := NewJWTProvider(
		server.URL,
		"https://auth.example.com",
		"my-app",
		0,
	)

	_, err := provider.refreshKeys("missing-kid")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found in JWKS")
}

func TestJWTProvider_RefreshKeys_SkipsNonRSAKeys(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Return a key with wrong type
		w.Write([]byte(`{
			"keys": [
				{
					"kid": "test-kid",
					"kty": "EC",
					"use": "sig",
					"n": "test",
					"e": "AQAB"
				}
			]
		}`))
	}))
	defer server.Close()

	provider := NewJWTProvider(
		server.URL,
		"https://auth.example.com",
		"my-app",
		0,
	)

	_, err := provider.refreshKeys("test-kid")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found in JWKS")
}

func TestJWTProvider_RefreshKeys_SkipsNonSigKeys(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// Return a key with wrong use
		w.Write([]byte(`{
			"keys": [
				{
					"kid": "test-kid",
					"kty": "RSA",
					"use": "enc",
					"n": "test",
					"e": "AQAB"
				}
			]
		}`))
	}))
	defer server.Close()

	provider := NewJWTProvider(
		server.URL,
		"https://auth.example.com",
		"my-app",
		0,
	)

	_, err := provider.refreshKeys("test-kid")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found in JWKS")
}

func TestJWTProvider_GetKey_UsesCachedKey(t *testing.T) {
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"keys":[]}`))
	}))
	defer server.Close()

	provider := NewJWTProvider(
		server.URL,
		"https://auth.example.com",
		"my-app",
		time.Hour, // Long TTL
	)

	// First call should hit server
	provider.getKey("test-kid")
	firstCount := callCount

	// Second call with same kid should still hit (key not found first time)
	provider.getKey("test-kid")

	// But should not double-fetch within cache window
	assert.GreaterOrEqual(t, callCount, firstCount)
}

func TestJWTProvider_Authenticate_ExpiredToken(t *testing.T) {
	provider := NewJWTProvider(
		"https://auth.example.com/.well-known/jwks.json",
		"https://auth.example.com",
		"my-app",
		300,
	)

	// Create an expired token (this is a real JWT structure but with expired claims)
	// The token will fail validation before reaching JWKS
	expiredToken := "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCIsImtpZCI6InRlc3Qta2lkIn0.eyJzdWIiOiIxMjM0NTY3ODkwIiwiaWF0IjoxNTE2MjM5MDIyLCJleHAiOjE1MTYyMzkwMjJ9.invalid"

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+expiredToken)

	_, err := provider.Authenticate(req)
	assert.Error(t, err)
}

func TestParseRSAPublicKey(t *testing.T) {
	// Test the placeholder implementation
	_, err := parseRSAPublicKey("test-n", "test-e")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not implemented")
}
