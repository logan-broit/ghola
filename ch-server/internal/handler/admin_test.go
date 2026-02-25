package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thinkwright/chapterhouse/ch-server/internal/auth"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository/sqlc"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

// mockSessionCreator implements SessionCreator for testing
type mockSessionCreator struct {
	createSessionFn func(ctx context.Context, userID uuid.UUID, ipAddress, userAgent string) (*http.Cookie, error)
	revokeSessionFn func(ctx context.Context, token string) error
	clearCookieFn   func() *http.Cookie
}

func (m *mockSessionCreator) CreateSession(ctx context.Context, userID uuid.UUID, ipAddress, userAgent string) (*http.Cookie, error) {
	if m.createSessionFn != nil {
		return m.createSessionFn(ctx, userID, ipAddress, userAgent)
	}
	return &http.Cookie{Name: auth.SessionCookieName, Value: "test-token"}, nil
}

func (m *mockSessionCreator) RevokeSession(ctx context.Context, token string) error {
	if m.revokeSessionFn != nil {
		return m.revokeSessionFn(ctx, token)
	}
	return nil
}

func (m *mockSessionCreator) ClearSessionCookie() *http.Cookie {
	if m.clearCookieFn != nil {
		return m.clearCookieFn()
	}
	return &http.Cookie{Name: auth.SessionCookieName, Value: "", MaxAge: -1}
}

func TestParseDuration(t *testing.T) {
	tests := []struct {
		input    string
		expected time.Duration
		wantErr  bool
	}{
		{"30d", 30 * 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"1w", 7 * 24 * time.Hour, false},
		{"2w", 14 * 24 * time.Hour, false},
		{"1m", 30 * 24 * time.Hour, false},
		{"1y", 365 * 24 * time.Hour, false},
		{"", 0, true},
		{"d", 0, true},
		{"0d", 0, true},
		{"-1d", 0, true},
		{"30x", 0, true},
		{"abc", 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result, err := parseDuration(tc.input)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tc.expected, result)
			}
		})
	}
}

func TestParseAdminPagination(t *testing.T) {
	tests := []struct {
		name         string
		queryLimit   string
		queryOffset  string
		defaultLimit int32
		maxLimit     int32
		expectLimit  int32
		expectOffset int32
	}{
		{
			name:         "default values",
			defaultLimit: 50,
			maxLimit:     1000,
			expectLimit:  50,
			expectOffset: 0,
		},
		{
			name:         "custom limit and offset",
			queryLimit:   "25",
			queryOffset:  "100",
			defaultLimit: 50,
			maxLimit:     1000,
			expectLimit:  25,
			expectOffset: 100,
		},
		{
			name:         "limit exceeds max",
			queryLimit:   "2000",
			defaultLimit: 50,
			maxLimit:     1000,
			expectLimit:  1000,
			expectOffset: 0,
		},
		{
			name:         "invalid limit uses default",
			queryLimit:   "invalid",
			defaultLimit: 50,
			maxLimit:     1000,
			expectLimit:  50,
			expectOffset: 0,
		},
		{
			name:         "negative limit uses default",
			queryLimit:   "-5",
			defaultLimit: 50,
			maxLimit:     1000,
			expectLimit:  50,
			expectOffset: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			q := req.URL.Query()
			if tc.queryLimit != "" {
				q.Set("limit", tc.queryLimit)
			}
			if tc.queryOffset != "" {
				q.Set("offset", tc.queryOffset)
			}
			req.URL.RawQuery = q.Encode()

			limit, offset := parseAdminPagination(req, tc.defaultLimit, tc.maxLimit)

			assert.Equal(t, tc.expectLimit, limit)
			assert.Equal(t, tc.expectOffset, offset)
		})
	}
}

func TestToUserResponse(t *testing.T) {
	now := time.Now()
	userID := uuid.New()

	t.Run("active user", func(t *testing.T) {
		user := sqlc.User{
			ID:          userID,
			Username:    "testuser",
			Email:       pgtype.Text{String: "test@example.com", Valid: true},
			DisplayName: pgtype.Text{String: "Test User", Valid: true},
			IsAdmin:     false,
			CreatedAt:   now,
			ModifiedAt:  now,
		}

		resp := toUserResponse(user)

		assert.Equal(t, userID.String(), resp.ID)
		assert.Equal(t, "testuser", resp.Username)
		assert.Equal(t, "test@example.com", resp.Email)
		assert.Equal(t, "Test User", resp.DisplayName)
		assert.False(t, resp.IsAdmin)
		assert.Nil(t, resp.DeactivatedAt)
	})

	t.Run("deactivated user", func(t *testing.T) {
		deactivatedAt := now.Add(-time.Hour)
		user := sqlc.User{
			ID:            userID,
			Username:      "deactivated",
			IsAdmin:       false,
			CreatedAt:     now,
			ModifiedAt:    now,
			DeactivatedAt: pgtype.Timestamptz{Time: deactivatedAt, Valid: true},
		}

		resp := toUserResponse(user)

		assert.NotNil(t, resp.DeactivatedAt)
		assert.Equal(t, deactivatedAt, *resp.DeactivatedAt)
	})
}

func TestToAPIKeyResponse(t *testing.T) {
	now := time.Now()
	keyID := uuid.New()
	userID := uuid.New()

	t.Run("active key without expiration", func(t *testing.T) {
		key := sqlc.ApiKey{
			ID:        keyID,
			UserID:    userID,
			KeyPrefix: "abcd1234",
			Name:      "Test Key",
			CreatedAt: now,
		}

		resp := toAPIKeyResponse(key)

		assert.Equal(t, keyID.String(), resp.ID)
		assert.Equal(t, userID.String(), resp.UserID)
		assert.Equal(t, "abcd1234", resp.KeyPrefix)
		assert.Equal(t, "Test Key", resp.Name)
		assert.Nil(t, resp.LastUsedAt)
		assert.Nil(t, resp.ExpiresAt)
		assert.Nil(t, resp.RevokedAt)
	})

	t.Run("key with all timestamps", func(t *testing.T) {
		lastUsed := now.Add(-time.Hour)
		expiresAt := now.Add(30 * 24 * time.Hour)
		revokedAt := now

		key := sqlc.ApiKey{
			ID:         keyID,
			UserID:     userID,
			KeyPrefix:  "abcd1234",
			Name:       "Revoked Key",
			CreatedAt:  now,
			LastUsedAt: pgtype.Timestamptz{Time: lastUsed, Valid: true},
			ExpiresAt:  pgtype.Timestamptz{Time: expiresAt, Valid: true},
			RevokedAt:  pgtype.Timestamptz{Time: revokedAt, Valid: true},
		}

		resp := toAPIKeyResponse(key)

		require.NotNil(t, resp.LastUsedAt)
		assert.Equal(t, lastUsed, *resp.LastUsedAt)
		require.NotNil(t, resp.ExpiresAt)
		assert.Equal(t, expiresAt, *resp.ExpiresAt)
		require.NotNil(t, resp.RevokedAt)
		assert.Equal(t, revokedAt, *resp.RevokedAt)
	})
}

func TestLoginRequest_Validation(t *testing.T) {
	tests := []struct {
		name    string
		req     LoginRequest
		wantErr bool
	}{
		{
			name:    "valid request",
			req:     LoginRequest{Username: "admin", Password: "secret"},
			wantErr: false,
		},
		{
			name:    "empty username",
			req:     LoginRequest{Username: "", Password: "secret"},
			wantErr: true,
		},
		{
			name:    "empty password",
			req:     LoginRequest{Username: "admin", Password: ""},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hasErr := tc.req.Username == "" || tc.req.Password == ""
			assert.Equal(t, tc.wantErr, hasErr)
		})
	}
}

func TestBcryptPasswordHashing(t *testing.T) {
	password := "testpassword123"

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	require.NoError(t, err)

	// Correct password should match
	err = bcrypt.CompareHashAndPassword(hash, []byte(password))
	assert.NoError(t, err)

	// Wrong password should not match
	err = bcrypt.CompareHashAndPassword(hash, []byte("wrongpassword"))
	assert.Error(t, err)
}

// Helper function to create a test request with JSON body
func newJSONRequest(method, path string, body any) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	return req
}

// Helper function to add chi URL params to request
func withURLParam(r *http.Request, key, value string) *http.Request {
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add(key, value)
	return r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx))
}

// ============================================================================
// Admin Mock Queries for Handler Testing
// ============================================================================

// adminMockQueries provides a configurable mock for admin handler testing.
// It embeds testutil.MockQueries and overrides methods with configurable functions.
type adminMockQueries struct {
	users   map[uuid.UUID]sqlc.User
	apiKeys map[uuid.UUID]sqlc.ApiKey

	// Configurable function hooks
	getUserByUsernameForAuthFn func(ctx context.Context, username string) (sqlc.User, error)
	getUserByUsernameFn        func(ctx context.Context, username string) (sqlc.User, error)
	getUserByIDFn              func(ctx context.Context, id uuid.UUID) (sqlc.User, error)
	createUserWithPasswordFn   func(ctx context.Context, arg sqlc.CreateUserWithPasswordParams) (sqlc.User, error)
	updateUserAdminFn          func(ctx context.Context, arg sqlc.UpdateUserAdminParams) (sqlc.User, error)
	deactivateUserFn           func(ctx context.Context, id uuid.UUID) error
	reactivateUserFn           func(ctx context.Context, id uuid.UUID) error
	createAPIKeyFn             func(ctx context.Context, arg sqlc.CreateAPIKeyParams) (sqlc.ApiKey, error)
	getAPIKeyByIDFn            func(ctx context.Context, id uuid.UUID) (sqlc.ApiKey, error)
	revokeAPIKeyFn             func(ctx context.Context, id uuid.UUID) error
	listAPIKeysByUserFn        func(ctx context.Context, userID uuid.UUID) ([]sqlc.ListAPIKeysByUserRow, error)
}

func newAdminMockQueries() *adminMockQueries {
	return &adminMockQueries{
		users:   make(map[uuid.UUID]sqlc.User),
		apiKeys: make(map[uuid.UUID]sqlc.ApiKey),
	}
}

func (m *adminMockQueries) GetUserByUsernameForAuth(ctx context.Context, username string) (sqlc.User, error) {
	if m.getUserByUsernameForAuthFn != nil {
		return m.getUserByUsernameForAuthFn(ctx, username)
	}
	for _, u := range m.users {
		if u.Username == username {
			return u, nil
		}
	}
	return sqlc.User{}, pgx.ErrNoRows
}

func (m *adminMockQueries) GetUserByUsername(ctx context.Context, username string) (sqlc.User, error) {
	if m.getUserByUsernameFn != nil {
		return m.getUserByUsernameFn(ctx, username)
	}
	for _, u := range m.users {
		if u.Username == username {
			return u, nil
		}
	}
	return sqlc.User{}, pgx.ErrNoRows
}

func (m *adminMockQueries) GetUserByID(ctx context.Context, id uuid.UUID) (sqlc.User, error) {
	if m.getUserByIDFn != nil {
		return m.getUserByIDFn(ctx, id)
	}
	if u, ok := m.users[id]; ok {
		return u, nil
	}
	return sqlc.User{}, pgx.ErrNoRows
}

func (m *adminMockQueries) CreateUserWithPassword(ctx context.Context, arg sqlc.CreateUserWithPasswordParams) (sqlc.User, error) {
	if m.createUserWithPasswordFn != nil {
		return m.createUserWithPasswordFn(ctx, arg)
	}
	now := time.Now()
	user := sqlc.User{
		ID:           arg.ID,
		Username:     arg.Username,
		Email:        arg.Email,
		DisplayName:  arg.DisplayName,
		PasswordHash: arg.PasswordHash,
		IsAdmin:      arg.IsAdmin,
		CreatedAt:    now,
		ModifiedAt:   now,
	}
	m.users[user.ID] = user
	return user, nil
}

func (m *adminMockQueries) UpdateUserAdmin(ctx context.Context, arg sqlc.UpdateUserAdminParams) (sqlc.User, error) {
	if m.updateUserAdminFn != nil {
		return m.updateUserAdminFn(ctx, arg)
	}
	if u, ok := m.users[arg.ID]; ok {
		u.Username = arg.Username
		u.Email = arg.Email
		u.DisplayName = arg.DisplayName
		u.IsAdmin = arg.IsAdmin
		u.ModifiedAt = time.Now()
		m.users[arg.ID] = u
		return u, nil
	}
	return sqlc.User{}, pgx.ErrNoRows
}

func (m *adminMockQueries) DeactivateUser(ctx context.Context, id uuid.UUID) error {
	if m.deactivateUserFn != nil {
		return m.deactivateUserFn(ctx, id)
	}
	if u, ok := m.users[id]; ok {
		u.DeactivatedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
		m.users[id] = u
		return nil
	}
	return pgx.ErrNoRows
}

func (m *adminMockQueries) ReactivateUser(ctx context.Context, id uuid.UUID) error {
	if m.reactivateUserFn != nil {
		return m.reactivateUserFn(ctx, id)
	}
	if u, ok := m.users[id]; ok {
		u.DeactivatedAt = pgtype.Timestamptz{Valid: false}
		m.users[id] = u
		return nil
	}
	return pgx.ErrNoRows
}

func (m *adminMockQueries) CreateAPIKey(ctx context.Context, arg sqlc.CreateAPIKeyParams) (sqlc.ApiKey, error) {
	if m.createAPIKeyFn != nil {
		return m.createAPIKeyFn(ctx, arg)
	}
	keyID := uuid.New()
	key := sqlc.ApiKey{
		ID:        keyID,
		UserID:    arg.UserID,
		KeyHash:   arg.KeyHash,
		KeyPrefix: arg.KeyPrefix,
		Name:      arg.Name,
		ExpiresAt: arg.ExpiresAt,
		CreatedAt: time.Now(),
	}
	m.apiKeys[key.ID] = key
	return key, nil
}

func (m *adminMockQueries) GetAPIKeyByID(ctx context.Context, id uuid.UUID) (sqlc.ApiKey, error) {
	if m.getAPIKeyByIDFn != nil {
		return m.getAPIKeyByIDFn(ctx, id)
	}
	if k, ok := m.apiKeys[id]; ok {
		return k, nil
	}
	return sqlc.ApiKey{}, pgx.ErrNoRows
}

func (m *adminMockQueries) RevokeAPIKey(ctx context.Context, id uuid.UUID) error {
	if m.revokeAPIKeyFn != nil {
		return m.revokeAPIKeyFn(ctx, id)
	}
	if k, ok := m.apiKeys[id]; ok {
		k.RevokedAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
		m.apiKeys[id] = k
		return nil
	}
	return pgx.ErrNoRows
}

func (m *adminMockQueries) ListAPIKeysByUser(ctx context.Context, userID uuid.UUID) ([]sqlc.ListAPIKeysByUserRow, error) {
	if m.listAPIKeysByUserFn != nil {
		return m.listAPIKeysByUserFn(ctx, userID)
	}
	var result []sqlc.ListAPIKeysByUserRow
	for _, k := range m.apiKeys {
		if k.UserID == userID {
			result = append(result, sqlc.ListAPIKeysByUserRow{
				ID:        k.ID,
				KeyPrefix: k.KeyPrefix,
				Name:      k.Name,
				CreatedAt: k.CreatedAt,
			})
		}
	}
	return result, nil
}

// Stub implementations for other Querier methods
func (m *adminMockQueries) CleanupExpiredAdminSessions(ctx context.Context) error { return nil }
func (m *adminMockQueries) CountActiveAPIKeysByUser(ctx context.Context, userID uuid.UUID) (int64, error) {
	return 0, nil
}
func (m *adminMockQueries) CountActiveAdminSessions(ctx context.Context) (int64, error) { return 0, nil }
func (m *adminMockQueries) CountAuditLogs(ctx context.Context, arg sqlc.CountAuditLogsParams) (int64, error) {
	return 0, nil
}
func (m *adminMockQueries) CountGitCommits(ctx context.Context, userID uuid.UUID) (int32, error) {
	return 0, nil
}
func (m *adminMockQueries) CountJournalEntriesByType(ctx context.Context, arg sqlc.CountJournalEntriesByTypeParams) (int32, error) {
	return 0, nil
}
func (m *adminMockQueries) CountMemoryBlockVersions(ctx context.Context, arg sqlc.CountMemoryBlockVersionsParams) (int32, error) {
	return 0, nil
}
func (m *adminMockQueries) CountMemoryBlocksByUser(ctx context.Context, userID uuid.UUID) (int32, error) {
	return 0, nil
}
func (m *adminMockQueries) CountUsers(ctx context.Context) (sqlc.CountUsersRow, error) {
	return sqlc.CountUsersRow{Total: int64(len(m.users)), Active: int64(len(m.users))}, nil
}
func (m *adminMockQueries) CreateAdminSession(ctx context.Context, arg sqlc.CreateAdminSessionParams) (sqlc.AdminSession, error) {
	return sqlc.AdminSession{}, nil
}
func (m *adminMockQueries) CreateAuditLog(ctx context.Context, arg sqlc.CreateAuditLogParams) (sqlc.AuditLog, error) {
	return sqlc.AuditLog{}, nil
}
func (m *adminMockQueries) CreateGitCommit(ctx context.Context, arg sqlc.CreateGitCommitParams) (sqlc.GitCommit, error) {
	return sqlc.GitCommit{}, nil
}
func (m *adminMockQueries) CreateJournalEntry(ctx context.Context, arg sqlc.CreateJournalEntryParams) (sqlc.Journal, error) {
	return sqlc.Journal{}, nil
}
func (m *adminMockQueries) CreateMemoryBlock(ctx context.Context, arg sqlc.CreateMemoryBlockParams) (sqlc.MemoryBlock, error) {
	return sqlc.MemoryBlock{}, nil
}
func (m *adminMockQueries) CreateUser(ctx context.Context, arg sqlc.CreateUserParams) (sqlc.User, error) {
	return sqlc.User{}, nil
}
func (m *adminMockQueries) DeleteExpiredMemories(ctx context.Context) error { return nil }
func (m *adminMockQueries) DeleteJournalEntry(ctx context.Context, arg sqlc.DeleteJournalEntryParams) error {
	return nil
}
func (m *adminMockQueries) DeleteMemoryBlock(ctx context.Context, arg sqlc.DeleteMemoryBlockParams) error {
	return nil
}
func (m *adminMockQueries) DeleteUser(ctx context.Context, id uuid.UUID) error { return nil }
func (m *adminMockQueries) ExportMemories(ctx context.Context, userID uuid.UUID) ([]sqlc.ExportMemoriesRow, error) {
	return nil, nil
}
func (m *adminMockQueries) GetAPIKeyByHash(ctx context.Context, keyHash string) (sqlc.GetAPIKeyByHashRow, error) {
	return sqlc.GetAPIKeyByHashRow{}, nil
}
func (m *adminMockQueries) GetAccessibleMemoryBlocks(ctx context.Context, userID uuid.UUID) ([]sqlc.CurrentMemoryBlock, error) {
	return nil, nil
}
func (m *adminMockQueries) GetAccessibleMemoryBlocksByType(ctx context.Context, arg sqlc.GetAccessibleMemoryBlocksByTypeParams) ([]sqlc.CurrentMemoryBlock, error) {
	return nil, nil
}
func (m *adminMockQueries) SearchAccessibleMemoryBlocks(ctx context.Context, arg sqlc.SearchAccessibleMemoryBlocksParams) ([]sqlc.CurrentMemoryBlock, error) {
	return nil, nil
}
func (m *adminMockQueries) SearchAccessibleMemoryBlocksByType(ctx context.Context, arg sqlc.SearchAccessibleMemoryBlocksByTypeParams) ([]sqlc.CurrentMemoryBlock, error) {
	return nil, nil
}
func (m *adminMockQueries) GetAdminSessionByToken(ctx context.Context, tokenHash string) (sqlc.GetAdminSessionByTokenRow, error) {
	return sqlc.GetAdminSessionByTokenRow{}, nil
}
func (m *adminMockQueries) GetAdminStats(ctx context.Context) (sqlc.GetAdminStatsRow, error) {
	return sqlc.GetAdminStatsRow{
		ActiveUsers:    int64(len(m.users)),
		AdminUsers:     0,
		ActiveApiKeys:  int64(len(m.apiKeys)),
		ActiveSessions: 0,
	}, nil
}
func (m *adminMockQueries) GetAuditLogsByResource(ctx context.Context, arg sqlc.GetAuditLogsByResourceParams) ([]sqlc.AuditLog, error) {
	return nil, nil
}
func (m *adminMockQueries) GetCurrentMemoryBlockByName(ctx context.Context, arg sqlc.GetCurrentMemoryBlockByNameParams) (sqlc.CurrentMemoryBlock, error) {
	return sqlc.CurrentMemoryBlock{}, nil
}
func (m *adminMockQueries) GetCurrentMemoryBlocks(ctx context.Context, userID uuid.UUID) ([]sqlc.CurrentMemoryBlock, error) {
	return nil, nil
}
func (m *adminMockQueries) GetCurrentMemoryBlocksByTier(ctx context.Context, arg sqlc.GetCurrentMemoryBlocksByTierParams) ([]sqlc.CurrentMemoryBlock, error) {
	return nil, nil
}
func (m *adminMockQueries) GetCurrentMemoryBlocksByType(ctx context.Context, arg sqlc.GetCurrentMemoryBlocksByTypeParams) ([]sqlc.CurrentMemoryBlock, error) {
	return nil, nil
}
func (m *adminMockQueries) GetGitCommit(ctx context.Context, arg sqlc.GetGitCommitParams) (sqlc.GitCommit, error) {
	return sqlc.GitCommit{}, nil
}
func (m *adminMockQueries) GetGitCommitByHash(ctx context.Context, arg sqlc.GetGitCommitByHashParams) (sqlc.GitCommit, error) {
	return sqlc.GitCommit{}, nil
}
func (m *adminMockQueries) GetJournalEntriesByType(ctx context.Context, arg sqlc.GetJournalEntriesByTypeParams) ([]sqlc.Journal, error) {
	return nil, nil
}
func (m *adminMockQueries) GetJournalEntriesByVectorIDs(ctx context.Context, arg sqlc.GetJournalEntriesByVectorIDsParams) ([]sqlc.Journal, error) {
	return nil, nil
}
func (m *adminMockQueries) GetJournalEntry(ctx context.Context, arg sqlc.GetJournalEntryParams) (sqlc.Journal, error) {
	return sqlc.Journal{}, nil
}
func (m *adminMockQueries) GetJournalEntryByID(ctx context.Context, arg sqlc.GetJournalEntryByIDParams) (sqlc.Journal, error) {
	return sqlc.Journal{}, nil
}
func (m *adminMockQueries) GetLatestGitCommit(ctx context.Context, userID uuid.UUID) (sqlc.GitCommit, error) {
	return sqlc.GitCommit{}, nil
}
func (m *adminMockQueries) GetMemoryBlockByGUID(ctx context.Context, arg sqlc.GetMemoryBlockByGUIDParams) (sqlc.MemoryBlock, error) {
	return sqlc.MemoryBlock{}, nil
}
func (m *adminMockQueries) GetMemoryBlockByID(ctx context.Context, arg sqlc.GetMemoryBlockByIDParams) (sqlc.MemoryBlock, error) {
	return sqlc.MemoryBlock{}, nil
}
func (m *adminMockQueries) GetMemoryBlockHistory(ctx context.Context, arg sqlc.GetMemoryBlockHistoryParams) ([]sqlc.MemoryBlock, error) {
	return nil, nil
}
func (m *adminMockQueries) GetMemoryContext(ctx context.Context, userID uuid.UUID) ([]sqlc.CurrentMemoryBlock, error) {
	return nil, nil
}
func (m *adminMockQueries) GetMemoryScopeDistribution(ctx context.Context) ([]sqlc.GetMemoryScopeDistributionRow, error) {
	return nil, nil
}
func (m *adminMockQueries) GetMemoryStats(ctx context.Context) (sqlc.GetMemoryStatsRow, error) {
	return sqlc.GetMemoryStatsRow{}, nil
}
func (m *adminMockQueries) GetMemoryTypeDistribution(ctx context.Context) ([]sqlc.GetMemoryTypeDistributionRow, error) {
	return nil, nil
}
func (m *adminMockQueries) GetNextMemoryBlockVersion(ctx context.Context, arg sqlc.GetNextMemoryBlockVersionParams) (int32, error) {
	return 1, nil
}
func (m *adminMockQueries) GetOrCreateUser(ctx context.Context, arg sqlc.GetOrCreateUserParams) (sqlc.User, error) {
	return sqlc.User{}, nil
}
func (m *adminMockQueries) GetRecentDecisions(ctx context.Context, arg sqlc.GetRecentDecisionsParams) ([]sqlc.Journal, error) {
	return nil, nil
}
func (m *adminMockQueries) GetRecentSolutions(ctx context.Context, arg sqlc.GetRecentSolutionsParams) ([]sqlc.Journal, error) {
	return nil, nil
}
func (m *adminMockQueries) GetTopTags(ctx context.Context, limit int32) ([]sqlc.GetTopTagsRow, error) {
	return nil, nil
}
func (m *adminMockQueries) ListActiveAdminSessions(ctx context.Context) ([]sqlc.ListActiveAdminSessionsRow, error) {
	return nil, nil
}
func (m *adminMockQueries) ListAllAPIKeys(ctx context.Context, arg sqlc.ListAllAPIKeysParams) ([]sqlc.ListAllAPIKeysRow, error) {
	return nil, nil
}
func (m *adminMockQueries) ListAuditLogs(ctx context.Context, arg sqlc.ListAuditLogsParams) ([]sqlc.AuditLog, error) {
	return nil, nil
}
func (m *adminMockQueries) ListAuditLogsByDateRange(ctx context.Context, arg sqlc.ListAuditLogsByDateRangeParams) ([]sqlc.AuditLog, error) {
	return nil, nil
}
func (m *adminMockQueries) ListGitCommits(ctx context.Context, arg sqlc.ListGitCommitsParams) ([]sqlc.GitCommit, error) {
	return nil, nil
}
func (m *adminMockQueries) ListGitCommitsByDateRange(ctx context.Context, arg sqlc.ListGitCommitsByDateRangeParams) ([]sqlc.GitCommit, error) {
	return nil, nil
}
func (m *adminMockQueries) ListJournalEntries(ctx context.Context, arg sqlc.ListJournalEntriesParams) ([]sqlc.Journal, error) {
	return nil, nil
}
func (m *adminMockQueries) ListJournalEntriesByDateRange(ctx context.Context, arg sqlc.ListJournalEntriesByDateRangeParams) ([]sqlc.Journal, error) {
	return nil, nil
}
func (m *adminMockQueries) ListUsers(ctx context.Context, arg sqlc.ListUsersParams) ([]sqlc.User, error) {
	return nil, nil
}
func (m *adminMockQueries) ListUsersAdmin(ctx context.Context, arg sqlc.ListUsersAdminParams) ([]sqlc.User, error) {
	var users []sqlc.User
	for _, u := range m.users {
		users = append(users, u)
	}
	return users, nil
}
func (m *adminMockQueries) RevokeAdminSession(ctx context.Context, id uuid.UUID) error { return nil }
func (m *adminMockQueries) RevokeAdminSessionByToken(ctx context.Context, tokenHash string) error {
	return nil
}
func (m *adminMockQueries) RevokeAllAPIKeysByUser(ctx context.Context, userID uuid.UUID) error {
	return nil
}
func (m *adminMockQueries) RevokeAllAdminSessionsByUser(ctx context.Context, userID uuid.UUID) error {
	return nil
}
func (m *adminMockQueries) SearchJournalFullText(ctx context.Context, arg sqlc.SearchJournalFullTextParams) ([]sqlc.SearchJournalFullTextRow, error) {
	return nil, nil
}
func (m *adminMockQueries) SetJournalVectorID(ctx context.Context, arg sqlc.SetJournalVectorIDParams) error {
	return nil
}
func (m *adminMockQueries) SetUserAdmin(ctx context.Context, arg sqlc.SetUserAdminParams) error {
	return nil
}
func (m *adminMockQueries) SetUserPassword(ctx context.Context, arg sqlc.SetUserPasswordParams) error {
	return nil
}
func (m *adminMockQueries) SupersedeJournalEntry(ctx context.Context, arg sqlc.SupersedeJournalEntryParams) error {
	return nil
}
func (m *adminMockQueries) UpdateAPIKeyLastUsed(ctx context.Context, id uuid.UUID) error { return nil }
func (m *adminMockQueries) UpdateJournalEntry(ctx context.Context, arg sqlc.UpdateJournalEntryParams) (sqlc.Journal, error) {
	return sqlc.Journal{}, nil
}
func (m *adminMockQueries) UpdateMemoryBlock(ctx context.Context, arg sqlc.UpdateMemoryBlockParams) (sqlc.MemoryBlock, error) {
	return sqlc.MemoryBlock{}, nil
}
func (m *adminMockQueries) UpdateMemoryBlockScope(ctx context.Context, arg sqlc.UpdateMemoryBlockScopeParams) (sqlc.MemoryBlock, error) {
	return sqlc.MemoryBlock{}, nil
}
func (m *adminMockQueries) UpdateMemoryBlockType(ctx context.Context, arg sqlc.UpdateMemoryBlockTypeParams) (sqlc.MemoryBlock, error) {
	return sqlc.MemoryBlock{}, nil
}
func (m *adminMockQueries) UpdateUser(ctx context.Context, arg sqlc.UpdateUserParams) (sqlc.User, error) {
	return sqlc.User{}, nil
}
func (m *adminMockQueries) GetAllCurrentMemoryBlocks(ctx context.Context) ([]sqlc.CurrentMemoryBlock, error) {
	return nil, nil
}
func (m *adminMockQueries) GetAllCurrentMemoryBlocksWithOrg(ctx context.Context) ([]sqlc.GetAllCurrentMemoryBlocksWithOrgRow, error) {
	return nil, nil
}

func (m *adminMockQueries) PruneOldVersions(ctx context.Context, retainCount int32) (int64, error) {
	return 0, nil
}

// Verify adminMockQueries implements sqlc.Querier
var _ sqlc.Querier = (*adminMockQueries)(nil)

// ============================================================================
// Login Handler Tests
// ============================================================================

func TestAdminHandler_Login_Success(t *testing.T) {
	mockQueries := newAdminMockQueries()
	mockSession := &mockSessionCreator{}

	// Create an admin user with hashed password
	userID := uuid.New()
	passwordHash, _ := bcrypt.GenerateFromPassword([]byte("correctpassword"), bcrypt.DefaultCost)
	mockQueries.users[userID] = sqlc.User{
		ID:           userID,
		Username:     "admin",
		Email:        pgtype.Text{String: "admin@example.com", Valid: true},
		PasswordHash: pgtype.Text{String: string(passwordHash), Valid: true},
		IsAdmin:      true,
	}

	handler := NewAdminHandler(mockQueries, mockSession)

	req := newJSONRequest(http.MethodPost, "/api/v1/admin/login", LoginRequest{
		Username: "admin",
		Password: "correctpassword",
	})
	rec := httptest.NewRecorder()

	handler.Login(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var resp LoginResponse
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "admin", resp.Username)
	assert.True(t, resp.IsAdmin)
}

func TestAdminHandler_Login_InvalidCredentials(t *testing.T) {
	mockQueries := newAdminMockQueries()
	mockSession := &mockSessionCreator{}

	userID := uuid.New()
	passwordHash, _ := bcrypt.GenerateFromPassword([]byte("correctpassword"), bcrypt.DefaultCost)
	mockQueries.users[userID] = sqlc.User{
		ID:           userID,
		Username:     "admin",
		PasswordHash: pgtype.Text{String: string(passwordHash), Valid: true},
		IsAdmin:      true,
	}

	handler := NewAdminHandler(mockQueries, mockSession)

	req := newJSONRequest(http.MethodPost, "/api/v1/admin/login", LoginRequest{
		Username: "admin",
		Password: "wrongpassword",
	})
	rec := httptest.NewRecorder()

	handler.Login(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAdminHandler_Login_UserNotFound(t *testing.T) {
	mockQueries := newAdminMockQueries()
	mockSession := &mockSessionCreator{}
	handler := NewAdminHandler(mockQueries, mockSession)

	req := newJSONRequest(http.MethodPost, "/api/v1/admin/login", LoginRequest{
		Username: "nonexistent",
		Password: "password",
	})
	rec := httptest.NewRecorder()

	handler.Login(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestAdminHandler_Login_NotAdmin(t *testing.T) {
	mockQueries := newAdminMockQueries()
	mockSession := &mockSessionCreator{}

	userID := uuid.New()
	passwordHash, _ := bcrypt.GenerateFromPassword([]byte("password"), bcrypt.DefaultCost)
	mockQueries.users[userID] = sqlc.User{
		ID:           userID,
		Username:     "regularuser",
		PasswordHash: pgtype.Text{String: string(passwordHash), Valid: true},
		IsAdmin:      false, // Not an admin
	}

	handler := NewAdminHandler(mockQueries, mockSession)

	req := newJSONRequest(http.MethodPost, "/api/v1/admin/login", LoginRequest{
		Username: "regularuser",
		Password: "password",
	})
	rec := httptest.NewRecorder()

	handler.Login(rec, req)

	// Non-admin users can still log in (for user-facing pages like my-keys, settings)
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAdminHandler_Login_EmptyCredentials(t *testing.T) {
	mockQueries := newAdminMockQueries()
	mockSession := &mockSessionCreator{}
	handler := NewAdminHandler(mockQueries, mockSession)

	req := newJSONRequest(http.MethodPost, "/api/v1/admin/login", LoginRequest{
		Username: "",
		Password: "",
	})
	rec := httptest.NewRecorder()

	handler.Login(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// ============================================================================
// CreateUser Handler Tests
// ============================================================================

func TestAdminHandler_CreateUser_Success(t *testing.T) {
	mockQueries := newAdminMockQueries()
	mockSession := &mockSessionCreator{}
	handler := NewAdminHandler(mockQueries, mockSession)

	req := newJSONRequest(http.MethodPost, "/api/v1/admin/users", CreateUserRequest{
		Username:    "newuser",
		Email:       "newuser@example.com",
		DisplayName: "New User",
		Password:    "securepassword",
		IsAdmin:     false,
	})
	rec := httptest.NewRecorder()

	handler.CreateUser(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)

	var resp UserResponse
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "newuser", resp.Username)
	assert.Equal(t, "newuser@example.com", resp.Email)
	assert.False(t, resp.IsAdmin)
}

func TestAdminHandler_CreateUser_DuplicateUsername(t *testing.T) {
	mockQueries := newAdminMockQueries()
	mockSession := &mockSessionCreator{}

	// Pre-create user
	userID := uuid.New()
	mockQueries.users[userID] = sqlc.User{
		ID:       userID,
		Username: "existinguser",
	}

	handler := NewAdminHandler(mockQueries, mockSession)

	req := newJSONRequest(http.MethodPost, "/api/v1/admin/users", CreateUserRequest{
		Username: "existinguser",
		Password: "password",
	})
	rec := httptest.NewRecorder()

	handler.CreateUser(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code)
}

func TestAdminHandler_CreateUser_EmptyUsername(t *testing.T) {
	mockQueries := newAdminMockQueries()
	mockSession := &mockSessionCreator{}
	handler := NewAdminHandler(mockQueries, mockSession)

	req := newJSONRequest(http.MethodPost, "/api/v1/admin/users", CreateUserRequest{
		Username: "",
		Password: "password",
	})
	rec := httptest.NewRecorder()

	handler.CreateUser(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAdminHandler_CreateUser_WithAdminRole(t *testing.T) {
	mockQueries := newAdminMockQueries()
	mockSession := &mockSessionCreator{}
	handler := NewAdminHandler(mockQueries, mockSession)

	req := newJSONRequest(http.MethodPost, "/api/v1/admin/users", CreateUserRequest{
		Username: "newadmin",
		Password: "adminpassword",
		IsAdmin:  true,
	})
	rec := httptest.NewRecorder()

	handler.CreateUser(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)

	var resp UserResponse
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.True(t, resp.IsAdmin)
}

// ============================================================================
// UpdateUser Handler Tests
// ============================================================================

func TestAdminHandler_UpdateUser_Success(t *testing.T) {
	mockQueries := newAdminMockQueries()
	mockSession := &mockSessionCreator{}

	userID := uuid.New()
	mockQueries.users[userID] = sqlc.User{
		ID:       userID,
		Username: "testuser",
		IsAdmin:  false,
	}

	handler := NewAdminHandler(mockQueries, mockSession)

	isAdmin := true
	req := newJSONRequest(http.MethodPut, "/api/v1/admin/users/"+userID.String(), UpdateUserRequest{
		IsAdmin: &isAdmin,
	})
	req = withURLParam(req, "id", userID.String())
	rec := httptest.NewRecorder()

	handler.UpdateUser(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestAdminHandler_UpdateUser_NotFound(t *testing.T) {
	mockQueries := newAdminMockQueries()
	mockSession := &mockSessionCreator{}
	handler := NewAdminHandler(mockQueries, mockSession)

	nonexistentID := uuid.New()
	req := newJSONRequest(http.MethodPut, "/api/v1/admin/users/"+nonexistentID.String(), UpdateUserRequest{})
	req = withURLParam(req, "id", nonexistentID.String())
	rec := httptest.NewRecorder()

	handler.UpdateUser(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAdminHandler_UpdateUser_InvalidID(t *testing.T) {
	mockQueries := newAdminMockQueries()
	mockSession := &mockSessionCreator{}
	handler := NewAdminHandler(mockQueries, mockSession)

	req := newJSONRequest(http.MethodPut, "/api/v1/admin/users/invalid-id", UpdateUserRequest{})
	req = withURLParam(req, "id", "invalid-id")
	rec := httptest.NewRecorder()

	handler.UpdateUser(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// ============================================================================
// DeactivateUser Handler Tests
// ============================================================================

func TestAdminHandler_DeactivateUser_Success(t *testing.T) {
	mockQueries := newAdminMockQueries()
	mockSession := &mockSessionCreator{}

	userID := uuid.New()
	mockQueries.users[userID] = sqlc.User{
		ID:       userID,
		Username: "userToDeactivate",
	}

	handler := NewAdminHandler(mockQueries, mockSession)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/users/"+userID.String(), nil)
	req = withURLParam(req, "id", userID.String())
	rec := httptest.NewRecorder()

	handler.DeactivateUser(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestAdminHandler_DeactivateUser_NotFound(t *testing.T) {
	mockQueries := newAdminMockQueries()
	mockSession := &mockSessionCreator{}
	handler := NewAdminHandler(mockQueries, mockSession)

	nonexistentID := uuid.New()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/users/"+nonexistentID.String(), nil)
	req = withURLParam(req, "id", nonexistentID.String())
	rec := httptest.NewRecorder()

	handler.DeactivateUser(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// ============================================================================
// ReactivateUser Handler Tests
// ============================================================================

func TestAdminHandler_ReactivateUser_Success(t *testing.T) {
	mockQueries := newAdminMockQueries()
	mockSession := &mockSessionCreator{}

	userID := uuid.New()
	mockQueries.users[userID] = sqlc.User{
		ID:            userID,
		Username:      "deactivatedUser",
		DeactivatedAt: pgtype.Timestamptz{Time: time.Now(), Valid: true},
	}

	handler := NewAdminHandler(mockQueries, mockSession)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/users/"+userID.String()+"/reactivate", nil)
	req = withURLParam(req, "id", userID.String())
	rec := httptest.NewRecorder()

	handler.ReactivateUser(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

// ============================================================================
// CreateAPIKey Handler Tests
// ============================================================================

func TestAdminHandler_CreateAPIKey_Success(t *testing.T) {
	mockQueries := newAdminMockQueries()
	mockSession := &mockSessionCreator{}

	userID := uuid.New()
	mockQueries.users[userID] = sqlc.User{
		ID:       userID,
		Username: "testuser",
	}

	handler := NewAdminHandler(mockQueries, mockSession)

	req := newJSONRequest(http.MethodPost, "/api/v1/admin/users/"+userID.String()+"/keys", CreateAPIKeyRequest{
		Name: "Test Key",
	})
	req = withURLParam(req, "id", userID.String())
	rec := httptest.NewRecorder()

	handler.CreateAPIKey(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)

	var resp CreateAPIKeyResponse
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "Test Key", resp.Name)
	assert.NotEmpty(t, resp.Key)
	assert.True(t, len(resp.Key) > 20) // Key should be substantial
}

func TestAdminHandler_CreateAPIKey_UserNotFound(t *testing.T) {
	mockQueries := newAdminMockQueries()
	mockSession := &mockSessionCreator{}
	handler := NewAdminHandler(mockQueries, mockSession)

	nonexistentID := uuid.New()
	req := newJSONRequest(http.MethodPost, "/api/v1/admin/users/"+nonexistentID.String()+"/keys", CreateAPIKeyRequest{
		Name: "Test Key",
	})
	req = withURLParam(req, "id", nonexistentID.String())
	rec := httptest.NewRecorder()

	handler.CreateAPIKey(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestAdminHandler_CreateAPIKey_EmptyName(t *testing.T) {
	mockQueries := newAdminMockQueries()
	mockSession := &mockSessionCreator{}

	userID := uuid.New()
	mockQueries.users[userID] = sqlc.User{ID: userID, Username: "testuser"}

	handler := NewAdminHandler(mockQueries, mockSession)

	req := newJSONRequest(http.MethodPost, "/api/v1/admin/users/"+userID.String()+"/keys", CreateAPIKeyRequest{
		Name: "",
	})
	req = withURLParam(req, "id", userID.String())
	rec := httptest.NewRecorder()

	handler.CreateAPIKey(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAdminHandler_CreateAPIKey_WithExpiration(t *testing.T) {
	mockQueries := newAdminMockQueries()
	mockSession := &mockSessionCreator{}

	userID := uuid.New()
	mockQueries.users[userID] = sqlc.User{ID: userID, Username: "testuser"}

	handler := NewAdminHandler(mockQueries, mockSession)

	req := newJSONRequest(http.MethodPost, "/api/v1/admin/users/"+userID.String()+"/keys", CreateAPIKeyRequest{
		Name:      "Expiring Key",
		ExpiresIn: "30d",
	})
	req = withURLParam(req, "id", userID.String())
	rec := httptest.NewRecorder()

	handler.CreateAPIKey(rec, req)

	assert.Equal(t, http.StatusCreated, rec.Code)
}

// ============================================================================
// RevokeAPIKey Handler Tests
// ============================================================================

func TestAdminHandler_RevokeAPIKey_Success(t *testing.T) {
	mockQueries := newAdminMockQueries()
	mockSession := &mockSessionCreator{}

	keyID := uuid.New()
	userID := uuid.New()
	mockQueries.apiKeys[keyID] = sqlc.ApiKey{
		ID:     keyID,
		UserID: userID,
		Name:   "Key to revoke",
	}

	handler := NewAdminHandler(mockQueries, mockSession)

	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/keys/"+keyID.String(), nil)
	req = withURLParam(req, "id", keyID.String())
	rec := httptest.NewRecorder()

	handler.RevokeAPIKey(rec, req)

	assert.Equal(t, http.StatusNoContent, rec.Code)
}

func TestAdminHandler_RevokeAPIKey_NotFound(t *testing.T) {
	mockQueries := newAdminMockQueries()
	mockSession := &mockSessionCreator{}
	handler := NewAdminHandler(mockQueries, mockSession)

	nonexistentID := uuid.New()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/keys/"+nonexistentID.String(), nil)
	req = withURLParam(req, "id", nonexistentID.String())
	rec := httptest.NewRecorder()

	handler.RevokeAPIKey(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// ============================================================================
// GetStats Handler Tests
// ============================================================================

func TestAdminHandler_GetStats_Success(t *testing.T) {
	mockQueries := newAdminMockQueries()
	mockSession := &mockSessionCreator{}

	// Add some users
	mockQueries.users[uuid.New()] = sqlc.User{Username: "user1"}
	mockQueries.users[uuid.New()] = sqlc.User{Username: "user2"}

	handler := NewAdminHandler(mockQueries, mockSession)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/stats", nil)
	rec := httptest.NewRecorder()

	handler.GetStats(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)

	var stats map[string]any
	err := json.Unmarshal(rec.Body.Bytes(), &stats)
	require.NoError(t, err)
	assert.Equal(t, float64(2), stats["active_users"])
}
