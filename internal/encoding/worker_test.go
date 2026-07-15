package encoding_test

import (
	"context"
	"encoding/json"
	"errors"
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
func (*recordingChapterhouse) ConsolidateWorkspace(context.Context, string) error {
	return nil
}

type nullEmbedder struct{}

func (nullEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{0.1, 0.2, 0.3, 0.4}, nil
}

// toggleEmbedder simulates embedder downtime: while down it errors, so
// Record degrades to a no-embedding write; flipping up lets the
// backfill pass embed the backlog.
type toggleEmbedder struct{ down bool }

func (e *toggleEmbedder) Embed(context.Context, string) ([]float32, error) {
	if e.down {
		return nil, errors.New("embedder down")
	}
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

// TestWorker_TickBackfillsThenShips: an event recorded while the
// embedder was down lands without an embedding (Record degrades) and
// Consolidate holds it back. Once the embedder recovers, the worker's
// per-tick backfill pass fills the embedding and the same tick ships
// the now-embedded event.
func TestWorker_TickBackfillsThenShips(t *testing.T) {
	store, err := sietch.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	ch := &recordingChapterhouse{}
	emb := &toggleEmbedder{down: true}
	c := core.New(store, ch, emb)

	cwd := "/test"
	sess, err := c.SessionStart(context.Background(), core.SessionStartInput{
		UserID: "u1", Cwd: &cwd,
	})
	require.NoError(t, err)
	text := "recorded while embedder down"
	rec, err := c.Record(context.Background(), core.RecordInput{
		SessionID: sess.ID, UserID: "u1",
		Event: core.Event{Type: "user", Text: &text,
			RawEvent: json.RawMessage(`{"t":"x"}`)},
	})
	require.NoError(t, err)

	// Confirm the precondition: the event is queued for backfill.
	need, err := store.EventsNeedingEmbedding(context.Background(), sess.ID, 0)
	require.NoError(t, err)
	require.Len(t, need, 1)

	emb.down = false

	w := encoding.NewWorker(c,
		func(ctx context.Context) ([]string, error) {
			return store.ActiveSessionIDs(ctx)
		},
		time.Hour, quietLogger())
	require.NoError(t, w.Tick(context.Background()))

	// Backfill filled the embedding, so nothing is left needing one...
	need, err = store.EventsNeedingEmbedding(context.Background(), sess.ID, 0)
	require.NoError(t, err)
	assert.Empty(t, need, "backfill must clear the un-embedded backlog")
	// ...and the now-embedded event shipped on the same tick.
	assert.Equal(t, int32(1), ch.ingestCalls.Load())
	require.Len(t, ch.lastEvents, 1)
	assert.Equal(t, rec.ID, ch.lastEvents[0].ID)
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

// TestWorker_TickGCsDrainedSession: a session that is ended, fully
// consolidated, and older than SietchRetention has its sietch file
// removed on the tick. A session ended only recently is kept. Uses an
// injectable clock (core.Now) rather than sleeps so the 8-day jump is
// instant.
func TestWorker_TickGCsDrainedSession(t *testing.T) {
	store, err := sietch.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	ch := &recordingChapterhouse{}
	c := core.New(store, ch, nullEmbedder{})
	now := time.Unix(1_700_000_000, 0).UTC()
	c.Now = func() time.Time { return now }

	cwd := "/test"
	// "old" will be fully drained and aged past retention; "recent"
	// ends right before the clock jump so it stays inside the window.
	oldSess, err := c.SessionStart(context.Background(),
		core.SessionStartInput{UserID: "u1", Cwd: &cwd})
	require.NoError(t, err)
	text := "an event"
	_, err = c.Record(context.Background(), core.RecordInput{
		SessionID: oldSess.ID, UserID: "u1",
		Event: core.Event{Type: "user", Text: &text,
			RawEvent: json.RawMessage(`{"t":"x"}`)},
	})
	require.NoError(t, err)
	require.NoError(t, c.SessionEnd(context.Background(), oldSess.ID))

	w := encoding.NewWorker(c,
		func(ctx context.Context) ([]string, error) {
			return store.ActiveSessionIDs(ctx)
		},
		time.Hour, quietLogger())

	// First tick: consolidates whatever's left; nothing is old enough
	// to GC yet (ended == now), so the file survives.
	require.NoError(t, w.Tick(context.Background()))
	ids, err := store.ActiveSessionIDs(context.Background())
	require.NoError(t, err)
	assert.Contains(t, ids, oldSess.ID, "session not yet past retention")

	// A second session that ends 1h before the jump — inside the 7d
	// window, so it must be kept after the jump.
	recentSess, err := c.SessionStart(context.Background(),
		core.SessionStartInput{UserID: "u1", Cwd: &cwd})
	require.NoError(t, err)
	_, err = c.Record(context.Background(), core.RecordInput{
		SessionID: recentSess.ID, UserID: "u1",
		Event: core.Event{Type: "user", Text: &text,
			RawEvent: json.RawMessage(`{"t":"y"}`)},
	})
	require.NoError(t, err)
	// End recentSess 8 days minus 1h ahead of the original clock, so
	// after the +8d jump it is only 1h old.
	now = now.Add(8*24*time.Hour - time.Hour)
	require.NoError(t, c.SessionEnd(context.Background(), recentSess.ID))

	// Jump the clock so oldSess is 8d+ past its end, recentSess only 1h.
	now = now.Add(time.Hour)
	require.NoError(t, w.Tick(context.Background()))

	ids, err = store.ActiveSessionIDs(context.Background())
	require.NoError(t, err)
	assert.NotContains(t, ids, oldSess.ID,
		"drained session past retention must be GC'd")
	assert.Contains(t, ids, recentSess.ID,
		"recently-ended session must be kept")
}

// TestWorker_TickGCsOrphanFile: a schema-only sietch file (no session
// row — what conn()'s create-on-open leaves behind when a recall names
// a GC'd session id) is enumerated by ActiveSessionIDs every tick.
// Consolidate fails on it (Watermark can't find the row), but GC must
// still run and remove the orphan — the per-session sequence may not
// bail on the Consolidate failure. A healthy session in the same tick
// is still consolidated, proving the pass isn't aborted.
func TestWorker_TickGCsOrphanFile(t *testing.T) {
	store, err := sietch.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	ch := &recordingChapterhouse{}
	c := core.New(store, ch, nullEmbedder{})
	now := time.Unix(1_700_000_000, 0).UTC()
	c.Now = func() time.Time { return now }

	// A healthy session with one pending event — must still ship.
	healthy := seedEvents(t, c, 1)

	// Create a schema-only orphan: GetSession on a never-opened id makes
	// conn() write the schema with no session row. Ignore the (expected)
	// ErrSessionNotFound; we only want the file on disk.
	orphan := core.NewID()
	_, _ = store.GetSession(context.Background(), orphan)

	ids, err := store.ActiveSessionIDs(context.Background())
	require.NoError(t, err)
	require.Contains(t, ids, orphan, "orphan file must be enumerated")

	w := encoding.NewWorker(c,
		func(ctx context.Context) ([]string, error) {
			return store.ActiveSessionIDs(ctx)
		},
		time.Hour, quietLogger())

	require.NoError(t, w.Tick(context.Background()))

	ids, err = store.ActiveSessionIDs(context.Background())
	require.NoError(t, err)
	assert.NotContains(t, ids, orphan,
		"orphan must be GC'd even though Consolidate failed on it")
	assert.Contains(t, ids, healthy,
		"healthy session must remain (not yet ended/past retention)")
	assert.Equal(t, int32(1), ch.ingestCalls.Load(),
		"healthy session still shipped — the pass was not aborted")
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
