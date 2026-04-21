package pipeline_a

import (
	"context"
	"log/slog"
	"time"

	"github.com/logan-broit/ghola/internal/core"
)

// SessionSource produces the set of session ids Pipeline A should
// consider on each tick. Typically backed by sietch.Store
// .ActiveSessionIDs but kept as a function type so tests can inject
// fixed slices without pulling in sietch.
type SessionSource func(ctx context.Context) ([]string, error)

// Worker is Pipeline A: a ticker goroutine that walks every active
// sietch session and calls Core.Consolidate, which flushes pending
// events to chapterhouse and advances the watermark.
//
// Lossless by construction: Core.Consolidate is idempotent (chapter-
// house upserts on event.id) and watermark-driven (pending events
// are the ones strictly after the last acknowledged event id). A
// failed tick leaves state unchanged; the next tick retries from the
// same watermark.
type Worker struct {
	core     *core.Core
	source   SessionSource
	interval time.Duration
	trigger  chan struct{}
	logger   *slog.Logger
}

// NewWorker builds a Worker. `interval` defaults to 5 minutes — the
// design-doc cadence for continuous consolidation.
func NewWorker(c *core.Core, source SessionSource, interval time.Duration, logger *slog.Logger) *Worker {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Worker{
		core:     c,
		source:   source,
		interval: interval,
		trigger:  make(chan struct{}, 1),
		logger:   logger,
	}
}

// Trigger schedules an immediate consolidation pass. Non-blocking:
// if a trigger is already pending the extra request collapses into
// the existing one.
func (w *Worker) Trigger() {
	select {
	case w.trigger <- struct{}{}:
	default:
	}
}

// Run blocks until the context is cancelled, ticking every
// w.interval and between ticks whenever Trigger() is called.
// Returns the context's error on shutdown.
func (w *Worker) Run(ctx context.Context) error {
	w.logger.Info("pipeline A worker started", "interval", w.interval)

	// Fire an initial tick so sessions recovered from disk at startup
	// get a chance to consolidate without waiting the full interval.
	if err := w.Tick(ctx); err != nil {
		w.logger.Warn("initial tick", "err", err.Error())
	}

	t := time.NewTicker(w.interval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			w.logger.Info("pipeline A worker stopping", "reason", ctx.Err())
			return ctx.Err()
		case <-t.C:
			if err := w.Tick(ctx); err != nil {
				w.logger.Warn("scheduled tick", "err", err.Error())
			}
		case <-w.trigger:
			if err := w.Tick(ctx); err != nil {
				w.logger.Warn("triggered tick", "err", err.Error())
			}
		}
	}
}

// TerminalBranchIdleThreshold is how long a branch must be quiet
// before Pipeline A considers it "terminal" and eligible for a
// coherence pass. The design doc specifies "no activity for >N
// minutes"; 15 minutes is the v1a default.
const TerminalBranchIdleThreshold = 15 * time.Minute

// coherencePass is the "C" half of the Hybrid C+D consolidation
// strategy from the design doc. On each tick we look for branches
// that have gone silent beyond the idle threshold and emit a
// coherence record.
//
// v1a IS INTENTIONALLY a no-op: the design reserves the LLM-assisted
// rewrite for later, and the "concat + metadata" fallback the plan
// describes overlaps with what the continuous-delta path (Tick) has
// already shipped. Once Pipeline B (Phase 8) is online we can have
// it read the branch structure straight out of episodic.events via
// the parent_id tree — no separate coherence artifact needed.
//
// Keeping the hook here so a future revision has a single place to
// plug in without having to re-thread the worker's ticker plumbing.
func (w *Worker) coherencePass(_ context.Context) {
	// Deliberate no-op for v1a. See comment above.
}

// Tick runs one consolidation pass across every active session. Any
// per-session failure is logged but does not abort the pass — other
// sessions still get their flush attempt.
func (w *Worker) Tick(ctx context.Context) error {
	ids, err := w.source(ctx)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}

	var total int
	for _, id := range ids {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		n, err := w.core.Consolidate(ctx, id)
		if err != nil {
			w.logger.Warn("consolidate failed",
				"session_id", id, "err", err.Error())
			continue
		}
		if n > 0 {
			w.logger.Info("consolidated session",
				"session_id", id, "flushed", n)
		}
		total += n
	}
	if total > 0 {
		w.logger.Info("pipeline A tick complete", "flushed_total", total)
	}

	// Coherence pass runs after the incremental-delta flush so the
	// terminal-branch detector sees the full session state in sietch
	// before deciding which branches are quiet.
	w.coherencePass(ctx)

	return nil
}
