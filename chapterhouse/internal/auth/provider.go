package auth

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
)

var (
	ErrUnauthorized    = errors.New("unauthorized")
	ErrInvalidToken    = errors.New("invalid token")
	ErrMissingToken    = errors.New("missing authorization token")
)

// Provider defines the interface for authentication providers.
type Provider interface {
	// Authenticate extracts and validates authentication from the request.
	Authenticate(r *http.Request) (*Context, error)
}

// DefaultProvider provides a static user context. ch-api uses it for /v1
// only when AUTH_DISABLED=true (local dev).
type DefaultProvider struct {
	defaultUserID uuid.UUID
	username      string
	email         string
}

// NewDefaultProvider creates a new default authentication provider.
func NewDefaultProvider(defaultUserID, username, email string) (*DefaultProvider, error) {
	uid, err := uuid.Parse(defaultUserID)
	if err != nil {
		return nil, fmt.Errorf("invalid default user ID: %w", err)
	}
	return &DefaultProvider{
		defaultUserID: uid,
		username:      username,
		email:         email,
	}, nil
}

// Authenticate always returns the default user context.
func (p *DefaultProvider) Authenticate(_ *http.Request) (*Context, error) {
	return &Context{
		UserID:   p.defaultUserID,
		Username: p.username,
		Email:    p.email,
		Roles:    []string{"user"},
	}, nil
}
