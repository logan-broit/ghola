package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	// SessionCookieName is the name of the admin session cookie
	SessionCookieName = "ch_admin_session"
	// SessionTokenLength is the number of random bytes in a session token (64 hex chars)
	SessionTokenLength = 32
	// DefaultSessionDuration is the default session expiration time
	DefaultSessionDuration = 8 * time.Hour
)

var (
	// ErrSessionNotFound indicates the session was not found or is inactive
	ErrSessionNotFound = errors.New("session not found or inactive")
	// ErrNotAdmin indicates the user is not an admin
	ErrNotAdmin = errors.New("user is not an admin")
)

// SessionQuerier defines the database operations needed by SessionProvider.
type SessionQuerier interface {
	GetAdminSessionByToken(ctx context.Context, tokenHash string) (SessionResult, error)
	CreateAdminSession(ctx context.Context, params CreateSessionParams) (uuid.UUID, error)
	RevokeAdminSessionByToken(ctx context.Context, tokenHash string) error
}

// SessionResult represents the result of looking up a session.
type SessionResult struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	OrgID     uuid.UUID
	Username  string
	Email     string
	IsAdmin   bool
	ExpiresAt time.Time
}

// CreateSessionParams contains parameters for creating a new session.
type CreateSessionParams struct {
	UserID    uuid.UUID
	TokenHash string
	IPAddress string
	UserAgent string
	ExpiresAt time.Time
}

// SessionProvider authenticates requests using session cookies.
type SessionProvider struct {
	querier         SessionQuerier
	sessionDuration time.Duration
	secure          bool // Whether to set Secure flag on cookies
}

// SessionProviderOption is a functional option for SessionProvider.
type SessionProviderOption func(*SessionProvider)

// WithSessionDuration sets the session duration.
func WithSessionDuration(d time.Duration) SessionProviderOption {
	return func(p *SessionProvider) {
		p.sessionDuration = d
	}
}

// WithSecureCookies enables the Secure flag on session cookies.
func WithSecureCookies(secure bool) SessionProviderOption {
	return func(p *SessionProvider) {
		p.secure = secure
	}
}

// NewSessionProvider creates a new session authentication provider.
func NewSessionProvider(querier SessionQuerier, opts ...SessionProviderOption) *SessionProvider {
	p := &SessionProvider{
		querier:         querier,
		sessionDuration: DefaultSessionDuration,
		secure:          true,
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Authenticate validates the session cookie.
func (p *SessionProvider) Authenticate(r *http.Request) (*Context, error) {
	cookie, err := r.Cookie(SessionCookieName)
	if err != nil {
		if errors.Is(err, http.ErrNoCookie) {
			return nil, ErrMissingToken
		}
		return nil, fmt.Errorf("failed to read session cookie: %w", err)
	}

	if cookie.Value == "" {
		return nil, ErrMissingToken
	}

	tokenHash := HashSessionToken(cookie.Value)

	result, err := p.querier.GetAdminSessionByToken(r.Context(), tokenHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrSessionNotFound
		}
		return nil, fmt.Errorf("failed to look up session: %w", err)
	}

	roles := []string{"user"}
	if result.IsAdmin {
		roles = append(roles, "admin")
	}

	return &Context{
		UserID:   result.UserID,
		OrgID:    result.OrgID,
		Username: result.Username,
		Email:    result.Email,
		Roles:    roles,
		Claims: map[string]any{
			"auth_method": "session",
			"session_id":  result.ID.String(),
		},
	}, nil
}

// CreateSession creates a new admin session and returns the session cookie.
func (p *SessionProvider) CreateSession(ctx context.Context, userID uuid.UUID, ipAddress, userAgent string) (*http.Cookie, error) {
	token, tokenHash, err := GenerateSessionToken()
	if err != nil {
		return nil, err
	}

	expiresAt := time.Now().Add(p.sessionDuration)

	_, err = p.querier.CreateAdminSession(ctx, CreateSessionParams{
		UserID:    userID,
		TokenHash: tokenHash,
		IPAddress: ipAddress,
		UserAgent: userAgent,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create session: %w", err)
	}

	cookie := &http.Cookie{
		Name:     SessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expiresAt,
		HttpOnly: true,
		Secure:   p.secure,
		SameSite: http.SameSiteLaxMode,
	}

	return cookie, nil
}

// RevokeSession revokes a session by its token.
func (p *SessionProvider) RevokeSession(ctx context.Context, token string) error {
	tokenHash := HashSessionToken(token)
	return p.querier.RevokeAdminSessionByToken(ctx, tokenHash)
}

// ClearSessionCookie returns a cookie that clears the session.
func (p *SessionProvider) ClearSessionCookie() *http.Cookie {
	return &http.Cookie{
		Name:     SessionCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1, // Delete the cookie
		HttpOnly: true,
		Secure:   p.secure,
		SameSite: http.SameSiteLaxMode,
	}
}

// HashSessionToken computes the SHA-256 hash of a session token.
func HashSessionToken(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}

// GenerateSessionToken generates a new session token.
// Returns the plaintext token and its hash.
func GenerateSessionToken() (plaintext, hash string, err error) {
	randomBytes := make([]byte, SessionTokenLength)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", "", fmt.Errorf("failed to generate random bytes: %w", err)
	}

	plaintext = hex.EncodeToString(randomBytes)
	hash = HashSessionToken(plaintext)

	return plaintext, hash, nil
}
