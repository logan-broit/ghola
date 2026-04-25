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
}

// ChapterhouseClient wraps the /v1 REST surface. Implementation in
// internal/chapterhouse; tests use fakes.
type ChapterhouseClient interface {
	IngestEpisodic(ctx context.Context, s Session, events []Event) (inserted, updated int, err error)
	QueryEpisodic(ctx context.Context, q EpisodicQuery) ([]RecallHit, error)
	ShareEpisodic(ctx context.Context, s ShareInput) (shareID string, err error)
	ForgetEpisodic(ctx context.Context, userID string, eventIDs []string) (forgotten int, err error)

	QuerySemantic(ctx context.Context, q SemanticQuery) ([]RecallHit, error)
}

// EpisodicQuery is the request shape for /v1/episodic/query. Mirrors
// the OpenAPI DTO; Core builds it from RecallInput + embedding.
type EpisodicQuery struct {
	UserID         string
	QueryText      string
	QueryEmbedding []float32
	Limit          int
	IncludeShared  bool
}

// SemanticQuery is the request shape for /v1/semantic/query.
type SemanticQuery struct {
	Workspace      string
	QueryText      string
	QueryEmbedding []float32
	Limit          int
}

// Embedder is whatever produces a vector for a piece of text. In
// production this is the Melange HTTP client (Phase 4.5); tests use
// a deterministic fake.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}
