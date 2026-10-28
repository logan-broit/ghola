package auth

import (
	"context"

	"github.com/google/uuid"
)

// contextKey is a private type for context keys to avoid collisions.
type contextKey int

const (
	authContextKey contextKey = iota
)

// Context holds authenticated user information.
type Context struct {
	UserID    uuid.UUID
	OrgID     uuid.UUID
	SessionID uuid.UUID // MCP transport session; uuid.Nil for stateless/unknown
	Username  string
	Email     string
	Roles     []string
	Claims    map[string]any
	IPAddress string // Client IP address from HTTP request
	UserAgent string // User-Agent header from HTTP request
}

// HasRole returns true if the user has the specified role.
func (c *Context) HasRole(role string) bool {
	for _, r := range c.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// IsAdmin returns true if the user has the admin role.
func (c *Context) IsAdmin() bool {
	return c.HasRole("admin")
}

// WithContext returns a new context with the auth context attached.
func WithContext(ctx context.Context, authCtx *Context) context.Context {
	return context.WithValue(ctx, authContextKey, authCtx)
}

// FromContext extracts the auth context from the context.
// Returns nil if no auth context is present.
func FromContext(ctx context.Context) *Context {
	if authCtx, ok := ctx.Value(authContextKey).(*Context); ok {
		return authCtx
	}
	return nil
}

// MustFromContext extracts the auth context from the context.
// Panics if no auth context is present.
func MustFromContext(ctx context.Context) *Context {
	authCtx := FromContext(ctx)
	if authCtx == nil {
		panic("auth context not found in context")
	}
	return authCtx
}

// UserIDFromContext extracts just the user ID from the context.
// Returns uuid.Nil if no auth context is present.
func UserIDFromContext(ctx context.Context) uuid.UUID {
	if authCtx := FromContext(ctx); authCtx != nil {
		return authCtx.UserID
	}
	return uuid.Nil
}
