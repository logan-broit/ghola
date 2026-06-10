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

func TestEventsNeedingEmbedding_RoundTrip(t *testing.T) {
	s := newTestStore(t)
	sess := mkSession("u1")
	require.NoError(t, s.OpenSession(context.Background(), sess))

	// e1: text but no embedding -> recorded while embedder was down.
	e1 := mkEvent(sess, "needs embedding", nil)
	// e2: text AND embedding -> already embedded, must not be returned.
	e2 := mkEvent(sess, "already embedded", []float32{1, 0, 0, 0})
	// e3: nil text (e.g. a tool-output-only event) -> nothing to embed.
	e3 := mkEvent(sess, "", nil)
	e3.Text = nil

	for _, e := range []core.Event{e1, e2, e3} {
		_, err := s.RecordEvent(context.Background(), e)
		require.NoError(t, err)
	}

	need, err := s.EventsNeedingEmbedding(context.Background(), sess.ID, 0)
	require.NoError(t, err)
	require.Len(t, need, 1)
	assert.Equal(t, e1.ID, need[0].ID)
	assert.Equal(t, sess.ID, need[0].SessionID)
	assert.Equal(t, "u1", need[0].UserID)
	require.NotNil(t, need[0].Text)
	assert.Equal(t, "needs embedding", *need[0].Text)

	// Backfill the embedding; e1 drops out of the needs-embedding set.
	require.NoError(t, s.SetEmbedding(context.Background(), sess.ID, e1.ID,
		[]float32{1, 0, 0, 0}))
	need, err = s.EventsNeedingEmbedding(context.Background(), sess.ID, 0)
	require.NoError(t, err)
	assert.Empty(t, need)

	// And the backfilled event now surfaces under vector search.
	hits, err := s.SearchVector(context.Background(), sess.ID,
		[]float32{1, 0, 0, 0}, 10)
	require.NoError(t, err)
	ids := make([]string, len(hits))
	for i, h := range hits {
		ids[i] = h.ID
	}
	assert.Contains(t, ids, e1.ID, "backfilled event must be searchable")
}

func TestEventsNeedingEmbedding_SkipsForgotten(t *testing.T) {
	s := newTestStore(t)
	sess := mkSession("u1")
	require.NoError(t, s.OpenSession(context.Background(), sess))

	e := mkEvent(sess, "sensitive, un-embedded", nil)
	_, err := s.RecordEvent(context.Background(), e)
	require.NoError(t, err)
	require.NoError(t, s.SoftForget(context.Background(), sess.ID, []string{e.ID}))

	need, err := s.EventsNeedingEmbedding(context.Background(), sess.ID, 0)
	require.NoError(t, err)
	assert.Empty(t, need, "forgotten events must never be backfilled")
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

func TestMarkEnded_StampsEndedAtBeforeClose(t *testing.T) {
	s := newTestStore(t)
	sess := mkSession("u1")
	require.NoError(t, s.OpenSession(context.Background(), sess))

	stamp := time.Unix(1_700_000_500, 0).UTC()
	require.NoError(t, s.MarkEnded(context.Background(), sess.ID, stamp))

	// GetSession must return the timestamp we just stamped, even though
	// CloseSession hasn't been called.
	got, err := s.GetSession(context.Background(), sess.ID)
	require.NoError(t, err)
	require.NotNil(t, got.EndedAt)
	assert.Equal(t, stamp.UnixMilli(), got.EndedAt.UnixMilli())

	// CloseSession after MarkEnded must NOT clobber the earlier
	// timestamp (COALESCE preserves it).
	require.NoError(t, s.CloseSession(context.Background(), sess.ID))
	sessions, err := s.ListSessions(context.Background(), "u1")
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.NotNil(t, sessions[0].EndedAt)
	assert.Equal(t, stamp.UnixMilli(), sessions[0].EndedAt.UnixMilli(),
		"CloseSession must not overwrite an explicit MarkEnded")
}

func TestGetSession_ReturnsRow(t *testing.T) {
	s := newTestStore(t)
	sess := mkSession("u1")
	require.NoError(t, s.OpenSession(context.Background(), sess))

	got, err := s.GetSession(context.Background(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, sess.ID, got.ID)
	assert.Equal(t, "u1", got.UserID)
	assert.Nil(t, got.EndedAt, "fresh session must have ended_at = nil")
}

func TestOpenSession_PersistsWorkspaceID(t *testing.T) {
	s := newTestStore(t)
	sess := core.Session{
		ID:          "sess-ws-1",
		UserID:      "u-1",
		StartedAt:   time.Now().UTC(),
		WorkspaceID: "11111111-2222-3333-4444-555555555555",
	}
	require.NoError(t, s.OpenSession(context.Background(), sess))

	got, err := s.GetSession(context.Background(), sess.ID)
	require.NoError(t, err)
	assert.Equal(t, sess.WorkspaceID, got.WorkspaceID)
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
