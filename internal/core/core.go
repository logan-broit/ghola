package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/logan-broit/ghola/internal/truthsayer"
)

// Core is the single behavioral surface the HTTP + MCP wrappers share.
type Core struct {
	Sietch       SietchStore
	Chapterhouse ChapterhouseClient
	Embedder     Embedder
	// Truthsayer is the cross-encoder reranker client. nil disables
	// Stage 2/3 — Recall falls back to RRF-only and never errors on
	// the rerank path. Wired by main.go from TRUTHSAYER_URL env.
	Truthsayer *truthsayer.Client
	// Now is the time source; tests inject a fixed clock.
	Now func() time.Time
	// RRFK is the reciprocal-rank-fusion constant used by Recall to
	// merge ranked tiers. 60 is the literature default. Smaller k lets
	// the very top ranks dominate; larger k flattens the curve.
	RRFK int
	// RerankTopK is the size of the candidate pool sent to Truthsayer.
	// 50 matches the bench reference and gives the cross-encoder a
	// meaningful chance to reorder beyond the user's top-N. Capped by
	// fan-out width when the actual fused pool is smaller.
	RerankTopK int
	// RerankWeight is the cross-encoder's share of the final fused
	// score, [0,1]. 0 = RRF only, 1 = reranker only.
	//
	// Default 0.5 is the bench-validated balance point. The PR-C.3
	// sweep on LongMemEval-S found 0.7 was a +0.6pp R@5 win at the
	// time (57.6% vs 57.0%), but only because two of the three RRF
	// tiers were empty (semantic.mnemes pending PR-E, BM25 keyword
	// pending PR-D), so RRF was effectively a single-tier rank-list.
	// With the full three-tier fan-out, the RRF agreement signal
	// strengthens and the equilibrium shifts back toward 0.5. Holding
	// the default here so we don't over-fit to the under-populated
	// RRF distribution; re-run the sweep after PR-D and again after
	// PR-E to set a data-driven default against the production shape.
	RerankWeight float64
	// Settle is the server default for the P4 recurrent-settle mode
	// applied when a recall request leaves RecallInput.Settle unset.
	// "channel" (the default) opts every recall into config B: chapterhouse
	// runs spreading activation over the Hebbian graph and activation
	// participates in score fusion. "off" disables settle for unset
	// requests (the pre-P4 pipeline); it is the deployment kill-switch,
	// wired from GHOLA_SETTLE in main.go. Explicit per-request Settle
	// values ("off"/"expand"/"channel") always override this default.
	//
	// Default "channel" flips the LongMemEval-gated on-by-default decision
	// (R@5 99.6% under channel@0.40 vs the 99.4% no-regression bar); see
	// docs/benchmarks.md "Settle gate".
	Settle string
	// ActivationWeight is the server default for the channel-mode
	// activation weight, applied when the effective settle mode is
	// "channel" and the request left RecallInput.ActivationWeight unset.
	// 0.40 is the activation-weight sweep's recommended default. Wired
	// from GHOLA_ACTIVATION_WEIGHT in main.go. An explicit per-request
	// ActivationWeight is respected verbatim.
	ActivationWeight float64
	// RerankTimeout caps the truthsayer round-trip. Failures (timeout,
	// 5xx, transport) degrade to RRF-only with a warn log.
	RerankTimeout time.Duration
	// TierTimeout caps each recall tier's round-trip independently.
	// A tier that exceeds it (or errors) is dropped from the fan-out
	// and reported in RecallResult.Degraded — one slow tier must not
	// stall or fail the whole recall.
	TierTimeout time.Duration
	// SietchRetention is how long an ended, fully-drained session's
	// sietch file is kept before GC deletes it. The file is a
	// redundant local cache once consolidation has shipped everything;
	// the buffer exists so a consolidation bug can be debugged against
	// the original. 0 disables GC entirely.
	SietchRetention time.Duration
	// SessionIdleTimeout is how long a session may sit without a new
	// event before it stops being reusable on the record path
	// (findOpenSession refuses it) and becomes eligible for the
	// encoding worker's idle sweep (SweepIdleSession closes it). Wired
	// from GHOLA_SESSION_IDLE_HOURS in main.go. 0 (or negative)
	// disables both seams: findOpenSession reuses regardless of age and
	// the sweep is a no-op. Default 4h (design: 13 gaps > 4h in the
	// dogfooding session yield ~14 natural episodes; 1h over-fragments,
	// 24h under-fragments).
	SessionIdleTimeout time.Duration
}

// New builds a Core with sensible defaults.
func New(s SietchStore, ch ChapterhouseClient, emb Embedder) *Core {
	return &Core{
		Sietch:             s,
		Chapterhouse:       ch,
		Embedder:           emb,
		Now:                func() time.Time { return time.Now().UTC() },
		RRFK:               60,
		RerankTopK:         50,
		RerankWeight:       0.5,
		Settle:             "channel",
		ActivationWeight:   0.40,
		RerankTimeout:      30 * time.Second,
		TierTimeout:        10 * time.Second,
		SietchRetention:    7 * 24 * time.Hour,
		SessionIdleTimeout: 4 * time.Hour,
	}
}

// ErrValidation marks input-validation errors that should surface as
// HTTP 400 at the boundary. Callers use errors.Is(err, ErrValidation)
// instead of string-matching the message text. Every sentinel below
// wraps it, plus any ad-hoc validation errors thrown inline (e.g. the
// invalid-UUID case in SessionStart).
var ErrValidation = errors.New("validation")

var (
	ErrMissingSessionID = fmt.Errorf("%w: session_id required", ErrValidation)
	ErrMissingUserID    = fmt.Errorf("%w: user_id required", ErrValidation)
	// ErrMissingWorkspace is retained as the legacy workspace-required
	// sentinel. No code path returns it any more — both SessionStart and
	// Recall now accept a cwd anchor and fail with ErrMissingWorkspaceOrCwd
	// — but it stays exported so external callers' errors.Is checks keep
	// compiling. Both wrap ErrValidation, so boundary 400-mapping is
	// unaffected either way.
	ErrMissingWorkspace = fmt.Errorf("%w: workspace required", ErrValidation)
	// ErrMissingWorkspaceOrCwd: SessionStart and Recall each need at
	// least one anchor for workspace scoping — an explicit workspace or
	// a cwd to derive one from. Together they enforce "every
	// chapterhouse-bound query carries a workspace, every ingested
	// session is scoped to one."
	ErrMissingWorkspaceOrCwd = fmt.Errorf("%w: workspace_id or cwd required", ErrValidation)
)

// AllowedEventTypes is the set accepted by the sietch events.type CHECK
// constraint. Callers (HTTP, MCP) validate at the boundary so an
// unrecognized type surfaces as 400 with this list named, not as an
// opaque 500 from SQLite.
var AllowedEventTypes = []string{"user", "assistant", "tool_result", "system"}

// MaxSettleNodeCap is the runaway ceiling for SettleParams.NodeCap,
// validated at the Recall boundary. Chapterhouse's DefaultSettleParams
// uses 2000 (see primitives.DefaultSettleParams); this cap sits an order
// of magnitude above that so legitimate tuning is unconstrained while an
// accidental 1_000_000 (which would fan out the whole Hebbian graph) is
// rejected as a 400 rather than silently building a runaway neighborhood.
const MaxSettleNodeCap = 20000

// ValidateEventType returns an ErrValidation-wrapped error if t is not
// in AllowedEventTypes. The error message names the offending value and
// lists the allowed set so callers can report it directly.
func ValidateEventType(t string) error {
	for _, allowed := range AllowedEventTypes {
		if t == allowed {
			return nil
		}
	}
	return fmt.Errorf("%w: event.type %q is not allowed; must be one of: %s",
		ErrValidation, t, strings.Join(AllowedEventTypes, ", "))
}

// ErrSessionNotFound marks a sietch store whose file exists but holds
// no session row — typically a file recreated by conn()'s
// create-on-open after GC. Callers distinguish it from real failures:
// GCSession treats it as an orphan to remove.
var ErrSessionNotFound = errors.New("session not found")

// SessionStart provisions a new sietch file + inserts the session row.
func (c *Core) SessionStart(ctx context.Context, in SessionStartInput) (Session, error) {
	if in.UserID == "" {
		return Session{}, ErrMissingUserID
	}

	// Resolve workspace_id: explicit > cwd-derived > error.
	//
	// The explicit `*in.Cwd != ""` guard catches both nil-pointer and
	// empty-string-pointer cases — without it a non-nil pointer to ""
	// would derive WorkspaceForCwd("") and silently collapse all
	// empty-cwd sessions to one workspace_id.
	var wsID uuid.UUID
	switch {
	case in.WorkspaceID != "":
		parsed, err := uuid.Parse(in.WorkspaceID)
		if err != nil {
			return Session{}, fmt.Errorf("%w: workspace_id must be a valid UUID: %w", ErrValidation, err)
		}
		wsID = parsed
	case in.Cwd != nil && *in.Cwd != "":
		wsID = WorkspaceForCwd(*in.Cwd)
	default:
		return Session{}, ErrMissingWorkspaceOrCwd
	}

	s := Session{
		ID:           NewID(),
		UserID:       in.UserID,
		StartedAt:    c.Now(),
		WorkspaceID:  wsID.String(),
		AgentKind:    in.AgentKind,
		Cwd:          in.Cwd,
		GitBranch:    in.GitBranch,
		SourceDevice: in.SourceDevice,
	}
	if err := c.Sietch.OpenSession(ctx, s); err != nil {
		return Session{}, fmt.Errorf("open session: %w", err)
	}
	return s, nil
}

// resolveImplicitSession picks (or provisions) a session when Record is
// called without an explicit SessionID. Rule: reuse the most-recent
// un-ended session for (UserID, WorkspaceForCwd(*Cwd)); else create
// one via SessionStart. Cwd is required — without it there's no
// workspace anchor, and we fail closed with ErrMissingWorkspaceOrCwd
// (a validation error → boundary returns 400).
func (c *Core) resolveImplicitSession(ctx context.Context, in RecordInput) (string, error) {
	if in.Cwd == nil || *in.Cwd == "" {
		return "", ErrMissingWorkspaceOrCwd
	}
	workspaceID := WorkspaceForCwd(*in.Cwd).String()

	if sid, ok, err := c.findOpenSession(ctx, in.UserID, workspaceID); err != nil {
		return "", err
	} else if ok {
		return sid, nil
	}

	sess, err := c.SessionStart(ctx, SessionStartInput{
		UserID: in.UserID,
		Cwd:    in.Cwd,
	})
	if err != nil {
		return "", fmt.Errorf("auto session_start: %w", err)
	}
	return sess.ID, nil
}

// findOpenSession returns the most-recent un-ended, still-fresh session
// for (userID, workspaceID), or ok=false when there is none. "Fresh"
// means its last activity is younger than c.SessionIdleTimeout (see
// sessionReusable). A stale session is skipped here — it stays open for
// the encoding worker's sweep to close — so the record path never
// fragments a live conversation yet also never re-attaches to a session
// that has gone cold.
func (c *Core) findOpenSession(ctx context.Context, userID, workspaceID string) (string, bool, error) {
	sessions, err := c.Sietch.ListSessions(ctx, userID)
	if err != nil {
		return "", false, fmt.Errorf("list sessions (user=%q): %w", userID, err)
	}
	now := c.Now()
	var best *Session
	for i := range sessions {
		s := &sessions[i]
		if s.WorkspaceID != workspaceID || s.EndedAt != nil {
			continue
		}
		if !sessionReusable(*s, now, c.SessionIdleTimeout) {
			continue
		}
		if best == nil || s.StartedAt.After(best.StartedAt) {
			best = s
		}
	}
	if best == nil {
		return "", false, nil
	}
	return best.ID, true, nil
}

// lastActivity is a session's most recent activity timestamp: its last
// event's created_at when present, else its start. The single source of
// truth for "how cold is this session", shared by sessionReusable
// (record path) and SweepIdleSession (worker path).
func lastActivity(s Session) time.Time {
	if s.LastEventAt != nil {
		return *s.LastEventAt
	}
	return s.StartedAt
}

// sessionReusable reports whether an open session is fresh enough to
// append to. idle <= 0 disables the check (always reusable). Otherwise
// the session is reusable only if its last activity is strictly younger
// than idle — age exactly equal to the timeout counts as stale.
//
// Traced edge: if the embedder is down, an event can get stuck on
// needsEmbedding, which blocks that session's drain-to-episodic. If
// such a session then goes stale (per this function), it falls out of
// the sietch tier's reusable scope while its tail is still not in
// episodic — a temporary recall blind spot. This self-heals once the
// embedder recovers: the encoding worker retries the stuck event every
// tick regardless of the session's staleness.
func sessionReusable(s Session, now time.Time, idle time.Duration) bool {
	if idle <= 0 {
		return true
	}
	return now.Sub(lastActivity(s)) < idle
}

// SessionEnd finalizes the session — flushes any remaining events to
// chapterhouse and closes the sietch handle. Pipeline A's continuous
// tick (Phase 5) supersedes most of this; SessionEnd is the explicit
// fallback for agents that quit before the tick.
//
// Order matters: MarkEnded must precede Consolidate so the
// chapterhouse upsert carries ended_at. The chapterhouse reconciler
// pools sessions where `ended_at IS NOT NULL AND l1_embedding IS
// NULL` — leaving ended_at NULL silently disables predictive replay
// for the entire session.
func (c *Core) SessionEnd(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return ErrMissingSessionID
	}
	if err := c.Sietch.MarkEnded(ctx, sessionID, c.Now()); err != nil {
		return fmt.Errorf("mark ended (session=%q): %w", sessionID, err)
	}
	if _, err := c.Consolidate(ctx, sessionID); err != nil {
		return fmt.Errorf("consolidate before end (session=%q): %w", sessionID, err)
	}
	return c.Sietch.CloseSession(ctx, sessionID)
}

// ListSessions enumerates a user's sessions (sietch-visible slice
// only for now — episodic listing lands with Pipeline B in Phase 8).
func (c *Core) ListSessions(ctx context.Context, userID string) ([]Session, error) {
	if userID == "" {
		return nil, ErrMissingUserID
	}
	return c.Sietch.ListSessions(ctx, userID)
}

// Record appends an event to the current session. The embedding is
// computed when absent.
//
// When SessionID is empty, Record falls back to cwd-derived session
// resolution (resolveImplicitSession): reuse the most-recent open
// session for (UserID, WorkspaceForCwd(*Cwd)), else provision one
// inline. This is the affordance for MCP hosts (Claude Code, Codex,
// Cursor) where the protocol has no conversation-start hook, so the
// model would otherwise have to remember to call session_start
// itself — which it usually doesn't.
func (c *Core) Record(ctx context.Context, in RecordInput) (Event, error) {
	if in.UserID == "" {
		return Event{}, ErrMissingUserID
	}
	if in.SessionID == "" {
		sid, err := c.resolveImplicitSession(ctx, in)
		if err != nil {
			return Event{}, err
		}
		in.SessionID = sid
	}

	ev := in.Event
	if ev.ID == "" {
		ev.ID = NewID()
	}
	ev.SessionID = in.SessionID
	ev.UserID = in.UserID
	if ev.ParentID == nil && in.ParentID != nil {
		ev.ParentID = in.ParentID
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = c.Now()
	}

	if len(ev.Embedding) == 0 && ev.Text != nil && *ev.Text != "" {
		emb, err := c.Embedder.Embed(ctx, *ev.Text)
		if err != nil {
			// Never lose a write because the embedder is down. The
			// event lands in sietch without an embedding; the encoding
			// worker's backfill pass (BackfillEmbeddings) fills it in
			// when the embedder recovers, and Consolidate holds it
			// back from chapterhouse until then.
			slog.WarnContext(ctx, "embed failed; recording without embedding",
				"session_id", ev.SessionID, "err", err.Error())
		} else {
			ev.Embedding = emb
		}
	}

	stored, err := c.Sietch.RecordEvent(ctx, ev)
	if err != nil {
		return Event{}, fmt.Errorf("record (session=%q): %w", ev.SessionID, err)
	}
	if err := c.Sietch.SetCurrent(ctx, stored.SessionID, stored.ID); err != nil {
		return stored, fmt.Errorf("set current: %w", err)
	}
	return stored, nil
}

// BackfillEmbeddings embeds events recorded while the embedder was
// down (Record degrades to no-embedding rather than losing the write).
// Returns the number backfilled. Called by the encoding worker before
// each Consolidate pass; safe to call any time.
func (c *Core) BackfillEmbeddings(ctx context.Context, sessionID string) (int, error) {
	if sessionID == "" {
		return 0, ErrMissingSessionID
	}
	const batch = 64
	total := 0
	for {
		evs, err := c.Sietch.EventsNeedingEmbedding(ctx, sessionID, batch)
		if err != nil {
			return total, fmt.Errorf("events needing embedding (session=%q): %w", sessionID, err)
		}
		if len(evs) == 0 {
			return total, nil
		}
		for _, ev := range evs {
			emb, err := c.Embedder.Embed(ctx, *ev.Text)
			if err != nil {
				return total, fmt.Errorf("backfill embed (session=%q event=%q): %w", sessionID, ev.ID, err)
			}
			if err := c.Sietch.SetEmbedding(ctx, sessionID, ev.ID, emb); err != nil {
				return total, fmt.Errorf("set embedding (event=%q): %w", ev.ID, err)
			}
			total++
		}
		if len(evs) < batch {
			return total, nil
		}
	}
}

// Branch starts a new child event from an existing parent and moves
// the session's current pointer to the new node.
func (c *Core) Branch(ctx context.Context, in RecordInput) (Event, error) {
	if in.ParentID == nil || *in.ParentID == "" {
		return Event{}, errors.New("parent_id required for branch")
	}
	return c.Record(ctx, in)
}

// Bookmark labels an event for later recall / review.
func (c *Core) Bookmark(ctx context.Context, sessionID, eventID, label string) error {
	if sessionID == "" {
		return ErrMissingSessionID
	}
	if eventID == "" || label == "" {
		return errors.New("event_id and label required")
	}
	return c.Sietch.SetBookmark(ctx, sessionID, eventID, label)
}

// Navigate moves the current-event pointer.
func (c *Core) Navigate(ctx context.Context, sessionID, eventID string) error {
	if sessionID == "" {
		return ErrMissingSessionID
	}
	if eventID == "" {
		return errors.New("event_id required")
	}
	return c.Sietch.SetCurrent(ctx, sessionID, eventID)
}

// Recall fans out across tiers, fuses their ranked outputs with
// reciprocal rank fusion (Cormack 2009; see internal/core/rrf.go) and
// returns the top-N hits. Each output hit's Score is the RRF score, not
// the original tier-level cosine/FTS score — the per-tier score scales
// don't compete on the same axis, which is exactly what RRF dodges.
//
// Tier dedup: within a tier, multiple events from the same session
// collapse to the highest-scoring event for ranking purposes. Across
// tiers, the same session/document is fused into one output hit. The
// exemplar event (used for Content + ID + Tier attribution in the
// output) is the one with the highest *raw* tier score across the
// tiers that returned it.
func (c *Core) Recall(ctx context.Context, in RecallInput) (RecallResult, error) {
	if in.UserID == "" {
		return RecallResult{}, ErrMissingUserID
	}
	// Workspace resolution mirrors Record: explicit wins, else derive
	// from cwd. The `*in.Cwd != ""` guard catches the pointer-to-empty
	// case so we don't collapse every empty-cwd recall onto a single
	// bogus WorkspaceForCwd("") id.
	if in.Workspace == "" && in.Cwd != nil && *in.Cwd != "" {
		in.Workspace = WorkspaceForCwd(*in.Cwd).String()
	}
	if in.Workspace == "" {
		return RecallResult{}, ErrMissingWorkspaceOrCwd
	}
	if in.Limit <= 0 {
		in.Limit = 10
	}
	// Settle server-default application. Runs BEFORE validation so the
	// effective mode + weight are checked by the same boundary rules an
	// explicit request would hit (wr+wa<=1, settle_params ranges). The
	// wire contract:
	//   ""        (unset)  → server default c.Settle. When that resolves
	//                        to "channel" and the caller left
	//                        ActivationWeight unset, apply c.ActivationWeight
	//                        so the default weight rides along with the
	//                        default mode.
	//   "off"              → settle disabled — the pre-P4 pipeline. This is
	//                        the explicit opt-out; normalized to the "" the
	//                        downstream gates key on (settle_params ignored).
	//   "expand"/"channel" → explicit opt-in, unchanged. An explicit
	//                        ActivationWeight is respected verbatim; an
	//                        unset one in channel mode falls back to
	//                        c.ActivationWeight.
	if in.Settle == "" {
		in.Settle = c.Settle
	}
	if in.Settle == "channel" && in.ActivationWeight == 0 {
		in.ActivationWeight = c.ActivationWeight
	}
	// "off" is the explicit disable: collapse it to the empty-string
	// "settle off" sentinel every downstream gate (in.Settle != "") keys
	// on, so the off path is byte-identical to the pre-P4 pipeline.
	if in.Settle == "off" {
		in.Settle = ""
	}
	// Settle mode validation. "off" / "" are the disabled paths; "expand"
	// (config A) and "channel" (config B) opt into the P4 recurrent-settle
	// expansion. Reject anything else with the boundary-400 validation
	// error style the rest of Recall uses (errors.Is(err, ErrValidation)).
	// Runs against the effective mode (post default + off-normalization).
	switch in.Settle {
	case "", "expand", "channel":
	default:
		return RecallResult{}, fmt.Errorf("%w: settle must be one of \"off\", \"expand\", \"channel\", got %q", ErrValidation, in.Settle)
	}
	// SettleParams validation. Reject out-of-range tuning knobs at the
	// boundary (400) instead of forwarding them to chapterhouse, which
	// silently substitutes DefaultSettleParams for any non-positive field
	// — so a typo like lambda=-0.7 would otherwise be absorbed rather than
	// surfaced. Zero values mean "use server default" (the wire contract)
	// and stay valid. Validated only when Settle != "" — with settle off
	// the params are ignored entirely, so an invalid block must not reject.
	if in.Settle != "" {
		p := in.SettleParams
		if p.Lambda != 0 && (p.Lambda <= 0 || p.Lambda >= 1) {
			return RecallResult{}, fmt.Errorf("%w: settle_params.lambda must be in (0, 1), got %v", ErrValidation, p.Lambda)
		}
		if p.Eps != 0 && p.Eps <= 0 {
			return RecallResult{}, fmt.Errorf("%w: settle_params.eps must be > 0, got %v", ErrValidation, p.Eps)
		}
		if p.MaxIters != 0 && p.MaxIters < 0 {
			return RecallResult{}, fmt.Errorf("%w: settle_params.max_iters must be > 0, got %v", ErrValidation, p.MaxIters)
		}
		if p.HopCap != 0 && p.HopCap < 0 {
			return RecallResult{}, fmt.Errorf("%w: settle_params.hop_cap must be > 0, got %v", ErrValidation, p.HopCap)
		}
		if p.TopM != 0 && p.TopM < 0 {
			return RecallResult{}, fmt.Errorf("%w: settle_params.top_m must be > 0, got %v", ErrValidation, p.TopM)
		}
		if p.NodeCap != 0 && (p.NodeCap < 0 || p.NodeCap > MaxSettleNodeCap) {
			return RecallResult{}, fmt.Errorf("%w: settle_params.node_cap must be in (0, %d], got %v", ErrValidation, MaxSettleNodeCap, p.NodeCap)
		}
	}
	// Channel-mode weight validation. ActivationWeight is only meaningful
	// (and only applied) in "channel" mode. Validate here so a missing or
	// out-of-range weight surfaces as a 400 at the boundary rather than
	// silently producing a mis-blended result. c.RerankWeight+wActivation
	// <= 1 is the mathematical invariant — the rrfWeight floor would go
	// negative otherwise, producing nonsensical scores.
	if in.Settle == "channel" {
		if in.ActivationWeight <= 0 {
			return RecallResult{}, fmt.Errorf("%w: activation_weight must be > 0 in channel mode, got %v", ErrValidation, in.ActivationWeight)
		}
		if c.RerankWeight+in.ActivationWeight > 1.0 {
			return RecallResult{}, fmt.Errorf("%w: rerank_weight (%v) + activation_weight (%v) must be <= 1", ErrValidation, c.RerankWeight, in.ActivationWeight)
		}
	}

	// Default to all three tiers when the caller didn't say.
	if !in.IncludeSietch && !in.IncludeEpisode && !in.IncludeSemant {
		in.IncludeSietch = true
		in.IncludeEpisode = true
		in.IncludeSemant = true
	}

	// The MCP agent rarely knows a session id. When the working tier is
	// requested without one, scope it to the most-recent open session
	// in this workspace — the same session Record would append to.
	// A miss or error just means no working-tier hits, not a failure.
	if in.IncludeSietch && in.SessionID == "" {
		if sid, ok, err := c.findOpenSession(ctx, in.UserID, in.Workspace); err == nil && ok {
			in.SessionID = sid
		}
	}

	// Per-stage wall-clock timings, opt-in via in.IncludeTimings.
	// Default off so agent callers (Claude via MCP) don't pay the
	// context-window cost of ~250 bytes of diagnostic JSON per recall;
	// bench harnesses and explicit debugging callers set the flag and
	// get the full breakdown. When off, the map stays nil, time.Now()
	// calls are skipped at the call sites, and the response Timings
	// field is omitted (omitempty on RecallResult.Timings).
	//
	// Concurrency-safe by construction: parallel tiers each capture
	// their own duration to a closure-local variable regardless of
	// the flag (cheap); the timings map is written only from the main
	// goroutine after Wait, gated on the flag.
	var timings map[string]float64
	var recallStart time.Time
	if in.IncludeTimings {
		timings = make(map[string]float64, 12)
		recallStart = time.Now()
	}

	var degraded []string
	var emb []float32
	if in.QueryText != "" {
		var embStart time.Time
		if in.IncludeTimings {
			embStart = time.Now()
		}
		e, err := c.Embedder.Embed(ctx, in.QueryText)
		if in.IncludeTimings {
			timings["embed"] = ms(time.Since(embStart))
		}
		if err != nil {
			// Lexical tiers don't need the query embedding; degrade
			// to FTS-only instead of failing the whole recall.
			slog.WarnContext(ctx, "query embed failed; lexical-only recall",
				"err", err.Error())
			degraded = append(degraded, "embed")
		} else {
			emb = e
		}
	}

	// Per-tier fan-out, parallelized via sync.WaitGroup. The tiers are
	// independent: each goroutine wraps its call in its own
	// context.WithTimeout(ctx, c.TierTimeout) and writes ONLY its own
	// captured hit slice, duration, and error variable. No shared
	// mutable state crosses goroutines, and wg.Wait() establishes a
	// happens-before edge for every read below — safe under -race.
	//
	// Degrade-and-report, not cancel-on-first-error: a tier that times
	// out or errors is dropped from the fan-out and its name is appended
	// to `degraded` after Wait. The surviving tiers' hits still fuse and
	// return. Only when EVERY attempted tier failed (failed == attempted,
	// attempted > 0) do we error — there are no hits to return. This is
	// the deliberate inverse of the old errgroup contract, where the
	// first tier error cancelled the rest and sank the whole recall;
	// one slow or down tier must not take recall with it.
	//
	// Ordering after Wait is irrelevant to RRF: dedup-by-key and fusion
	// are order-independent, and exemplar selection picks the highest
	// raw score across tiers. The degraded-tier list IS order-stable
	// (fixed check order below) so callers get deterministic JSON.
	var (
		sietchVectorHits  []RecallHit
		sietchFTSHits     []RecallHit
		episodicHits      []RecallHit
		keywordHits       []RecallHit
		sessionVectorHits []RecallHit
		semanticHits      []RecallHit
		// primitivesHits is the 4th sub-list from QueryEpisodicMulti,
		// surfaced when in.Primitives is true AND the chapterhouse
		// handler returned a non-nil Primitives slice. nil → tier absent
		// from the fan-out (either flag was off, OR the server-side
		// association lookup failed and the handler dropped the field);
		// non-nil empty → flag was on but no in-set boosts surfaced (the
		// seeded set has no associations among themselves) — RRF treats
		// this as a tier with zero entries, a clean no-op.
		primitivesHits []RecallHit
		// expansionHits is the P4 recurrent-settle sub-list (config A/B).
		// Unlike primitivesHits it is NOT an RRF tier — it is appended to
		// the rerank pool after fusion (see the seam below). nil when
		// settle was off or the handler dropped it.
		expansionHits []ExpansionHit

		sietchVectorDur  time.Duration
		sietchFTSDur     time.Duration
		episodicMultiDur time.Duration
		semanticDur      time.Duration

		// Per-tier errors. Each goroutine writes only its own; read
		// after Wait. sietch vector + FTS land in the episodic / sietch
		// degrade buckets below via the fixed check order.
		sietchVectorErr error
		sietchFTSErr    error
		episodicErr     error
		semanticErr     error
	)

	var fanoutStart time.Time
	if in.IncludeTimings {
		fanoutStart = time.Now()
	}

	var wg sync.WaitGroup
	run := func(f func()) { wg.Add(1); go func() { defer wg.Done(); f() }() }
	// attempted counts the tiers actually fanned out (gates passed), so
	// the all-fail check below distinguishes "every tier we tried died"
	// from "no tier was eligible" (attempted == 0, not an error).
	attempted := 0

	if in.IncludeSietch && in.SessionID != "" {
		if len(emb) > 0 {
			attempted++
			run(func() {
				tctx, cancel := context.WithTimeout(ctx, c.TierTimeout)
				defer cancel()
				s := time.Now()
				h, err := c.Sietch.SearchVector(tctx, in.SessionID, emb, in.Limit)
				sietchVectorDur = time.Since(s)
				if err != nil {
					sietchVectorErr = fmt.Errorf("sietch vector (session=%q): %w", in.SessionID, err)
					return
				}
				sietchVectorHits = h
			})
		}
		if in.QueryText != "" {
			attempted++
			run(func() {
				tctx, cancel := context.WithTimeout(ctx, c.TierTimeout)
				defer cancel()
				s := time.Now()
				h, err := c.Sietch.SearchFTS(tctx, in.SessionID, in.QueryText, in.Limit)
				sietchFTSDur = time.Since(s)
				if err != nil {
					sietchFTSErr = fmt.Errorf("sietch fts (session=%q): %w", in.SessionID, err)
					return
				}
				sietchFTSHits = h
			})
		}
	}

	if in.IncludeEpisode {
		// Build the rankings subset by gating each tier on the inputs
		// it actually uses:
		//   - "vector" runs unconditionally: the chapterhouse handler
		//     short-circuits when QueryEmbedding is empty, so the
		//     wasted call cost is negligible and the symmetry keeps
		//     the gate logic simple.
		//   - "fts" requires a query string; FTS over an empty query
		//     is meaningless.
		//   - "session_vector" requires an embedding; without one the
		//     pooled-vector cosine has nothing to score against.
		// One round-trip drives all requested tiers; it runs in parallel
		// with the semantic call.
		rankings := make([]string, 0, 3)
		rankings = append(rankings, "vector")
		if in.QueryText != "" {
			rankings = append(rankings, "fts")
		}
		if len(emb) > 0 {
			rankings = append(rankings, "session_vector")
		}

		attempted++
		run(func() {
			tctx, cancel := context.WithTimeout(ctx, c.TierTimeout)
			defer cancel()
			s := time.Now()
			res, err := c.Chapterhouse.QueryEpisodicMulti(tctx, EpisodicMultiQuery{
				UserID:         in.UserID,
				WorkspaceID:    in.Workspace,
				QueryText:      in.QueryText,
				QueryEmbedding: emb,
				Limit:          in.Limit,
				IncludeShared:  in.IncludeShared,
				TagsAny:        in.TagsAny,
				Rankings:       rankings,
				Primitives:     in.Primitives,
				// Any non-empty Settle mode ("expand"/"channel") carries
				// the settle block; the params passthrough forwards the
				// tuning knobs (zero → chapterhouse default). Config A vs B
				// diverges only in ghola-side fusion, not in the query.
				Settle:       in.Settle != "",
				SettleParams: in.SettleParams,
			})
			episodicMultiDur = time.Since(s)
			if err != nil {
				episodicErr = fmt.Errorf("episodic multi (user=%q workspace=%q): %w", in.UserID, in.Workspace, err)
				return
			}
			// Map each requested ranking sub-list back onto the
			// per-tier hit slot the post-fan-out RRF logic already
			// consumes. Tier strings on the response are the
			// downstream-canonical ones the chapterhouse client
			// already remapped ("episodic", "keyword",
			// "session_vector"), so hitKey() and TierCounts work
			// unchanged.
			episodicHits = res.Vector
			keywordHits = res.FTS
			sessionVectorHits = res.SessionVector
			// Primitives uses pointer-to-slice for 3-state semantics
			// (nil → field absent on the wire; non-nil → flag was on
			// and the handler returned the sub-list, possibly empty).
			// Dereference only on non-nil; an empty slice flows through
			// to RRF as a zero-entry tier (no-op).
			if res.Primitives != nil {
				primitivesHits = *res.Primitives
			}
			// Expansion (P4): the settle sub-list. Captured here so the
			// post-fan-out seam (after the RRF pool loop, before rerank)
			// can append it. nil when settle was off OR the handler
			// dropped it on best-effort failure — either way the seam
			// treats it as "no expansion". This shares the "episodic"
			// degrade bucket with the other sub-lists: a QueryEpisodicMulti
			// failure sinks expansion too, which is correct (no episodic
			// call, no seeds, no expansion).
			expansionHits = res.Expansion
		})
	}

	if in.IncludeSemant {
		attempted++
		run(func() {
			tctx, cancel := context.WithTimeout(ctx, c.TierTimeout)
			defer cancel()
			s := time.Now()
			h, err := c.Chapterhouse.QuerySemantic(tctx, SemanticQuery{
				Workspace:      in.Workspace,
				QueryText:      in.QueryText,
				QueryEmbedding: emb,
				Limit:          in.Limit,
			})
			semanticDur = time.Since(s)
			if err != nil {
				semanticErr = fmt.Errorf("semantic: %w", err)
				return
			}
			semanticHits = h
		})
	}

	wg.Wait()

	// Tally tier failures in a fixed order so RecallResult.Degraded is
	// stable JSON for callers. sietch's two sub-queries collapse into
	// "sietch_vector" / "sietch_fts" degrade entries; the episodic multi
	// call is one "episodic" entry; semantic is one "semantic" entry.
	checks := []struct {
		name string
		err  error
	}{
		{"sietch_vector", sietchVectorErr},
		{"sietch_fts", sietchFTSErr},
		{"episodic", episodicErr},
		{"semantic", semanticErr},
	}
	var firstErr error
	failed := 0
	for _, ck := range checks {
		if ck.err == nil {
			continue
		}
		failed++
		degraded = append(degraded, ck.name)
		if firstErr == nil {
			firstErr = ck.err
		}
		slog.WarnContext(ctx, "recall tier failed; degrading", "tier", ck.name, "err", ck.err.Error())
	}
	if attempted > 0 && failed == attempted {
		return RecallResult{}, fmt.Errorf("recall: all %d tiers failed (%s): %w", attempted, strings.Join(degraded, ", "), firstErr)
	}

	if in.IncludeTimings {
		timings["fanout_total"] = ms(time.Since(fanoutStart))
		if sietchVectorDur > 0 {
			timings["sietch_vector"] = ms(sietchVectorDur)
		}
		if sietchFTSDur > 0 {
			timings["sietch_fts"] = ms(sietchFTSDur)
		}
		// One key for the multi-ranking call replaces the previous
		// per-tier (episodic/keyword/session_vector) trio that A6 left
		// all carrying the same round-trip elapsed — a misleading
		// signal for any future reader trying to attribute latency
		// to a specific tier.
		if episodicMultiDur > 0 {
			timings["episodic_multi"] = ms(episodicMultiDur)
		}
		if semanticDur > 0 {
			timings["semantic"] = ms(semanticDur)
		}
	}

	// Merge sietch's two sub-tiers back into a single working-tier
	// slice. Append order is irrelevant; dedup-by-key collapses to the
	// highest score per key regardless of which sub-tier surfaced it.
	sietchHits := append(sietchVectorHits, sietchFTSHits...)
	var rrfStart time.Time
	if in.IncludeTimings {
		rrfStart = time.Now()
	}

	// Per-tier dedup → one entry per dedup key (session_id when present,
	// id otherwise — the semantic tier returns mneme rows that have no
	// session_id; using their id makes them participate in RRF as
	// distinct "documents"). Higher-score event wins within a tier.
	//
	// Session-vector hits' id is the session_id, so dedup-by-key
	// naturally collapses them with per-event episodic hits from the
	// same session. The exemplar selection below picks the highest
	// raw-tier-score across tiers — what we want.
	sietchByKey := dedupByKey(sietchHits)
	episodicByKey := dedupByKey(episodicHits)
	keywordByKey := dedupByKey(keywordHits)
	sessionVectorByKey := dedupByKey(sessionVectorHits)
	semanticByKey := dedupByKey(semanticHits)
	// primitivesByKey: 6th tier (D3). Empty/absent when in.Primitives
	// is off OR the chapterhouse handler dropped the field on
	// associations failure — dedupByKey on nil/empty returns an empty
	// map and rankedKeys yields []. FuseRRF's spec is "missing tiers
	// contribute nothing", so adding it unconditionally is safe — the
	// flag's effect is purely "did chapterhouse populate this slice".
	primitivesByKey := dedupByKey(primitivesHits)

	// Per-tier ranked key lists (best-first). These feed FuseRRF as
	// six independent ranked lists; missing tiers (empty input)
	// contribute nothing per FuseRRF's spec.
	//
	// Primitives joins as a 6th equal-weight tier rather than a
	// side-channel. Equal-weight is the safe first cut: a hit that
	// ranks #1 in primitives AND #1 in episodic gets the same RRF
	// agreement boost any other cross-tier match gets, so the eval
	// harness (D5) can measure the lift directly against the existing
	// 5-tier baseline. If the experiment shows a need for tilted
	// weights, the lever lives here (FuseRRF accepts equal-weight
	// only — a weighted variant would replace this call).
	tiers := [][]string{
		rankedKeys(sietchByKey),
		rankedKeys(episodicByKey),
		rankedKeys(keywordByKey),
		rankedKeys(sessionVectorByKey),
		rankedKeys(semanticByKey),
		rankedKeys(primitivesByKey),
	}
	fused := FuseRRF(tiers, c.RRFK)

	// Build the candidate pool. Cap depends on whether Stage 2 (rerank)
	// is configured: the cross-encoder needs a wider pool than the
	// user's top-N to have room to reorder, so we emit up to
	// RerankTopK candidates when Truthsayer is set, falling back to
	// in.Limit when it isn't (rerank disabled).
	poolSize := in.Limit
	if c.Truthsayer != nil && c.RerankTopK > poolSize {
		poolSize = c.RerankTopK
	}

	// SessionChunkText is a session-level attribute carried by whichever
	// hit happened to surface it (typically the episodic tier — see
	// chapterhouse's /v1/episodic/query JOIN). The exemplar event
	// chosen for output attribution may come from a tier that doesn't
	// carry it (sietch/working), so build a per-key lookup table from
	// every hit across every tier and copy onto the exemplar below.
	chunkTextByKey := collectSessionChunkText(sietchHits, episodicHits, keywordHits, sessionVectorHits, semanticHits, primitivesHits)

	// Re-emit one RecallHit per fused id, up to poolSize. Exemplar
	// selection: the highest raw-tier-score hit across the tiers that
	// contained this key. The output hit's Score is the RRF score; the
	// raw tier score doesn't compete on a comparable axis across tiers.
	hits := make([]RecallHit, 0, poolSize)
	rrfByID := make(map[string]float64, poolSize)
	tierMaps := []map[string]RecallHit{sietchByKey, episodicByKey, keywordByKey, sessionVectorByKey, semanticByKey, primitivesByKey}
	for _, sd := range fused {
		var best *RecallHit
		for _, tm := range tierMaps {
			if h, ok := tm[sd.ID]; ok {
				if best == nil || h.Score > best.Score {
					hCopy := h
					best = &hCopy
				}
			}
		}
		if best == nil {
			continue
		}
		best.Score = sd.Score
		// Surface the session-scoped chunk text on the exemplar so the
		// output hit carries it (downstream HTTP/MCP clients see it)
		// AND so rerankAndFuse can read it without a parallel param.
		if best.SessionChunkText == "" {
			best.SessionChunkText = chunkTextByKey[sd.ID]
		}
		hits = append(hits, *best)
		rrfByID[best.ID] = sd.Score
		if len(hits) >= poolSize {
			break
		}
	}

	if in.IncludeTimings {
		timings["rrf_dedup"] = ms(time.Since(rrfStart))
	}

	// P4 recurrent-settle expansion seam (Task 5). AFTER the fused pool
	// is built, BEFORE rerank: append the settle expansion candidates so
	// the cross-encoder can pull a graph-reachable-but-not-query-near hit
	// into the final top-K. Each expansion hit enters with rrfByID = 0
	// (zero RRF mass — config A's whole premise: it can only surface via
	// rerank score). activationByID retains the settle activation so
	// Task 6 (config B / "channel") can blend it as a third fusion
	// channel; config A ignores it. The map is threaded into
	// rerankAndFuse — the cleanest seam, since that is where FuseScores
	// (the function Task 6 extends) is called.
	//
	// activationByID is always allocated (nil-safe, cheap) so the
	// rerankAndFuse signature is uniform whether or not settle ran.
	activationByID := make(map[string]float64)
	if in.Settle != "" && len(expansionHits) > 0 {
		// Dedup against the existing pool by hitKey(). Expansion hits are
		// event-grain (chapterhouse hydrates event ids), so build the
		// pool key set using hitKey() for tier-aware namespacing. However,
		// activationByID is keyed by RAW hit ID (not hitKey) because
		// FuseScores, rrfByID, and rerankByID all use raw IDs — a hitKey
		// lookup in FuseScores would silently miss every entry (Step 0 fix).
		poolKeys := make(map[string]struct{}, len(hits))
		for _, h := range hits {
			poolKeys[hitKey(h)] = struct{}{}
		}
		var droppedNoText, deduped, appended int
		for _, e := range expansionHits {
			eh := RecallHit{
				// "expansion" is the provenance label: RecallHit has no
				// separate source field, Tier carries provenance across
				// the whole pipeline (dedup, TierCounts, output). A new
				// tier string keeps expansion hits distinguishable from
				// the six RRF tiers in TierCounts and downstream telemetry.
				Tier:    "expansion",
				ID:      e.ID,
				Content: e.Text,
			}
			poolKey := hitKey(eh) // tier-namespaced, for pool dedup only
			// Drop text-less expansion entries. Per the Task 4 finding,
			// chapterhouse hydrates expansion text best-effort and an
			// entry whose event text is NULL / ACL-denied surfaces with
			// empty text. Such a hit can neither be scored by the
			// cross-encoder (no text to embed) nor consumed by the caller
			// (no content to return), so it has no path into the final
			// top-K — carrying it would only inflate the pool. Record its
			// activation for Task 6 even so, then drop it from the pool.
			if e.Text == "" {
				activationByID[e.ID] = e.Activation // raw ID — matches FuseScores key space
				droppedNoText++
				continue
			}
			if _, exists := poolKeys[poolKey]; exists {
				// Already in the pool from an RRF tier — keep the existing
				// entry, just retain the activation for Task 6's channel.
				activationByID[e.ID] = e.Activation // raw ID
				deduped++
				continue
			}
			// Append WITHOUT truncating the pool (experiment-first: during
			// P4 measurement the rerank pool is deliberately allowed to
			// exceed RerankTopK so no text-bearing expansion candidate is
			// dropped before the cross-encoder sees it). If measurement
			// shows the wide pool hurts latency or quality, capping here
			// is the lever.
			hits = append(hits, eh)
			rrfByID[eh.ID] = 0
			activationByID[eh.ID] = e.Activation // raw ID — matches rrfByID key space
			poolKeys[poolKey] = struct{}{}
			appended++
		}
		slog.DebugContext(ctx, "settle expansion merged into rerank pool",
			"mode", in.Settle,
			"expansion_returned", len(expansionHits),
			"appended", appended,
			"deduped", deduped,
			"dropped_no_text", droppedNoText,
			"pool_size", len(hits))
	}

	// Stage 2/3: cross-encoder rerank + score fusion. Only runs when
	// Truthsayer is configured AND there's a non-empty query string
	// (the cross-encoder is a query-vs-document model — without a
	// query the call is meaningless). Failures degrade to RRF-only
	// silently (warn-logged): recall must never error because rerank
	// is down.
	if c.Truthsayer != nil && in.QueryText != "" && len(hits) > 0 {
		var rerankStart time.Time
		if in.IncludeTimings {
			rerankStart = time.Now()
		}
		hits = c.rerankAndFuse(ctx, in.QueryText, hits, rrfByID, activationByID, in.Settle, in.ActivationWeight)
		if in.IncludeTimings {
			timings["rerank"] = ms(time.Since(rerankStart))
		}
	}

	// Truncate to user-requested limit (rerank pool was wider).
	if len(hits) > in.Limit {
		hits = hits[:in.Limit]
	}

	counts := map[string]int{"working": 0, "episodic": 0, "keyword": 0, "session_vector": 0, "semantic": 0, "primitives": 0}
	// Only surface the "expansion" count key when settle is on, so a
	// settle-off recall returns a TierCounts map byte-identical to the
	// pre-P4 pipeline (the regression contract). When settle ran, the
	// key is present even at zero so callers can tell "settle ran, no
	// expansion survived" from "settle never ran".
	if in.Settle != "" {
		counts["expansion"] = 0
	}
	for _, h := range hits {
		counts[h.Tier]++
	}
	if in.IncludeTimings {
		timings["total"] = ms(time.Since(recallStart))
	}
	return RecallResult{Hits: hits, TierCounts: counts, Timings: timings, Degraded: degraded}, nil
}

// ms converts a duration to milliseconds with sub-ms precision (3 decimals).
func ms(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000.0
}

// rerankAndFuse runs Stage 2 (cross-encoder rerank) and Stage 3 (score
// fusion) on the post-RRF candidate pool. On any error the function
// returns the input hits unchanged with a warn log — recall never
// fails because rerank is unavailable.
//
// activationByID carries the P4 settle activation keyed by raw hit ID
// (NOT hitKey — FuseScores, rrfByID, and rerankByID all use raw IDs).
// settleMode and wActivation gate the third fusion channel:
//   - settleMode == "channel": activation is blended as a third score
//     channel with weight wActivation (validated > 0 and
//     c.RerankWeight + wActivation <= 1 by Recall before this call).
//   - settleMode == "expand" or "": activation is ignored; FuseScores
//     receives nil activation and wActivation=0, producing the same
//     two-channel result as the pre-Task-6 pipeline (regression guard).
func (c *Core) rerankAndFuse(ctx context.Context, query string, hits []RecallHit, rrfByID map[string]float64, activationByID map[string]float64, settleMode string, wActivation float64) []RecallHit {
	// Build candidates only for hits that have text the reranker can
	// score. Skip no-text hits (semantic mnemes today; possibly other
	// tiers in the future) — sending them as empty-text candidates
	// would have the cross-encoder return ~0, and FuseScores would
	// then systematically demote them despite their RRF signal.
	// FuseScores handles missing-rerank entries by trusting the RRF
	// prior, so omitting them here keeps the prior intact.
	candidates := make([]truthsayer.Candidate, 0, len(hits))
	for _, h := range hits {
		text := h.SessionChunkText
		if text == "" {
			text = h.Content
		}
		if text == "" {
			continue
		}
		candidates = append(candidates, truthsayer.Candidate{ID: h.ID, Text: text})
	}
	if len(candidates) == 0 {
		// No candidate had text — nothing to rerank. Score fusion
		// against an empty rerank map degenerates cleanly to RRF-only.
		return hits
	}

	rerankCtx, cancel := context.WithTimeout(ctx, c.RerankTimeout)
	defer cancel()
	scored, err := c.Truthsayer.Rerank(rerankCtx, query, candidates, 0)
	if err != nil {
		slog.WarnContext(ctx, "truthsayer rerank failed; falling back to RRF-only",
			"err", err.Error(), "candidates", len(candidates))
		return hits
	}

	rerankByID := make(map[string]float64, len(scored))
	for _, s := range scored {
		rerankByID[s.ID] = s.Score
	}
	// Channel mode ("channel"): pass activation as the third channel.
	// All other modes ("expand", ""): nil activation + wActivation=0
	// so FuseScores produces a two-channel result byte-identical to the
	// pre-Task-6 pipeline (regression invariant pinned in tests).
	var actMap map[string]float64
	var wAct float64
	if settleMode == "channel" {
		actMap = activationByID
		wAct = wActivation
	}
	fused := FuseScores(rrfByID, rerankByID, actMap, c.RerankWeight, wAct)

	for i := range hits {
		if v, ok := fused[hits[i].ID]; ok {
			hits[i].Score = v
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	return hits
}

// dedupByKey collapses hits by SessionID (when set) or ID (otherwise),
// keeping the highest-score hit per key. Used per-tier before RRF so a
// session that produced multiple matching events doesn't multi-count.
func dedupByKey(hits []RecallHit) map[string]RecallHit {
	byKey := make(map[string]RecallHit, len(hits))
	for _, h := range hits {
		key := hitKey(h)
		cur, ok := byKey[key]
		if !ok || h.Score > cur.Score {
			byKey[key] = h
		}
	}
	return byKey
}

// hitKey returns the rerank/RRF dedup key for a hit, namespaced by
// grain so that session-grain hits (session-vector tier, where the
// hit's id IS the session_id and represents the whole session as one
// document) live in a separate key space from event-grain hits
// (episodic / keyword / working — one row per matched event).
//
// Pre-fix bug: session-vector hits collided with event-grain hits from
// the same session and the exemplar-selection step picked the higher
// raw-score one (always the session-vector hit, raw cosine 1.0 on the
// pooled embedding), silently erasing the per-event identities of the
// event-grain matches. Empirical impact: top-K event recall drops
// because resolving commits/PRs that score lower than the surrounding
// session summary never make it into the output.
//
// Grain rules:
//   - session_vector tier: session-grain. Key = "session:" + session_id
//     (h.ID == session_id for this tier).
//   - working / episodic / keyword: event-grain. Key = "event:" + event_id.
//   - semantic: mneme-grain (no session_id, distinct documents). Key =
//     "event:" + h.ID is fine — semantic hits don't collide with
//     either of the other grains because mneme ids are distinct from
//     event ids.
func hitKey(h RecallHit) string {
	if h.Tier == "session_vector" {
		return "session:" + h.ID
	}
	return "event:" + h.ID
}

// collectSessionChunkText builds a per-key lookup of the role-prefixed
// full session text. Chapterhouse JOINs episodic.sessions.l1_chunk_text
// into the /v1/episodic/query response, so episodic hits carry it; the
// sietch and semantic tiers don't. First non-empty value per key wins
// (all events from the same session row carry the same value, so the
// "first" choice is just deterministic-by-iteration).
func collectSessionChunkText(tiers ...[]RecallHit) map[string]string {
	out := make(map[string]string, 32)
	for _, tier := range tiers {
		for _, h := range tier {
			if h.SessionChunkText == "" {
				continue
			}
			key := hitKey(h)
			if _, ok := out[key]; ok {
				continue
			}
			out[key] = h.SessionChunkText
		}
	}
	return out
}

// rankedKeys returns the dedup keys sorted by raw tier score descending.
// Stable on score-tie so the result is deterministic.
func rankedKeys(byKey map[string]RecallHit) []string {
	type entry struct {
		key   string
		score float64
	}
	all := make([]entry, 0, len(byKey))
	for k, h := range byKey {
		all = append(all, entry{k, h.Score})
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].score != all[j].score {
			return all[i].score > all[j].score
		}
		return all[i].key < all[j].key
	})
	out := make([]string, len(all))
	for i, e := range all {
		out[i] = e.key
	}
	return out
}

// Forget soft-deletes events in sietch + asks chapterhouse to do the
// same in episodic. Semantic is never forgotten by event id — distilled
// mnemes flow out through replay decay, not direct deletion.
func (c *Core) Forget(ctx context.Context, in ForgetInput) (int, error) {
	if in.UserID == "" {
		return 0, ErrMissingUserID
	}
	if len(in.EventIDs) == 0 {
		return 0, errors.New("event_ids required")
	}
	if in.SessionID != "" {
		if err := c.Sietch.SoftForget(ctx, in.SessionID, in.EventIDs); err != nil {
			return 0, fmt.Errorf("sietch forget: %w", err)
		}
	}
	return c.Chapterhouse.ForgetEpisodic(ctx, in.UserID, in.EventIDs)
}

// Share grants cross-user visibility to a scope.
func (c *Core) Share(ctx context.Context, in ShareInput) (string, error) {
	if in.UserID == "" {
		return "", ErrMissingUserID
	}
	if in.ScopeID == "" {
		return "", errors.New("scope_id required")
	}
	return c.Chapterhouse.ShareEpisodic(ctx, in)
}

// ExpandSessionWorkspace tags an existing session into an additional
// workspace. Used when conversation drift makes the session relevant
// to a workspace beyond its primary at session_start. Pre-condition:
// the session has already been consolidated into chapterhouse at
// least once; otherwise the FK on session_workspaces.session_id
// rejects the row and chapterhouse returns 409 (surfaced as
// *chapterhouse.StatusError for the HTTP layer to re-emit).
func (c *Core) ExpandSessionWorkspace(ctx context.Context, in AddSessionWorkspaceInput) (bool, error) {
	if in.UserID == "" {
		return false, ErrMissingUserID
	}
	if in.SessionID == "" {
		return false, ErrMissingSessionID
	}
	if in.WorkspaceID == "" {
		return false, errors.New("workspace_id required")
	}
	return c.Chapterhouse.AddSessionWorkspace(ctx, in)
}

// Consolidate flushes the pending events (after the watermark) to
// chapterhouse's episodic store. Phase 5's Pipeline A worker calls
// this on a tick; Core exposes it so agents can force a flush on
// demand (e.g. before they exit).
func (c *Core) Consolidate(ctx context.Context, sessionID string) (int, error) {
	if sessionID == "" {
		return 0, ErrMissingSessionID
	}
	watermark, err := c.Sietch.Watermark(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("watermark: %w", err)
	}
	pending, err := c.Sietch.PendingEvents(ctx, sessionID, watermark)
	if err != nil {
		return 0, fmt.Errorf("pending: %w", err)
	}
	if len(pending) == 0 {
		return 0, nil
	}

	// Ship only the contiguous prefix of embedded events. Events past
	// the first un-embedded one are held back — the watermark must stay
	// a contiguous prefix of the id-ordered log, and the backfill pass
	// (BackfillEmbeddings) will fill the gap before the next tick.
	cut := len(pending)
	for i := range pending {
		if needsEmbedding(pending[i]) {
			cut = i
			break
		}
	}
	if held := len(pending) - cut; held > 0 {
		slog.InfoContext(ctx, "consolidate: holding back events awaiting embedding",
			"session_id", sessionID, "held", held)
	}
	if cut == 0 {
		return 0, nil
	}
	pending = pending[:cut]

	// Source the session row from sietch so ended_at, cwd, agent_kind,
	// etc. all flow through to chapterhouse on every consolidation
	// pass. Chapterhouse UPSERTs on id, so the same session metadata
	// can land repeatedly — but ended_at MUST propagate or the
	// predictive-replay reconciler (eligibility predicate
	// `ended_at IS NOT NULL AND l1_embedding IS NULL`) will skip the
	// session forever.
	//
	// Pipeline A's encoding worker tick reaches here with the session
	// still active: GetSession returns ended_at = nil, the upsert
	// keeps ended_at NULL, and the next tick (or eventual SessionEnd)
	// will refresh it. SessionEnd reaches here with ended_at already
	// stamped via Sietch.MarkEnded, so the row carries the timestamp.
	sess, err := c.Sietch.GetSession(ctx, sessionID)
	if err != nil {
		return 0, fmt.Errorf("get session: %w", err)
	}
	// Defensive defaults: if sietch ever hands back a partially-zero
	// row (e.g. a session with no events), still emit something the
	// chapterhouse session-row schema accepts. UserID + StartedAt are
	// the only NOT NULL fields; pull them from the first pending event
	// when sietch left them zero.
	if sess.ID == "" {
		sess.ID = sessionID
	}
	if sess.UserID == "" {
		sess.UserID = pending[0].UserID
	}
	if sess.StartedAt.IsZero() {
		sess.StartedAt = pending[0].CreatedAt
	}
	sess.EventCount = len(pending)
	if _, _, err := c.Chapterhouse.IngestEpisodic(ctx, sess, pending); err != nil {
		return 0, fmt.Errorf("ingest: %w", err)
	}
	last := pending[len(pending)-1].ID
	if err := c.Sietch.SetWatermark(ctx, sessionID, last); err != nil {
		return 0, fmt.Errorf("advance watermark: %w", err)
	}
	return len(pending), nil
}

// ConsolidateWorkspace triggers chapterhouse's episodic->semantic
// consolidation batch (cluster closed sessions, enrich with excerpts,
// optionally label/digest) for one workspace, synchronously. Thin
// passthrough to Chapterhouse.ConsolidateWorkspace — the manual-trigger
// counterpart to chapterhouse's own nightly worker schedule, sharing
// its RunWorkspace code path. Distinct from Consolidate above, which
// flushes one session's pending sietch events to episodic (Pipeline A)
// — a different pipeline entirely, see the consolidation-phase plan's
// seam decision 1.
//
// Typical caller: an agent about to have its context cleared or
// compacted, front-loading the semantic tier so recall has fresh
// content afterward.
//
// Workspace resolution mirrors Recall: an explicit Workspace wins;
// otherwise Cwd (when non-empty) is derived via WorkspaceForCwd so an
// agent that only knows its working directory can still trigger
// consolidation. Neither set is a validation error. A workspace that
// fails to parse as a UUID is also rejected here rather than round-
// tripping to chapterhouse only to bounce off its own validation.
func (c *Core) ConsolidateWorkspace(ctx context.Context, in ConsolidateWorkspaceInput) error {
	if in.Workspace == "" && in.Cwd != nil && *in.Cwd != "" {
		in.Workspace = WorkspaceForCwd(*in.Cwd).String()
	}
	if in.Workspace == "" {
		return ErrMissingWorkspaceOrCwd
	}
	if _, err := uuid.Parse(in.Workspace); err != nil {
		return fmt.Errorf("%w: workspace must be a valid UUID: %w", ErrValidation, err)
	}
	return c.Chapterhouse.ConsolidateWorkspace(ctx, in.Workspace)
}

// GCSession deletes the session's sietch file once it is a redundant
// local cache: ended longer ago than SietchRetention, fully
// consolidated (nothing past the watermark), and nothing awaiting
// embedding backfill. The backfill check is belt-and-suspenders — an
// un-embedded event is also unconsolidated — guarding any future
// watermark change. Returns true when the file was removed.
func (c *Core) GCSession(ctx context.Context, sessionID string) (bool, error) {
	if sessionID == "" {
		return false, ErrMissingSessionID
	}
	if c.SietchRetention <= 0 {
		return false, nil
	}
	sess, err := c.Sietch.GetSession(ctx, sessionID)
	if errors.Is(err, ErrSessionNotFound) {
		// Schema-only orphan (file recreated by create-on-open after
		// GC): nothing to consolidate, nothing to keep — remove it.
		if rmErr := c.Sietch.RemoveSession(ctx, sessionID); rmErr != nil {
			return false, fmt.Errorf("gc remove orphan (session=%q): %w", sessionID, rmErr)
		}
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("gc get session (session=%q): %w", sessionID, err)
	}
	if sess.EndedAt == nil || c.Now().Sub(*sess.EndedAt) < c.SietchRetention {
		return false, nil
	}
	watermark, err := c.Sietch.Watermark(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("gc watermark: %w", err)
	}
	pending, err := c.Sietch.PendingEvents(ctx, sessionID, watermark)
	if err != nil {
		return false, fmt.Errorf("gc pending: %w", err)
	}
	if len(pending) > 0 {
		return false, nil
	}
	need, err := c.Sietch.EventsNeedingEmbedding(ctx, sessionID, 1) // existence probe; limit 1
	if err != nil {
		return false, fmt.Errorf("gc needs-embedding: %w", err)
	}
	if len(need) > 0 {
		return false, nil
	}
	if err := c.Sietch.RemoveSession(ctx, sessionID); err != nil {
		return false, fmt.Errorf("gc remove (session=%q): %w", sessionID, err)
	}
	return true, nil
}

// needsEmbedding reports whether an event should carry an embedding
// but doesn't yet (recorded while the embedder was unreachable).
func needsEmbedding(ev Event) bool {
	return len(ev.Embedding) == 0 && ev.Text != nil && *ev.Text != ""
}
