package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	// APIKeyPrefix is the prefix for all API keys
	APIKeyPrefix = "ch_k1_"
	// APIKeyLength is the number of random bytes in an API key (64 hex chars)
	APIKeyLength = 32
)

var (
	// ErrInvalidAPIKey indicates the API key format is invalid
	ErrInvalidAPIKey = errors.New("invalid API key format")
	// ErrAPIKeyNotFound indicates the API key was not found or is inactive
	ErrAPIKeyNotFound = errors.New("API key not found or inactive")
)

// APIKeyQuerier defines the database operations needed by APIKeyProvider.
type APIKeyQuerier interface {
	GetAPIKeyByHash(ctx context.Context, keyHash string) (APIKeyResult, error)
	UpdateAPIKeyLastUsed(ctx context.Context, id uuid.UUID) error
}

// APIKeyResult represents the result of looking up an API key.
type APIKeyResult struct {
	ID       uuid.UUID
	UserID   uuid.UUID
	OrgID    uuid.UUID
	Username string
	Email    string
	IsAdmin  bool
}

// APIKeyProvider authenticates requests using API keys.
type APIKeyProvider struct {
	querier APIKeyQuerier
}

// NewAPIKeyProvider creates a new API key authentication provider.
func NewAPIKeyProvider(querier APIKeyQuerier) *APIKeyProvider {
	return &APIKeyProvider{
		querier: querier,
	}
}

// Authenticate validates the API key from the Authorization header.
// Supports both "Bearer ch_k1_..." and "ApiKey ch_k1_..." formats.
func (p *APIKeyProvider) Authenticate(r *http.Request) (*Context, error) {
	apiKey, err := extractAPIKey(r)
	if err != nil {
		return nil, err
	}

	keyHash := HashAPIKey(apiKey)

	result, err := p.querier.GetAPIKeyByHash(r.Context(), keyHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrAPIKeyNotFound
		}
		return nil, fmt.Errorf("failed to look up API key: %w", err)
	}

	// Update last used timestamp asynchronously
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*1e9) // 5 seconds
		defer cancel()
		_ = p.querier.UpdateAPIKeyLastUsed(ctx, result.ID)
	}()

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
			"auth_method": "api_key",
			"api_key_id":  result.ID.String(),
		},
	}, nil
}

// extractAPIKey extracts the API key from the request.
func extractAPIKey(r *http.Request) (string, error) {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return "", ErrMissingToken
	}

	// Check for "Bearer ch_k1_..." format
	if strings.HasPrefix(authHeader, "Bearer ") {
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if strings.HasPrefix(token, APIKeyPrefix) {
			return token, nil
		}
		// Not an API key, return missing token so composite provider can try next
		return "", ErrMissingToken
	}

	// Check for "ApiKey ch_k1_..." format
	if strings.HasPrefix(authHeader, "ApiKey ") {
		token := strings.TrimPrefix(authHeader, "ApiKey ")
		if strings.HasPrefix(token, APIKeyPrefix) {
			return token, nil
		}
		return "", ErrInvalidAPIKey
	}

	return "", ErrMissingToken
}

// HashAPIKey computes the SHA-256 hash of an API key.
func HashAPIKey(apiKey string) string {
	hash := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(hash[:])
}

// GenerateAPIKey generates a new API key.
// Returns the plaintext key, its hash, and the prefix (first 8 chars after ch_k1_).
func GenerateAPIKey() (plaintext, hash, prefix string, err error) {
	randomBytes := make([]byte, APIKeyLength)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", "", "", fmt.Errorf("failed to generate random bytes: %w", err)
	}

	randomHex := hex.EncodeToString(randomBytes)
	plaintext = APIKeyPrefix + randomHex

	hash = HashAPIKey(plaintext)
	prefix = randomHex[:8]

	return plaintext, hash, prefix, nil
}

// ValidateAPIKeyFormat checks if an API key has a valid format.
func ValidateAPIKeyFormat(apiKey string) bool {
	if !strings.HasPrefix(apiKey, APIKeyPrefix) {
		return false
	}
	randomPart := strings.TrimPrefix(apiKey, APIKeyPrefix)
	if len(randomPart) != APIKeyLength*2 { // hex encoding doubles the length
		return false
	}
	// Check if it's valid hex
	_, err := hex.DecodeString(randomPart)
	return err == nil
}
