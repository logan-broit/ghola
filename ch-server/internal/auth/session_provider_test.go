package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockSessionQuerier is a mock implementation of SessionQuerier
type mockSessionQuerier struct {
	getAdminSessionByTokenFn    func(ctx context.Context, tokenHash string) (SessionResult, error)
	createAdminSessionFn        func(ctx context.Context, params CreateSessionParams) (uuid.UUID, error)
	revokeAdminSessionByTokenFn func(ctx context.Context, tokenHash string) error
}

func (m *mockSessionQuerier) GetAdminSessionByToken(ctx context.Context, tokenHash string) (SessionResult, error) {
	if m.getAdminSessionByTokenFn != nil {
		return m.getAdminSessionByTokenFn(ctx, tokenHash)
	}
	return SessionResult{}, pgx.ErrNoRows
}

func (m *mockSessionQuerier) CreateAdminSession(ctx context.Context, params CreateSessionParams) (uuid.UUID, error) {
	if m.createAdminSessionFn != nil {
		return m.createAdminSessionFn(ctx, params)
	}
	return uuid.New(), nil
}

func (m *mockSessionQuerier) RevokeAdminSessionByToken(ctx context.Context, tokenHash string) error {
	if m.revokeAdminSessionByTokenFn != nil {
		return m.revokeAdminSessionByTokenFn(ctx, tokenHash)
	}
	return nil
}

func TestGenerateSessionToken(t *testing.T) {
	plaintext, hash, err := GenerateSessionToken()

	require.NoError(t, err)
	assert.Len(t, plaintext, 64) // 32 bytes = 64 hex chars
	assert.Len(t, hash, 64)      // SHA-256 = 64 hex chars

	// Verify the hash is consistent
	assert.Equal(t, hash, HashSessionToken(plaintext))
}

func TestGenerateSessionToken_Uniqueness(t *testing.T) {
	tokens := make(map[string]bool)
	for i := 0; i < 100; i++ {
		plaintext, _, err := GenerateSessionToken()
		require.NoError(t, err)
		assert.False(t, tokens[plaintext], "duplicate token generated")
		tokens[plaintext] = true
	}
}

func TestHashSessionToken(t *testing.T) {
	token := "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

	hash1 := HashSessionToken(token)
	hash2 := HashSessionToken(token)

	// Verify consistent hashing
	assert.Equal(t, hash1, hash2)
	assert.Len(t, hash1, 64)

	// Verify different tokens produce different hashes
	differentHash := HashSessionToken("different_token")
	assert.NotEqual(t, hash1, differentHash)
}

func TestNewSessionProvider(t *testing.T) {
	querier := &mockSessionQuerier{}

	t.Run("default options", func(t *testing.T) {
		provider := NewSessionProvider(querier)
		assert.Equal(t, DefaultSessionDuration, provider.sessionDuration)
		assert.True(t, provider.secure)
	})

	t.Run("custom options", func(t *testing.T) {
		provider := NewSessionProvider(querier,
			WithSessionDuration(24*time.Hour),
			WithSecureCookies(false),
		)
		assert.Equal(t, 24*time.Hour, provider.sessionDuration)
		assert.False(t, provider.secure)
	})
}

func TestSessionProvider_Authenticate_Success(t *testing.T) {
	userID := uuid.New()
	sessionID := uuid.New()

	querier := &mockSessionQuerier{
		getAdminSessionByTokenFn: func(ctx context.Context, tokenHash string) (SessionResult, error) {
			return SessionResult{
				ID:        sessionID,
				UserID:    userID,
				Username:  "adminuser",
				Email:     "admin@example.com",
				IsAdmin:   true,
				ExpiresAt: time.Now().Add(time.Hour),
			}, nil
		},
	}

	provider := NewSessionProvider(querier)

	req := httptest.NewRequest(http.MethodGet, "/admin/test", nil)
	req.AddCookie(&http.Cookie{
		Name:  SessionCookieName,
		Value: "test-session-token",
	})

	ctx, err := provider.Authenticate(req)

	require.NoError(t, err)
	assert.Equal(t, userID, ctx.UserID)
	assert.Equal(t, "adminuser", ctx.Username)
	assert.Equal(t, "admin@example.com", ctx.Email)
	assert.Contains(t, ctx.Roles, "user")
	assert.Contains(t, ctx.Roles, "admin")
	assert.True(t, ctx.IsAdmin())
	assert.Equal(t, "session", ctx.Claims["auth_method"])
	assert.Equal(t, sessionID.String(), ctx.Claims["session_id"])
}

func TestSessionProvider_Authenticate_NoCookie(t *testing.T) {
	querier := &mockSessionQuerier{}
	provider := NewSessionProvider(querier)

	req := httptest.NewRequest(http.MethodGet, "/admin/test", nil)

	ctx, err := provider.Authenticate(req)

	assert.Nil(t, ctx)
	assert.ErrorIs(t, err, ErrMissingToken)
}

func TestSessionProvider_Authenticate_EmptyCookie(t *testing.T) {
	querier := &mockSessionQuerier{}
	provider := NewSessionProvider(querier)

	req := httptest.NewRequest(http.MethodGet, "/admin/test", nil)
	req.AddCookie(&http.Cookie{
		Name:  SessionCookieName,
		Value: "",
	})

	ctx, err := provider.Authenticate(req)

	assert.Nil(t, ctx)
	assert.ErrorIs(t, err, ErrMissingToken)
}

func TestSessionProvider_Authenticate_SessionNotFound(t *testing.T) {
	querier := &mockSessionQuerier{
		getAdminSessionByTokenFn: func(ctx context.Context, tokenHash string) (SessionResult, error) {
			return SessionResult{}, pgx.ErrNoRows
		},
	}

	provider := NewSessionProvider(querier)

	req := httptest.NewRequest(http.MethodGet, "/admin/test", nil)
	req.AddCookie(&http.Cookie{
		Name:  SessionCookieName,
		Value: "invalid-session-token",
	})

	ctx, err := provider.Authenticate(req)

	assert.Nil(t, ctx)
	assert.ErrorIs(t, err, ErrSessionNotFound)
}

func TestSessionProvider_Authenticate_DatabaseError(t *testing.T) {
	dbError := errors.New("database connection failed")
	querier := &mockSessionQuerier{
		getAdminSessionByTokenFn: func(ctx context.Context, tokenHash string) (SessionResult, error) {
			return SessionResult{}, dbError
		},
	}

	provider := NewSessionProvider(querier)

	req := httptest.NewRequest(http.MethodGet, "/admin/test", nil)
	req.AddCookie(&http.Cookie{
		Name:  SessionCookieName,
		Value: "some-token",
	})

	ctx, err := provider.Authenticate(req)

	assert.Nil(t, ctx)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to look up session")
}

func TestSessionProvider_CreateSession(t *testing.T) {
	userID := uuid.New()
	sessionID := uuid.New()
	var capturedParams CreateSessionParams

	querier := &mockSessionQuerier{
		createAdminSessionFn: func(ctx context.Context, params CreateSessionParams) (uuid.UUID, error) {
			capturedParams = params
			return sessionID, nil
		},
	}

	provider := NewSessionProvider(querier,
		WithSessionDuration(4*time.Hour),
		WithSecureCookies(true),
	)

	cookie, err := provider.CreateSession(context.Background(), userID, "192.168.1.1", "Mozilla/5.0")

	require.NoError(t, err)
	assert.NotNil(t, cookie)

	// Verify cookie properties
	assert.Equal(t, SessionCookieName, cookie.Name)
	assert.NotEmpty(t, cookie.Value)
	assert.Equal(t, "/", cookie.Path)
	assert.True(t, cookie.HttpOnly)
	assert.True(t, cookie.Secure)
	assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)

	// Verify session was created with correct params
	assert.Equal(t, userID, capturedParams.UserID)
	assert.Equal(t, "192.168.1.1", capturedParams.IPAddress)
	assert.Equal(t, "Mozilla/5.0", capturedParams.UserAgent)
	assert.NotEmpty(t, capturedParams.TokenHash)
	assert.True(t, capturedParams.ExpiresAt.After(time.Now()))

	// Verify cookie value hashes to the stored token hash
	assert.Equal(t, capturedParams.TokenHash, HashSessionToken(cookie.Value))
}

func TestSessionProvider_CreateSession_Error(t *testing.T) {
	dbError := errors.New("database error")
	querier := &mockSessionQuerier{
		createAdminSessionFn: func(ctx context.Context, params CreateSessionParams) (uuid.UUID, error) {
			return uuid.Nil, dbError
		},
	}

	provider := NewSessionProvider(querier)

	cookie, err := provider.CreateSession(context.Background(), uuid.New(), "192.168.1.1", "Mozilla/5.0")

	assert.Nil(t, cookie)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to create session")
}

func TestSessionProvider_RevokeSession(t *testing.T) {
	var capturedTokenHash string

	querier := &mockSessionQuerier{
		revokeAdminSessionByTokenFn: func(ctx context.Context, tokenHash string) error {
			capturedTokenHash = tokenHash
			return nil
		},
	}

	provider := NewSessionProvider(querier)

	token := "test-session-token"
	err := provider.RevokeSession(context.Background(), token)

	require.NoError(t, err)
	assert.Equal(t, HashSessionToken(token), capturedTokenHash)
}

func TestSessionProvider_RevokeSession_Error(t *testing.T) {
	dbError := errors.New("database error")
	querier := &mockSessionQuerier{
		revokeAdminSessionByTokenFn: func(ctx context.Context, tokenHash string) error {
			return dbError
		},
	}

	provider := NewSessionProvider(querier)

	err := provider.RevokeSession(context.Background(), "some-token")

	assert.Error(t, err)
	assert.Equal(t, dbError, err)
}

func TestSessionProvider_ClearSessionCookie(t *testing.T) {
	provider := NewSessionProvider(&mockSessionQuerier{},
		WithSecureCookies(true),
	)

	cookie := provider.ClearSessionCookie()

	assert.Equal(t, SessionCookieName, cookie.Name)
	assert.Empty(t, cookie.Value)
	assert.Equal(t, "/", cookie.Path)
	assert.Equal(t, -1, cookie.MaxAge) // Delete cookie
	assert.True(t, cookie.HttpOnly)
	assert.True(t, cookie.Secure)
	assert.Equal(t, http.SameSiteLaxMode, cookie.SameSite)
}

func TestSessionProvider_ClearSessionCookie_Insecure(t *testing.T) {
	provider := NewSessionProvider(&mockSessionQuerier{},
		WithSecureCookies(false),
	)

	cookie := provider.ClearSessionCookie()

	assert.False(t, cookie.Secure)
}
