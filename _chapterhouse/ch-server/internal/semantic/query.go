package semantic

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/thinkwright/chapterhouse/ch-server/internal/mentat"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
)

// RecallRequest is the input to Querier.Recall. RecentContext is the
// caller-supplied tail of the workspace's event stream (typed +
// embedded); the query event itself is appended internally so the
// pool sees `recent_context || query`.
type RecallRequest struct {
	WorkspaceID    uuid.UUID
	QueryEmbedding []float32
	RecentContext  []mentat.Event
	Limit          int
}

// Querier owns the v0.3 read path: pool (recent context + query) into
// an L1 probe vector, then HNSW-cosine against semantic.mnemes.
//
// Held by `*SemanticHandler` so the HTTP layer is a thin shell over
// this concrete type — the interfaces would buy us nothing the e2e
// test in PR1.9 doesn't already cover.
type Querier struct {
	repo   *repository.Repository
	m      *mentat.Client
	logger *slog.Logger
}

// NewQuerier constructs a Querier. A nil mentat client is permitted:
// when the deployment runs without mentat (MENTAT_URL unset), Recall
// returns zero hits without ever calling the network. This keeps the
// design invariant intact — semantic-tier failure must never break
// user-visible recall, since the other tiers still answer.
func NewQuerier(repo *repository.Repository, m *mentat.Client, logger *slog.Logger) *Querier {
	if logger == nil {
		logger = slog.Default()
	}
	return &Querier{repo: repo, m: m, logger: logger}
}

// Recall pools (recent context + query) into an L1 probe vector, then
// runs HNSW cosine against semantic.mnemes. If mentat is unreachable
// or unconfigured, returns zero hits and logs — the design invariant
// is that a semantic-tier failure never breaks user-visible recall
// (other tiers still answer).
func (q *Querier) Recall(ctx context.Context, req RecallRequest) ([]repository.MnemeHit, error) {
	if q.m == nil {
		q.logger.Debug("semantic: mentat unconfigured; returning 0 hits",
			"workspace_id", req.WorkspaceID)
		return nil, nil
	}
	if len(req.QueryEmbedding) == 0 {
		return nil, nil
	}

	events := make([]mentat.Event, 0, len(req.RecentContext)+1)
	events = append(events, req.RecentContext...)
	events = append(events, mentat.Event{Type: "user", Embedding: req.QueryEmbedding})

	pooled, err := q.m.Pool(ctx, mentat.PoolRequest{
		WorkspaceID: req.WorkspaceID,
		Events:      events,
	})
	if err != nil {
		q.logger.Warn("semantic: mentat pool failed; returning 0 hits",
			"workspace_id", req.WorkspaceID, "err", err.Error())
		return nil, nil
	}
	return q.repo.QueryMnemesByEmbedding(ctx, req.WorkspaceID, pooled.Embedding, req.Limit)
}
