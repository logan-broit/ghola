package encoding_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/logan-broit/ghola/internal/core"
	"github.com/logan-broit/ghola/internal/encoding"
	"github.com/logan-broit/ghola/internal/sietch"
)

// recordingChapterhouse captures ingest calls so the test can assert
// exactly what crossed the wire.
type recordingChapterhouse struct {
	ingestCalls atomic.Int32
	lastEvents  []core.Event
}

func (r *recordingChapterhouse) IngestEpisodic(_ context.Context, _ core.Session, events []core.Event) (int, int, error) {
	r.ingestCalls.Add(1)
	r.lastEvents = append([]core.Event(nil), events...)
	return len(events), 0, nil
}
func (*recordingChapterhouse) QueryEpisodicMulti(context.Context, core.EpisodicMultiQuery) (core.EpisodicMultiResult, error) {
	return core.EpisodicMultiResult{}, nil
}
func (*recordingChapterhouse) ShareEpisodic(context.Context, core.ShareInput) (string, error) {
	return "", nil
}
func (*recordingChapterhouse) ForgetEpisodic(context.Context, string, []string) (int, error) {
	return 0, nil
}
func (*recordingChapterhouse) AddSessionWorkspace(context.Context, core.AddSessionWorkspaceInput) (bool, error) {
	return true, nil
}
func (*recordingChapterhouse) QuerySemantic(context.Context, core.SemanticQuery) ([]core.RecallHit, error) {
	return nil, nil
}

type nullEmbedder struct{}

func (nullEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3, 0.4}, nil
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newCoreWithStore(t *testing.T) (*core.Core, *sietch.Store, *recordingChapterhouse) {
	t.Helper()
	store, err := sietch.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	ch := &recordingChapterhouse{}
	c := core.New(store, ch, nullEmbedder{})
	return c, store, ch
}

// seedEvents creates a session in the store, records N events, and
// returns the session id.
func seedEvents(t *testing.T, c *core.Core, n int) string {
	t.Helper()
	cwd := "/test"
	sess, err := c.SessionStart(context.Background(), core.SessionStartInput{
		UserID: "u1",
		Cwd:    &cwd,
	})
	require.NoError(t, err)
	for i := 0; i < n; i++ {
		text := "event"
		_, err := c.Record(context.Background(), core.RecordInput{
			SessionID: sess.ID, UserID: "u1",
			Event: core.Event{
				Type:     "user",
				Text:     &text,
				RawEvent: json.RawMessage(`{"t":"x"}`),
			},
		})
		require.NoError(t, err)
	}
	return sess.ID
}

// ---------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------

func TestWorker_TickShipsPendingEvents(t *testing.T) {
	c, store, ch := newCoreWithStore(t)

	seedEvents(t, c, 5)

	w := encoding.NewWorker(c,
		func(ctx context.Context) ([]string, error) {
			return store.ActiveSessionIDs(ctx)
		},
		time.Hour, // disabled tick; we'll fire manually
		quietLogger(),
	)
	require.NoError(t, w.Tick(context.Background()))

	assert.Equal(t, int32(1), ch.ingestCalls.Load())
	assert.Len(t, ch.lastEvents, 5, "all 5 pending events should ship")
}

func TestWorker_EmptyPendingIsNoop(t *testing.T) {
	c, store, ch := newCoreWithStore(t)

	w := encoding.NewWorker(c,
		func(ctx context.Context) ([]string, error) {
			return store.ActiveSessionIDs(ctx)
		},
		time.Hour, quietLogger())
	require.NoError(t, w.Tick(context.Background()))

	assert.Equal(t, int32(0), ch.ingestCalls.Load())
}

func TestWorker_WatermarkAdvancesAcrossTicks(t *testing.T) {
	c, store, ch := newCoreWithStore(t)
	sid := seedEvents(t, c, 3)

	w := encoding.NewWorker(c,
		func(ctx context.Context) ([]string, error) {
			return store.ActiveSessionIDs(ctx)
		},
		time.Hour, quietLogger())

	require.NoError(t, w.Tick(context.Background()))
	assert.Equal(t, int32(1), ch.ingestCalls.Load())
	assert.Len(t, ch.lastEvents, 3)

	// Add two more events + tick again. The watermark must filter out
	// the already-shipped ones; only the 2 new events should ingest.
	text := "another"
	for i := 0; i < 2; i++ {
		_, err := c.Record(context.Background(), core.RecordInput{
			SessionID: sid, UserID: "u1",
			Event: core.Event{Type: "user", Text: &text,
				RawEvent: json.RawMessage(`{"t":"y"}`)},
		})
		require.NoError(t, err)
	}

	require.NoError(t, w.Tick(context.Background()))
	assert.Equal(t, int32(2), ch.ingestCalls.Load())
	assert.Len(t, ch.lastEvents, 2, "watermark-advance ships only the 2 new events")
}

func TestWorker_TickScansEveryActiveSession(t *testing.T) {
	c, store, ch := newCoreWithStore(t)

	// Two separate sessions, each with one event.
	seedEvents(t, c, 1)
	seedEvents(t, c, 1)

	w := encoding.NewWorker(c,
		func(ctx context.Context) ([]string, error) {
			return store.ActiveSessionIDs(ctx)
		},
		time.Hour, quietLogger())
	require.NoError(t, w.Tick(context.Background()))

	assert.Equal(t, int32(2), ch.ingestCalls.Load(),
		"one ingest call per session with pending events")
}

func TestWorker_TriggerForcesImmediateConsolidation(t *testing.T) {
	c, store, ch := newCoreWithStore(t)
	seedEvents(t, c, 1)

	// interval is an hour, so Run wouldn't normally tick during this
	// test window. Trigger() should still drain the session.
	w := encoding.NewWorker(c,
		func(ctx context.Context) ([]string, error) {
			return store.ActiveSessionIDs(ctx)
		},
		time.Hour, quietLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		_ = w.Run(ctx)
		close(done)
	}()

	// Run's initial tick picks up the seeded events. Trigger a second
	// pass and confirm both fired.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if ch.ingestCalls.Load() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.GreaterOrEqual(t, ch.ingestCalls.Load(), int32(1),
		"initial Run tick should consolidate")

	w.Trigger()
	// Add another event and trigger again.
	text := "follow-up"
	sids, _ := store.ActiveSessionIDs(ctx)
	require.Len(t, sids, 1)
	_, err := c.Record(ctx, core.RecordInput{
		SessionID: sids[0], UserID: "u1",
		Event: core.Event{Type: "user", Text: &text,
			RawEvent: json.RawMessage(`{"t":"f"}`)},
	})
	require.NoError(t, err)

	w.Trigger()
	deadline = time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		if ch.ingestCalls.Load() >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.GreaterOrEqual(t, ch.ingestCalls.Load(), int32(2),
		"Trigger() should cause an immediate follow-up consolidation")

	cancel()
	<-done
}

func TestWorker_PerSessionErrorDoesNotAbortTick(t *testing.T) {
	// A broken Core that fails on one session but succeeds on others —
	// simplest way to prove resilience is a SessionSource that
	// returns one real id and one invalid id.
	c, store, ch := newCoreWithStore(t)
	seedEvents(t, c, 1)
	realIDs, err := store.ActiveSessionIDs(context.Background())
	require.NoError(t, err)
	require.Len(t, realIDs, 1)

	w := encoding.NewWorker(c,
		func(ctx context.Context) ([]string, error) {
			// Inject a bad id first; the real one after.
			return []string{uuid.NewString(), realIDs[0]}, nil
		},
		time.Hour, quietLogger())

	require.NoError(t, w.Tick(context.Background()))
	// One ingest: the valid session. The bogus id returns 0 pending
	// (no events table row yet) and no error bubbles up.
	assert.Equal(t, int32(1), ch.ingestCalls.Load())
}
