package testutil

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/thinkwright/chapterhouse/ch-server/internal/auth"
	"github.com/thinkwright/chapterhouse/ch-server/internal/embedding"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository/sqlc"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

// MockEmbeddingProvider implements embedding.Provider for testing.
type MockEmbeddingProvider struct {
	Mu         sync.Mutex
	EmbedFunc  func(ctx context.Context, text string) ([]float32, error)
	Calls      []string
	dimensions int
}

func NewMockEmbeddingProvider(dimensions int) *MockEmbeddingProvider {
	return &MockEmbeddingProvider{
		dimensions: dimensions,
		EmbedFunc: func(ctx context.Context, text string) ([]float32, error) {
			vec := make([]float32, dimensions)
			for i := range vec {
				vec[i] = float32(len(text)%10) / 10.0
			}
			return vec, nil
		},
	}
}

func (m *MockEmbeddingProvider) Embed(ctx context.Context, text string) ([]float32, error) {
	m.Mu.Lock()
	m.Calls = append(m.Calls, text)
	m.Mu.Unlock()
	return m.EmbedFunc(ctx, text)
}

func (m *MockEmbeddingProvider) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	results := make([][]float32, len(texts))
	for i, text := range texts {
		vec, err := m.Embed(ctx, text)
		if err != nil {
			return nil, err
		}
		results[i] = vec
	}
	return results, nil
}

func (m *MockEmbeddingProvider) Dimensions() int { return m.dimensions }
func (m *MockEmbeddingProvider) Name() string     { return "mock" }

var _ embedding.Provider = (*MockEmbeddingProvider)(nil)

// ErrorEmbeddingProvider returns errors for testing error paths.
type ErrorEmbeddingProvider struct {
	Err error
}

func NewErrorEmbeddingProvider(err error) *ErrorEmbeddingProvider {
	return &ErrorEmbeddingProvider{Err: err}
}

func (e *ErrorEmbeddingProvider) Embed(context.Context, string) ([]float32, error) {
	return nil, e.Err
}
func (e *ErrorEmbeddingProvider) EmbedBatch(context.Context, []string) ([][]float32, error) {
	return nil, e.Err
}
func (e *ErrorEmbeddingProvider) Dimensions() int { return 384 }
func (e *ErrorEmbeddingProvider) Name() string     { return "error-mock" }

var _ embedding.Provider = (*ErrorEmbeddingProvider)(nil)

// MockQueries implements sqlc.Querier for testing.
// Embeds NoopQueries for all methods that don't need real logic.
type MockQueries struct {
	NoopQueries
	Mu           sync.Mutex
	memoryBlocks map[string][]sqlc.MemoryBlock // key: userID
	nextID       int64
	nextVersion  map[string]int32 // key: userID:name
}

func NewMockQueries() *MockQueries {
	return &MockQueries{
		memoryBlocks: make(map[string][]sqlc.MemoryBlock),
		nextID:       1,
		nextVersion:  make(map[string]int32),
	}
}

func (m *MockQueries) GetNextMemoryBlockVersion(ctx context.Context, arg sqlc.GetNextMemoryBlockVersionParams) (int32, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()

	key := arg.UserID.String() + ":" + arg.Name
	version := m.nextVersion[key]
	m.nextVersion[key] = version + 1
	return version + 1, nil
}

func (m *MockQueries) PruneOldVersions(ctx context.Context, retainCount int32) error {
	return nil
}

func (m *MockQueries) CreateMemoryBlock(ctx context.Context, arg sqlc.CreateMemoryBlockParams) (sqlc.MemoryBlock, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()

	block := sqlc.MemoryBlock{
		ID:         m.nextID,
		UserID:     arg.UserID,
		Name:       arg.Name,
		Tier:       arg.Tier,
		Value:      arg.Value,
		Version:    arg.Version,
		SortOrder:  arg.SortOrder,
		Tags:       arg.Tags,
		SessionID:  arg.SessionID,
		MemoryType: arg.MemoryType,
		Scope:      arg.Scope,
		CreatedAt:  time.Now(),
		ModifiedAt: time.Now(),
	}
	m.nextID++

	userKey := arg.UserID.String()
	m.memoryBlocks[userKey] = append(m.memoryBlocks[userKey], block)

	return block, nil
}

func (m *MockQueries) GetCurrentMemoryBlocks(ctx context.Context, userID uuid.UUID) ([]sqlc.CurrentMemoryBlock, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()

	return m.currentBlocks(userID), nil
}

// GetAllBlocks returns all stored memory blocks across all users (test helper, not a Querier method).
func (m *MockQueries) GetAllBlocks() []sqlc.MemoryBlock {
	m.Mu.Lock()
	defer m.Mu.Unlock()

	var all []sqlc.MemoryBlock
	for _, blocks := range m.memoryBlocks {
		all = append(all, blocks...)
	}
	return all
}

func (m *MockQueries) DeleteMemoryBlock(ctx context.Context, arg sqlc.DeleteMemoryBlockParams) error {
	m.Mu.Lock()
	defer m.Mu.Unlock()

	userKey := arg.UserID.String()
	var kept []sqlc.MemoryBlock
	for _, b := range m.memoryBlocks[userKey] {
		if b.Name != arg.Name {
			kept = append(kept, b)
		}
	}
	m.memoryBlocks[userKey] = kept
	return nil
}

func (m *MockQueries) GetMemoryBlockByID(ctx context.Context, arg sqlc.GetMemoryBlockByIDParams) (sqlc.MemoryBlock, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()

	for _, b := range m.memoryBlocks[arg.UserID.String()] {
		if b.ID == arg.ID {
			return b, nil
		}
	}
	return sqlc.MemoryBlock{}, errors.New("no rows in result set")
}

func (m *MockQueries) GetMemoryContext(ctx context.Context, userID uuid.UUID) ([]sqlc.CurrentMemoryBlock, error) {
	return m.GetCurrentMemoryBlocks(ctx, userID)
}

func (m *MockQueries) GetOrCreateUser(ctx context.Context, arg sqlc.GetOrCreateUserParams) (sqlc.User, error) {
	return sqlc.User{ID: arg.ID, Username: arg.Username}, nil
}

func (m *MockQueries) GetUserByID(ctx context.Context, id uuid.UUID) (sqlc.User, error) {
	return sqlc.User{ID: id}, nil
}

func (m *MockQueries) CreateAPIKey(ctx context.Context, arg sqlc.CreateAPIKeyParams) (sqlc.ApiKey, error) {
	return sqlc.ApiKey{ID: uuid.New(), UserID: arg.UserID, KeyHash: arg.KeyHash, KeyPrefix: arg.KeyPrefix, Name: arg.Name}, nil
}

func (m *MockQueries) CreateAdminSession(ctx context.Context, arg sqlc.CreateAdminSessionParams) (sqlc.AdminSession, error) {
	return sqlc.AdminSession{ID: uuid.New(), UserID: arg.UserID}, nil
}

func (m *MockQueries) CreateUserWithPassword(ctx context.Context, arg sqlc.CreateUserWithPasswordParams) (sqlc.User, error) {
	return sqlc.User{ID: arg.ID, Username: arg.Username, IsAdmin: arg.IsAdmin}, nil
}

func (m *MockQueries) GetAccessibleMemoryBlocks(ctx context.Context, userID uuid.UUID) ([]sqlc.CurrentMemoryBlock, error) {
	return m.GetCurrentMemoryBlocks(ctx, userID)
}

func (m *MockQueries) SearchAccessibleMemoryBlocks(ctx context.Context, arg sqlc.SearchAccessibleMemoryBlocksParams) ([]sqlc.CurrentMemoryBlock, error) {
	return m.GetCurrentMemoryBlocks(ctx, arg.UserID)
}

func (m *MockQueries) ListUserSessions(ctx context.Context, arg sqlc.ListUserSessionsParams) ([]sqlc.ListUserSessionsRow, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()

	blocks := m.currentBlocks(arg.UserID)
	sessions := make(map[pgtype.UUID]*sqlc.ListUserSessionsRow)
	for _, b := range blocks {
		if !b.SessionID.Valid {
			continue
		}
		key := b.SessionID
		if s, ok := sessions[key]; ok {
			s.MemoryCount++
			if b.CreatedAt.Before(s.FirstActivity) {
				s.FirstActivity = b.CreatedAt
			}
			if b.CreatedAt.After(s.LastActivity) {
				s.LastActivity = b.CreatedAt
			}
		} else {
			sessions[key] = &sqlc.ListUserSessionsRow{
				SessionID:     b.SessionID,
				MemoryCount:   1,
				FirstActivity: b.CreatedAt,
				LastActivity:  b.CreatedAt,
			}
		}
	}

	var result []sqlc.ListUserSessionsRow
	for _, s := range sessions {
		result = append(result, *s)
	}
	if int32(len(result)) > arg.ResultLimit {
		result = result[:arg.ResultLimit]
	}
	return result, nil
}

func (m *MockQueries) GetSessionMemories(ctx context.Context, arg sqlc.GetSessionMemoriesParams) ([]sqlc.CurrentMemoryBlock, error) {
	m.Mu.Lock()
	defer m.Mu.Unlock()

	blocks := m.currentBlocks(arg.UserID)
	var result []sqlc.CurrentMemoryBlock
	for _, b := range blocks {
		if b.SessionID.Valid && b.SessionID == arg.SessionID {
			result = append(result, b)
		}
	}
	return result, nil
}

// currentBlocks returns deduplicated current blocks for a user (must hold m.Mu).
func (m *MockQueries) currentBlocks(userID uuid.UUID) []sqlc.CurrentMemoryBlock {
	latest := make(map[string]sqlc.MemoryBlock)
	for _, b := range m.memoryBlocks[userID.String()] {
		if existing, ok := latest[b.Name]; !ok || b.Version > existing.Version {
			latest[b.Name] = b
		}
	}

	var result []sqlc.CurrentMemoryBlock
	for _, b := range latest {
		result = append(result, sqlc.CurrentMemoryBlock{
			ID:         b.ID,
			UserID:     b.UserID,
			Name:       b.Name,
			Tier:       b.Tier,
			Value:      b.Value,
			Tags:       b.Tags,
			Version:    b.Version,
			SortOrder:  b.SortOrder,
			SessionID:  b.SessionID,
			MemoryType: b.MemoryType,
			Scope:      b.Scope,
			CreatedAt:  b.CreatedAt,
			ModifiedAt: b.ModifiedAt,
		})
	}
	return result
}

var _ sqlc.Querier = (*MockQueries)(nil)

// MockAuthProvider implements auth.Provider for testing.
type MockAuthProvider struct {
	AuthFunc func(r any) (*auth.Context, error)
	UserID   uuid.UUID
}

func NewMockAuthProvider(userID uuid.UUID) *MockAuthProvider {
	return &MockAuthProvider{UserID: userID}
}

// TestOrgID is a default org UUID for testing.
var TestOrgID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

func (m *MockAuthProvider) Authenticate(r any) (*auth.Context, error) {
	if m.AuthFunc != nil {
		return m.AuthFunc(r)
	}
	return &auth.Context{
		UserID:   m.UserID,
		OrgID:    TestOrgID,
		Username: "testuser",
		Email:    "test@example.com",
	}, nil
}

// Helper functions

func NewTestAuthContext(userID uuid.UUID) *auth.Context {
	return &auth.Context{
		UserID:   userID,
		OrgID:    TestOrgID,
		Username: "testuser",
		Email:    "test@example.com",
	}
}

func NewTestMemoryBlock(id int64, userID uuid.UUID, name, value string) sqlc.CurrentMemoryBlock {
	return sqlc.CurrentMemoryBlock{
		ID:        id,
		UserID:    userID,
		Name:      name,
		Tier:      "index",
		Value:     pgtype.Text{String: value, Valid: true},
		Version:   1,
		SortOrder: 0,
	}
}

// ErrQueryFailed is a standard error for testing query failures.
var ErrQueryFailed = errors.New("query failed")

func CreateMemoryBlockParams(userID uuid.UUID, name, value string) sqlc.CreateMemoryBlockParams {
	return sqlc.CreateMemoryBlockParams{
		UserID:    userID,
		Name:      name,
		Tier:      "index",
		Value:     pgtype.Text{String: value, Valid: true},
		Version:   1,
		SortOrder: 0,
	}
}
