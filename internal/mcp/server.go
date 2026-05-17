// Package mcp registers the 12 canonical Core operations as MCP
// tools for Claude Code (and any other MCP client) to call over
// stdio. The server is a thin stdio<->HTTP bridge: each tool
// translates the MCP request into a POST against the already-running
// ghola local service on localhost:7421. Single source of truth —
// sietch state, Pipeline A, embedder — all stays in the ghola
// daemon; the MCP process is short-lived per Claude Code session.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	mcppkg "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Config holds the per-instance settings an MCP registrar needs.
type Config struct {
	// BaseURL points at the ghola HTTP daemon (e.g.
	// "http://localhost:7421").
	BaseURL string
	// HTTPClient may be nil; a default 30-second client is used.
	HTTPClient *http.Client
}

// ToolSink is anything that can have MCP tools added to it. Both
// server.MCPServer (production) and mcptest.Server (tests) satisfy
// this shape, so Register works against either.
type ToolSink interface {
	AddTool(tool mcppkg.Tool, handler server.ToolHandlerFunc)
}

// Register installs the 12 canonical operations as MCP tools on the
// given sink. Each tool is a passthrough: MCP args -> JSON body
// -> POST to cfg.BaseURL + /v1/<op> -> body back to MCP as text.
func Register(s ToolSink, cfg Config) {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	h := &handler{cfg: cfg}

	for _, t := range tools() {
		s.AddTool(t.Tool, h.proxy(t.Path))
	}
}

// ---------------------------------------------------------------------
// Tool catalog — one entry per Core operation.
//
// Each tool exposes the request body fields agents need, not more.
// Field names match the HTTP/JSON contract in internal/http.Server so
// the bridge is a trivial map -> JSON -> POST transform.
// ---------------------------------------------------------------------

type toolSpec struct {
	Tool mcppkg.Tool
	Path string // "/v1/<op>"
}

func tools() []toolSpec {
	return []toolSpec{
		{
			Tool: mcppkg.NewTool("session_start",
				mcppkg.WithDescription("Provision a new session (fresh sietch file)."),
				mcppkg.WithString("user_id",
					mcppkg.Description("Optional. Falls back to AUTH_DEFAULT_USER env var if omitted.")),
				mcppkg.WithString("agent_kind",
					mcppkg.Description("claude-code, pi-mono, etc.")),
				mcppkg.WithString("cwd"),
				mcppkg.WithString("git_branch"),
				mcppkg.WithString("source_device"),
			),
			Path: "/v1/session_start",
		},
		{
			Tool: mcppkg.NewTool("session_end",
				mcppkg.WithDescription("Final flush + close the session."),
				mcppkg.WithString("session_id", mcppkg.Required()),
			),
			Path: "/v1/session_end",
		},
		{
			Tool: mcppkg.NewTool("list_sessions",
				mcppkg.WithDescription("Enumerate the caller's sessions."),
				mcppkg.WithString("user_id",
					mcppkg.Description("Optional. Falls back to AUTH_DEFAULT_USER env var if omitted.")),
			),
			Path: "/v1/list_sessions",
		},
		{
			Tool: mcppkg.NewTool("record",
				mcppkg.WithDescription(
					"Append an event to the current session. The local service "+
						"computes the embedding if it's omitted."),
				mcppkg.WithString("session_id", mcppkg.Required()),
				mcppkg.WithString("user_id",
					mcppkg.Description("Optional. Falls back to AUTH_DEFAULT_USER env var if omitted.")),
				mcppkg.WithString("parent_id",
					mcppkg.Description("Parent event id — unset appends to the current leaf.")),
				mcppkg.WithObject("event", mcppkg.Required(),
					mcppkg.Description("Event DTO (type, text, tool_name, tool_input/output, raw_event, ...).")),
			),
			Path: "/v1/record",
		},
		{
			Tool: mcppkg.NewTool("branch",
				mcppkg.WithDescription(
					"Fork a new child event from an existing parent. Like record() "+
						"but parent_id is required."),
				mcppkg.WithString("session_id", mcppkg.Required()),
				mcppkg.WithString("user_id",
					mcppkg.Description("Optional. Falls back to AUTH_DEFAULT_USER env var if omitted.")),
				mcppkg.WithString("parent_id", mcppkg.Required()),
				mcppkg.WithObject("event", mcppkg.Required()),
			),
			Path: "/v1/branch",
		},
		{
			Tool: mcppkg.NewTool("bookmark",
				mcppkg.WithDescription("Label an event for later recall ('remember this point')."),
				mcppkg.WithString("session_id", mcppkg.Required()),
				mcppkg.WithString("event_id", mcppkg.Required()),
				mcppkg.WithString("label", mcppkg.Required()),
			),
			Path: "/v1/bookmark",
		},
		{
			Tool: mcppkg.NewTool("navigate",
				mcppkg.WithDescription("Move the session's current-event pointer."),
				mcppkg.WithString("session_id", mcppkg.Required()),
				mcppkg.WithString("event_id", mcppkg.Required()),
			),
			Path: "/v1/navigate",
		},
		{
			Tool: mcppkg.NewTool("recall",
				mcppkg.WithDescription(
					"Hybrid query across working (sietch) + episodic + semantic; "+
						"returns score-ranked hits with tier attribution."),
				mcppkg.WithString("session_id"),
				mcppkg.WithString("user_id",
					mcppkg.Description("Optional. Falls back to AUTH_DEFAULT_USER env var if omitted.")),
				mcppkg.WithString("workspace",
					mcppkg.Description("Semantic-tier workspace id. Required for semantic hits.")),
				mcppkg.WithString("query_text"),
				mcppkg.WithNumber("limit",
					mcppkg.Description("Max merged hits (default 10)."),
					mcppkg.DefaultNumber(10),
				),
				mcppkg.WithBoolean("include_shared",
					mcppkg.Description("Include events shared with the caller (episodic tier)."),
					mcppkg.DefaultBool(true),
				),
				mcppkg.WithBoolean("include_sietch"),
				mcppkg.WithBoolean("include_episode"),
				mcppkg.WithBoolean("include_semant"),
			),
			Path: "/v1/recall",
		},
		{
			Tool: mcppkg.NewTool("forget",
				mcppkg.WithDescription(
					"Soft-delete events. Flips text to '[forgotten]' and nulls "+
						"embedding in both sietch and episodic while preserving tree structure."),
				mcppkg.WithString("session_id"),
				mcppkg.WithString("user_id",
					mcppkg.Description("Optional. Falls back to AUTH_DEFAULT_USER env var if omitted.")),
				mcppkg.WithArray("event_ids", mcppkg.Required(),
					mcppkg.Items(map[string]any{"type": "string"})),
			),
			Path: "/v1/forget",
		},
		{
			Tool: mcppkg.NewTool("expand_session_workspace",
				mcppkg.WithDescription(
					"Tag an existing session into an additional workspace. "+
						"Use when the conversation has drifted into a topic "+
						"outside the session's primary workspace, so future "+
						"recalls scoped to that workspace can find this session."),
				mcppkg.WithString("session_id", mcppkg.Required()),
				mcppkg.WithString("workspace_id", mcppkg.Required()),
				mcppkg.WithString("user_id",
					mcppkg.Description("Optional. Falls back to AUTH_DEFAULT_USER env var if omitted.")),
			),
			Path: "/v1/session_workspace",
		},
		{
			Tool: mcppkg.NewTool("share",
				mcppkg.WithDescription("Grant team or user visibility to a session / branch / event."),
				mcppkg.WithString("user_id",
					mcppkg.Description("Optional. Falls back to AUTH_DEFAULT_USER env var if omitted.")),
				mcppkg.WithString("target", mcppkg.Required(),
					mcppkg.Enum("team", "user")),
				mcppkg.WithString("target_id",
					mcppkg.Description("Required when target='user'.")),
				mcppkg.WithString("scope_type", mcppkg.Required(),
					mcppkg.Enum("session", "branch", "event")),
				mcppkg.WithString("scope_id", mcppkg.Required()),
			),
			Path: "/v1/share",
		},
		{
			Tool: mcppkg.NewTool("consolidate",
				mcppkg.WithDescription(
					"Force-run Pipeline A for the given session: flushes all "+
						"events past the watermark to episodic."),
				mcppkg.WithString("session_id", mcppkg.Required()),
			),
			Path: "/v1/consolidate",
		},
	}
}

// ---------------------------------------------------------------------
// Handler plumbing
// ---------------------------------------------------------------------

type handler struct {
	cfg Config
}

// proxy builds a ToolHandlerFunc that marshals the MCP arguments map
// to JSON and POSTs to cfg.BaseURL + path. The response body comes
// back to the caller as a text content part.
func (h *handler) proxy(path string) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcppkg.CallToolRequest) (*mcppkg.CallToolResult, error) {
		args := req.GetArguments()
		if args == nil {
			args = map[string]any{}
		}
		body, err := json.Marshal(args)
		if err != nil {
			return nil, fmt.Errorf("marshal args: %w", err)
		}

		httpReq, err := http.NewRequestWithContext(ctx,
			http.MethodPost, strings.TrimRight(h.cfg.BaseURL, "/")+path, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("build request: %w", err)
		}
		httpReq.Header.Set("Content-Type", "application/json")

		resp, err := h.cfg.HTTPClient.Do(httpReq)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		defer resp.Body.Close()

		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return mcppkg.NewToolResultError(
				fmt.Sprintf("%s: %d %s: %s",
					path, resp.StatusCode, http.StatusText(resp.StatusCode),
					strings.TrimSpace(string(payload))),
			), nil
		}

		return &mcppkg.CallToolResult{
			Content: []mcppkg.Content{
				mcppkg.TextContent{Type: "text", Text: string(payload)},
			},
		}, nil
	}
}
