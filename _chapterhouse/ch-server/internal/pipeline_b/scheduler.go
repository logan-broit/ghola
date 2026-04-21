package pipeline_b

import (
	"context"
	"log/slog"
	"time"
)

// Scheduler runs a job once per day at the configured local hour:min.
// Now / Wait are injection points so tests can advance a virtual clock
// without sleeping. Zero values use wall time + a real timer.
type Scheduler struct {
	Hour int
	Min  int
	Now  func() time.Time
	Wait func(ctx context.Context, d time.Duration) error
}

// Run blocks until ctx is cancelled, firing job() each time the next
// scheduled instant arrives. Job errors are logged, never fatal.
func (s *Scheduler) Run(ctx context.Context, job func(context.Context) error) error {
	now := s.Now
	if now == nil {
		now = time.Now
	}
	wait := s.Wait
	if wait == nil {
		wait = func(ctx context.Context, d time.Duration) error {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-t.C:
				return nil
			}
		}
	}

	for {
		t := now()
		next := time.Date(t.Year(), t.Month(), t.Day(), s.Hour, s.Min, 0, 0, t.Location())
		if !next.After(t) {
			next = next.Add(24 * time.Hour)
		}
		if err := wait(ctx, next.Sub(t)); err != nil {
			return err
		}
		if err := job(ctx); err != nil {
			slog.Error("pipeline B job failed", "err", err.Error())
		}
	}
}
