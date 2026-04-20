package core_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/logan-broit/ghola/internal/core"
)

// ---------------------------------------------------------------------
// Recording fakes — simple in-memory implementations that tests use
// via reflection-free assertions ("did this method get called with
// the expected args?"). Kept intentionally boring so the test
// intents read at the test level, not the fake level.
// ---------------------------------------------------------------------

type fakeSietch struct {
	opened   []core.Session
	closed   []string
	events   []core.Event
	current  map[string]string
	bookmarks []struct{ SessionID, EventID, Label string }
	softForget []struct{ SessionID string; IDs []string }

	watermarks map[string]string
	pending    map[string][]core.Event

	vectorHits map[string][]core.RecallHit
	ftsHits    map[string][]core.RecallHit
	sessions   []core.Session
}

func newFakeSietch() *fakeSietch {
	return &fakeSietch{
		current:    map[string]string{},
		watermarks: map[string]string{},
		pending:    map[string][]core.Event{},
		vectorHits: map[string][]core.RecallHit{},
		ftsHits:    map[string][]core.RecallHit{},
	}
}

func (f *fakeSietch) OpenSession(_ context.Context, s core.Session) error {
	f.opened = append(f.opened, s)
	return nil
}
func (f *fakeSietch) CloseSession(_ context.Context, id string) error {
	f.closed = append(f.closed, id)
	return nil
}
func (f *fakeSietch) RecordEvent(_ context.Context, ev core.Event) (core.Event, error) {
	f.events = append(f.events, ev)
	return ev, nil
}
func (f *fakeSietch) SetBookmark(_ context.Context, sid, eid, label string) error {
	f.bookmarks = append(f.bookmarks, struct{ SessionID, EventID, Label string }{sid, eid, label})
	return nil
}
func (f *fakeSietch) SetCurrent(_ context.Context, sid, eid string) error {
	f.current[sid] = eid
	return nil
}
func (f *fakeSietch) CurrentEvent(_ context.Context, sid string) (string, error) {
	return f.current[sid], nil
}
func (f *fakeSietch) SearchVector(_ context.Context, sid string, _ []float32, _ int) ([]core.RecallHit, error) {
	return f.vectorHits[sid], nil
}
func (f *fakeSietch) SearchFTS(_ context.Context, sid, _ string, _ int) ([]core.RecallHit, error) {
	return f.ftsHits[sid], nil
}
func (f *fakeSietch) ListSessions(_ context.Context, _ string) ([]core.Session, error) {
	return f.sessions, nil
}
func (f *fakeSietch) SoftForget(_ context.Context, sid string, ids []string) error {
	f.softForget = append(f.softForget, struct{ SessionID string; IDs []string }{sid, ids})
	return nil
}
func (f *fakeSietch) Watermark(_ context.Context, sid string) (string, error) {
	return f.watermarks[sid], nil
}
func (f *fakeSietch) SetWatermark(_ context.Context, sid, eid string) error {
	f.watermarks[sid] = eid
	return nil
}
func (f *fakeSietch) PendingEvents(_ context.Context, sid, _ string) ([]core.Event, error) {
	return f.pending[sid], nil
}

type fakeChapterhouse struct {
	ingests    []struct{ Sess core.Session; Events []core.Event }
	episQueries []core.EpisodicQuery
	semQueries []core.SemanticQuery
	shares     []core.ShareInput
	forgets    []struct{ UserID string; IDs []string }
	feedbacks  []struct{ MnemeID string; Evidence float64 }

	inserted, updated int
	episResp          []core.RecallHit
	semResp           []core.RecallHit
	shareID           string
	forgetCount       int
	feedbackNewConf   float64
	err               error
}

func newFakeChapterhouse() *fakeChapterhouse {
	return &fakeChapterhouse{shareID: "share-id-42"}
}

func (f *fakeChapterhouse) IngestEpisodic(_ context.Context, s core.Session, events []core.Event) (int, int, error) {
	f.ingests = append(f.ingests, struct{ Sess core.Session; Events []core.Event }{s, events})
	return f.inserted, f.updated, f.err
}
func (f *fakeChapterhouse) QueryEpisodic(_ context.Context, q core.EpisodicQuery) ([]core.RecallHit, error) {
	f.episQueries = append(f.episQueries, q)
	return f.episResp, f.err
}
func (f *fakeChapterhouse) ShareEpisodic(_ context.Context, s core.ShareInput) (string, error) {
	f.shares = append(f.shares, s)
	return f.shareID, f.err
}
func (f *fakeChapterhouse) ForgetEpisodic(_ context.Context, uid string, ids []string) (int, error) {
	f.forgets = append(f.forgets, struct{ UserID string; IDs []string }{uid, ids})
	return f.forgetCount, f.err
}
func (f *fakeChapterhouse) QuerySemantic(_ context.Context, q core.SemanticQuery) ([]core.RecallHit, error) {
	f.semQueries = append(f.semQueries, q)
	return f.semResp, f.err
}
func (f *fakeChapterhouse) FeedbackSemantic(_ context.Context, id string, ev float64) (float64, error) {
	f.feedbacks = append(f.feedbacks, struct{ MnemeID string; Evidence float64 }{id, ev})
	return f.feedbackNewConf, f.err
}

type fakeEmbedder struct {
	called []string
	vec    []float32
	err    error
}

func (f *fakeEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	f.called = append(f.called, text)
	return f.vec, f.err
}

func newCore() (*core.Core, *fakeSietch, *fakeChapterhouse, *fakeEmbedder) {
	s := newFakeSietch()
	ch := newFakeChapterhouse()
	emb := &fakeEmbedder{vec: []float32{0.1, 0.2, 0.3}}
	c := core.New(s, ch, emb)
	c.Now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	return c, s, ch, emb
}

func strPtr(s string) *string { return &s }

// ---------------------------------------------------------------------
// Per-operation tests (one happy-path assertion each). Combined
// with input-validation checks they anchor each Core method.
// ---------------------------------------------------------------------

func TestSessionStart_ProvisionsSietch(t *testing.T) {
	c, s, _, _ := newCore()

	sess, err := c.SessionStart(context.Background(), core.SessionStartInput{UserID: "u1"})
	require.NoError(t, err)
	assert.NotEmpty(t, sess.ID)
	assert.Equal(t, "u1", sess.UserID)
	require.Len(t, s.opened, 1)
	assert.Equal(t, sess.ID, s.opened[0].ID)
}

func TestSessionEnd_ConsolidatesAndCloses(t *testing.T) {
	c, s, ch, _ := newCore()
	s.pending["sess"] = []core.Event{{ID: "e1", UserID: "u1", SessionID: "sess", Type: "user"}}

	require.NoError(t, c.SessionEnd(context.Background(), "sess"))
	assert.Len(t, ch.ingests, 1, "consolidation must fire before close")
	assert.Contains(t, s.closed, "sess")
}

func TestListSessions_DelegatesToSietch(t *testing.T) {
	c, s, _, _ := newCore()
	s.sessions = []core.Session{{ID: "a", UserID: "u1"}}

	out, err := c.ListSessions(context.Background(), "u1")
	require.NoError(t, err)
	assert.Len(t, out, 1)
}

func TestRecord_EmbedsTextAndPersists(t *testing.T) {
	c, s, _, emb := newCore()

	ev, err := c.Record(context.Background(), core.RecordInput{
		SessionID: "sess",
		UserID:    "u1",
		Event:     core.Event{Type: "user", Text: strPtr("hello world")},
	})
	require.NoError(t, err)
	assert.NotEmpty(t, ev.ID)
	assert.Equal(t, emb.vec, ev.Embedding, "embedder output must be attached")
	assert.Len(t, s.events, 1)
	assert.Equal(t, ev.ID, s.current["sess"])
}

func TestBranch_RequiresParent(t *testing.T) {
	c, _, _, _ := newCore()
	_, err := c.Branch(context.Background(), core.RecordInput{
		SessionID: "sess", UserID: "u1",
		Event: core.Event{Type: "user", Text: strPtr("x")},
	})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "parent"))
}

func TestBranch_Happy(t *testing.T) {
	c, s, _, _ := newCore()
	parent := "parent-1"
	ev, err := c.Branch(context.Background(), core.RecordInput{
		SessionID: "sess", UserID: "u1", ParentID: &parent,
		Event: core.Event{Type: "assistant", Text: strPtr("reply")},
	})
	require.NoError(t, err)
	require.NotNil(t, ev.ParentID)
	assert.Equal(t, parent, *ev.ParentID)
	assert.Len(t, s.events, 1)
}

func TestBookmark_RecordsLabel(t *testing.T) {
	c, s, _, _ := newCore()
	require.NoError(t, c.Bookmark(context.Background(), "sess", "evt", "milestone"))
	require.Len(t, s.bookmarks, 1)
	assert.Equal(t, "milestone", s.bookmarks[0].Label)
}

func TestNavigate_SetsCurrent(t *testing.T) {
	c, s, _, _ := newCore()
	require.NoError(t, c.Navigate(context.Background(), "sess", "evt-7"))
	assert.Equal(t, "evt-7", s.current["sess"])
}

func TestRecall_FansOutAcrossTiersAndMerges(t *testing.T) {
	c, s, ch, _ := newCore()
	s.vectorHits["sess"] = []core.RecallHit{{Tier: "working", Score: 0.9, ID: "w1"}}
	ch.episResp = []core.RecallHit{{Tier: "episodic", Score: 0.5, ID: "e1"}}
	ch.semResp = []core.RecallHit{{Tier: "semantic", Score: 0.7, ID: "m1"}}

	out, err := c.Recall(context.Background(), core.RecallInput{
		SessionID: "sess", UserID: "u1", Workspace: "ws1",
		QueryText: "kubernetes",
	})
	require.NoError(t, err)
	require.Len(t, out.Hits, 3)
	assert.Equal(t, "w1", out.Hits[0].ID, "highest score first")
	assert.Equal(t, "m1", out.Hits[1].ID)
	assert.Equal(t, "e1", out.Hits[2].ID)
	assert.Equal(t, map[string]int{"working": 1, "episodic": 1, "semantic": 1}, out.TierCounts)
}

func TestForget_AcrossTiers(t *testing.T) {
	c, s, ch, _ := newCore()
	ch.forgetCount = 2

	n, err := c.Forget(context.Background(), core.ForgetInput{
		SessionID: "sess", UserID: "u1",
		EventIDs: []string{"e1", "e2"},
	})
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Len(t, s.softForget, 1, "sietch soft-delete must fire")
	assert.Len(t, ch.forgets, 1, "chapterhouse forget must fire")
}

func TestShare_DelegatesToChapterhouse(t *testing.T) {
	c, _, ch, _ := newCore()
	ch.shareID = "share-xyz"
	id, err := c.Share(context.Background(), core.ShareInput{
		UserID: "u1", Target: "team", ScopeType: "session", ScopeID: "sess",
	})
	require.NoError(t, err)
	assert.Equal(t, "share-xyz", id)
	require.Len(t, ch.shares, 1)
}

func TestConsolidate_AdvancesWatermark(t *testing.T) {
	c, s, ch, _ := newCore()
	s.pending["sess"] = []core.Event{
		{ID: "e1", SessionID: "sess", UserID: "u1", Type: "user", CreatedAt: c.Now()},
		{ID: "e2", SessionID: "sess", UserID: "u1", Type: "assistant", CreatedAt: c.Now()},
	}
	n, err := c.Consolidate(context.Background(), "sess")
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Len(t, ch.ingests, 1)
	assert.Equal(t, "e2", s.watermarks["sess"], "watermark advances to last event")
}

func TestConsolidate_NoopWhenEmpty(t *testing.T) {
	c, _, ch, _ := newCore()
	n, err := c.Consolidate(context.Background(), "sess")
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Empty(t, ch.ingests)
}

func TestFeedback_DelegatesAndReturnsNewConfidence(t *testing.T) {
	c, _, ch, _ := newCore()
	ch.feedbackNewConf = 0.82
	conf, err := c.Feedback(context.Background(), "mneme-1", 0.95)
	require.NoError(t, err)
	assert.InDelta(t, 0.82, conf, 1e-9)
	require.Len(t, ch.feedbacks, 1)
}

func TestFeedback_RejectsOutOfRangeEvidence(t *testing.T) {
	c, _, _, _ := newCore()
	_, err := c.Feedback(context.Background(), "m1", 1.5)
	require.Error(t, err)
}

// A minimal validation sweep so the input-guard branches are covered
// without a bespoke test per operation.
func TestInputValidation_MissingIDs(t *testing.T) {
	c, _, _, _ := newCore()
	ctx := context.Background()

	_, err := c.SessionStart(ctx, core.SessionStartInput{})
	require.Error(t, err)

	require.Error(t, c.SessionEnd(ctx, ""))

	_, err = c.ListSessions(ctx, "")
	require.Error(t, err)

	_, err = c.Record(ctx, core.RecordInput{UserID: "u1", Event: core.Event{Type: "user"}})
	require.Error(t, err, "record without session_id")

	require.Error(t, c.Bookmark(ctx, "", "e", "l"))
	require.Error(t, c.Navigate(ctx, "", "e"))

	_, err = c.Recall(ctx, core.RecallInput{})
	require.Error(t, err)

	_, err = c.Forget(ctx, core.ForgetInput{UserID: "u1"})
	require.Error(t, err, "forget without ids")

	_, err = c.Share(ctx, core.ShareInput{})
	require.Error(t, err)

	_, err = c.Consolidate(ctx, "")
	require.Error(t, err)

	_, err = c.Feedback(ctx, "", 0.5)
	require.Error(t, err)
}

// Sanity: embedder failures surface.
func TestRecord_EmbedderErrorPropagates(t *testing.T) {
	c, _, _, emb := newCore()
	emb.err = errors.New("melange down")
	_, err := c.Record(context.Background(), core.RecordInput{
		SessionID: "sess", UserID: "u1",
		Event: core.Event{Type: "user", Text: strPtr("needs embedding")},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "embed")
}
