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

// tools is the agent-facing MCP surface: five tools the model uses
// turn-to-turn. The lifecycle / admin operations (session_start,
// session_end, list_sessions, branch, expand_session_workspace,
// share, consolidate) stay reachable over HTTP at /v1/* for hosts
// that drive memory programmatically (pi-mono and friends) — they're
// just hidden from the model's tool catalog so it doesn't have to
// reason about session boundaries it has no good signal for.
//
// `record` accepts an optional cwd; when session_id is omitted, core
// uses cwd to derive the workspace and either reuses the most-recent
// open session for (user, workspace) or provisions one inline. This
// removes the "model forgot to call session_start" failure mode that
// every MCP host hits.
func tools() []toolSpec {
	return []toolSpec{
		{
			Tool: mcppkg.NewTool("record",
				mcppkg.WithDescription(
					"Append an event to the active session. Pass cwd (the "+
						"current project directory) and the local service will "+
						"find or create the right session automatically — no "+
						"session_id bookkeeping required. The embedding is "+
						"computed if omitted."),
				mcppkg.WithString("user_id",
					mcppkg.Description("Optional. Falls back to AUTH_DEFAULT_USER env var if omitted.")),
				mcppkg.WithString("cwd",
					mcppkg.Description("Current project directory. Required when session_id is omitted; ignored otherwise.")),
				mcppkg.WithString("session_id",
					mcppkg.Description("Optional. If set, used verbatim; otherwise derived from cwd.")),
				mcppkg.WithString("parent_id",
					mcppkg.Description("Parent event id — unset appends to the current leaf.")),
				mcppkg.WithObject("event", mcppkg.Required(),
					mcppkg.Description("Event DTO. Required field: type — must be one of: "+
						"\"user\", \"assistant\", \"tool_result\", \"system\". "+
						"Other fields: text, tool_name, tool_input, tool_output, raw_event, ...")),
			),
			Path: "/v1/record",
		},
		{
			Tool: mcppkg.NewTool("recall",
				mcppkg.WithDescription(
					"Hybrid query across working (sietch) + episodic + semantic; "+
						"returns score-ranked hits with tier attribution. "+
						"Partial-failure tolerant: a degraded field lists any "+
						"tiers that were skipped. "+
						"Settle (P4 recurrent-settle) is on by default (channel@0.40); "+
						"pass settle=off to disable, or omit for the server default."),
				mcppkg.WithString("user_id",
					mcppkg.Description("Optional. Falls back to AUTH_DEFAULT_USER env var if omitted.")),
				mcppkg.WithString("workspace",
					mcppkg.Description("Workspace id (uuid). Optional when cwd is provided — the workspace is derived from cwd.")),
				mcppkg.WithString("cwd",
					mcppkg.Description("Current project directory. The workspace is derived from it when workspace is omitted — same mapping record uses.")),
				mcppkg.WithString("session_id",
					mcppkg.Description("Optional. Scopes the working-tier (sietch) hits.")),
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
				mcppkg.WithString("settle",
					mcppkg.Enum("off", "expand", "channel"),
					mcppkg.Description(
						"Settle mode. Omit for the server default (channel@0.40 — on by default). "+
							"\"off\" (byte-identical to pre-P4), "+
							"\"expand\" (config A: spreading activation, expansion sub-list appended "+
							"to rerank pool), \"channel\" (config B: activation also participates in "+
							"score fusion as a third channel alongside RRF and reranker)."),
				),
				mcppkg.WithNumber("activation_weight",
					mcppkg.Description(
						"Activation weight for channel mode (0, 1]. Omit to use the server "+
							"default (0.40); an explicit value overrides it. Validated at the Recall "+
							"boundary: rerank_weight + activation_weight must not exceed 1 (default "+
							"rerank_weight 0.5 implies activation_weight < 0.5). Ignored in other modes."),
				),
				mcppkg.WithObject("settle_params",
					mcppkg.Description(
						"Optional settle tuning knobs; omit for server defaults. "+
							"Ignored when settle is off. Each field omitted uses the server default."),
					mcppkg.Properties(map[string]any{
						"lambda": map[string]any{
							"type":        "number",
							"description": "Damping/decay contraction, open interval (0, 1); omit for server default (0.7).",
						},
						"eps": map[string]any{
							"type":        "number",
							"description": "L1 convergence threshold, must be > 0; omit for server default (1e-6).",
						},
						"max_iters": map[string]any{
							"type":        "integer",
							"description": "Hard iteration stop, must be > 0; omit for server default (20).",
						},
						"hop_cap": map[string]any{
							"type":        "integer",
							"description": "Neighborhood hop radius, must be > 0; omit for server default (3).",
						},
						"node_cap": map[string]any{
							"type":        "integer",
							"description": "Neighborhood node ceiling, (0, 20000]; omit for server default (2000).",
						},
						"top_m": map[string]any{
							"type":        "integer",
							"description": "Expansion candidates returned, must be > 0; omit for server default (25).",
						},
					}),
				),
			),
			Path: "/v1/recall",
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
