// Package semantic is the v0.3 replacement for internal/replay. It owns
// the session -> L1 write path (PR1.7), the recall read path (PR1.8),
// and (later PRs) clustering + training orchestration.
//
// The package deliberately stays small: Writer takes a concrete
// *repository.Repository and *mentat.Client directly rather than
// behind interfaces, because the e2e test in PR1.9 exercises the real
// path end-to-end and we don't want a "Pooler interface" to grow just
// to support unit-level mocking.
package semantic

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/thinkwright/chapterhouse/ch-server/internal/mentat"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
)

// Writer pools a session's events into a single L1 embedding via
// mentat and writes the result to episodic.sessions.l1_embedding.
type Writer struct {
	repo *repository.Repository
	m    *mentat.Client
}

// NewWriter constructs a Writer. Both arguments are required; nil
// values are caller errors and will surface as nil-deref at first use.
func NewWriter(repo *repository.Repository, m *mentat.Client) *Writer {
	return &Writer{repo: repo, m: m}
}

// PoolSessionToL1 fetches a session's embedded events, pools them via
// mentat, and writes both the pooled vector and a role-prefixed
// concatenation of the events' text to episodic.sessions
// (l1_embedding + l1_chunk_text).
//
// Idempotent by construction: re-running overwrites with the current
// pooler's output. Sessions with zero embedded events are skipped
// silently — there is nothing to pool, and leaving l1_embedding NULL
// keeps the partial HNSW index honest.
func (w *Writer) PoolSessionToL1(ctx context.Context, workspaceID, sessionID uuid.UUID) error {
	events, err := w.repo.SessionEvents(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("session events: %w", err)
	}
	if len(events) == 0 {
		return nil
	}

	req := mentat.PoolRequest{
		WorkspaceID: workspaceID,
		Events:      make([]mentat.Event, len(events)),
	}
	for i, e := range events {
		req.Events[i] = mentat.Event{Type: e.Type, Embedding: e.Embedding}
	}

	resp, err := w.m.Pool(ctx, req)
	if err != nil {
		return fmt.Errorf("mentat pool: %w", err)
	}
	chunkText := buildChunkText(events)
	if err := w.repo.UpdateSessionL1(ctx, sessionID, resp.Embedding, chunkText); err != nil {
		return fmt.Errorf("update l1: %w", err)
	}
	return nil
}

// buildChunkText concatenates a session's events into a single
// role-prefixed string for cross-encoder rerank. Format mirrors the
// LongMemEval bench backend so the recall pipeline feeds the reranker
// the same artifact shape it was tuned against:
//
//	user: hello
//	assistant: hi, how can i help?
//	...
//
// Events with no text contribute nothing. The full result is persisted
// even if it's longer than the reranker's max-length — the reranker
// will truncate at its token limit, but bench-style chunking happens
// at read time on ghola's side (configurable per recall).
func buildChunkText(events []repository.SessionEventRow) string {
	var b strings.Builder
	for _, e := range events {
		if e.Text == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(e.Type)
		b.WriteString(": ")
		b.WriteString(e.Text)
	}
	return b.String()
}
