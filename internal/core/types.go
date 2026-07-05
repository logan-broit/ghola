// Package core holds the canonical memory operations the ghola
// local service exposes to agents. The API is:
//
//	record          append an event to the current session
//	branch          fork a new child event from an existing parent
//	bookmark        label an event for later "remember this point"
//	navigate        move the session's current pointer
//	recall          hybrid query fanning out across sietch (working)
//	                + episodic (chapterhouse) + semantic
//	                (chapterhouse), merged by score with tier
//	                attribution
//	forget          mark events for deletion across tiers
//	share           grant another user or the team visibility
//	consolidate     force-run Pipeline A for the current session
//	                (Phase 5 fills in the actual worker; 4.2 stubs
//	                the hook)
//	session_start   provision a new sietch file for a fresh session
//	session_end     final Pipeline A flush + branch-coherence pass
//	list_sessions   enumerate a user's episodic sessions
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
	ID            string          `json:"id"`
	ParentID      *string         `json:"parent_id,omitempty"`
	SessionID     string          `json:"session_id"`
	UserID        string          `json:"user_id"`
	RequestID     *string         `json:"request_id,omitempty"`
	Type          string          `json:"type"`
	Role          *string         `json:"role,omitempty"`
	Text          *string         `json:"text,omitempty"`
	ToolName      *string         `json:"tool_name,omitempty"`
	ToolUseID     *string         `json:"tool_use_id,omitempty"`
	ToolInput     json.RawMessage `json:"tool_input,omitempty"`
	ToolOutput    json.RawMessage `json:"tool_output,omitempty"`
	BookmarkLabel *string         `json:"bookmark_label,omitempty"`
	Cwd           *string         `json:"cwd,omitempty"`
	GitBranch     *string         `json:"git_branch,omitempty"`
	AgentID       *string         `json:"agent_id,omitempty"`
	IsSidechain   bool            `json:"is_sidechain"`
	Model         *string         `json:"model,omitempty"`
	RawEvent      json.RawMessage `json:"raw_event"`
	Embedding     []float32       `json:"embedding,omitempty"`
	Entities      []string        `json:"entities,omitempty"`
	Tags          []string        `json:"tags,omitempty"`
	SourceDevice  *string         `json:"source_device,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
}

// Session metadata — shared shape across sietch + episodic.
type Session struct {
	ID           string     `json:"id"`
	UserID       string     `json:"user_id"`
	StartedAt    time.Time  `json:"started_at"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	EventCount   int        `json:"event_count"`
	Summary      *string    `json:"summary,omitempty"`
	WorkspaceID  string     `json:"workspace_id,omitempty"`
	Cwd          *string    `json:"cwd,omitempty"`
	GitBranch    *string    `json:"git_branch,omitempty"`
	AgentKind    *string    `json:"agent_kind,omitempty"`
	SourceDevice *string    `json:"source_device,omitempty"`
}

// RecordInput: what agents send with `record`. The service computes
// the embedding + the projected `text` field if the caller doesn't
// supply them.
type RecordInput struct {
	SessionID string `json:"session_id"`
	UserID    string `json:"user_id"`
	// Cwd is optional. When SessionID is empty, Record uses Cwd to
	// derive a workspace via WorkspaceForCwd and either reuses the
	// most-recent open session for (UserID, workspace) or provisions
	// one inline. With an explicit SessionID, Cwd is ignored.
	Cwd      *string `json:"cwd,omitempty"`
	ParentID *string `json:"parent_id,omitempty"`
	Event    Event   `json:"event"` // partial — ID/embedding/CreatedAt may be filled
}

// RecallInput is the unified cross-tier query. The service fans out
// to sietch, then to chapterhouse's /v1/episodic/query + /v1/semantic/
// query, merges by score with tier attribution, and returns at most
// Limit rows.
type RecallInput struct {
	SessionID string `json:"session_id,omitempty"`
	UserID    string `json:"user_id"`
	// Workspace scopes every chapterhouse-backed tier (episodic,
	// keyword, session-vector, semantic) via the session_workspaces
	// join. Required unless Cwd is set, in which case it is derived
	// (see Cwd). A recall with neither is rejected — the
	// "search everything for this user" mode that 19k-row recalls
	// implied is structurally bounded around 57% R@5 on full corpora,
	// so the architectural lever is to scope first.
	Workspace string `json:"workspace"`
	// Cwd is optional. When Workspace is empty, Recall derives the
	// workspace via WorkspaceForCwd — the same mapping Record uses —
	// so MCP agents can recall with the directory they already know
	// instead of a workspace UUID they don't.
	Cwd            *string `json:"cwd,omitempty"`
	QueryText      string  `json:"query_text,omitempty"`
	Limit          int     `json:"limit,omitempty"`
	IncludeShared  bool    `json:"include_shared,omitempty"`
	IncludeSietch  bool    `json:"include_sietch,omitempty"`
	IncludeEpisode bool    `json:"include_episode,omitempty"`
	IncludeSemant  bool    `json:"include_semant,omitempty"`
	// IncludeTimings opts-in to per-stage wall-clock timings on the
	// response. Default off so agent callers (Claude via MCP) don't
	// pay the context-window cost of ~250 bytes of diagnostic JSON
	// per recall. Bench harnesses and explicit debugging callers set
	// this to true.
	IncludeTimings bool `json:"include_timings,omitempty"`
	// TagsAny is the H3.c structural era filter: when non-empty, only
	// event-grain hits whose `tags` column overlaps the supplied list
	// participate in the fan-out. Plumbed only to event-grain tiers
	// (episodic dense + episodic keyword); session_vector and semantic
	// are deliberately unfiltered (different grain — sessions and
	// mnemes — and the eval-harness experiment is event-grain only).
	// Empty/nil → no filter applied.
	TagsAny []string `json:"tags_any,omitempty"`
	// Primitives, when true, asks chapterhouse to compute a 4th
	// Hebbian-boosted sub-list (D1) over the union of the requested
	// per-tier candidates and feeds it into Recall's RRF accumulator
	// as a 6th ranked tier (alongside working / episodic / keyword /
	// session_vector / semantic). Opt-in: callers that don't care pay
	// no extra cost. Default off so agent callers (Claude via MCP) and
	// the existing eval harness baseline keep their current behavior;
	// the seeding eval harness (D4/D5) flips it to measure the lift.
	// Degraded path: chapterhouse drops the primitives field on
	// association-lookup failure, which surfaces as a nil sub-list
	// here and the RRF tier is simply absent — no error, no fallback
	// noise.
	Primitives bool `json:"primitives,omitempty"`
	// Settle opts recall into the P4 recurrent-settle expansion. Values:
	//   ""        — off (default): byte-identical to the pre-P4 pipeline.
	//   "expand"  — config A: chapterhouse runs spreading activation over
	//               the Hebbian graph and returns an `expansion` sub-list;
	//               Recall appends those text-bearing hits to the rerank
	//               pool with zero RRF mass so they can only enter the
	//               final top-K via cross-encoder score.
	//   "channel" — config B (Task 6): same expansion, but activation
	//               additionally participates in score fusion as a third
	//               channel. Task 5 carries the activation map to the
	//               fusion seam but does NOT change fusion behavior yet.
	// Any other value is rejected with ErrValidation. Validated in Recall.
	Settle string `json:"settle,omitempty"`
	// SettleParams passes the settle tuning knobs through to chapterhouse.
	// Any zero field means "server default" (chapterhouse's
	// DefaultSettleParams). Ignored entirely when Settle == "".
	SettleParams SettleParams `json:"settle_params,omitempty"`
	// ActivationWeight is the settle activation's share of the final fused
	// score when Settle == "channel" (config B). Range (0, 1]; validation
	// in Recall requires 0 < ActivationWeight and RerankWeight +
	// ActivationWeight <= 1. Ignored in all other settle modes and when
	// Settle == "" — neither parsed nor applied. Default 0.2 is applied by
	// the caller (e.g. bench harness) when channel mode is requested; the
	// zero value means "caller did not set it" and triggers a validation
	// error in channel mode so an accidental zero weight is rejected rather
	// than silently producing a two-channel result.
	ActivationWeight float64 `json:"activation_weight,omitempty"`
}

// SettleParams is the passthrough tuning block for the P4 recurrent
// settle. Zero-valued fields fall back to chapterhouse's
// DefaultSettleParams() server-side — this struct never applies its own
// defaults, it only forwards non-zero overrides so a single source of
// truth (the chapterhouse settle package) owns the defaults.
type SettleParams struct {
	Lambda   float64 `json:"lambda,omitempty"`
	HopCap   int     `json:"hop_cap,omitempty"`
	NodeCap  int     `json:"node_cap,omitempty"`
	TopM     int     `json:"top_m,omitempty"`
	Eps      float64 `json:"eps,omitempty"`
	MaxIters int     `json:"max_iters,omitempty"`
}

// RecallHit is one merged result across all tiers with attribution.
//
// SessionChunkText (when set) is the role-prefixed concatenation of
// the hit's full session, persisted at session-close by chapterhouse
// (episodic.sessions.l1_chunk_text). Recall surfaces it so the cross-
// encoder reranker can score against full session text instead of the
// single matching event's text. Empty when the session hasn't been
// consolidated yet (mid-tick, open session, or pre-migration) —
// readers must fall back to Content in that case.
type RecallHit struct {
	Tier             string  `json:"tier"` // "working" | "episodic" | "semantic"
	ID               string  `json:"id"`
	Score            float64 `json:"score"`
	Content          string  `json:"content"`
	SessionID        *string `json:"session_id,omitempty"`
	SessionChunkText string  `json:"session_chunk_text,omitempty"`
}

// RecallResult is the full response: merged list + per-tier counts
// for client diagnostics + per-stage wall-clock timings (ms) so
// clients can see where the time went.
type RecallResult struct {
	Hits       []RecallHit    `json:"hits"`
	TierCounts map[string]int `json:"tier_counts"`
	// Timings is a per-stage wall-clock breakdown in milliseconds.
	// Keys: embed, sietch_vector, sietch_fts, episodic, keyword,
	// session_vector, semantic, fanout_total, rrf_dedup, rerank,
	// total. Stages that didn't run for a given query (because gates
	// were false, e.g. no QueryText) are absent from the map.
	Timings map[string]float64 `json:"timings,omitempty"`
	// Degraded lists the recall stages that failed and were skipped
	// ("embed", "sietch_vector", "sietch_fts", "episodic", "semantic").
	// Empty/omitted means every attempted stage succeeded. Hits are
	// still valid when set — they just come from the surviving tiers.
	// "episodic" covers the single multi-ranking round-trip — its
	// vector, keyword, session_vector and primitives sub-lists fail
	// together.
	Degraded []string `json:"degraded,omitempty"`
}

// ShareInput for `share`.
type ShareInput struct {
	UserID    string  `json:"user_id"`
	Target    string  `json:"target"`              // "team" | "user"
	TargetID  *string `json:"target_id,omitempty"` // uuid, required when target="user"
	ScopeType string  `json:"scope_type"`          // "session" | "branch" | "event"
	ScopeID   string  `json:"scope_id"`
}

// ForgetInput for `forget`.
type ForgetInput struct {
	SessionID string   `json:"session_id,omitempty"`
	UserID    string   `json:"user_id"`
	EventIDs  []string `json:"event_ids"`
}

// AddSessionWorkspaceInput tags an existing session into an additional
// workspace. The session must already exist in chapterhouse (i.e.
// consolidate has fired at least once); pre-consolidate calls return
// 409 from the server and surface as *chapterhouse.StatusError to the
// HTTP layer.
type AddSessionWorkspaceInput struct {
	UserID      string `json:"user_id"`
	SessionID   string `json:"session_id"`
	WorkspaceID string `json:"workspace_id"`
}

// SessionStartInput for `session_start`.
type SessionStartInput struct {
	UserID       string  `json:"user_id"`
	WorkspaceID  string  `json:"workspace_id,omitempty"`
	AgentKind    *string `json:"agent_kind,omitempty"`
	Cwd          *string `json:"cwd,omitempty"`
	GitBranch    *string `json:"git_branch,omitempty"`
	SourceDevice *string `json:"source_device,omitempty"`
}
