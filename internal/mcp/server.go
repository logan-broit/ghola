// Package mcp registers the agent-facing subset of Core operations as MCP
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

// Register installs the agent-facing tool catalog (see tools) as MCP tools
// on the given sink. Each tool is a passthrough: MCP args -> JSON body
// -> POST to cfg.BaseURL + /v1/<op> -> body back to MCP as text.
func Register(s ToolSink, cfg Config) {
	h := newHandler(cfg)
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

// tools is the agent-facing MCP surface: six tools the model uses
// turn-to-turn. The lifecycle / admin operations (session_start,
// session_end, list_sessions, branch, expand_session_workspace,
// share, consolidate) stay reachable over HTTP at /v1/* for hosts
// that drive memory programmatically (pi-mono and friends) — they're
// just hidden from the model's tool catalog so it doesn't have to
// reason about session boundaries it has no good signal for.
//
// consolidate_workspace is the one exception to "consolidation stays
// hidden": unlike the session-scoped `consolidate` op (sietch->episodic
// flush, a bookkeeping detail the model has no good signal for), the
// episodic->semantic batch is something the model itself decides to
// trigger — typically right before its own context is about to be
// cleared or compacted, so the semantic tier has fresh content when
// the conversation resumes. That's a judgment call only the agent can
// make, so it belongs in the model-facing catalog.
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
		{
			Tool: mcppkg.NewTool("consolidate_workspace",
				mcppkg.WithDescription(
					"Trigger chapterhouse's episodic->semantic consolidation batch "+
						"for a workspace (cluster closed sessions, enrich with "+
						"representative excerpts, optionally label/digest). Call this "+
						"right before your own context is about to be cleared or "+
						"compacted, so the semantic tier has fresh, readable content "+
						"for recall to surface once the conversation resumes. "+
						"Synchronous — the call blocks until the batch completes. "+
						"Distinct from the hidden `consolidate` op, which flushes one "+
						"session's pending working-memory events to episodic storage; "+
						"this triggers the separate episodic->semantic clustering "+
						"pipeline the nightly worker also runs."),
				mcppkg.WithString("workspace",
					mcppkg.Description("Workspace id (uuid). Optional when cwd is provided — the workspace is derived from cwd.")),
				mcppkg.WithString("cwd",
					mcppkg.Description("Current project directory. The workspace is derived from it when workspace is omitted — same mapping record/recall use.")),
			),
			Path: "/v1/semantic/consolidate",
		},
	}
}

// ---------------------------------------------------------------------
// Handler plumbing
// ---------------------------------------------------------------------

const (
	// consolidateWorkspacePath is the one proxied route whose upstream work
	// (the full episodic->semantic batch) legitimately runs for minutes.
	consolidateWorkspacePath = "/v1/semantic/consolidate"
	// defaultProxyTimeout bounds a normal proxied call to the ghola daemon.
	defaultProxyTimeout = 30 * time.Second
	// consolidateProxyTimeout bounds the consolidate_workspace hop, whose
	// upstream runs the full batch (tens of seconds to minutes). It overrides
	// the shared default for this route only.
	consolidateProxyTimeout = 10 * time.Minute
)

// proxyTimeout returns the per-request deadline for a proxied path. The
// consolidate route runs for minutes upstream; everything else uses the
// default. proxy() layers this on the request context.
func proxyTimeout(path string) time.Duration {
	if path == consolidateWorkspacePath {
		return consolidateProxyTimeout
	}
	return defaultProxyTimeout
}

type handler struct {
	cfg      Config
	longHTTP *http.Client
}

// newHandler builds the proxy handler. It derives a second, uncapped http
// client for long-running routes: the shared cfg.HTTPClient carries a 30s
// Timeout that would clip the consolidate call before its 10m context
// deadline fires, so those routes use a client with no Timeout cap and rely
// on the context deadline alone (proxyTimeout).
func newHandler(cfg Config) *handler {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultProxyTimeout}
	}
	return &handler{
		cfg:      cfg,
		longHTTP: &http.Client{Transport: cfg.HTTPClient.Transport},
	}
}

// clientForPath picks the http client whose Timeout won't clip the request
// before its context deadline: long-running routes (consolidate) get the
// uncapped client so proxyTimeout's deadline is the only bound.
func (h *handler) clientForPath(path string) *http.Client {
	if path == consolidateWorkspacePath {
		return h.longHTTP
	}
	return h.cfg.HTTPClient
}

// proxy builds a ToolHandlerFunc that marshals the MCP arguments map
// to JSON and POSTs to cfg.BaseURL + path. The response body comes
// back to the caller as a text content part.
func (h *handler) proxy(path string) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcppkg.CallToolRequest) (*mcppkg.CallToolResult, error) {
		// Per-route deadline: the consolidate hop runs for minutes upstream;
		// everything else uses the default. Layered on ctx so a caller
		// deadline still wins.
		ctx, cancel := context.WithTimeout(ctx, proxyTimeout(path))
		defer cancel()

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

		resp, err := h.clientForPath(path).Do(httpReq)
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
