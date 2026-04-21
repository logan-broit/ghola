package replay_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/thinkwright/chapterhouse/ch-server/internal/replay"
)

// TestScheduler_FiresAtConfiguredHour advances a virtual clock and
// asserts the scheduler sleeps exactly until 02:00 before firing.
func TestScheduler_FiresAtConfiguredHour(t *testing.T) {
	virtualNow := time.Date(2026, 4, 20, 1, 59, 0, 0, time.Local)

	var waitCalls []time.Duration
	done := make(chan struct{})
	var fired int

	s := replay.Scheduler{
		Hour: 2, Min: 0,
		Now: func() time.Time { return virtualNow },
		Wait: func(ctx context.Context, d time.Duration) error {
			waitCalls = append(waitCalls, d)
			// Advance clock by the requested delay so the next
			// iteration computes a fresh 24h delay.
			virtualNow = virtualNow.Add(d)
			if len(waitCalls) >= 2 {
				close(done)
				<-ctx.Done()
				return ctx.Err()
			}
			return nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		<-done
		cancel()
	}()

	_ = s.Run(ctx, func(context.Context) error {
		fired++
		return nil
	})

	assert.Equal(t, 1, fired, "job should fire exactly once in the window")
	assert.Equal(t, time.Minute, waitCalls[0], "first sleep should be 01:59 -> 02:00 = 1 minute")
}

// TestScheduler_NextDayIfPastHour: when current time is already past
// today's scheduled hour, the first wait is until tomorrow's hour.
func TestScheduler_NextDayIfPastHour(t *testing.T) {
	virtualNow := time.Date(2026, 4, 20, 3, 0, 0, 0, time.Local) // 03:00, past 02:00
	done := make(chan struct{})
	var waitCalls []time.Duration

	s := replay.Scheduler{
		Hour: 2, Min: 0,
		Now: func() time.Time { return virtualNow },
		Wait: func(ctx context.Context, d time.Duration) error {
			waitCalls = append(waitCalls, d)
			close(done)
			<-ctx.Done()
			return ctx.Err()
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() { <-done; cancel() }()

	_ = s.Run(ctx, func(context.Context) error { return nil })

	require := assert.New(t)
	require.Equal(23*time.Hour, waitCalls[0], "03:00 -> next 02:00 = 23h")
}
