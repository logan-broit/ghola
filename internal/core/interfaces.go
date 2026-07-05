package core

import (
	"context"
	"time"
)

// SietchStore is the per-session, on-device working-memory layer.
// Implementations keep one SQLite file per session (see
// internal/sietch) and must be safe for concurrent access from the
// HTTP server's request goroutines.
type SietchStore interface {
	OpenSession(ctx context.Context, s Session) error
	CloseSession(ctx context.Context, sessionID string) error
	// MarkEnded stamps ended_at on the session row without closing the
	// SQLite handle. SessionEnd calls this before Consolidate so the
	// chapterhouse upsert lands with ended_at populated — the
	// reconciler eligibility predicate is `ended_at IS NOT NULL AND
	// l1_embedding IS NULL`, and Pipeline A's incremental tick must
	// not flip it.
	MarkEnded(ctx context.Context, sessionID string, t time.Time) error
	// GetSession returns the session row's metadata. Consolidate uses
	// this to source ended_at + cwd + git_branch + agent_kind for the
	// chapterhouse session payload.
	GetSession(ctx context.Context, sessionID string) (Session, error)

	RecordEvent(ctx context.Context, ev Event) (Event, error)
	SetBookmark(ctx context.Context, sessionID, eventID, label string) error

	SetCurrent(ctx context.Context, sessionID, eventID string) error
	CurrentEvent(ctx context.Context, sessionID string) (string, error)

	// SearchVector returns events whose embedding is nearest to `emb`
	// under cosine similarity (sqlite-vec). Tier attribution = "working".
	SearchVector(ctx context.Context, sessionID string, emb []float32, limit int) ([]RecallHit, error)
	// SearchFTS returns events whose text matches the FTS5 query.
	SearchFTS(ctx context.Context, sessionID, text string, limit int) ([]RecallHit, error)

	ListSessions(ctx context.Context, userID string) ([]Session, error)
	SoftForget(ctx context.Context, sessionID string, eventIDs []string) error

	// Watermark returns the id of the last event successfully
	// consolidated into episodic. Pipeline A (Phase 5) reads/writes
	// this; Core only reads on Consolidate + SessionEnd triggers.
	Watermark(ctx context.Context, sessionID string) (string, error)
	SetWatermark(ctx context.Context, sessionID, eventID string) error

	// PendingEvents returns events created after `afterEventID` so
	// Consolidate/SessionEnd can ship them to chapterhouse.
	PendingEvents(ctx context.Context, sessionID, afterEventID string) ([]Event, error)

	// EventsNeedingEmbedding returns active events with text but no
	// embedding — those recorded while the embedder was unreachable
	// (Record degrades rather than losing the write). BackfillEmbeddings
	// drains this set; only ID/SessionID/UserID/Text are populated.
	EventsNeedingEmbedding(ctx context.Context, sessionID string, limit int) ([]Event, error)
	// SetEmbedding backfills one event's embedding. State-guarded so a
	// concurrent forget is not resurrected by a late backfill.
	SetEmbedding(ctx context.Context, sessionID, eventID string, emb []float32) error

	// RemoveSession deletes the session's working-tier file. GCSession
	// calls this only after the session is ended and fully consolidated,
	// at which point the file is a redundant local cache of what
	// chapterhouse already holds. Must be idempotent — removing an
	// absent session is a no-op.
	RemoveSession(ctx context.Context, sessionID string) error
}

// ChapterhouseClient wraps the /v1 REST surface. Implementation in
// internal/chapterhouse; tests use fakes.
type ChapterhouseClient interface {
	IngestEpisodic(ctx context.Context, s Session, events []Event) (inserted, updated int, err error)
	// QueryEpisodicMulti is the single entry point for event-grain
	// fan-out: one HTTP round-trip against /v1/episodic/query that
	// returns parallel ranked sub-lists already mapped onto
	// core.RecallHit with the downstream tier strings core.Recall
	// expects ("episodic" for vector, "keyword" for fts,
	// "session_vector" for session_vector). The caller names the
	// subset of {"vector","fts","session_vector"} to fan out across
	// via q.Rankings.
	QueryEpisodicMulti(ctx context.Context, q EpisodicMultiQuery) (EpisodicMultiResult, error)
	ShareEpisodic(ctx context.Context, s ShareInput) (shareID string, err error)
	ForgetEpisodic(ctx context.Context, userID string, eventIDs []string) (forgotten int, err error)
	// AddSessionWorkspace tags an existing chapterhouse session into an
	// additional workspace. Pre-consolidate the session row hasn't
	// landed yet, so chapterhouse returns 409; the wire error surfaces
	// as *chapterhouse.StatusError for the ghola HTTP layer to re-emit.
	AddSessionWorkspace(ctx context.Context, in AddSessionWorkspaceInput) (added bool, err error)

	QuerySemantic(ctx context.Context, q SemanticQuery) ([]RecallHit, error)
}

// EpisodicMultiQuery is the request shape for
// QueryEpisodicMulti — one round-trip that fans the requested subset
// of event-grain rankings (vector / fts / session_vector) inside
// chapterhouse. Shared identifiers (UserID, WorkspaceID, Limit,
// IncludeShared, TagsAny) apply to every requested tier; the server
// only forwards what each tier's repo function actually consumes
// (e.g. session_vector ignores TagsAny — the server-side handler runs
// that decision, not core).
//
// WorkspaceID scopes the candidate pool through chapterhouse's
// session_workspaces join — workspace is required end-to-end. No
// fallback to "search everything for this user" (the corpus-shape
// experiment that established 19k-candidate full-workspace recall
// caps around 57% R@5 regardless of model/tier choices).
//
// Rankings is the multi-vs-no-op discriminator. A non-empty subset of
// {"vector","fts","session_vector"} names the tiers the caller wants
// ranked separately. core.Recall sends the natural fan-out
// {"vector","fts","session_vector"} when it has both a query string
// and an embedding, and prunes the list when an input gate is missing
// (no embedding → drop "vector"+"session_vector"; no query_text →
// drop "fts").
type EpisodicMultiQuery struct {
	UserID         string
	WorkspaceID    string
	QueryText      string
	QueryEmbedding []float32
	Limit          int
	IncludeShared  bool
	// TagsAny — overlap-style tag filter on `tags && $`. Forwarded by
	// the chapterhouse multi-ranking handler to every event-grain tier
	// (vector, fts); session_vector ignores it (different grain).
	// Empty/nil → no filter applied.
	TagsAny []string
	// Rankings names the per-tier subset to fan out across. The
	// chapterhouse server treats the field's *presence* (omitempty
	// when empty) as the multi-ranking discriminator: a non-nil,
	// non-empty list selects the multi-ranking path, while an
	// empty/absent list is rejected with 400. The wire type
	// (chapterhouse.QueryEpisodicMultiRequest) carries the same
	// `omitempty` JSON tag so a zero-value request encodes cleanly.
	Rankings []string
	// Primitives, when true, asks the chapterhouse handler to compute
	// a fourth Hebbian-boosted sub-list (D1) over the union of the
	// requested per-tier candidates. The flag is opt-in so callers
	// that don't care pay no extra cost; the chapterhouse handler
	// handles its own degraded-path semantics (associations lookup
	// failure → primitives field dropped from response). The wire
	// type (chapterhouse.QueryEpisodicMultiRequest) carries
	// `omitempty` so a zero-value request never serializes the flag.
	Primitives bool
	// Settle, when true, asks the chapterhouse handler to run the P4
	// recurrent-settle spreading activation over the Hebbian graph
	// seeded by the vector+fts hits and return an `expansion` sub-list.
	// SettleParams carries the tuning overrides; zero fields fall back
	// to chapterhouse's DefaultSettleParams(). The wire request
	// (chapterhouse.QueryEpisodicMultiRequest.Settle) is a pointer block
	// that is only marshaled when this flag is set.
	Settle       bool
	SettleParams SettleParams
}

// EpisodicMultiResult carries the parallel ranked sub-lists returned
// by QueryEpisodicMulti, already projected onto core.RecallHit with
// the downstream tier strings core.Recall expects ("episodic" for
// vector, "keyword" for fts, "session_vector" for session_vector).
// Sub-lists for tiers the caller did not request decode as nil; a
// requested-but-empty tier decodes as a non-nil empty slice so RRF
// fan-in can iterate without nil-checking.
//
// Primitives uses pointer-to-slice for 3-state semantics that mirror
// the wire (chapterhouse.QueryEpisodicMultiResponse.Primitives):
//   - nil → flag was off, OR flag was on but the chapterhouse-side
//     association lookup failed and the handler dropped the field.
//   - non-nil pointer to empty slice → flag was on, no in-set boosts
//     surfaced (the seeded set has no associations among themselves).
//   - non-nil pointer to populated slice → primitives ranking is live.
//
// The tier string on each hit passes through unchanged ("primitives"),
// distinct from the legacy vector→episodic / fts→keyword remap; D2
// keeps the two surfaces faithful end-to-end so downstream consumers
// can tell whether a hit came from primitives.
type EpisodicMultiResult struct {
	Vector        []RecallHit
	FTS           []RecallHit
	SessionVector []RecallHit
	Primitives    *[]RecallHit
	// Expansion is the P4 recurrent-settle sub-list (Task 4 contract):
	// non-seed nodes surfaced by spreading activation, activation-
	// descending. nil when the caller didn't set Settle OR the
	// chapterhouse handler dropped the field on a best-effort settle
	// failure — either way, no expansion is applied (no error). A
	// non-nil (possibly empty) slice means settle ran. Entries MAY have
	// an empty Text: the Task 4 handler hydrates text best-effort, and a
	// row whose text column is NULL / ACL-denied surfaces with Text == ""
	// (documented Task 4 finding). Recall drops those (see below).
	Expansion []ExpansionHit
}

// ExpansionHit is one entry in the settle expansion sub-list, projected
// from chapterhouse's wire ExpansionHit. Text is empty when the
// chapterhouse handler could not hydrate the event's text (NULL column
// or ACL denial) — a documented Task 4 finding. Such text-less entries
// are dropped by Recall because they can neither be scored by the
// cross-encoder nor consumed by the caller.
type ExpansionHit struct {
	ID         string
	Activation float64
	Text       string
}

// SemanticQuery is the request shape for /v1/semantic/query.
type SemanticQuery struct {
	Workspace      string
	QueryText      string
	QueryEmbedding []float32
	Limit          int
}

// Embedder is whatever produces a vector for a piece of text. In
// production this is the Guild HTTP client (Phase 4.5); tests use
// a deterministic fake.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}
