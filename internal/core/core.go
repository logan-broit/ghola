package core

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

// Core is the single behavioral surface the HTTP + MCP wrappers share.
type Core struct {
	Sietch       SietchStore
	Chapterhouse ChapterhouseClient
	Embedder     Embedder
	// Now is the time source; tests inject a fixed clock.
	Now func() time.Time
}

// New builds a Core with sensible defaults.
func New(s SietchStore, ch ChapterhouseClient, emb Embedder) *Core {
	return &Core{
		Sietch:       s,
		Chapterhouse: ch,
		Embedder:     emb,
		Now:          func() time.Time { return time.Now().UTC() },
	}
}

var errMissingSessionID = errors.New("session_id required")
var errMissingUserID = errors.New("user_id required")

// SessionStart provisions a new sietch file + inserts the session row.
func (c *Core) SessionStart(ctx context.Context, in SessionStartInput) (Session, error) {
	if in.UserID == "" {
		return Session{}, errMissingUserID
	}
	s := Session{
		ID:           NewID(),
		UserID:       in.UserID,
		StartedAt:    c.Now(),
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
		return errMissingSessionID
	}
	if err := c.Sietch.MarkEnded(ctx, sessionID, c.Now()); err != nil {
		return fmt.Errorf("mark ended: %w", err)
	}
	if _, err := c.Consolidate(ctx, sessionID); err != nil {
		return fmt.Errorf("consolidate before end: %w", err)
	}
	return c.Sietch.CloseSession(ctx, sessionID)
}

// ListSessions enumerates a user's sessions (sietch-visible slice
// only for now — episodic listing lands with Pipeline B in Phase 8).
func (c *Core) ListSessions(ctx context.Context, userID string) ([]Session, error) {
	if userID == "" {
		return nil, errMissingUserID
	}
	return c.Sietch.ListSessions(ctx, userID)
}

// Record appends an event to the current session. The embedding is
// computed when absent.
func (c *Core) Record(ctx context.Context, in RecordInput) (Event, error) {
	if in.SessionID == "" {
		return Event{}, errMissingSessionID
	}
	if in.UserID == "" {
		return Event{}, errMissingUserID
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
			return Event{}, fmt.Errorf("embed: %w", err)
		}
		ev.Embedding = emb
	}

	stored, err := c.Sietch.RecordEvent(ctx, ev)
	if err != nil {
		return Event{}, fmt.Errorf("record: %w", err)
	}
	if err := c.Sietch.SetCurrent(ctx, stored.SessionID, stored.ID); err != nil {
		return stored, fmt.Errorf("set current: %w", err)
	}
	return stored, nil
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
		return errMissingSessionID
	}
	if eventID == "" || label == "" {
		return errors.New("event_id and label required")
	}
	return c.Sietch.SetBookmark(ctx, sessionID, eventID, label)
}

// Navigate moves the current-event pointer.
func (c *Core) Navigate(ctx context.Context, sessionID, eventID string) error {
	if sessionID == "" {
		return errMissingSessionID
	}
	if eventID == "" {
		return errors.New("event_id required")
	}
	return c.Sietch.SetCurrent(ctx, sessionID, eventID)
}

// Recall fans out across tiers and returns a merged, score-sorted list.
func (c *Core) Recall(ctx context.Context, in RecallInput) (RecallResult, error) {
	if in.UserID == "" {
		return RecallResult{}, errMissingUserID
	}
	if in.Limit <= 0 {
		in.Limit = 10
	}

	// Default to all three tiers when the caller didn't say.
	if !in.IncludeSietch && !in.IncludeEpisode && !in.IncludeSemant {
		in.IncludeSietch = true
		in.IncludeEpisode = true
		in.IncludeSemant = true
	}

	var emb []float32
	if in.QueryText != "" {
		e, err := c.Embedder.Embed(ctx, in.QueryText)
		if err != nil {
			return RecallResult{}, fmt.Errorf("embed query: %w", err)
		}
		emb = e
	}

	var merged []RecallHit

	if in.IncludeSietch && in.SessionID != "" {
		if len(emb) > 0 {
			h, err := c.Sietch.SearchVector(ctx, in.SessionID, emb, in.Limit)
			if err != nil {
				return RecallResult{}, fmt.Errorf("sietch vector: %w", err)
			}
			merged = append(merged, h...)
		}
		if in.QueryText != "" {
			h, err := c.Sietch.SearchFTS(ctx, in.SessionID, in.QueryText, in.Limit)
			if err != nil {
				return RecallResult{}, fmt.Errorf("sietch fts: %w", err)
			}
			merged = append(merged, h...)
		}
	}

	if in.IncludeEpisode {
		h, err := c.Chapterhouse.QueryEpisodic(ctx, EpisodicQuery{
			UserID:         in.UserID,
			QueryText:      in.QueryText,
			QueryEmbedding: emb,
			Limit:          in.Limit,
			IncludeShared:  in.IncludeShared,
		})
		if err != nil {
			return RecallResult{}, fmt.Errorf("episodic: %w", err)
		}
		merged = append(merged, h...)
	}

	if in.IncludeSemant && in.Workspace != "" {
		h, err := c.Chapterhouse.QuerySemantic(ctx, SemanticQuery{
			Workspace:      in.Workspace,
			QueryText:      in.QueryText,
			QueryEmbedding: emb,
			Limit:          in.Limit,
		})
		if err != nil {
			return RecallResult{}, fmt.Errorf("semantic: %w", err)
		}
		merged = append(merged, h...)
	}

	// Stable sort by score descending.
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].Score > merged[j].Score })
	if len(merged) > in.Limit {
		merged = merged[:in.Limit]
	}

	counts := map[string]int{"working": 0, "episodic": 0, "semantic": 0}
	for _, h := range merged {
		counts[h.Tier]++
	}
	return RecallResult{Hits: merged, TierCounts: counts}, nil
}

// Forget soft-deletes events in sietch + asks chapterhouse to do the
// same in episodic. Semantic is never forgotten by event id — if an
// agent wants to forget a distilled mneme it uses feedback with low
// evidence instead.
func (c *Core) Forget(ctx context.Context, in ForgetInput) (int, error) {
	if in.UserID == "" {
		return 0, errMissingUserID
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
		return "", errMissingUserID
	}
	if in.ScopeID == "" {
		return "", errors.New("scope_id required")
	}
	return c.Chapterhouse.ShareEpisodic(ctx, in)
}

// Consolidate flushes the pending events (after the watermark) to
// chapterhouse's episodic store. Phase 5's Pipeline A worker calls
// this on a tick; Core exposes it so agents can force a flush on
// demand (e.g. before they exit).
func (c *Core) Consolidate(ctx context.Context, sessionID string) (int, error) {
	if sessionID == "" {
		return 0, errMissingSessionID
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

// Feedback applies Bayesian evidence to a semantic mneme.
func (c *Core) Feedback(ctx context.Context, mnemeID string, evidence float64) (float64, error) {
	if mnemeID == "" {
		return 0, errors.New("mneme_id required")
	}
	if evidence < 0 || evidence > 1 {
		return 0, errors.New("evidence must be in [0,1]")
	}
	return c.Chapterhouse.FeedbackSemantic(ctx, mnemeID, evidence)
}
