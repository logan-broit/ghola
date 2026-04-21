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
	return nil
}
