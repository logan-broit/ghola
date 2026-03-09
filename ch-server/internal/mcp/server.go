package mcp

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/thinkwright/chapterhouse/ch-server/internal/auth"
	"github.com/thinkwright/chapterhouse/ch-server/internal/mneme"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository/sqlc"
	"github.com/thinkwright/chapterhouse/ch-server/internal/secrets"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
)

//go:embed descriptions/remember.txt
var rememberDescription string

//go:embed descriptions/recall.txt
var recallDescription string

//go:embed descriptions/list_sessions.txt
var listSessionsDescription string

//go:embed descriptions/session_summary.txt
var sessionSummaryDescription string

//go:embed descriptions/session_context.txt
var sessionContextDescription string

// AuditQuerier defines the minimal database operations still needed by the MCP server.
type AuditQuerier interface {
	CreateAuditLog(ctx context.Context, arg sqlc.CreateAuditLogParams) (sqlc.AuditLog, error)
}

type Server struct {
	store   *mneme.Store
	audit   AuditQuerier
	logger  *slog.Logger
	scanner *secrets.Scanner
	wg      sync.WaitGroup
}

func NewServer(store *mneme.Store, audit AuditQuerier, logger *slog.Logger) *Server {
	return &Server{
		store:   store,
		audit:   audit,
		logger:  logger,
		scanner: secrets.New(),
	}
}

// Wait blocks until all in-flight background operations have completed.
func (s *Server) Wait() {
	s.wg.Wait()
}

// goBackground runs fn in a goroutine tracked by the server's WaitGroup.
func (s *Server) goBackground(fn func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		fn()
	}()
}

// Tools returns the list of available MCP tools.
func (s *Server) Tools() []Tool {
	return []Tool{
		{
			Name:        "remember",
			Description: rememberDescription,
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"fact": {
						Type:        "string",
						Description: "The fact or information to remember. Be specific and self-contained.",
					},
					"tags": {
						Type:        "array",
						Description: "Optional tags for categorization (e.g., 'kubernetes', 'ssl', 'payments-service').",
						Items:       &Items{Type: "string"},
					},
					"memory_type": {
						Type:        "string",
						Description: "REQUIRED CLASSIFICATION: Choose the appropriate type: 'factual' (standards, policies, infrastructure), 'experiential' (solutions, lessons learned), 'working' (temporary session context, expires in 7 days). Defaults to 'factual' if not specified, but should be actively chosen based on the nature of the memory.",
						Enum:        []string{"factual", "experiential", "working"},
					},
					"scope": {
						Type:        "string",
						Description: "Memory visibility scope: 'personal' (default, only you see it) or 'org' (shared with everyone in your organization). Most memories should be personal unless they contain team-wide knowledge.",
						Enum:        []string{"personal", "org"},
					},
					"session_id": {
						Type:        "string",
						Description: "Optional session UUID to group this memory with. Links related memories for later retrieval via list_sessions/session_context.",
					},
				},
				Required: []string{"fact"},
			},
		},
		{
			Name:        "share_memory",
			Description: "Make a personal memory shared with your organization, or make an org memory personal. Only the creator of a memory can change its scope.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"fact_id": {
						Type:        "string",
						Description: "The UUID of the memory to change scope for",
					},
					"scope": {
						Type:        "string",
						Description: "New scope: 'personal' or 'org'",
						Enum:        []string{"personal", "org"},
					},
				},
				Required: []string{"fact_id", "scope"},
			},
		},
		{
			Name:        "recall",
			Description: recallDescription,
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"query": {
						Type:        "string",
						Description: "Search query - can be keywords, a question, or a topic. Semantic search will find related concepts.",
					},
					"limit": {
						Type:        "integer",
						Description: "Maximum number of results (default: 10).",
					},
					"tags": {
						Type:        "array",
						Description: "Optional tags to filter results.",
						Items:       &Items{Type: "string"},
					},
					"memory_type": {
						Type:        "string",
						Description: "Filter by memory type: 'factual', 'experiential', or 'working'.",
						Enum:        []string{"factual", "experiential", "working"},
					},
					"mode": {
						Type:        "string",
						Description: "Search mode: 'semantic' (meaning-based), 'keyword' (exact match), 'hybrid' (both). Default: hybrid.",
						Enum:        []string{"semantic", "keyword", "hybrid"},
					},
					"session_id": {
						Type:        "string",
						Description: "Filter results to a specific MCP session (UUID). Useful for recalling context from a particular session.",
					},
				},
				Required: []string{"query"},
			},
		},
		{
			Name:        "forget",
			Description: "Remove a fact from memory by its UUID.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"fact_id": {
						Type:        "string",
						Description: "The UUID of the fact to remove.",
					},
				},
				Required: []string{"fact_id"},
			},
		},
		{
			Name:        "list_memories",
			Description: "List all stored memories, optionally filtered by tags and type.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"tags": {
						Type:        "array",
						Description: "Optional tags to filter results.",
						Items:       &Items{Type: "string"},
					},
					"memory_type": {
						Type:        "string",
						Description: "Filter by memory type: 'factual', 'experiential', or 'working'.",
						Enum:        []string{"factual", "experiential", "working"},
					},
					"limit": {
						Type:        "integer",
						Description: "Maximum number of results (default: 50).",
					},
					"session_id": {
						Type:        "string",
						Description: "Filter results to a specific MCP session (UUID).",
					},
				},
			},
		},
		{
			Name:        "export_memories",
			Description: "Export memories to JSONL format for RAG ingestion, backup, or analysis. Each line contains a complete JSON object with memory metadata.",
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"memory_type": {
						Type:        "string",
						Description: "Filter by memory type: 'factual', 'experiential', or 'working'",
						Enum:        []string{"factual", "experiential", "working"},
					},
					"scope": {
						Type:        "string",
						Description: "Filter by scope: 'personal' or 'org'",
						Enum:        []string{"personal", "org"},
					},
					"tags": {
						Type:        "array",
						Description: "Filter by tags (AND logic - memory must have all specified tags)",
						Items:       &Items{Type: "string"},
					},
					"since": {
						Type:        "string",
						Description: "Only export memories created/modified since this RFC3339 timestamp",
					},
					"session_id": {
						Type:        "string",
						Description: "Filter results to a specific MCP session (UUID).",
					},
				},
			},
		},
		{
			Name:        "list_sessions",
			Description: listSessionsDescription,
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"limit": {
						Type:        "integer",
						Description: "Maximum number of sessions to return (default: 10).",
					},
				},
			},
		},
		{
			Name:        "session_summary",
			Description: sessionSummaryDescription,
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"session_id": {
						Type:        "string",
						Description: "Session UUID to summarize.",
					},
				},
				Required: []string{"session_id"},
			},
		},
		{
			Name:        "session_context",
			Description: sessionContextDescription,
			InputSchema: InputSchema{
				Type: "object",
				Properties: map[string]Property{
					"session_id": {
						Type:        "string",
						Description: "Session UUID to load context from.",
					},
				},
				Required: []string{"session_id"},
			},
		},
	}
}

func (s *Server) HandleRequest(ctx *auth.Context, req Request) Response {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req)
	case "notifications/initialized":
		return Response{}
	case "tools/list":
		return s.handleToolsList(req)
	case "tools/call":
		return s.handleToolsCall(ctx, req)
	default:
		return Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &Error{Code: -32601, Message: fmt.Sprintf("Method not found: %s", req.Method)},
		}
	}
}

func (s *Server) handleInitialize(req Request) Response {
	return Response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result: InitializeResult{
			ProtocolVersion: "2024-11-05",
			Capabilities: Capabilities{
				Tools: &ToolsCapability{},
			},
			ServerInfo: ServerInfo{
				Name:    "chapterhouse",
				Version: "0.2.0",
			},
		},
	}
}

func (s *Server) handleToolsList(req Request) Response {
	return Response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  ToolsListResult{Tools: s.Tools()},
	}
}

func (s *Server) handleToolsCall(authCtx *auth.Context, req Request) Response {
	var params CallToolParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return Response{
			JSONRPC: "2.0",
			ID:      req.ID,
			Error:   &Error{Code: -32602, Message: "Invalid params"},
		}
	}

	result := s.callTool(authCtx, params)
	return Response{
		JSONRPC: "2.0",
		ID:      req.ID,
		Result:  result,
	}
}

func (s *Server) callTool(authCtx *auth.Context, params CallToolParams) CallToolResult {
	start := time.Now()
	s.logger.Info("tool call started",
		slog.String("tool", params.Name),
		slog.String("user", authCtx.UserID.String()),
	)

	var result CallToolResult
	switch params.Name {
	case "remember":
		result = s.handleRemember(authCtx, params.Arguments)
	case "recall":
		result = s.handleRecall(authCtx, params.Arguments)
	case "forget":
		result = s.handleForget(authCtx, params.Arguments)
	case "list_memories":
		result = s.handleListMemories(authCtx, params.Arguments)
	case "share_memory":
		result = s.handleShareMemory(authCtx, params.Arguments)
	case "export_memories":
		result = s.handleExportMemories(authCtx, params.Arguments)
	case "list_sessions":
		result = s.handleListSessions(authCtx, params.Arguments)
	case "session_summary":
		result = s.handleSessionSummary(authCtx, params.Arguments)
	case "session_context":
		result = s.handleSessionContext(authCtx, params.Arguments)
	default:
		result = toolError(fmt.Sprintf("Unknown tool: %s", params.Name))
	}

	duration := time.Since(start)
	s.logger.Info("tool call completed",
		slog.String("tool", params.Name),
		slog.Duration("duration", duration),
		slog.Bool("is_error", result.IsError),
	)

	s.goBackground(func() { s.createAuditLog(authCtx, params, result, duration) })

	return result
}

func (s *Server) createAuditLog(authCtx *auth.Context, params CallToolParams, result CallToolResult, duration time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	details := map[string]any{
		"tool":        params.Name,
		"duration_ms": duration.Milliseconds(),
		"is_error":    result.IsError,
	}
	if authCtx.SessionID != uuid.Nil {
		details["session_id"] = authCtx.SessionID.String()
	}

	switch params.Name {
	case "remember":
		if fact, ok := params.Arguments["fact"].(string); ok {
			if len(fact) > 100 {
				fact = fact[:100] + "..."
			}
			details["fact_preview"] = fact
		}
		if tags, ok := params.Arguments["tags"].([]any); ok {
			details["tags_count"] = len(tags)
		}
		if memType, ok := params.Arguments["memory_type"].(string); ok {
			details["memory_type"] = memType
		}
	case "recall":
		if query, ok := params.Arguments["query"].(string); ok {
			if len(query) > 100 {
				query = query[:100] + "..."
			}
			details["query"] = query
		}
		if mode, ok := params.Arguments["mode"].(string); ok {
			details["mode"] = mode
		}
	case "forget":
		if factID, ok := params.Arguments["fact_id"].(string); ok {
			details["fact_id"] = factID
		}
	case "list_memories":
		if tags, ok := params.Arguments["tags"].([]any); ok {
			details["tags_filter"] = tags
		}
	case "list_sessions":
		if limit, ok := params.Arguments["limit"].(float64); ok {
			details["limit"] = int(limit)
		}
	case "session_summary", "session_context":
		if sid, ok := params.Arguments["session_id"].(string); ok && len(sid) >= 8 {
			details["target_session"] = sid[:8] + "..."
		}
	}

	detailsJSON, err := json.Marshal(details)
	if err != nil {
		s.logger.Warn("failed to marshal audit details", slog.String("error", err.Error()))
		return
	}

	action := "mcp." + params.Name
	if result.IsError {
		action += ".error"
	}

	_, err = s.audit.CreateAuditLog(ctx, sqlc.CreateAuditLogParams{
		UserID:       pgtype.UUID{Bytes: authCtx.UserID, Valid: true},
		Action:       action,
		ResourceType: "memory",
		ResourceID:   pgtype.Text{Valid: false},
		Details:      detailsJSON,
		IpAddress:    pgtype.Text{String: authCtx.IPAddress, Valid: authCtx.IPAddress != ""},
		UserAgent:    pgtype.Text{String: authCtx.UserAgent, Valid: authCtx.UserAgent != ""},
	})
	if err != nil {
		s.logger.Warn("failed to create audit log", slog.String("error", err.Error()))
	}
}
