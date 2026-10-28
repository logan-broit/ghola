package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockProvider is a simple mock for testing
type mockProvider struct {
	authenticateFn func(r *http.Request) (*Context, error)
}

func (m *mockProvider) Authenticate(r *http.Request) (*Context, error) {
	if m.authenticateFn != nil {
		return m.authenticateFn(r)
	}
	return nil, ErrMissingToken
}

func TestNewCompositeProvider(t *testing.T) {
	p1 := &mockProvider{}
	p2 := &mockProvider{}

	composite := NewCompositeProvider(p1, p2)

	assert.NotNil(t, composite)
	assert.Len(t, composite.providers, 2)
}

func TestCompositeProvider_Authenticate_FirstProviderSucceeds(t *testing.T) {
	userID := uuid.New()
	p1 := &mockProvider{
		authenticateFn: func(r *http.Request) (*Context, error) {
			return &Context{UserID: userID, Username: "user1"}, nil
		},
	}
	p2 := &mockProvider{
		authenticateFn: func(r *http.Request) (*Context, error) {
			return &Context{UserID: uuid.New(), Username: "user2"}, nil
		},
	}

	composite := NewCompositeProvider(p1, p2)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)

	ctx, err := composite.Authenticate(req)

	require.NoError(t, err)
	assert.Equal(t, userID, ctx.UserID)
	assert.Equal(t, "user1", ctx.Username)
}

func TestCompositeProvider_Authenticate_SecondProviderSucceeds(t *testing.T) {
	userID := uuid.New()
	p1 := &mockProvider{
		authenticateFn: func(r *http.Request) (*Context, error) {
			return nil, ErrMissingToken // First provider has no token
		},
	}
	p2 := &mockProvider{
		authenticateFn: func(r *http.Request) (*Context, error) {
			return &Context{UserID: userID, Username: "user2"}, nil
		},
	}

	composite := NewCompositeProvider(p1, p2)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)

	ctx, err := composite.Authenticate(req)

	require.NoError(t, err)
	assert.Equal(t, userID, ctx.UserID)
	assert.Equal(t, "user2", ctx.Username)
}

func TestCompositeProvider_Authenticate_ThirdProviderSucceeds(t *testing.T) {
	userID := uuid.New()
	p1 := &mockProvider{
		authenticateFn: func(r *http.Request) (*Context, error) {
			return nil, ErrMissingToken
		},
	}
	p2 := &mockProvider{
		authenticateFn: func(r *http.Request) (*Context, error) {
			return nil, ErrInvalidToken // Some other error
		},
	}
	p3 := &mockProvider{
		authenticateFn: func(r *http.Request) (*Context, error) {
			return &Context{UserID: userID, Username: "user3"}, nil
		},
	}

	composite := NewCompositeProvider(p1, p2, p3)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)

	ctx, err := composite.Authenticate(req)

	require.NoError(t, err)
	assert.Equal(t, userID, ctx.UserID)
}

func TestCompositeProvider_Authenticate_AllFail_ReturnsLastError(t *testing.T) {
	customErr := errors.New("custom auth error")
	p1 := &mockProvider{
		authenticateFn: func(r *http.Request) (*Context, error) {
			return nil, ErrMissingToken
		},
	}
	p2 := &mockProvider{
		authenticateFn: func(r *http.Request) (*Context, error) {
			return nil, customErr
		},
	}
	p3 := &mockProvider{
		authenticateFn: func(r *http.Request) (*Context, error) {
			return nil, ErrMissingToken
		},
	}

	composite := NewCompositeProvider(p1, p2, p3)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)

	ctx, err := composite.Authenticate(req)

	assert.Nil(t, ctx)
	// Should return the most informative error (not ErrMissingToken)
	assert.Equal(t, customErr, err)
}

func TestCompositeProvider_Authenticate_AllMissingToken(t *testing.T) {
	p1 := &mockProvider{
		authenticateFn: func(r *http.Request) (*Context, error) {
			return nil, ErrMissingToken
		},
	}
	p2 := &mockProvider{
		authenticateFn: func(r *http.Request) (*Context, error) {
			return nil, ErrMissingToken
		},
	}

	composite := NewCompositeProvider(p1, p2)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)

	ctx, err := composite.Authenticate(req)

	assert.Nil(t, ctx)
	assert.ErrorIs(t, err, ErrMissingToken)
}

func TestCompositeProvider_Authenticate_NoProviders(t *testing.T) {
	composite := NewCompositeProvider()
	req := httptest.NewRequest(http.MethodGet, "/test", nil)

	ctx, err := composite.Authenticate(req)

	assert.Nil(t, ctx)
	assert.ErrorIs(t, err, ErrMissingToken)
}

func TestCompositeProvider_Authenticate_SkipsAfterError(t *testing.T) {
	// Even if first provider returns an error (not missing token),
	// we should continue to try other providers
	userID := uuid.New()
	p1 := &mockProvider{
		authenticateFn: func(r *http.Request) (*Context, error) {
			return nil, ErrInvalidToken
		},
	}
	p2 := &mockProvider{
		authenticateFn: func(r *http.Request) (*Context, error) {
			return &Context{UserID: userID}, nil
		},
	}

	composite := NewCompositeProvider(p1, p2)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)

	ctx, err := composite.Authenticate(req)

	require.NoError(t, err)
	assert.Equal(t, userID, ctx.UserID)
}

func TestCompositeProvider_Authenticate_RealWorldScenario(t *testing.T) {
	// Simulate: API key provider + JWT provider
	// Request has JWT token, API key provider returns missing token, JWT succeeds

	userID := uuid.New()

	// API key provider - doesn't find API key prefix
	apiKeyProvider := &mockProvider{
		authenticateFn: func(r *http.Request) (*Context, error) {
			return nil, ErrMissingToken // Bearer token but not API key
		},
	}

	// JWT provider - validates the JWT
	jwtProvider := &mockProvider{
		authenticateFn: func(r *http.Request) (*Context, error) {
			return &Context{
				UserID:   userID,
				Username: "jwtuser",
				Claims:   map[string]any{"auth_method": "jwt"},
			}, nil
		},
	}

	composite := NewCompositeProvider(apiKeyProvider, jwtProvider)
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9...")

	ctx, err := composite.Authenticate(req)

	require.NoError(t, err)
	assert.Equal(t, userID, ctx.UserID)
	assert.Equal(t, "jwt", ctx.Claims["auth_method"])
}
