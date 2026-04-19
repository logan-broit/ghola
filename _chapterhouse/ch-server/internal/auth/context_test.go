package auth

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContext_HasRole(t *testing.T) {
	tests := []struct {
		name     string
		roles    []string
		checkFor string
		expected bool
	}{
		{
			name:     "has role",
			roles:    []string{"user", "admin"},
			checkFor: "admin",
			expected: true,
		},
		{
			name:     "does not have role",
			roles:    []string{"user"},
			checkFor: "admin",
			expected: false,
		},
		{
			name:     "empty roles",
			roles:    []string{},
			checkFor: "user",
			expected: false,
		},
		{
			name:     "nil roles",
			roles:    nil,
			checkFor: "user",
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &Context{Roles: tc.roles}
			assert.Equal(t, tc.expected, ctx.HasRole(tc.checkFor))
		})
	}
}

func TestContext_IsAdmin(t *testing.T) {
	tests := []struct {
		name     string
		roles    []string
		expected bool
	}{
		{
			name:     "is admin",
			roles:    []string{"user", "admin"},
			expected: true,
		},
		{
			name:     "not admin",
			roles:    []string{"user"},
			expected: false,
		},
		{
			name:     "empty roles",
			roles:    []string{},
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := &Context{Roles: tc.roles}
			assert.Equal(t, tc.expected, ctx.IsAdmin())
		})
	}
}

func TestWithContext_FromContext(t *testing.T) {
	userID := uuid.New()
	authCtx := &Context{
		UserID:   userID,
		Username: "testuser",
		Email:    "test@example.com",
		Roles:    []string{"user"},
	}

	ctx := WithContext(context.Background(), authCtx)

	retrieved := FromContext(ctx)
	require.NotNil(t, retrieved)
	assert.Equal(t, userID, retrieved.UserID)
	assert.Equal(t, "testuser", retrieved.Username)
	assert.Equal(t, "test@example.com", retrieved.Email)
	assert.Equal(t, []string{"user"}, retrieved.Roles)
}

func TestFromContext_NoAuth(t *testing.T) {
	ctx := context.Background()
	retrieved := FromContext(ctx)
	assert.Nil(t, retrieved)
}

func TestFromContext_WrongType(t *testing.T) {
	ctx := context.WithValue(context.Background(), authContextKey, "not an auth context")
	retrieved := FromContext(ctx)
	assert.Nil(t, retrieved)
}

func TestMustFromContext_Success(t *testing.T) {
	userID := uuid.New()
	authCtx := &Context{UserID: userID}
	ctx := WithContext(context.Background(), authCtx)

	retrieved := MustFromContext(ctx)
	assert.Equal(t, userID, retrieved.UserID)
}

func TestMustFromContext_Panics(t *testing.T) {
	ctx := context.Background()

	assert.Panics(t, func() {
		MustFromContext(ctx)
	})
}

func TestUserIDFromContext(t *testing.T) {
	userID := uuid.New()
	authCtx := &Context{UserID: userID}
	ctx := WithContext(context.Background(), authCtx)

	retrieved := UserIDFromContext(ctx)
	assert.Equal(t, userID, retrieved)
}

func TestUserIDFromContext_NoAuth(t *testing.T) {
	ctx := context.Background()
	retrieved := UserIDFromContext(ctx)
	assert.Equal(t, uuid.Nil, retrieved)
}

func TestContext_Claims(t *testing.T) {
	claims := map[string]any{
		"sub":   "123",
		"email": "test@example.com",
		"custom": map[string]any{
			"org": "acme",
		},
	}

	authCtx := &Context{
		UserID: uuid.New(),
		Claims: claims,
	}

	assert.Equal(t, "123", authCtx.Claims["sub"])
	assert.Equal(t, "test@example.com", authCtx.Claims["email"])

	custom, ok := authCtx.Claims["custom"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "acme", custom["org"])
}
