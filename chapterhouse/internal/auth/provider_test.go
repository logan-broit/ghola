package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

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
