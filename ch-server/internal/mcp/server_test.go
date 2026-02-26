package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thinkwright/chapterhouse/ch-server/internal/auth"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestServer(t *testing.T) (*Server, *testutil.MockQueries, *testutil.MockEmbeddingProvider, *testutil.MockVectorDB) {
	queries := testutil.NewMockQueries()
	embedder := testutil.NewMockEmbeddingProvider(384)
	vectorDB := testutil.NewMockVectorDB()
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelError}))

	server := NewServer(queries, logger, embedder, vectorDB)
	return server, queries, embedder, vectorDB
}

func TestServer_Tools(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	tools := server.Tools()

	assert.Len(t, tools, 6)

	toolNames := make(map[string]bool)
	for _, tool := range tools {
		toolNames[tool.Name] = true
	}

	assert.True(t, toolNames["remember"], "remember tool should exist")
	assert.True(t, toolNames["share_memory"], "share_memory tool should exist")
	assert.True(t, toolNames["recall"], "recall tool should exist")
	assert.True(t, toolNames["forget"], "forget tool should exist")
	assert.True(t, toolNames["list_memories"], "list_memories tool should exist")
	assert.True(t, toolNames["export_memories"], "export_memories tool should exist")
}

func TestServer_HandleRequest_Initialize(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
	}

	resp := server.HandleRequest(authCtx, req)

	assert.Equal(t, "2.0", resp.JSONRPC)
	assert.Equal(t, 1, resp.ID)
	assert.Nil(t, resp.Error)

	result, ok := resp.Result.(InitializeResult)
	require.True(t, ok)
	assert.Equal(t, "2024-11-05", result.ProtocolVersion)
	assert.Equal(t, "chapterhouse", result.ServerInfo.Name)
	assert.NotNil(t, result.Capabilities.Tools)
}

func TestServer_HandleRequest_ToolsList(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	req := Request{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/list",
	}

	resp := server.HandleRequest(authCtx, req)

	assert.Equal(t, "2.0", resp.JSONRPC)
	assert.Nil(t, resp.Error)

	result, ok := resp.Result.(ToolsListResult)
	require.True(t, ok)
	assert.Len(t, result.Tools, 6)
}

func TestServer_HandleRequest_MethodNotFound(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	req := Request{
		JSONRPC: "2.0",
		ID:      3,
		Method:  "unknown/method",
	}

	resp := server.HandleRequest(authCtx, req)

	assert.Equal(t, "2.0", resp.JSONRPC)
	assert.NotNil(t, resp.Error)
	assert.Equal(t, -32601, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "Method not found")
}

func TestServer_HandleRequest_NotificationsInitialized(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	req := Request{
		JSONRPC: "2.0",
		Method:  "notifications/initialized",
	}

	resp := server.HandleRequest(authCtx, req)

	// Notifications should return empty response
	assert.Nil(t, resp.ID)
	assert.Nil(t, resp.Result)
	assert.Nil(t, resp.Error)
}

func TestServer_Remember(t *testing.T) {
	server, queries, _, _ := newTestServer(t)
	userID := uuid.New()
	authCtx := testutil.NewTestAuthContext(userID)

	params := CallToolParams{
		Name: "remember",
		Arguments: map[string]any{
			"fact": "The database is PostgreSQL 15",
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)

	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.False(t, result.IsError)
	assert.Len(t, result.Content, 1)
	assert.Contains(t, result.Content[0].Text, "Remembered")
	assert.Contains(t, result.Content[0].Text, "id=1")

	// Verify block was created
	blocks, err := queries.GetCurrentMemoryBlocks(nil, userID)
	require.NoError(t, err)
	assert.Len(t, blocks, 1)
	assert.Contains(t, blocks[0].Value.String, "PostgreSQL 15")
}

func TestServer_Remember_WithTags(t *testing.T) {
	server, queries, _, _ := newTestServer(t)
	userID := uuid.New()
	authCtx := testutil.NewTestAuthContext(userID)

	params := CallToolParams{
		Name: "remember",
		Arguments: map[string]any{
			"fact": "Use pgx driver for PostgreSQL",
			"tags": []any{"database", "go"},
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)

	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.False(t, result.IsError)

	// Verify tags are stored in the tags column (not prepended to value)
	blocks, err := queries.GetCurrentMemoryBlocks(nil, userID)
	require.NoError(t, err)
	assert.Len(t, blocks, 1)
	assert.Equal(t, "Use pgx driver for PostgreSQL", blocks[0].Value.String)
	assert.Contains(t, blocks[0].Tags, "database")
	assert.Contains(t, blocks[0].Tags, "go")
}

func TestServer_Remember_EmptyFact(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	params := CallToolParams{
		Name: "remember",
		Arguments: map[string]any{
			"fact": "",
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)

	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "fact is required")
}

func TestServer_Remember_RejectsSecrets(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	tests := []struct {
		name string
		fact string
	}{
		{"OpenAI key", "My API key is sk-abcdefghijklmnopqrstuvwx"},
		{"AWS key", "AWS key: AKIAIOSFODNN7EXAMPLE"},
		{"Private key", "-----BEGIN RSA PRIVATE KEY-----"},
		{"Database URL", "postgres://admin:secret@db.example.com:5432/prod"},
		{"JWT token", "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			params := CallToolParams{
				Name: "remember",
				Arguments: map[string]any{
					"fact": tc.fact,
				},
			}
			paramsJSON, _ := json.Marshal(params)

			req := Request{
				JSONRPC: "2.0",
				ID:      1,
				Method:  "tools/call",
				Params:  paramsJSON,
			}

			resp := server.HandleRequest(authCtx, req)

			require.Nil(t, resp.Error)
			result, ok := resp.Result.(CallToolResult)
			require.True(t, ok)
			assert.True(t, result.IsError)
			assert.Contains(t, result.Content[0].Text, "Memory rejected")
		})
	}
}

func TestServer_Remember_AllowsSafeContent(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	userID := uuid.New()
	authCtx := testutil.NewTestAuthContext(userID)

	params := CallToolParams{
		Name: "remember",
		Arguments: map[string]any{
			"fact": "The database is PostgreSQL 15 on AWS using IAM roles",
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)
	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "Remembered")
}

func TestServer_Remember_VectorEmbedding(t *testing.T) {
	server, _, embedder, vectorDB := newTestServer(t)
	userID := uuid.New()
	authCtx := testutil.NewTestAuthContext(userID)

	params := CallToolParams{
		Name: "remember",
		Arguments: map[string]any{
			"fact": "Vector search is enabled",
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)
	require.Nil(t, resp.Error)

	// Wait for async embedding goroutine
	time.Sleep(100 * time.Millisecond)

	// Verify embedding was generated
	embedder.Mu.Lock()
	assert.GreaterOrEqual(t, len(embedder.Calls), 1)
	embedder.Mu.Unlock()

	// Verify point was stored in vector DB
	vectorDB.Mu.Lock()
	assert.GreaterOrEqual(t, len(vectorDB.Points), 1)
	vectorDB.Mu.Unlock()
}

func TestServer_Recall_Semantic(t *testing.T) {
	server, queries, _, vectorDB := newTestServer(t)
	userID := uuid.New()
	authCtx := testutil.NewTestAuthContext(userID)

	// Pre-populate vector DB
	vectorDB.Mu.Lock()
	vectorDB.Points["test-1"] = testutil.MockVectorPoint(userID, 1, "PostgreSQL is the database")
	vectorDB.Mu.Unlock()

	params := CallToolParams{
		Name: "recall",
		Arguments: map[string]any{
			"query": "What database do we use?",
			"mode":  "semantic",
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)

	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "PostgreSQL")

	// Verify search was called
	vectorDB.Mu.Lock()
	assert.Len(t, vectorDB.Searches, 1)
	assert.Equal(t, userID, vectorDB.Searches[0].UserID)
	vectorDB.Mu.Unlock()

	_ = queries // silence unused
}

func TestServer_Recall_Keyword(t *testing.T) {
	server, queries, _, _ := newTestServer(t)
	userID := uuid.New()
	authCtx := testutil.NewTestAuthContext(userID)

	// Create a memory block
	queries.CreateMemoryBlock(nil, testutil.CreateMemoryBlockParams(userID, "test-memory", "Redis is used for caching"))

	params := CallToolParams{
		Name: "recall",
		Arguments: map[string]any{
			"query": "redis",
			"mode":  "keyword",
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)

	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "Redis")
	assert.Contains(t, result.Content[0].Text, "keyword")
}

func TestServer_Recall_EmptyQuery(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	params := CallToolParams{
		Name: "recall",
		Arguments: map[string]any{
			"query": "",
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)

	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "query is required")
}

func TestServer_Recall_NoResults(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	params := CallToolParams{
		Name: "recall",
		Arguments: map[string]any{
			"query": "nonexistent term",
			"mode":  "keyword",
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)

	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "No matching memories found")
}

func TestServer_Recall_WithLimit(t *testing.T) {
	server, queries, _, _ := newTestServer(t)
	userID := uuid.New()
	authCtx := testutil.NewTestAuthContext(userID)

	// Create multiple memories
	for i := 0; i < 5; i++ {
		queries.CreateMemoryBlock(nil, testutil.CreateMemoryBlockParams(
			userID,
			strings.ReplaceAll("test-memory-"+string(rune('a'+i)), " ", "_"),
			"Test fact about testing",
		))
	}

	params := CallToolParams{
		Name: "recall",
		Arguments: map[string]any{
			"query": "test",
			"mode":  "keyword",
			"limit": float64(2), // JSON numbers are float64
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)

	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.False(t, result.IsError)

	// Count lines (results)
	lines := strings.Split(result.Content[0].Text, "\n")
	assert.LessOrEqual(t, len(lines), 2)
}

func TestServer_Forget(t *testing.T) {
	server, queries, _, _ := newTestServer(t)
	userID := uuid.New()
	authCtx := testutil.NewTestAuthContext(userID)

	// Create a memory block
	block, err := queries.CreateMemoryBlock(nil, testutil.CreateMemoryBlockParams(userID, "test-memory", "Something to forget"))
	require.NoError(t, err)

	params := CallToolParams{
		Name: "forget",
		Arguments: map[string]any{
			"fact_id": float64(block.ID),
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)

	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "Removed memory")

	// Verify block was deleted
	blocks, err := queries.GetCurrentMemoryBlocks(nil, userID)
	require.NoError(t, err)
	assert.Len(t, blocks, 0)
}

func TestServer_Forget_NotFound(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	params := CallToolParams{
		Name: "forget",
		Arguments: map[string]any{
			"fact_id": float64(999),
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)

	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "not found")
}

func TestServer_Forget_MissingID(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	params := CallToolParams{
		Name:      "forget",
		Arguments: map[string]any{},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)

	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "fact_id is required")
}

func TestServer_ListMemories(t *testing.T) {
	server, queries, _, _ := newTestServer(t)
	userID := uuid.New()
	authCtx := testutil.NewTestAuthContext(userID)

	// Create some memories
	queries.CreateMemoryBlock(nil, testutil.CreateMemoryBlockParams(userID, "memory-1", "First memory"))
	queries.CreateMemoryBlock(nil, testutil.CreateMemoryBlockParams(userID, "memory-2", "Second memory"))

	params := CallToolParams{
		Name:      "list_memories",
		Arguments: map[string]any{},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)

	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "First memory")
	assert.Contains(t, result.Content[0].Text, "Second memory")
}

func TestServer_ListMemories_Empty(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	params := CallToolParams{
		Name:      "list_memories",
		Arguments: map[string]any{},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)

	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "No memories found")
}

func TestServer_ListMemories_FilterByTags(t *testing.T) {
	server, queries, _, _ := newTestServer(t)
	userID := uuid.New()
	authCtx := testutil.NewTestAuthContext(userID)

	// Create memories with tags
	p1 := testutil.CreateMemoryBlockParams(userID, "memory-1", "PostgreSQL config")
	p1.Tags = []string{"database"}
	queries.CreateMemoryBlock(nil, p1)
	p2 := testutil.CreateMemoryBlockParams(userID, "memory-2", "REST endpoints")
	p2.Tags = []string{"api"}
	queries.CreateMemoryBlock(nil, p2)

	params := CallToolParams{
		Name: "list_memories",
		Arguments: map[string]any{
			"tags": []any{"database"},
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)

	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "PostgreSQL")
	assert.NotContains(t, result.Content[0].Text, "REST endpoints")
}

func TestServer_UnknownTool(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	params := CallToolParams{
		Name:      "unknown_tool",
		Arguments: map[string]any{},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)

	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "Unknown tool")
}

func TestServer_InvalidParams(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  json.RawMessage(`invalid json`),
	}

	resp := server.HandleRequest(authCtx, req)

	assert.NotNil(t, resp.Error)
	assert.Equal(t, -32602, resp.Error.Code)
	assert.Contains(t, resp.Error.Message, "Invalid params")
}

func TestSanitizeName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"Hello World", "hello_world"},
		{"PostgreSQL 15", "postgresql_15"},
		{"Use pgx-driver!", "use_pgx-driver"},
		{"Test@#$%Special", "testspecial"},
		{"UPPERCASE", "uppercase"},
		{"a very long string that exceeds fifty characters limit should be truncated here", "a_very_long_string_that_exceeds_fifty_characters_l"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			result := sanitizeName(tc.input)
			assert.Equal(t, tc.expected, result)
		})
	}
}

func TestServer_UserIsolation(t *testing.T) {
	server, queries, _, _ := newTestServer(t)
	user1 := uuid.New()
	user2 := uuid.New()
	authCtx1 := testutil.NewTestAuthContext(user1)
	authCtx2 := testutil.NewTestAuthContext(user2)

	// User 1 creates a memory
	params1 := CallToolParams{
		Name: "remember",
		Arguments: map[string]any{
			"fact": "User 1 secret",
		},
	}
	paramsJSON1, _ := json.Marshal(params1)

	req1 := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON1,
	}
	server.HandleRequest(authCtx1, req1)

	// User 2 lists memories - should not see User 1's memory
	params2 := CallToolParams{
		Name:      "list_memories",
		Arguments: map[string]any{},
	}
	paramsJSON2, _ := json.Marshal(params2)

	req2 := Request{
		JSONRPC: "2.0",
		ID:      2,
		Method:  "tools/call",
		Params:  paramsJSON2,
	}
	resp2 := server.HandleRequest(authCtx2, req2)

	result, ok := resp2.Result.(CallToolResult)
	require.True(t, ok)
	assert.Contains(t, result.Content[0].Text, "No memories found")

	// Verify User 1 can see their own memory
	resp1 := server.HandleRequest(authCtx1, req2)
	result1, ok := resp1.Result.(CallToolResult)
	require.True(t, ok)
	assert.Contains(t, result1.Content[0].Text, "User 1 secret")

	_ = queries
}

// ============================================================================
// StdioHandler Tests
// ============================================================================

func TestStdioHandler_Run(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	userID := uuid.New()
	authCtx := testutil.NewTestAuthContext(userID)

	handler := NewStdioHandler(server, authCtx)

	// Prepare input with initialize request
	initReq := `{"jsonrpc":"2.0","id":1,"method":"initialize"}`
	input := bytes.NewBufferString(initReq + "\n")
	output := &bytes.Buffer{}

	// Run in goroutine since it blocks on scanner
	done := make(chan struct{})
	go func() {
		handler.Run(input, output)
		close(done)
	}()

	// Wait for processing
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
	}

	// Verify output contains response
	outStr := output.String()
	assert.Contains(t, outStr, "jsonrpc")
	assert.Contains(t, outStr, "2024-11-05") // protocol version
}

func TestStdioHandler_ParseError(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	handler := NewStdioHandler(server, authCtx)

	input := bytes.NewBufferString("invalid json\n")
	output := &bytes.Buffer{}

	done := make(chan struct{})
	go func() {
		handler.Run(input, output)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
	}

	outStr := output.String()
	assert.Contains(t, outStr, "Parse error")
	assert.Contains(t, outStr, "-32700")
}

func TestStdioHandler_EmptyLines(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	handler := NewStdioHandler(server, authCtx)

	// Empty lines should be skipped
	input := bytes.NewBufferString("\n\n")
	output := &bytes.Buffer{}

	done := make(chan struct{})
	go func() {
		handler.Run(input, output)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
	}

	// No output for empty lines
	assert.Empty(t, output.String())
}

func TestStdioHandler_Notification(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	handler := NewStdioHandler(server, authCtx)

	// Notification should not produce response
	notifReq := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	input := bytes.NewBufferString(notifReq + "\n")
	output := &bytes.Buffer{}

	done := make(chan struct{})
	go func() {
		handler.Run(input, output)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
	}

	// No output for notifications
	assert.Empty(t, output.String())
}

func TestStdioHandler_ToolsCall(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	userID := uuid.New()
	authCtx := testutil.NewTestAuthContext(userID)

	handler := NewStdioHandler(server, authCtx)

	// Call remember tool
	rememberReq := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"remember","arguments":{"fact":"test fact"}}}`
	input := bytes.NewBufferString(rememberReq + "\n")
	output := &bytes.Buffer{}

	done := make(chan struct{})
	go func() {
		handler.Run(input, output)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
	}

	outStr := output.String()
	assert.Contains(t, outStr, "Remembered")
}

// ============================================================================
// StreamableHTTPHandler Tests
// ============================================================================

type mockHTTPAuthProvider struct {
	userID uuid.UUID
	err    error
}

func (m *mockHTTPAuthProvider) Authenticate(r *http.Request) (*auth.Context, error) {
	if m.err != nil {
		return nil, m.err
	}
	return &auth.Context{
		UserID:   m.userID,
		Username: "testuser",
		Email:    "test@example.com",
	}, nil
}

func newTestHTTPHandler(t *testing.T) (*StreamableHTTPHandler, *testutil.MockQueries) {
	queries := testutil.NewMockQueries()
	embedder := testutil.NewMockEmbeddingProvider(384)
	vectorDB := testutil.NewMockVectorDB()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	server := NewServer(queries, logger, embedder, vectorDB)
	userID := uuid.New()
	authProvider := &mockHTTPAuthProvider{userID: userID}

	handler := &StreamableHTTPHandler{
		server:       server,
		authProvider: authProvider,
		logger:       logger,
		sessions:     make(map[string]*httpSession),
	}
	return handler, queries
}

func TestStreamableHTTPHandler_Options(t *testing.T) {
	// CORS is now handled by router-level middleware, not the handler.
	// OPTIONS requests reaching the handler directly get 405.
	handler, _ := newTestHTTPHandler(t)

	req := httptest.NewRequest(http.MethodOptions, "/mcp", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestStreamableHTTPHandler_MethodNotAllowed(t *testing.T) {
	handler, _ := newTestHTTPHandler(t)

	req := httptest.NewRequest(http.MethodPut, "/mcp", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

func TestStreamableHTTPHandler_Initialize(t *testing.T) {
	handler, _ := newTestHTTPHandler(t)

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize"}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.NotEmpty(t, rec.Header().Get("Mcp-Session-Id"))

	var resp Response
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Nil(t, resp.Error)
}

func TestStreamableHTTPHandler_InitializeAuthFailure(t *testing.T) {
	queries := testutil.NewMockQueries()
	embedder := testutil.NewMockEmbeddingProvider(384)
	vectorDB := testutil.NewMockVectorDB()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	server := NewServer(queries, logger, embedder, vectorDB)
	authProvider := &mockHTTPAuthProvider{err: auth.ErrUnauthorized}

	handler := &StreamableHTTPHandler{
		server:       server,
		authProvider: authProvider,
		logger:       logger,
		sessions:     make(map[string]*httpSession),
	}

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize"}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestStreamableHTTPHandler_PostInvalidJSON(t *testing.T) {
	handler, _ := newTestHTTPHandler(t)

	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader("invalid json"))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)

	var resp Response
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, -32700, resp.Error.Code)
}

func TestStreamableHTTPHandler_PostMissingSession(t *testing.T) {
	handler, _ := newTestHTTPHandler(t)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestStreamableHTTPHandler_PostSessionNotFound(t *testing.T) {
	handler, _ := newTestHTTPHandler(t)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Mcp-Session-Id", "nonexistent-session")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestStreamableHTTPHandler_PostWithSession(t *testing.T) {
	handler, _ := newTestHTTPHandler(t)

	// First initialize to get a session
	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize"}`
	initReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(initBody))
	initRec := httptest.NewRecorder()
	handler.ServeHTTP(initRec, initReq)

	sessionID := initRec.Header().Get("Mcp-Session-Id")
	require.NotEmpty(t, sessionID)

	// Now make a request with the session
	listBody := `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
	listReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(listBody))
	listReq.Header.Set("Mcp-Session-Id", sessionID)
	listRec := httptest.NewRecorder()

	handler.ServeHTTP(listRec, listReq)

	assert.Equal(t, http.StatusOK, listRec.Code)

	var resp Response
	err := json.Unmarshal(listRec.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Nil(t, resp.Error)
}

func TestStreamableHTTPHandler_PostNotification(t *testing.T) {
	handler, _ := newTestHTTPHandler(t)

	// Initialize first
	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize"}`
	initReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(initBody))
	initRec := httptest.NewRecorder()
	handler.ServeHTTP(initRec, initReq)

	sessionID := initRec.Header().Get("Mcp-Session-Id")

	// Send notification (no id field)
	notifBody := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	notifReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(notifBody))
	notifReq.Header.Set("Mcp-Session-Id", sessionID)
	notifRec := httptest.NewRecorder()

	handler.ServeHTTP(notifRec, notifReq)

	assert.Equal(t, http.StatusAccepted, notifRec.Code)
}

func TestStreamableHTTPHandler_DeleteMissingSession(t *testing.T) {
	handler, _ := newTestHTTPHandler(t)

	req := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestStreamableHTTPHandler_Delete(t *testing.T) {
	handler, _ := newTestHTTPHandler(t)

	// Initialize first
	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize"}`
	initReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(initBody))
	initRec := httptest.NewRecorder()
	handler.ServeHTTP(initRec, initReq)

	sessionID := initRec.Header().Get("Mcp-Session-Id")

	// Delete session
	delReq := httptest.NewRequest(http.MethodDelete, "/mcp", nil)
	delReq.Header.Set("Mcp-Session-Id", sessionID)
	delRec := httptest.NewRecorder()

	handler.ServeHTTP(delRec, delReq)

	assert.Equal(t, http.StatusOK, delRec.Code)

	// Verify session is gone
	handler.mu.RLock()
	_, exists := handler.sessions[sessionID]
	handler.mu.RUnlock()
	assert.False(t, exists)
}

func TestStreamableHTTPHandler_GetMissingSession(t *testing.T) {
	handler, _ := newTestHTTPHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestStreamableHTTPHandler_GetSessionNotFound(t *testing.T) {
	handler, _ := newTestHTTPHandler(t)

	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Mcp-Session-Id", "nonexistent")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestStreamableHTTPHandler_GetWithSession(t *testing.T) {
	handler, queries := newTestHTTPHandler(t)

	// Initialize first
	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize"}`
	initReq := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(initBody))
	initRec := httptest.NewRecorder()
	handler.ServeHTTP(initRec, initReq)

	sessionID := initRec.Header().Get("Mcp-Session-Id")

	// Get session's auth context to add memories for that user
	handler.mu.RLock()
	session := handler.sessions[sessionID]
	handler.mu.RUnlock()

	// Add a memory for this user
	queries.CreateMemoryBlock(context.Background(), testutil.CreateMemoryBlockParams(
		session.authCtx.UserID, "test-memory", "Test memory content"))

	// GET request with timeout context
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	getReq := httptest.NewRequest(http.MethodGet, "/mcp", nil).WithContext(ctx)
	getReq.Header.Set("Mcp-Session-Id", sessionID)
	getRec := httptest.NewRecorder()

	handler.ServeHTTP(getRec, getReq)

	// Should get SSE response
	assert.Equal(t, "text/event-stream", getRec.Header().Get("Content-Type"))
}

func TestStreamableHTTPHandler_SendJSONError(t *testing.T) {
	handler, _ := newTestHTTPHandler(t)

	rec := httptest.NewRecorder()
	handler.sendJSONError(rec, -32600, "Test error", 123)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var resp Response
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, -32600, resp.Error.Code)
	assert.Equal(t, "Test error", resp.Error.Message)
	assert.Equal(t, float64(123), resp.ID)
}

// ============================================================================
// Memory Type Tests
// ============================================================================

func TestServer_Remember_WithMemoryType(t *testing.T) {
	server, queries, _, _ := newTestServer(t)
	userID := uuid.New()
	authCtx := testutil.NewTestAuthContext(userID)

	params := CallToolParams{
		Name: "remember",
		Arguments: map[string]any{
			"fact":        "Debugging tip: check logs first",
			"memory_type": "experiential",
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)
	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "Remembered")

	_ = queries
}

func TestServer_Remember_InvalidMemoryType(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	params := CallToolParams{
		Name: "remember",
		Arguments: map[string]any{
			"fact":        "Some fact",
			"memory_type": "invalid_type",
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)
	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "memory_type must be")
}

func TestServer_Remember_WithScope(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	userID := uuid.New()
	authCtx := testutil.NewTestAuthContext(userID)

	params := CallToolParams{
		Name: "remember",
		Arguments: map[string]any{
			"fact":  "Team standard: use Go 1.24",
			"scope": "org",
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)
	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "Remembered")
}

func TestServer_Remember_InvalidScope(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	params := CallToolParams{
		Name: "remember",
		Arguments: map[string]any{
			"fact":  "Some fact",
			"scope": "invalid_scope",
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)
	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "scope must be")
}

// ============================================================================
// Share Memory Tests
// ============================================================================

func TestServer_ShareMemory(t *testing.T) {
	server, queries, _, _ := newTestServer(t)
	userID := uuid.New()
	authCtx := testutil.NewTestAuthContext(userID)

	// Create a memory first
	block, err := queries.CreateMemoryBlock(nil, testutil.CreateMemoryBlockParams(userID, "test-mem", "Test memory"))
	require.NoError(t, err)

	params := CallToolParams{
		Name: "share_memory",
		Arguments: map[string]any{
			"fact_id": float64(block.ID),
			"scope":   "org",
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)
	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "organization")
}

func TestServer_ShareMemory_NotFound(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	params := CallToolParams{
		Name: "share_memory",
		Arguments: map[string]any{
			"fact_id": float64(999),
			"scope":   "org",
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)
	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "not found")
}

func TestServer_ShareMemory_MissingFactID(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	params := CallToolParams{
		Name: "share_memory",
		Arguments: map[string]any{
			"scope": "org",
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)
	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "fact_id is required")
}

func TestServer_ShareMemory_MissingScope(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	params := CallToolParams{
		Name: "share_memory",
		Arguments: map[string]any{
			"fact_id": float64(1),
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)
	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "scope is required")
}

func TestServer_ShareMemory_InvalidScope(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	params := CallToolParams{
		Name: "share_memory",
		Arguments: map[string]any{
			"fact_id": float64(1),
			"scope":   "invalid",
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)
	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "scope must be")
}

// ============================================================================
// Export Memories Tests
// ============================================================================

func TestServer_ExportMemories_Empty(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	params := CallToolParams{
		Name:      "export_memories",
		Arguments: map[string]any{},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)
	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.False(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "No memories")
}

func TestServer_ExportMemories_InvalidSinceFormat(t *testing.T) {
	server, _, _, _ := newTestServer(t)
	authCtx := testutil.NewTestAuthContext(uuid.New())

	params := CallToolParams{
		Name: "export_memories",
		Arguments: map[string]any{
			"since": "not-a-valid-timestamp",
		},
	}
	paramsJSON, _ := json.Marshal(params)

	req := Request{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "tools/call",
		Params:  paramsJSON,
	}

	resp := server.HandleRequest(authCtx, req)

	require.Nil(t, resp.Error)
	result, ok := resp.Result.(CallToolResult)
	require.True(t, ok)
	assert.True(t, result.IsError)
	assert.Contains(t, result.Content[0].Text, "Invalid since timestamp")
}
