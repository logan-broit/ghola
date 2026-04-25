// Package core holds the canonical memory operations the ghola
// local service exposes to agents. The API is:
//
//   record          append an event to the current session
//   branch          fork a new child event from an existing parent
//   bookmark        label an event for later "remember this point"
//   navigate        move the session's current pointer
//   recall          hybrid query fanning out across sietch (working)
//                   + episodic (chapterhouse) + semantic
//                   (chapterhouse), merged by score with tier
//                   attribution
//   forget          mark events for deletion across tiers
//   share           grant another user or the team visibility
//   consolidate     force-run Pipeline A for the current session
//                   (Phase 5 fills in the actual worker; 4.2 stubs
//                   the hook)
//   session_start   provision a new sietch file for a fresh session
//   session_end     final Pipeline A flush + branch-coherence pass
//   list_sessions   enumerate a user's episodic sessions
//
// The same Core type backs both protocol wrappings (HTTP/JSON in
// cmd/ghola, MCP in cmd/ghola-mcp) — one behavioral surface, two
// wire formats.
package core

import (
	"encoding/json"
	"time"
)

// Event is the row-per-JSONL-line shape from
// docs/2026-04-20-jsonl-native-event-shape.md, projected into Go.
type Event struct {
	ID             string          `json:"id"`
	ParentID       *string         `json:"parent_id,omitempty"`
	SessionID      string          `json:"session_id"`
	UserID         string          `json:"user_id"`
	RequestID      *string         `json:"request_id,omitempty"`
	Type           string          `json:"type"`
	Role           *string         `json:"role,omitempty"`
	Text           *string         `json:"text,omitempty"`
	ToolName       *string         `json:"tool_name,omitempty"`
	ToolUseID      *string         `json:"tool_use_id,omitempty"`
	ToolInput      json.RawMessage `json:"tool_input,omitempty"`
	ToolOutput     json.RawMessage `json:"tool_output,omitempty"`
	BookmarkLabel  *string         `json:"bookmark_label,omitempty"`
	Cwd            *string         `json:"cwd,omitempty"`
	GitBranch      *string         `json:"git_branch,omitempty"`
	AgentID        *string         `json:"agent_id,omitempty"`
	IsSidechain    bool            `json:"is_sidechain"`
	Model          *string         `json:"model,omitempty"`
	RawEvent       json.RawMessage `json:"raw_event"`
	Embedding      []float32       `json:"embedding,omitempty"`
	Entities       []string        `json:"entities,omitempty"`
	Tags           []string        `json:"tags,omitempty"`
	SourceDevice   *string         `json:"source_device,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
}

// Session metadata — shared shape across sietch + episodic.
type Session struct {
	ID           string     `json:"id"`
	UserID       string     `json:"user_id"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	EventCount   int        `json:"event_count"`
	Summary      *string    `json:"summary,omitempty"`
	Cwd          *string    `json:"cwd,omitempty"`
	GitBranch    *string    `json:"git_branch,omitempty"`
	AgentKind    *string    `json:"agent_kind,omitempty"`
	SourceDevice *string    `json:"source_device,omitempty"`
}

// RecordInput: what agents send with `record`. The service computes
// the embedding + the projected `text` field if the caller doesn't
// supply them.
type RecordInput struct {
	SessionID string  `json:"session_id"`
	UserID    string  `json:"user_id"`
	ParentID  *string `json:"parent_id,omitempty"`
	Event     Event   `json:"event"` // partial — ID/embedding/CreatedAt may be filled
}

// RecallInput is the unified cross-tier query. The service fans out
// to sietch, then to chapterhouse's /v1/episodic/query + /v1/semantic/
// query, merges by score with tier attribution, and returns at most
// Limit rows.
type RecallInput struct {
	SessionID      string `json:"session_id,omitempty"`
	UserID         string `json:"user_id"`
	Workspace      string `json:"workspace,omitempty"`
	QueryText      string `json:"query_text,omitempty"`
	Limit          int    `json:"limit,omitempty"`
	IncludeShared  bool   `json:"include_shared,omitempty"`
	IncludeSietch  bool   `json:"include_sietch,omitempty"`
	IncludeEpisode bool   `json:"include_episode,omitempty"`
	IncludeSemant  bool   `json:"include_semant,omitempty"`
}

// RecallHit is one merged result across all tiers with attribution.
type RecallHit struct {
	Tier         string  `json:"tier"` // "working" | "episodic" | "semantic"
	ID           string  `json:"id"`
	Score        float64 `json:"score"`
	Content      string  `json:"content"`
	SessionID    *string `json:"session_id,omitempty"`
	Concept      *string `json:"concept,omitempty"` // semantic tier
	Confidence   *float64 `json:"confidence,omitempty"`
}

// RecallResult is the full response: merged list + per-tier counts
// for client diagnostics.
type RecallResult struct {
	Hits        []RecallHit    `json:"hits"`
	TierCounts  map[string]int `json:"tier_counts"`
}

// ShareInput for `share`.
type ShareInput struct {
	UserID    string  `json:"user_id"`
	Target    string  `json:"target"`               // "team" | "user"
	TargetID  *string `json:"target_id,omitempty"`  // uuid, required when target="user"
	ScopeType string  `json:"scope_type"`           // "session" | "branch" | "event"
	ScopeID   string  `json:"scope_id"`
}

// ForgetInput for `forget`.
type ForgetInput struct {
	SessionID string   `json:"session_id,omitempty"`
	UserID    string   `json:"user_id"`
	EventIDs  []string `json:"event_ids"`
}

// SessionStartInput for `session_start`.
type SessionStartInput struct {
	UserID       string  `json:"user_id"`
	AgentKind    *string `json:"agent_kind,omitempty"`
	Cwd          *string `json:"cwd,omitempty"`
	GitBranch    *string `json:"git_branch,omitempty"`
	SourceDevice *string `json:"source_device,omitempty"`
}
