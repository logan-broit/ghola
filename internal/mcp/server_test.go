package mcp_test

import (
	"context"
	"encoding/json"
	"io"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	mcppkg "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/mcptest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	ghmcp "github.com/logan-broit/ghola/internal/mcp"
)

// fakeGhola is an httptest.Server that records the path + body of
// every request it sees. Lets the MCP bridge tests assert translation
// without wiring the real ghola daemon.
type fakeGhola struct {
	mu       sync.Mutex
	calls    []call
	response map[string]string // path -> JSON response body
}

type call struct {
	Path string
	Body map[string]any
}

func newFakeGhola(t *testing.T) (*fakeGhola, *httptest.Server) {
	fg := &fakeGhola{response: map[string]string{}}
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		b, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var body map[string]any
		if len(b) > 0 {
			_ = json.Unmarshal(b, &body)
		}
		fg.mu.Lock()
		fg.calls = append(fg.calls, call{Path: r.URL.Path, Body: body})
		resp := fg.response[r.URL.Path]
		fg.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if resp == "" {
			resp = `{"status":"ok"}`
		}
		_, _ = w.Write([]byte(resp))
	}))
	t.Cleanup(srv.Close)
	return fg, srv
}

// newClient starts an in-memory mcptest.Server with the ghola tools
// registered and returns a connected client.
func newClient(t *testing.T, gholaURL string) *mcptest.Server {
	t.Helper()
	s := mcptest.NewUnstartedServer(t)

	// Register installs against the ToolSink interface that both
	// *server.MCPServer (production) and *mcptest.Server (here)
	// satisfy.
	ghmcp.Register(s, ghmcp.Config{BaseURL: gholaURL, HTTPClient: &stdhttp.Client{Timeout: 5 * time.Second}})

	// Start must get a long-lived ctx: the server goroutine lives on
	// it for the whole test, so a short deadline would kill the
	// stdio pipe mid-call and block the client on write.
	require.NoError(t, s.Start(context.Background()))
	t.Cleanup(s.Close)
	return s
}

// callTool drives a tool over the mcptest client + asserts no error.
func callTool(t *testing.T, s *mcptest.Server, name string, args map[string]any) *mcppkg.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := mcppkg.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = args
	out, err := s.Client().CallTool(ctx, req)
	require.NoError(t, err)
	require.NotNil(t, out)
	return out
}

// ---------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------

// TestTools_AgentSurface confirms the MCP catalog is exactly the five
// agent-relevant tools. The lifecycle / admin operations (session_start,
// session_end, list_sessions, branch, expand_session_workspace, share,
// consolidate) are reachable over HTTP but deliberately hidden from
// the model's tool list so it doesn't have to reason about session
// boundaries it has no good signal for. If a future change re-adds
// one to the catalog, this test surfaces that intentionally.
func TestTools_AgentSurface(t *testing.T) {
	fg, hs := newFakeGhola(t)
	_ = fg
	s := newClient(t, hs.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	list, err := s.Client().ListTools(ctx, mcppkg.ListToolsRequest{})
	require.NoError(t, err)

	names := make(map[string]bool, len(list.Tools))
	for _, tool := range list.Tools {
		names[tool.Name] = true
	}

	expected := []string{"record", "recall", "bookmark", "navigate", "forget"}
	assert.Len(t, list.Tools, len(expected))
	for _, want := range expected {
		assert.True(t, names[want], "missing tool %q", want)
	}

	// Explicitly assert the lifecycle / admin tools are NOT exposed
	// to the model. They stay reachable via HTTP for pi-mono and
	// other hosts that drive memory programmatically.
	for _, hidden := range []string{
		"session_start", "session_end", "list_sessions",
		"branch", "expand_session_workspace", "share", "consolidate",
	} {
		assert.False(t, names[hidden],
			"%q must not be in the model-facing MCP catalog", hidden)
	}
}

// TestProxy_TranslatesRecordCall proves the MCP -> HTTP bridge
// shape: tool args become the POST body; the ghola response body
// flows back as a text content part.
func TestProxy_TranslatesRecordCall(t *testing.T) {
	fg, hs := newFakeGhola(t)
	fg.response["/v1/record"] = `{"event":{"id":"evt-1","session_id":"s1","user_id":"u1"}}`

	s := newClient(t, hs.URL)

	out := callTool(t, s, "record", map[string]any{
		"session_id": "s1",
		"user_id":    "u1",
		"event": map[string]any{
			"type": "user",
			"text": "hello",
		},
	})

	// The tool's TextContent should be the daemon's response body
	// verbatim — downstream clients can json-decode themselves.
	require.NotEmpty(t, out.Content)
	text := out.Content[0].(mcppkg.TextContent).Text
	assert.Contains(t, text, `"id":"evt-1"`)

	fg.mu.Lock()
	defer fg.mu.Unlock()
	require.Len(t, fg.calls, 1)
	assert.Equal(t, "/v1/record", fg.calls[0].Path)
	assert.Equal(t, "s1", fg.calls[0].Body["session_id"])
	assert.Equal(t, "u1", fg.calls[0].Body["user_id"])
	assert.IsType(t, map[string]any{}, fg.calls[0].Body["event"])
}

// TestProxy_SurfacesDaemonError returns an MCP error result when
// the daemon HTTP call fails (401 / 500 / ...).
func TestProxy_SurfacesDaemonError(t *testing.T) {
	fg := &fakeGhola{response: map[string]string{}}
	hs := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		w.WriteHeader(stdhttp.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"no api key"}`))
	}))
	defer hs.Close()
	_ = fg

	s := newClient(t, hs.URL)

	out := callTool(t, s, "recall", map[string]any{
		"user_id":    "u1",
		"workspace":  "w1",
		"query_text": "anything",
	})

	require.True(t, out.IsError, "daemon 401 must surface as an error result")
	require.NotEmpty(t, out.Content)
	text := out.Content[0].(mcppkg.TextContent).Text
	assert.Contains(t, text, "401")
	assert.Contains(t, text, "no api key")
}

// TestProxy_EndToEndOps exercises one tool per arg-family so the wire
// shape stays stable — record (object arg + cwd-fallback path),
// recall (bool/number args), forget (array arg).
func TestProxy_EndToEndOps(t *testing.T) {
	fg, hs := newFakeGhola(t)
	fg.response["/v1/record"] = `{"event":{"id":"e1"}}`
	fg.response["/v1/recall"] = `{"hits":[],"tier_counts":{"working":0}}`
	fg.response["/v1/forget"] = `{"forgotten":3}`

	s := newClient(t, hs.URL)

	callTool(t, s, "record", map[string]any{
		"user_id": "u1",
		"cwd":     "/tmp/proj",
		"event":   map[string]any{"type": "user", "text": "hi"},
	})
	callTool(t, s, "recall", map[string]any{
		"user_id":    "u1",
		"workspace":  "w1",
		"query_text": "kubernetes",
		"limit":      5,
	})
	callTool(t, s, "forget", map[string]any{
		"user_id":   "u1",
		"event_ids": []any{"e1", "e2", "e3"},
	})

	fg.mu.Lock()
	defer fg.mu.Unlock()
	require.Len(t, fg.calls, 3)

	paths := []string{fg.calls[0].Path, fg.calls[1].Path, fg.calls[2].Path}
	assert.Equal(t, []string{"/v1/record", "/v1/recall", "/v1/forget"}, paths)

	// recall's number/bool args must round-trip.
	assert.Equal(t, float64(5), fg.calls[1].Body["limit"])
	assert.Equal(t, "kubernetes", fg.calls[1].Body["query_text"])

	// forget's array arg must round-trip.
	ids, ok := fg.calls[2].Body["event_ids"].([]any)
	require.True(t, ok, "event_ids should survive as array")
	assert.ElementsMatch(t, []any{"e1", "e2", "e3"}, ids)

	// Strings shouldn't have gotten mangled by url-trimming.
	assert.False(t, strings.Contains(fg.calls[0].Path, "//"))
}
