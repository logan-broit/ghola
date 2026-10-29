package auth

import (
	"context"

	"github.com/logan-broit/ghola/chapterhouse/internal/repository/sqlc"

	"github.com/google/uuid"
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

// NewAPIKeyProviderWithAdapter creates an APIKeyProvider with a sqlc adapter.
func NewAPIKeyProviderWithAdapter(queries *sqlc.Queries) *APIKeyProvider {
	adapter := NewAPIKeyQuerierAdapter(queries)
	return NewAPIKeyProvider(adapter)
}
