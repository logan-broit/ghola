package auth

import (
	"context"
	"time"

	"github.com/thinkwright/chapterhouse/ch-server/internal/repository/sqlc"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// APIKeyQuerierAdapter adapts sqlc.Queries to implement APIKeyQuerier.
type APIKeyQuerierAdapter struct {
	queries *sqlc.Queries
}

// NewAPIKeyQuerierAdapter creates a new adapter.
func NewAPIKeyQuerierAdapter(queries *sqlc.Queries) *APIKeyQuerierAdapter {
	return &APIKeyQuerierAdapter{queries: queries}
}

// GetAPIKeyByHash implements APIKeyQuerier.
func (a *APIKeyQuerierAdapter) GetAPIKeyByHash(ctx context.Context, keyHash string) (APIKeyResult, error) {
	row, err := a.queries.GetAPIKeyByHash(ctx, keyHash)
	if err != nil {
		return APIKeyResult{}, err
	}

	return APIKeyResult{
		ID:       row.ID,
		UserID:   row.UserID,
		OrgID:    row.OrgID,
		Username: row.Username,
		Email:    row.Email.String,
		IsAdmin:  row.IsAdmin,
	}, nil
}

// UpdateAPIKeyLastUsed implements APIKeyQuerier.
func (a *APIKeyQuerierAdapter) UpdateAPIKeyLastUsed(ctx context.Context, id uuid.UUID) error {
	return a.queries.UpdateAPIKeyLastUsed(ctx, id)
}

// SessionQuerierAdapter adapts sqlc.Queries to implement SessionQuerier.
type SessionQuerierAdapter struct {
	queries *sqlc.Queries
}

// NewSessionQuerierAdapter creates a new adapter.
func NewSessionQuerierAdapter(queries *sqlc.Queries) *SessionQuerierAdapter {
	return &SessionQuerierAdapter{queries: queries}
}

// GetAdminSessionByToken implements SessionQuerier.
func (a *SessionQuerierAdapter) GetAdminSessionByToken(ctx context.Context, tokenHash string) (SessionResult, error) {
	row, err := a.queries.GetAdminSessionByToken(ctx, tokenHash)
	if err != nil {
		return SessionResult{}, err
	}

	return SessionResult{
		ID:        row.ID,
		UserID:    row.UserID,
		OrgID:     row.OrgID,
		Username:  row.Username,
		Email:     row.Email.String,
		IsAdmin:   row.IsAdmin,
		ExpiresAt: row.ExpiresAt,
	}, nil
}

// CreateAdminSession implements SessionQuerier.
func (a *SessionQuerierAdapter) CreateAdminSession(ctx context.Context, params CreateSessionParams) (uuid.UUID, error) {
	session, err := a.queries.CreateAdminSession(ctx, sqlc.CreateAdminSessionParams{
		UserID:    params.UserID,
		TokenHash: params.TokenHash,
		IpAddress: pgtype.Text{String: params.IPAddress, Valid: params.IPAddress != ""},
		UserAgent: pgtype.Text{String: params.UserAgent, Valid: params.UserAgent != ""},
		ExpiresAt: params.ExpiresAt,
	})
	if err != nil {
		return uuid.Nil, err
	}
	return session.ID, nil
}

// RevokeAdminSessionByToken implements SessionQuerier.
func (a *SessionQuerierAdapter) RevokeAdminSessionByToken(ctx context.Context, tokenHash string) error {
	return a.queries.RevokeAdminSessionByToken(ctx, tokenHash)
}

// SessionProviderConfig contains configuration for creating a SessionProvider with adapters.
type SessionProviderConfig struct {
	Queries         *sqlc.Queries
	SessionDuration time.Duration
	SecureCookies   bool
}

// NewSessionProviderWithAdapter creates a SessionProvider with a sqlc adapter.
func NewSessionProviderWithAdapter(cfg SessionProviderConfig) *SessionProvider {
	adapter := NewSessionQuerierAdapter(cfg.Queries)
	opts := []SessionProviderOption{}

	if cfg.SessionDuration > 0 {
		opts = append(opts, WithSessionDuration(cfg.SessionDuration))
	}
	opts = append(opts, WithSecureCookies(cfg.SecureCookies))

	return NewSessionProvider(adapter, opts...)
}

// NewAPIKeyProviderWithAdapter creates an APIKeyProvider with a sqlc adapter.
func NewAPIKeyProviderWithAdapter(queries *sqlc.Queries) *APIKeyProvider {
	adapter := NewAPIKeyQuerierAdapter(queries)
	return NewAPIKeyProvider(adapter)
}
