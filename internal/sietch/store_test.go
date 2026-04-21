package sietch_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/logan-broit/ghola/internal/core"
	"github.com/logan-broit/ghola/internal/sietch"
)

func newTestStore(t *testing.T) *sietch.Store {
	t.Helper()
	s, err := sietch.Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mkSession(userID string) core.Session {
	return core.Session{
		ID:        core.NewID(),
		UserID:    userID,
		StartedAt: time.Now().UTC(),
	}
}

// mkEvent uses core.NewID so event ids sort chronologically (ULID).
// Tests that pin custom CreatedAt values rely on this — post-ULID
// PendingEvents orders by id alone, and a non-ULID uuid would
// randomize the pending-set membership.
func mkEvent(sess core.Session, text string, emb []float32) core.Event {
	return core.Event{
		ID:        core.NewID(),
		SessionID: sess.ID,
		UserID:    sess.UserID,
		Type:      "user",
		Text:      &text,
		RawEvent:  json.RawMessage(`{"t":"x"}`),
		Embedding: emb,
		CreatedAt: time.Now().UTC(),
	}
}

func TestOpenSession_Idempotent(t *testing.T) {
	s := newTestStore(t)
	sess := mkSession("u1")

	require.NoError(t, s.OpenSession(context.Background(), sess))
	require.NoError(t, s.OpenSession(context.Background(), sess), "re-open is a no-op upsert")

	sessions, err := s.ListSessions(context.Background(), "u1")
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, sess.ID, sessions[0].ID)
}

func TestRecordEvent_Persists(t *testing.T) {
	s := newTestStore(t)
	sess := mkSession("u1")
	require.NoError(t, s.OpenSession(context.Background(), sess))

	ev := mkEvent(sess, "hello", []float32{1, 0, 0, 0})
	stored, err := s.RecordEvent(context.Background(), ev)
	require.NoError(t, err)
	assert.Equal(t, ev.ID, stored.ID)

	pending, err := s.PendingEvents(context.Background(), sess.ID, "")
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "hello", *pending[0].Text)
	assert.Equal(t, []float32{1, 0, 0, 0}, pending[0].Embedding)
}

func TestSearchVector_RanksByCosine(t *testing.T) {
	s := newTestStore(t)
	sess := mkSession("u1")
	require.NoError(t, s.OpenSession(context.Background(), sess))

	// Three mnemes along the positive x-, y-, z-axes of a 4-d space.
	// A query along +x should rank x first, xy mix second, y last.
	xOnly := mkEvent(sess, "alpha", []float32{1, 0, 0, 0})
	xy := mkEvent(sess, "beta", []float32{0.7, 0.7, 0, 0})
	yOnly := mkEvent(sess, "gamma", []float32{0, 1, 0, 0})

	for _, e := range []core.Event{xOnly, xy, yOnly} {
		_, err := s.RecordEvent(context.Background(), e)
		require.NoError(t, err)
	}

	hits, err := s.SearchVector(context.Background(), sess.ID,
		[]float32{1, 0, 0, 0}, 10)
	require.NoError(t, err)
	require.Len(t, hits, 3)
	assert.Equal(t, xOnly.ID, hits[0].ID, "x-only aligned with query")
	assert.Equal(t, xy.ID, hits[1].ID, "xy mix second")
	assert.Equal(t, yOnly.ID, hits[2].ID, "y-only last (orthogonal)")
	for _, h := range hits {
		assert.Equal(t, "working", h.Tier)
	}
}

func TestSearchFTS_MatchesTextContent(t *testing.T) {
	s := newTestStore(t)
	sess := mkSession("u1")
	require.NoError(t, s.OpenSession(context.Background(), sess))

	_, _ = s.RecordEvent(context.Background(),
		mkEvent(sess, "kubernetes pod scheduling", nil))
	_, _ = s.RecordEvent(context.Background(),
		mkEvent(sess, "docker image layer cache", nil))
	_, _ = s.RecordEvent(context.Background(),
		mkEvent(sess, "helm chart versioning", nil))

	hits, err := s.SearchFTS(context.Background(), sess.ID, "kubernetes", 10)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	assert.Contains(t, hits[0].Content, "kubernetes")
}

func TestCurrentEvent_PointerMoves(t *testing.T) {
	s := newTestStore(t)
	sess := mkSession("u1")
	require.NoError(t, s.OpenSession(context.Background(), sess))
	e := mkEvent(sess, "x", nil)
	_, _ = s.RecordEvent(context.Background(), e)

	require.NoError(t, s.SetCurrent(context.Background(), sess.ID, e.ID))
	got, err := s.CurrentEvent(context.Background(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, e.ID, got)
}

func TestBookmark_Set(t *testing.T) {
	s := newTestStore(t)
	sess := mkSession("u1")
	require.NoError(t, s.OpenSession(context.Background(), sess))
	e := mkEvent(sess, "x", nil)
	_, _ = s.RecordEvent(context.Background(), e)

	require.NoError(t, s.SetBookmark(context.Background(), sess.ID, e.ID, "milestone"))

	pending, err := s.PendingEvents(context.Background(), sess.ID, "")
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.NotNil(t, pending[0].BookmarkLabel)
	assert.Equal(t, "milestone", *pending[0].BookmarkLabel)
}

func TestSoftForget_FlipsState(t *testing.T) {
	s := newTestStore(t)
	sess := mkSession("u1")
	require.NoError(t, s.OpenSession(context.Background(), sess))

	e := mkEvent(sess, "sensitive", []float32{1, 0, 0, 0})
	_, _ = s.RecordEvent(context.Background(), e)
	require.NoError(t, s.SoftForget(context.Background(), sess.ID, []string{e.ID}))

	// Forgotten events must not surface in search results.
	hits, err := s.SearchVector(context.Background(), sess.ID,
		[]float32{1, 0, 0, 0}, 10)
	require.NoError(t, err)
	assert.Empty(t, hits)

	// They also drop out of PendingEvents because state != 'active'.
	pending, err := s.PendingEvents(context.Background(), sess.ID, "")
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func TestWatermark_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	sess := mkSession("u1")
	require.NoError(t, s.OpenSession(context.Background(), sess))

	w, err := s.Watermark(context.Background(), sess.ID)
	require.NoError(t, err)
	assert.Empty(t, w, "new session has no watermark")

	require.NoError(t, s.SetWatermark(context.Background(), sess.ID, "e42"))
	w, err = s.Watermark(context.Background(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "e42", w)
}

func TestPendingEvents_AfterCutoff(t *testing.T) {
	s := newTestStore(t)
	sess := mkSession("u1")
	require.NoError(t, s.OpenSession(context.Background(), sess))

	a := mkEvent(sess, "a", nil)
	a.CreatedAt = time.Now().Add(-3 * time.Second).UTC()
	b := mkEvent(sess, "b", nil)
	b.CreatedAt = time.Now().Add(-2 * time.Second).UTC()
	c := mkEvent(sess, "c", nil)
	c.CreatedAt = time.Now().Add(-1 * time.Second).UTC()

	for _, e := range []core.Event{a, b, c} {
		_, err := s.RecordEvent(context.Background(), e)
		require.NoError(t, err)
	}

	pending, err := s.PendingEvents(context.Background(), sess.ID, b.ID)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, c.ID, pending[0].ID, "only events strictly after `b` are pending")
}

func TestCloseSession_MarksEnded(t *testing.T) {
	s := newTestStore(t)
	sess := mkSession("u1")
	require.NoError(t, s.OpenSession(context.Background(), sess))
	require.NoError(t, s.CloseSession(context.Background(), sess.ID))

	// Reopen to read the session row; ended_at should be populated.
	sessions, err := s.ListSessions(context.Background(), "u1")
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.NotNil(t, sessions[0].EndedAt)
}

func TestListSessions_UserScoping(t *testing.T) {
	s := newTestStore(t)
	alice := mkSession("alice")
	bob := mkSession("bob")
	require.NoError(t, s.OpenSession(context.Background(), alice))
	require.NoError(t, s.OpenSession(context.Background(), bob))

	out, err := s.ListSessions(context.Background(), "alice")
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, alice.ID, out[0].ID)
}
