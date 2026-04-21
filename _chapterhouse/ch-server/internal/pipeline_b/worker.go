package pipeline_b

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Embedder is the minimum embedding-provider surface Pipeline B
// needs. internal/embedding.Provider satisfies it, as does any test
// stub that returns a fixed vector.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
}

// Distiller abstracts the vLLM call for tests.
type Distiller interface {
	Distill(ctx context.Context, in DistillInput) (*Mneme, error)
}

// WorkerConfig captures everything the nightly worker needs. The
// workspace id is a per-deployment constant until we split team-scoped
// workspaces.
type WorkerConfig struct {
	Window      time.Duration
	MinSupport  int
	WorkspaceID uuid.UUID
	Dedup       DedupConfig
}

// Worker orchestrates detect → distill → upsert.
type Worker struct {
	Pool      *pgxpool.Pool
	Mentat    Distiller
	Embedder  Embedder
	Cfg       WorkerConfig
}

// RunOnce executes a single Pipeline B pass. Per-pair failures are
// logged; we continue to the next pair so one bad LLM reply doesn't
// stall the whole run.
func (w *Worker) RunOnce(ctx context.Context) error {
	window := w.Cfg.Window
	if window == 0 {
		window = 24 * time.Hour
	}
	minSupport := w.Cfg.MinSupport
	if minSupport == 0 {
		minSupport = 3
	}

	pairs, err := DetectPairs(ctx, w.Pool, window, minSupport)
	if err != nil {
		return fmt.Errorf("detect: %w", err)
	}
	slog.Info("pipeline B detected pairs", "count", len(pairs))

	for _, p := range pairs {
		if err := w.processPair(ctx, p); err != nil {
			slog.Error("pipeline B pair failed",
				"e1", p.E1, "e2", p.E2, "err", err.Error())
		}
	}
	return nil
}

// processPair runs the distill+upsert sequence for one entity pair.
func (w *Worker) processPair(ctx context.Context, p EntityPair) error {
	turns, contribs, sources, err := w.gatherContext(ctx, p)
	if err != nil {
		return fmt.Errorf("gather: %w", err)
	}
	if len(turns) == 0 {
		return nil
	}

	mneme, err := w.Mentat.Distill(ctx, DistillInput{E1: p.E1, E2: p.E2, Turns: turns})
	if err != nil {
		return fmt.Errorf("distill: %w", err)
	}

	emb, err := w.Embedder.Embed(ctx, mneme.Concept+". "+mneme.Content)
	if err != nil {
		return fmt.Errorf("embed: %w", err)
	}

	_, err = Upsert(ctx, w.Pool, w.Cfg.Dedup, UpsertInput{
		WorkspaceID:        w.Cfg.WorkspaceID,
		Mneme:              *mneme,
		Embedding:          emb,
		SourceEpisodicIDs:  sources,
		ContributorUserIDs: contribs,
	})
	return err
}

// gatherContext pulls up to 20 recent turn texts + contributor user
// ids + source event ids for the given pair's sessions.
func (w *Worker) gatherContext(ctx context.Context, p EntityPair) ([]string, []uuid.UUID, []uuid.UUID, error) {
	rows, err := w.Pool.Query(ctx, `
		SELECT id, user_id, coalesce(text, '')
		  FROM episodic.events
		 WHERE session_id::text = ANY($1)
		   AND $2::text = ANY(entities)
		   AND $3::text = ANY(entities)
		   AND text IS NOT NULL
		 ORDER BY created_at DESC
		 LIMIT 20
	`, p.SessionIDs, p.E1, p.E2)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()

	seenUsers := map[uuid.UUID]struct{}{}
	var turns []string
	var contribs, sources []uuid.UUID
	for rows.Next() {
		var id, userID uuid.UUID
		var text string
		if err := rows.Scan(&id, &userID, &text); err != nil {
			return nil, nil, nil, err
		}
		turns = append(turns, text)
		sources = append(sources, id)
		if _, ok := seenUsers[userID]; !ok {
			seenUsers[userID] = struct{}{}
			contribs = append(contribs, userID)
		}
	}
	return turns, contribs, sources, rows.Err()
}
