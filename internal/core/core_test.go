package core_test

import (
	"context"
	"encoding/json"
	"errors"
	stdhttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/logan-broit/ghola/internal/core"
	"github.com/logan-broit/ghola/internal/truthsayer"
)

// ---------------------------------------------------------------------
// Recording fakes — simple in-memory implementations that tests use
// via reflection-free assertions ("did this method get called with
// the expected args?"). Kept intentionally boring so the test
// intents read at the test level, not the fake level.
// ---------------------------------------------------------------------

type fakeSietch struct {
	opened     []core.Session
	closed     []string
	ended      map[string]time.Time
	events     []core.Event
	current    map[string]string
	bookmarks  []struct{ SessionID, EventID, Label string }
	softForget []struct {
		SessionID string
		IDs       []string
	}

	watermarks map[string]string
	pending    map[string][]core.Event

	// needEmbedding is the canned EventsNeedingEmbedding result per
	// session; setEmbeddings records the (sessionID,eventID) backfills
	// BackfillEmbeddings drives.
	needEmbedding map[string][]core.Event
	setEmbeddings []struct{ SessionID, EventID string }

	vectorHits map[string][]core.RecallHit
	ftsHits    map[string][]core.RecallHit
	sessions   []core.Session

	// searchVectorSessions / searchFTSSessions capture the session id
	// each working-tier sub-query was invoked with — tests assert the
	// implicit open-session resolution scoped the call correctly.
	searchVectorSessions []string
	searchFTSSessions    []string

	// metadata returned by GetSession, keyed by session id. Tests that
	// care about ended_at / cwd / etc. populate this directly.
	sessionRows map[string]core.Session

	// getSessionErr lets a test force GetSession to fail for a given id
	// (e.g. returning core.ErrSessionNotFound for the orphan-GC path).
	getSessionErr map[string]error

	// removeSessions records the ids passed to RemoveSession so GC
	// tests can assert whether (and which) sessions were dropped.
	removeSessions []string
}

func newFakeSietch() *fakeSietch {
	return &fakeSietch{
		current:       map[string]string{},
		watermarks:    map[string]string{},
		pending:       map[string][]core.Event{},
		vectorHits:    map[string][]core.RecallHit{},
		ftsHits:       map[string][]core.RecallHit{},
		ended:         map[string]time.Time{},
		sessionRows:   map[string]core.Session{},
		needEmbedding: map[string][]core.Event{},
		getSessionErr: map[string]error{},
	}
}

func (f *fakeSietch) OpenSession(_ context.Context, s core.Session) error {
	f.opened = append(f.opened, s)
	f.sessionRows[s.ID] = s
	return nil
}
func (f *fakeSietch) CloseSession(_ context.Context, id string) error {
	f.closed = append(f.closed, id)
	return nil
}
func (f *fakeSietch) MarkEnded(_ context.Context, id string, t time.Time) error {
	f.ended[id] = t
	row := f.sessionRows[id]
	row.ID = id
	tt := t
	row.EndedAt = &tt
	f.sessionRows[id] = row
	return nil
}
func (f *fakeSietch) GetSession(_ context.Context, id string) (core.Session, error) {
	if err := f.getSessionErr[id]; err != nil {
		return core.Session{}, err
	}
	if row, ok := f.sessionRows[id]; ok {
		return row, nil
	}
	return core.Session{ID: id}, nil
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
	f.searchVectorSessions = append(f.searchVectorSessions, sid)
	return f.vectorHits[sid], nil
}
func (f *fakeSietch) SearchFTS(_ context.Context, sid, _ string, _ int) ([]core.RecallHit, error) {
	f.searchFTSSessions = append(f.searchFTSSessions, sid)
	return f.ftsHits[sid], nil
}
func (f *fakeSietch) ListSessions(_ context.Context, _ string) ([]core.Session, error) {
	return f.sessions, nil
}
func (f *fakeSietch) SoftForget(_ context.Context, sid string, ids []string) error {
	f.softForget = append(f.softForget, struct {
		SessionID string
		IDs       []string
	}{sid, ids})
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
func (f *fakeSietch) EventsNeedingEmbedding(_ context.Context, sid string, _ int) ([]core.Event, error) {
	return f.needEmbedding[sid], nil
}
func (f *fakeSietch) SetEmbedding(_ context.Context, sid, eid string, _ []float32) error {
	f.setEmbeddings = append(f.setEmbeddings, struct{ SessionID, EventID string }{sid, eid})
	return nil
}
func (f *fakeSietch) RemoveSession(_ context.Context, sid string) error {
	f.removeSessions = append(f.removeSessions, sid)
	return nil
}

type fakeChapterhouse struct {
	ingests []struct {
		Sess   core.Session
		Events []core.Event
	}
	// multiQueries records every QueryEpisodicMulti call — the only
	// event-grain entry point core.Recall uses post-A6.
	multiQueries []core.EpisodicMultiQuery
	semQueries   []core.SemanticQuery
	shares       []core.ShareInput
	forgets      []struct {
		UserID string
		IDs    []string
	}

	inserted, updated int
	// Per-tier canned responses keyed by the multi-ranking sub-list
	// they populate (vector → Vector, fts → FTS, session_vector →
	// SessionVector). Tests set these directly; QueryEpisodicMulti
	// projects them onto the response based on q.Rankings.
	episResp []core.RecallHit
	kwResp   []core.RecallHit
	svResp   []core.RecallHit
	semResp  []core.RecallHit
	// primResp drives the 4th sub-list (Primitives). Three-state
	// semantics mirror the wire (chapterhouse.QueryEpisodicMultiResponse):
	//   - nil → primitives field absent on the response (flag off, OR
	//     flag on but the chapterhouse-side association lookup failed
	//     and the handler dropped the field).
	//   - non-nil pointer → primitives field present (empty slice when
	//     no in-set boosts surfaced, populated otherwise).
	// QueryEpisodicMulti only surfaces this when q.Primitives=true — so
	// tests that set primResp without flipping the flag still pin the
	// "off → no primitives sub-list" property.
	primResp *[]core.RecallHit
	// expResp drives the P4 expansion sub-list. QueryEpisodicMulti only
	// surfaces it when q.Settle is true (mirrors the chapterhouse handler
	// contract: no settle block → no expansion). nil → EpisodicMultiResult
	// .Expansion stays nil (settle off, or best-effort settle failure).
	expResp     []core.ExpansionHit
	shareID     string
	forgetCount int
	err         error

	// multiErr / semErr let recall-degradation tests fail one
	// chapterhouse tier independently of the other (the shared `err`
	// field above fails everything at once, which the happy-path tests
	// don't touch). When set they take precedence over `err` for that
	// method.
	multiErr error
	semErr   error
	// semBlockUntilCtxDone makes QuerySemantic block until its context
	// is cancelled, then return ctx.Err(). Drives the per-tier timeout
	// test without a fixed sleep — the tier unblocks the instant
	// TierTimeout fires.
	semBlockUntilCtxDone bool

	// consolidateWorkspaces records every ConsolidateWorkspace call's
	// resolved workspace id. consolidateErr, when set, is returned
	// instead of nil (independent of the shared `err` field so
	// consolidate-specific error tests don't perturb other methods).
	consolidateWorkspaces []string
	consolidateErr        error
}

func newFakeChapterhouse() *fakeChapterhouse {
	return &fakeChapterhouse{shareID: "share-id-42"}
}

func (f *fakeChapterhouse) IngestEpisodic(_ context.Context, s core.Session, events []core.Event) (int, int, error) {
	f.ingests = append(f.ingests, struct {
		Sess   core.Session
		Events []core.Event
	}{s, events})
	return f.inserted, f.updated, f.err
}

// QueryEpisodicMulti is the only event-grain entry point. Returns the
// canned per-tier responses gated on the request's Rankings list —
// tiers the caller did not request decode as nil, mirroring the wire
// contract ghola.client implements.
func (f *fakeChapterhouse) QueryEpisodicMulti(_ context.Context, q core.EpisodicMultiQuery) (core.EpisodicMultiResult, error) {
	f.multiQueries = append(f.multiQueries, q)
	if f.multiErr != nil {
		return core.EpisodicMultiResult{}, f.multiErr
	}
	if f.err != nil {
		return core.EpisodicMultiResult{}, f.err
	}
	requested := make(map[string]struct{}, len(q.Rankings))
	for _, name := range q.Rankings {
		requested[name] = struct{}{}
	}
	out := core.EpisodicMultiResult{}
	if _, ok := requested["vector"]; ok {
		out.Vector = append([]core.RecallHit(nil), f.episResp...)
	}
	if _, ok := requested["fts"]; ok {
		out.FTS = append([]core.RecallHit(nil), f.kwResp...)
	}
	if _, ok := requested["session_vector"]; ok {
		out.SessionVector = append([]core.RecallHit(nil), f.svResp...)
	}
	// Primitives is opt-in via q.Primitives — even if primResp is set,
	// the flag must be flipped for the sub-list to surface. Mirrors
	// the chapterhouse handler contract.
	if q.Primitives && f.primResp != nil {
		copyHits := append([]core.RecallHit(nil), (*f.primResp)...)
		out.Primitives = &copyHits
	}
	// Expansion is opt-in via q.Settle — even if expResp is set, the
	// settle flag must be on for the sub-list to surface. Mirrors the
	// chapterhouse handler contract.
	if q.Settle && f.expResp != nil {
		out.Expansion = append([]core.ExpansionHit(nil), f.expResp...)
	}
	return out, nil
}
func (f *fakeChapterhouse) ShareEpisodic(_ context.Context, s core.ShareInput) (string, error) {
	f.shares = append(f.shares, s)
	return f.shareID, f.err
}
func (f *fakeChapterhouse) ForgetEpisodic(_ context.Context, uid string, ids []string) (int, error) {
	f.forgets = append(f.forgets, struct {
		UserID string
		IDs    []string
	}{uid, ids})
	return f.forgetCount, f.err
}
func (f *fakeChapterhouse) AddSessionWorkspace(_ context.Context, _ core.AddSessionWorkspaceInput) (bool, error) {
	return true, f.err
}
func (f *fakeChapterhouse) QuerySemantic(ctx context.Context, q core.SemanticQuery) ([]core.RecallHit, error) {
	f.semQueries = append(f.semQueries, q)
	if f.semBlockUntilCtxDone {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if f.semErr != nil {
		return nil, f.semErr
	}
	return f.semResp, f.err
}
func (f *fakeChapterhouse) ConsolidateWorkspace(_ context.Context, workspaceID string) error {
	f.consolidateWorkspaces = append(f.consolidateWorkspaces, workspaceID)
	return f.consolidateErr
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

	cwd := "/tmp/test"
	sess, err := c.SessionStart(context.Background(), core.SessionStartInput{
		UserID: "u1",
		Cwd:    &cwd,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, sess.ID)
	assert.Equal(t, "u1", sess.UserID)
	require.Len(t, s.opened, 1)
	assert.Equal(t, sess.ID, s.opened[0].ID)
}

// TestSessionStart_DerivesWorkspaceFromCwd: when no workspace_id is
// passed, SessionStart must derive one from cwd via WorkspaceForCwd.
// This is the common case for agent invocations — they pass cwd, not
// an explicit workspace UUID.
func TestSessionStart_DerivesWorkspaceFromCwd(t *testing.T) {
	c, _, _, _ := newCore()
	cwd := "/path/to/project"
	in := core.SessionStartInput{
		UserID: "u1",
		Cwd:    &cwd,
	}

	s, err := c.SessionStart(context.Background(), in)
	require.NoError(t, err)

	want := core.WorkspaceForCwd(cwd).String()
	assert.Equal(t, want, s.WorkspaceID,
		"absent workspace_id, session must derive from cwd")
}

// TestSessionStart_RespectsExplicitWorkspaceID: when an explicit
// workspace_id is passed, it wins over cwd-derivation. This is the
// path callers use when they want to scope to a workspace that
// doesn't match the literal cwd (e.g. agent on host A operating on a
// repo synced from host B).
func TestSessionStart_RespectsExplicitWorkspaceID(t *testing.T) {
	c, _, _, _ := newCore()
	cwd := "/should/be/ignored"
	explicit := "11111111-2222-3333-4444-555555555555"
	in := core.SessionStartInput{
		UserID:      "u1",
		WorkspaceID: explicit,
		Cwd:         &cwd,
	}

	s, err := c.SessionStart(context.Background(), in)
	require.NoError(t, err)
	assert.Equal(t, explicit, s.WorkspaceID,
		"explicit workspace_id wins over cwd derivation")
}

// TestSessionStart_RejectsMissingBoth pins the symmetry with the
// recall path: every chapterhouse-bound query carries a workspace,
// every ingested session is scoped to one. Neither input -> error.
func TestSessionStart_RejectsMissingBoth(t *testing.T) {
	c, _, _, _ := newCore()
	in := core.SessionStartInput{UserID: "u1"}

	_, err := c.SessionStart(context.Background(), in)
	require.ErrorIs(t, err, core.ErrMissingWorkspaceOrCwd)
}

// TestSessionStart_RejectsInvalidWorkspaceUUID: explicit workspace_id
// must parse as a UUID. Otherwise the session row would carry a
// non-UUID workspace_id that would silently fail downstream joins.
func TestSessionStart_RejectsInvalidWorkspaceUUID(t *testing.T) {
	c, _, _, _ := newCore()
	cwd := "/path/to/project"
	in := core.SessionStartInput{
		UserID:      "u1",
		WorkspaceID: "not-a-uuid",
		Cwd:         &cwd,
	}

	_, err := c.SessionStart(context.Background(), in)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "workspace_id must be a valid UUID")
}

func TestSessionEnd_ConsolidatesAndCloses(t *testing.T) {
	c, s, ch, _ := newCore()
	s.pending["sess"] = []core.Event{{ID: "e1", UserID: "u1", SessionID: "sess", Type: "user"}}

	require.NoError(t, c.SessionEnd(context.Background(), "sess"))
	assert.Len(t, ch.ingests, 1, "consolidation must fire before close")
	assert.Contains(t, s.closed, "sess")
}

// SessionEnd must stamp ended_at on sietch BEFORE Consolidate runs so
// the chapterhouse session upsert lands with ended_at populated. Without
// this, the predictive-replay reconciler (`ended_at IS NOT NULL AND
// l1_embedding IS NULL`) silently skips every session that ended via
// SessionEnd — i.e. every real ghola session.
func TestSessionEnd_PropagatesEndedAtToChapterhouse(t *testing.T) {
	c, s, ch, _ := newCore()
	s.pending["sess"] = []core.Event{{ID: "e1", UserID: "u1", SessionID: "sess", Type: "user"}}

	require.NoError(t, c.SessionEnd(context.Background(), "sess"))
	require.Len(t, ch.ingests, 1)
	require.NotNil(t, ch.ingests[0].Sess.EndedAt,
		"chapterhouse must receive ended_at — bug: Consolidate built Session{} from event fields and never read sietch's ended_at")
	assert.Equal(t, c.Now(), *ch.ingests[0].Sess.EndedAt)
}

// Pipeline A's encoding worker calls Consolidate on a tick while the
// session is still live. Sietch hasn't seen MarkEnded for these
// sessions, so GetSession returns ended_at = nil — which must propagate
// as nil to chapterhouse. The reconciler is supposed to skip these
// rows; flipping ended_at non-nil mid-session would let half-written
// sessions get pooled into l1_embedding prematurely.
func TestConsolidate_LeavesEndedAtNilForActiveSessions(t *testing.T) {
	c, s, ch, _ := newCore()
	s.pending["sess"] = []core.Event{{ID: "e1", UserID: "u1", SessionID: "sess", Type: "user"}}
	// no MarkEnded call — session is still active

	_, err := c.Consolidate(context.Background(), "sess")
	require.NoError(t, err)
	require.Len(t, ch.ingests, 1)
	assert.Nil(t, ch.ingests[0].Sess.EndedAt,
		"active session must not carry ended_at; reconciler eligibility flips on this column")
}

// TestConsolidate_HoldsBackUnembeddedSuffix: the watermark must stay a
// contiguous prefix of the id-ordered log. An un-embedded event (one
// recorded while the embedder was down) cuts the shippable prefix —
// everything from it onward is held back until backfill catches up.
func TestConsolidate_HoldsBackUnembeddedSuffix(t *testing.T) {
	c, s, ch, _ := newCore()
	e1 := core.Event{ID: "e1", UserID: "u1", SessionID: "sess", Type: "user",
		Text: strPtr("a"), Embedding: []float32{1, 0}}
	e2 := core.Event{ID: "e2", UserID: "u1", SessionID: "sess", Type: "user",
		Text: strPtr("b")} // no embedding -> the cut point
	e3 := core.Event{ID: "e3", UserID: "u1", SessionID: "sess", Type: "user",
		Text: strPtr("c"), Embedding: []float32{0, 1}}
	s.pending["sess"] = []core.Event{e1, e2, e3}

	n, err := c.Consolidate(context.Background(), "sess")
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only the embedded prefix ships")
	require.Len(t, ch.ingests, 1)
	require.Len(t, ch.ingests[0].Events, 1)
	assert.Equal(t, "e1", ch.ingests[0].Events[0].ID)
	assert.Equal(t, "e1", s.watermarks["sess"], "watermark advances only to the shipped prefix")
}

// TestConsolidate_AllUnembedded_NoOp: when the very first pending event
// still needs an embedding there is no contiguous embedded prefix to
// ship. Consolidate must no-op: no ingest, no watermark advance.
func TestConsolidate_AllUnembedded_NoOp(t *testing.T) {
	c, s, ch, _ := newCore()
	s.pending["sess"] = []core.Event{
		{ID: "e1", UserID: "u1", SessionID: "sess", Type: "user", Text: strPtr("a")},
	}

	n, err := c.Consolidate(context.Background(), "sess")
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	assert.Empty(t, ch.ingests, "nothing embedded -> nothing ships")
	assert.Empty(t, s.watermarks["sess"], "watermark must not advance")
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

// TestRecord_AutoCreatesSessionFromCwd: when a caller omits session_id
// but supplies cwd, Record provisions a session inline (workspace
// derived from cwd) and tags the event with the new session's ID.
// This is the "agent forgot to call session_start" affordance — every
// MCP host hits this because the protocol has no conversation-start
// hook.
func TestRecord_AutoCreatesSessionFromCwd(t *testing.T) {
	c, s, _, _ := newCore()
	cwd := "/tmp/proj"

	ev, err := c.Record(context.Background(), core.RecordInput{
		UserID: "u1",
		Cwd:    &cwd,
		Event:  core.Event{Type: "user", Text: strPtr("hello")},
	})
	require.NoError(t, err)
	require.Len(t, s.opened, 1, "auto-created exactly one session")
	assert.Equal(t, s.opened[0].ID, ev.SessionID,
		"event tagged with auto-created session id")
	assert.Equal(t, core.WorkspaceForCwd(cwd).String(), s.opened[0].WorkspaceID,
		"workspace derived from cwd")
}

// TestRecord_ReusesExistingOpenSessionForSameCwd: a subsequent record
// for the same user+workspace must reuse the live session rather than
// fragmenting the conversation across one-event-per-session shards.
// Reuse rule: most-recent un-ended session whose workspace_id matches
// uuid5(NS_workspace, cwd).
func TestRecord_ReusesExistingOpenSessionForSameCwd(t *testing.T) {
	c, s, _, _ := newCore()
	cwd := "/tmp/proj"
	s.sessions = []core.Session{{
		ID:          "sess-existing",
		UserID:      "u1",
		WorkspaceID: core.WorkspaceForCwd(cwd).String(),
		StartedAt:   time.Unix(1_699_999_000, 0).UTC(),
		// EndedAt nil -> still open
	}}

	ev, err := c.Record(context.Background(), core.RecordInput{
		UserID: "u1",
		Cwd:    &cwd,
		Event:  core.Event{Type: "user", Text: strPtr("turn 2")},
	})
	require.NoError(t, err)
	assert.Equal(t, "sess-existing", ev.SessionID, "reused existing open session")
	assert.Empty(t, s.opened, "no new session created")
}

// TestRecord_PicksMostRecentOpenSession: when two open sessions exist
// for the same user+workspace, the newer one wins. Older orphans get
// left alone (caller closes them explicitly via session_end).
func TestRecord_PicksMostRecentOpenSession(t *testing.T) {
	c, s, _, _ := newCore()
	cwd := "/tmp/proj"
	ws := core.WorkspaceForCwd(cwd).String()
	s.sessions = []core.Session{
		{ID: "older", UserID: "u1", WorkspaceID: ws,
			StartedAt: time.Unix(1_699_000_000, 0).UTC()},
		{ID: "newer", UserID: "u1", WorkspaceID: ws,
			StartedAt: time.Unix(1_700_000_000, 0).UTC()},
	}

	ev, err := c.Record(context.Background(), core.RecordInput{
		UserID: "u1", Cwd: &cwd,
		Event: core.Event{Type: "user", Text: strPtr("x")},
	})
	require.NoError(t, err)
	assert.Equal(t, "newer", ev.SessionID)
}

// TestRecord_IgnoresEndedSessionsAndCreatesNew: an ended session for
// the same user+workspace is not eligible for reuse. New record gets
// a fresh session.
func TestRecord_IgnoresEndedSessionsAndCreatesNew(t *testing.T) {
	c, s, _, _ := newCore()
	cwd := "/tmp/proj"
	ws := core.WorkspaceForCwd(cwd).String()
	endedAt := time.Unix(1_699_999_999, 0).UTC()
	s.sessions = []core.Session{{
		ID: "closed", UserID: "u1", WorkspaceID: ws,
		StartedAt: time.Unix(1_699_999_000, 0).UTC(), EndedAt: &endedAt,
	}}

	ev, err := c.Record(context.Background(), core.RecordInput{
		UserID: "u1", Cwd: &cwd,
		Event: core.Event{Type: "user", Text: strPtr("hi")},
	})
	require.NoError(t, err)
	assert.NotEqual(t, "closed", ev.SessionID, "must not reuse an ended session")
	require.Len(t, s.opened, 1, "must auto-create a new session")
}

// TestRecord_NoSessionNoCwdReturnsValidation: without session_id AND
// without cwd, Record can't derive a workspace and must surface a
// validation error so the boundary returns 400 instead of leaking
// an internal "session required" 500.
func TestRecord_NoSessionNoCwdReturnsValidation(t *testing.T) {
	c, _, _, _ := newCore()
	_, err := c.Record(context.Background(), core.RecordInput{
		UserID: "u1",
		Event:  core.Event{Type: "user", Text: strPtr("hi")},
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrValidation)
}

// TestRecord_ExplicitSessionIDWinsOverCwd: when both are supplied,
// session_id is honored verbatim. cwd is decorative in that case —
// no workspace re-derivation, no session lookup, no implicit creation.
func TestRecord_ExplicitSessionIDWinsOverCwd(t *testing.T) {
	c, s, _, _ := newCore()
	cwd := "/tmp/proj"
	ev, err := c.Record(context.Background(), core.RecordInput{
		SessionID: "explicit",
		UserID:    "u1",
		Cwd:       &cwd,
		Event:     core.Event{Type: "user", Text: strPtr("hi")},
	})
	require.NoError(t, err)
	assert.Equal(t, "explicit", ev.SessionID)
	assert.Empty(t, s.opened, "no implicit session creation when SessionID is set")
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

// TestRecall_UsesMultiRankingCall pins the A6 migration end-to-end:
// Recall fires exactly ONE chapterhouse call for the event-grain
// fan-out (vector + fts + session_vector) via QueryEpisodicMulti
// instead of the three legacy per-tier methods. Without this property
// the fan-out is three round-trips again (the regression A6 prevents).
//
// Tags_any contract (carry-over from A1 review): the multi-ranking
// request must carry the same tags_any list the caller passed on
// RecallInput, so chapterhouse forwards it to every event-grain
// ranking handler. Pinned end-to-end here because A6 is the first
// caller to flow tags through to the new shared entry point.
func TestRecall_UsesMultiRankingCall(t *testing.T) {
	c, _, ch, _ := newCore()
	// Populate per-tier responses so Recall has hits to fuse — the
	// fake's QueryEpisodicMulti decomposes the multi call into the
	// matching per-tier sub-lists from these slots.
	ch.episResp = []core.RecallHit{{Tier: "episodic", Score: 0.7, ID: "e1"}}
	ch.kwResp = []core.RecallHit{{Tier: "keyword", Score: 0.5, ID: "k1"}}
	ch.svResp = []core.RecallHit{{Tier: "session_vector", Score: 0.6, ID: "sv1"}}

	out, err := c.Recall(context.Background(), core.RecallInput{
		UserID:    "u1",
		Workspace: "ws1",
		QueryText: "kubernetes",
		TagsAny:   []string{"era:v15"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, out.Hits, "Recall should return fused hits")

	// Single multi-ranking call drives all three event-grain tiers in
	// one HTTP round-trip — the whole point of A6 (and the only
	// surface left after A8 deleted the legacy per-tier methods).
	require.Len(t, ch.multiQueries, 1, "Recall must fire exactly one QueryEpisodicMulti call")

	// Tags_any contract: forwarded to the multi-ranking request so
	// chapterhouse plumbs it to every event-grain tier.
	assert.Equal(t, []string{"era:v15"}, ch.multiQueries[0].TagsAny,
		"tags_any must propagate from RecallInput to QueryEpisodicMulti")

	// Rankings list: when the recall has both query_text AND embedding
	// (default fan-out path), all three event-grain rankings are
	// requested. core.Recall's ranking subset is the load-bearing
	// signal the chapterhouse handler keys off of, so pin it.
	assert.ElementsMatch(t,
		[]string{"vector", "fts", "session_vector"},
		ch.multiQueries[0].Rankings,
		"fan-out with query_text+embedding requests all three event-grain tiers")

	// Identifiers + query inputs survive the migration.
	assert.Equal(t, "u1", ch.multiQueries[0].UserID)
	assert.Equal(t, "ws1", ch.multiQueries[0].WorkspaceID)
	assert.Equal(t, "kubernetes", ch.multiQueries[0].QueryText)
	assert.NotEmpty(t, ch.multiQueries[0].QueryEmbedding,
		"embedder ran (query_text non-empty) — embedding must reach the multi call")
}

func TestRecall_FansOutAcrossTiersAndFusesByRRF(t *testing.T) {
	c, s, ch, _ := newCore()
	// Each tier returns one hit at rank 1. Raw scores differ (working
	// 0.9, semantic 0.7, episodic 0.5) but RRF ignores raw scores and
	// uses ranks: all three documents end up tied at 1/(60+1). Order
	// is then determined by first-seen across the tier inputs (sietch,
	// episodic, semantic in that fan-out order).
	s.vectorHits["sess"] = []core.RecallHit{{Tier: "working", Score: 0.9, ID: "w1"}}
	ch.episResp = []core.RecallHit{{Tier: "episodic", Score: 0.5, ID: "e1"}}
	ch.semResp = []core.RecallHit{{Tier: "semantic", Score: 0.7, ID: "m1"}}

	out, err := c.Recall(context.Background(), core.RecallInput{
		SessionID: "sess", UserID: "u1", Workspace: "ws1",
		QueryText: "kubernetes",
		// Post settle-default flip: pin the pre-P4 fan-out shape explicitly
		// with settle off, so the RRF-only ordering and TierCounts (no
		// "expansion" key) are what this test asserts.
		Settle: "off",
	})
	require.NoError(t, err)
	require.Len(t, out.Hits, 3)
	assert.Equal(t, "w1", out.Hits[0].ID, "sietch (working) first by fan-out order")
	assert.Equal(t, "e1", out.Hits[1].ID, "episodic next")
	assert.Equal(t, "m1", out.Hits[2].ID, "semantic last")
	// All three RRF scores equal at rank 1: 1/(60+1).
	for i, h := range out.Hits {
		assert.InDelta(t, 1.0/61.0, h.Score, 1e-12, "hit %d uses RRF score, not raw tier score", i)
	}
	assert.Equal(t, map[string]int{
		"working": 1, "episodic": 1, "keyword": 0,
		"session_vector": 0, "semantic": 1, "primitives": 0,
	}, out.TierCounts)
}

// TestRecall_RRFFavorsCrossTierAgreement pins the property that a
// document appearing in multiple tiers (even at modest ranks) beats one
// that only shows up at the top of a single tier. This is the win RRF
// gives over max-score-merge: cross-tier agreement compounds.
//
// Post-grain-aware-dedup (PR-fix): cross-tier agreement requires the
// SAME event_id across tiers, not just the same session_id. Different
// events from the same session are different documents and stay
// separate (which is correct: a session whose event A matches in
// episodic and event B matches in semantic is a one-vote-each session,
// not a two-vote document).
func TestRecall_RRFFavorsCrossTierAgreement(t *testing.T) {
	c, s, ch, _ := newCore()
	sidShared := "s-shared"
	sidSolo := "s-solo"
	sharedID := "evt-shared"
	// Working: solo event at rank 1.
	s.vectorHits["sess"] = []core.RecallHit{
		{Tier: "working", Score: 0.99, ID: "w-solo", SessionID: &sidSolo},
	}
	// Episodic: shared event at rank 1.
	ch.episResp = []core.RecallHit{
		{Tier: "episodic", Score: 0.40, ID: sharedID, SessionID: &sidShared},
	}
	// Keyword: same shared event at rank 1 too (so it's in 2 tiers).
	// Use keyword instead of semantic so the shared id refers to a
	// real event row in both tiers (semantic is mneme-grain, no
	// session_id, distinct documents).
	ch.kwResp = []core.RecallHit{
		{Tier: "keyword", Score: 0.30, ID: sharedID, SessionID: &sidShared},
	}

	out, err := c.Recall(context.Background(), core.RecallInput{
		SessionID: "sess", UserID: "u1", Workspace: "ws1",
		QueryText: "kubernetes",
	})
	require.NoError(t, err)
	require.Len(t, out.Hits, 2, "shared event deduped across episodic+keyword")
	// shared > solo because shared got two votes (1/61 + 1/61) and
	// solo got one (1/61), even though solo had the higher raw score.
	assert.Equal(t, sharedID, out.Hits[0].ID, "shared event wins by cross-tier agreement")
	assert.Equal(t, "w-solo", out.Hits[1].ID)
	assert.Greater(t, out.Hits[0].Score, out.Hits[1].Score)
}

// TestRecall_KeywordTierParticipatesInRRF pins that PR-D's keyword
// tier shows up as an independent ranked list in FuseRRF — an event
// that only matches via FTS (and isn't returned by the dense vector
// path) still surfaces in Recall output, and one that gets keyword
// agreement on top of dense agreement compounds further.
//
// Layout (event-grain after the dedup-by-grain fix):
//
//	evt-fts:   only the keyword tier matches (literal phrase that
//	           didn't embed well — proper noun, code identifier).
//	evt-dense: only the dense episodic tier matches.
//	evt-both:  matches in dense AND keyword (same event_id surfaced
//	           by both tiers — cross-tier agreement compounds RRF).
func TestRecall_KeywordTierParticipatesInRRF(t *testing.T) {
	c, s, ch, _ := newCore()
	sidFTS := "s-fts"
	sidDense := "s-dense"
	sidBoth := "s-both"
	bothID := "evt-both"

	_ = s

	ch.episResp = []core.RecallHit{
		{Tier: "episodic", Score: 0.7, ID: "evt-dense", SessionID: &sidDense},
		{Tier: "episodic", Score: 0.5, ID: bothID, SessionID: &sidBoth},
	}
	ch.kwResp = []core.RecallHit{
		{Tier: "keyword", Score: 0.9, ID: "evt-fts", SessionID: &sidFTS},
		{Tier: "keyword", Score: 0.4, ID: bothID, SessionID: &sidBoth},
	}

	out, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText: "kubernetes",
	})
	require.NoError(t, err)
	require.Len(t, out.Hits, 3, "three distinct events: fts-only, dense-only, both-tiers")
	require.Len(t, ch.multiQueries, 1, "single multi-ranking call")
	assert.Contains(t, ch.multiQueries[0].Rankings, "fts",
		"keyword (fts) tier must be requested when query_text is non-empty")

	// Build event_id -> rank map for ordering-independent assertions.
	rankByID := map[string]int{}
	for i, h := range out.Hits {
		rankByID[h.ID] = i
	}
	assert.Less(t, rankByID[bothID], rankByID["evt-fts"],
		"two-tier agreement (evt-both) outranks single-tier keyword-only")
	assert.Less(t, rankByID[bothID], rankByID["evt-dense"],
		"two-tier agreement (evt-both) outranks single-tier dense-only")
}

// TestRecall_SessionVectorTierParticipatesInRRF pins that the session-
// vector tier shows up as an independent ranked list in FuseRRF: a
// session that only matches via the pooled session embedding (and
// isn't returned by the per-event vector path) still surfaces in
// Recall output.
//
// Layout:
//
//	sid-sv-only:  matches only on episodic.sessions.l1_embedding
//	              (paraphrase-style query the per-event vector misses).
//	sid-ep-only:  matches only on the per-event dense tier.
//	sid-both:     matches in both tiers — surfaces as TWO hits because
//	              the session-vector tier produces session-grain output
//	              and the per-event tier produces event-grain output.
//	              They are not the same "document"; collapsing them
//	              erases the per-event identity, which is what dedup-
//	              by-grain prevents.
func TestRecall_SessionVectorTierParticipatesInRRF(t *testing.T) {
	c, _, ch, _ := newCore()
	sidSVOnly := "s-sv-only"
	sidEPOnly := "s-ep-only"
	sidBoth := "s-both"

	// Per-event dense tier: the per-event vector hit and the both-tier
	// session.
	ch.episResp = []core.RecallHit{
		{Tier: "episodic", Score: 0.7, ID: "e-ep", SessionID: &sidEPOnly},
		{Tier: "episodic", Score: 0.5, ID: "e-both", SessionID: &sidBoth},
	}
	// Session-vector tier: id == session_id (the unit is the session,
	// not an event). After the grain-aware dedup fix, session-grain
	// hits live in their own key namespace and do NOT collapse with
	// per-event hits whose session_id happens to match.
	ch.svResp = []core.RecallHit{
		{Tier: "session_vector", Score: 0.9, ID: sidSVOnly, SessionID: &sidSVOnly},
		{Tier: "session_vector", Score: 0.4, ID: sidBoth, SessionID: &sidBoth},
	}

	out, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText: "kubernetes",
	})
	require.NoError(t, err)
	require.Len(t, ch.multiQueries, 1, "single multi-ranking call")
	assert.Contains(t, ch.multiQueries[0].Rankings, "session_vector",
		"session-vector tier must be requested when an embedding is available")
	// Four hits total: e-ep (event), e-both (event), sidSVOnly (session),
	// sidBoth-from-sv (session). The two sidBoth surfacings are kept
	// distinct because they're different grains.
	require.Len(t, out.Hits, 4, "two grains × two sessions per tier => 4 hits, no cross-grain collapse")

	idsByTier := map[string][]string{}
	for _, h := range out.Hits {
		idsByTier[h.Tier] = append(idsByTier[h.Tier], h.ID)
	}
	assert.Contains(t, idsByTier["episodic"], "e-ep")
	assert.Contains(t, idsByTier["episodic"], "e-both")
	assert.Contains(t, idsByTier["session_vector"], sidSVOnly)
	assert.Contains(t, idsByTier["session_vector"], sidBoth,
		"session-vector hit for sidBoth must NOT be erased by an event-grain hit on the same session")
}

// TestRecall_Dedup_DoesNotCollapseEventGrainAcrossSessionVectorHits is
// the load-bearing regression test for the grain-aware dedup fix.
//
// Pre-fix bug: hitKey() returned session_id for any hit that carried
// one, so session-vector hits (whose id IS the session_id) collided in
// the same key space as event-grain episodic hits from the same session.
// Exemplar selection then picked the highest raw-tier-score per key,
// which was always the session-vector hit (raw 1.0 cosine on the pooled
// embedding) over the merged-rank-then-rescored episodic events. The
// per-event identities of episodic matches were silently erased.
//
// Setup mirrors the empirical chapterhouse-side observation on the
// vercel/next.js#93146 case: a session whose summary embedding scores
// high (session_vector tier) and per-event matches inside that same
// session that should NOT be erased by it.
func TestRecall_Dedup_DoesNotCollapseEventGrainAcrossSessionVectorHits(t *testing.T) {
	c, _, ch, _ := newCore()
	s1 := "s-shared"
	// Two distinct events in the same session, both surfaced by the
	// per-event episodic vector tier.
	ch.episResp = []core.RecallHit{
		{Tier: "episodic", Score: 0.83, ID: "event-a", SessionID: &s1},
		{Tier: "episodic", Score: 0.81, ID: "event-b", SessionID: &s1},
	}
	// Session-vector tier returns the same session as a session-grain
	// hit. Pre-fix, this would erase event-a and event-b from the output.
	ch.svResp = []core.RecallHit{
		{Tier: "session_vector", Score: 1.0, ID: s1, SessionID: &s1},
	}

	out, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText: "where did we resolve issue 93146",
	})
	require.NoError(t, err)

	ids := make(map[string]string, len(out.Hits))
	for _, h := range out.Hits {
		ids[h.ID] = h.Tier
	}
	assert.Contains(t, ids, "event-a", "event-grain hit must survive session-vector overlap")
	assert.Contains(t, ids, "event-b", "event-grain hit must survive session-vector overlap")
	assert.Equal(t, "episodic", ids["event-a"], "event-a's tier attribution stays episodic")
	assert.Equal(t, "episodic", ids["event-b"], "event-b's tier attribution stays episodic")
}

// TestRecall_Dedup_StillCollapsesWithinSameTier pins that the
// grain-aware key change does NOT regress within-tier dedup: when the
// same event_id shows up multiple times from one tier (e.g. an event
// matched by both vector and FTS paths inside chapterhouse), the
// higher-score row still wins and we don't double-emit.
//
// Note on grain semantics: two distinct events from the same session
// are NOT a duplicate — they're two events, and the post-fix dedup
// correctly keeps both (this is the whole point of the fix). What
// dedup-by-key collapses now is exact-id repeats within a tier.
func TestRecall_Dedup_StillCollapsesWithinSameTier(t *testing.T) {
	c, _, ch, _ := newCore()
	s1 := "s-1"
	// Same event surfaced twice with different scores — within-tier
	// dedup keeps only the higher-scored row.
	ch.episResp = []core.RecallHit{
		{Tier: "episodic", Score: 0.4, ID: "evt-dup", SessionID: &s1},
		{Tier: "episodic", Score: 0.9, ID: "evt-dup", SessionID: &s1},
	}

	out, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText: "kubernetes",
	})
	require.NoError(t, err)
	require.Len(t, out.Hits, 1, "two rows with the same event_id collapse to one")
	assert.Equal(t, "evt-dup", out.Hits[0].ID)
}

// TestRecall_Dedup_SessionVectorAndEpisodicDifferentSessions verifies
// the trivial case: when there's no overlap (different sessions) the
// dedup change is a no-op — both hits surface unchanged.
func TestRecall_Dedup_SessionVectorAndEpisodicDifferentSessions(t *testing.T) {
	c, _, ch, _ := newCore()
	s1 := "s-1"
	s2 := "s-2"
	ch.episResp = []core.RecallHit{
		{Tier: "episodic", Score: 0.7, ID: "event-x", SessionID: &s1},
	}
	ch.svResp = []core.RecallHit{
		{Tier: "session_vector", Score: 0.9, ID: s2, SessionID: &s2},
	}

	out, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText: "kubernetes",
	})
	require.NoError(t, err)
	require.Len(t, out.Hits, 2, "no overlap, no collision: both hits surface")
	tiers := map[string]string{}
	for _, h := range out.Hits {
		tiers[h.ID] = h.Tier
	}
	assert.Equal(t, "episodic", tiers["event-x"])
	assert.Equal(t, "session_vector", tiers[s2])
}

// TestRecall_SessionVectorTierSkippedWhenNoEmbedding: a keyword-only
// (QueryText, no embedding) recall must not request the session-vector
// ranking — without an embedding, dense cosine has nothing to score
// against. The Embedder isn't called when QueryText is empty, so
// emb stays nil; gate on len(emb) > 0 to skip.
func TestRecall_SessionVectorTierSkippedWhenNoEmbedding(t *testing.T) {
	c, _, ch, _ := newCore()
	_, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		// QueryText omitted: emb stays nil, no dense fan-out.
	})
	require.NoError(t, err)
	require.Len(t, ch.multiQueries, 1)
	assert.NotContains(t, ch.multiQueries[0].Rankings, "session_vector",
		"session_vector ranking must not be requested without an embedding")
}

// TestRecall_KeywordTierSkippedWhenNoQueryText: when the caller fires
// an embedding-only recall (QueryText == ""), the keyword path makes
// no sense and must not be requested — the multi-ranking call must
// omit "fts" from the Rankings list.
func TestRecall_KeywordTierSkippedWhenNoQueryText(t *testing.T) {
	c, _, ch, _ := newCore()
	_, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		// QueryText omitted — this is the "I have an embedding, not a
		// query string" path.
	})
	require.NoError(t, err)
	require.Len(t, ch.multiQueries, 1)
	assert.NotContains(t, ch.multiQueries[0].Rankings, "fts",
		"fts ranking must not be requested without query_text")
}

// TestRecall_TagsAnyPlumbsToEventGrainTiersOnly — H3.c structural
// filter: RecallInput.TagsAny must propagate to the multi-ranking
// request (which forwards it to the event-grain tiers server-side)
// but NOT to the semantic query (out of scope for the era-aware
// retrieval experiment).
//
// Pin the no-over-plumb behavior so a future refactor that "helpfully"
// forwards the filter to semantic is caught here.
func TestRecall_TagsAnyPlumbsToEventGrainTiersOnly(t *testing.T) {
	c, _, ch, _ := newCore()
	_, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText: "kubernetes",
		TagsAny:   []string{"era:v15"},
	})
	require.NoError(t, err)

	require.Len(t, ch.multiQueries, 1, "single multi-ranking call")
	assert.Equal(t, []string{"era:v15"}, ch.multiQueries[0].TagsAny,
		"tags_any must reach the multi-ranking request (server forwards "+
			"it to the event-grain tiers)")
	assert.ElementsMatch(t,
		[]string{"vector", "fts", "session_vector"},
		ch.multiQueries[0].Rankings,
		"all three event-grain rankings requested when emb+query_text both present")

	// Semantic query type intentionally has no TagsAny field. Pin
	// that the filter does not get smuggled in via some future field.
	require.Len(t, ch.semQueries, 1, "semantic queried once")
}

// TestRecall_TagsAnyEmptyIsNoop — empty/nil tags_any must leave the
// downstream calls unchanged (current behavior). Pins the optional-
// filter contract explicitly.
func TestRecall_TagsAnyEmptyIsNoop(t *testing.T) {
	c, _, ch, _ := newCore()
	_, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText: "kubernetes",
		// TagsAny intentionally omitted.
	})
	require.NoError(t, err)
	require.Len(t, ch.multiQueries, 1)
	assert.Empty(t, ch.multiQueries[0].TagsAny)
}

// TestRecall_PrimitivesFlagPlumbsToMultiQuery — D3 plumbing pin:
// RecallInput.Primitives must propagate to the multi-ranking request
// so the chapterhouse handler computes the 4th Hebbian-boosted
// sub-list. When the caller doesn't set the flag, the request must
// carry Primitives=false (zero value), preserving prior behavior.
func TestRecall_PrimitivesFlagPlumbsToMultiQuery(t *testing.T) {
	c, _, ch, _ := newCore()
	_, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText:  "kubernetes",
		Primitives: true,
	})
	require.NoError(t, err)
	require.Len(t, ch.multiQueries, 1, "single multi-ranking call")
	assert.True(t, ch.multiQueries[0].Primitives,
		"primitives flag must propagate from RecallInput to QueryEpisodicMulti")
}

// TestRecall_PrimitivesDefaultIsOff pins the optional-flag contract:
// without an explicit Primitives=true on RecallInput, the multi-query
// must carry the zero value so the chapterhouse handler stays on the
// legacy 3-tier path. Protects against accidental on-by-default flips.
func TestRecall_PrimitivesDefaultIsOff(t *testing.T) {
	c, _, ch, _ := newCore()
	_, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText: "kubernetes",
		// Primitives intentionally omitted.
	})
	require.NoError(t, err)
	require.Len(t, ch.multiQueries, 1)
	assert.False(t, ch.multiQueries[0].Primitives,
		"primitives must default to false on the wire")
}

// TestRecall_PrimitivesParticipatesInRRFAsSixthTier pins the RRF
// fan-in contract: when chapterhouse returns a non-nil Primitives
// sub-list, those hits feed the FuseRRF accumulator as a 6th ranked
// list (alongside working / episodic / keyword / session_vector /
// semantic). Equal-weight tier-additive RRF is the safe first cut —
// a hit that ranks #1 in primitives AND #1 in episodic is fused on
// equal RRF terms, so cross-tier agreement compounds the same way it
// already does for the existing tiers.
//
// Test shape: a single hit appears in BOTH episodic AND primitives.
// A second hit only in episodic. RRF agreement (2 lists @ rank 1 vs
// 1 list @ rank 1) must put the agreed-on hit first.
func TestRecall_PrimitivesParticipatesInRRFAsSixthTier(t *testing.T) {
	c, _, ch, _ := newCore()
	// "shared" appears in both episodic and primitives at rank 1.
	// "epi-only" appears only in episodic at rank 2.
	ch.episResp = []core.RecallHit{
		{Tier: "episodic", Score: 0.9, ID: "shared"},
		{Tier: "episodic", Score: 0.5, ID: "epi-only"},
	}
	primHits := []core.RecallHit{
		{Tier: "primitives", Score: 0.8, ID: "shared"},
	}
	ch.primResp = &primHits

	out, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText:  "kubernetes",
		Primitives: true,
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(out.Hits), 2, "both hits surface")
	assert.Equal(t, "shared", out.Hits[0].ID,
		"primitives + episodic agreement must outrank single-tier episodic")
	assert.Equal(t, "epi-only", out.Hits[1].ID,
		"single-tier hit ranks below the cross-tier agreement")
}

// TestRecall_PrimitivesEmptySubListNoOp — the chapterhouse handler
// returns a non-nil empty Primitives sub-list when the flag was on
// but no in-set boosts surfaced (seeded set has no associations among
// themselves). RRF must treat this as a tier with zero entries — no
// crash, no spurious hits, no error. Effectively a no-op overlay on
// top of the existing 5-tier fan-out.
func TestRecall_PrimitivesEmptySubListNoOp(t *testing.T) {
	c, _, ch, _ := newCore()
	ch.episResp = []core.RecallHit{{Tier: "episodic", Score: 0.9, ID: "e1"}}
	empty := []core.RecallHit{}
	ch.primResp = &empty

	out, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText:  "kubernetes",
		Primitives: true,
	})
	require.NoError(t, err)
	require.Len(t, out.Hits, 1, "only the episodic hit surfaces")
	assert.Equal(t, "e1", out.Hits[0].ID)
}

// TestRecall_PrimitivesNilSubListNoOp — when the chapterhouse handler
// drops the primitives field entirely (association lookup failure on
// the server side, OR flag was off), the response decodes as nil and
// RRF must skip it cleanly. Pins the degraded-path contract: a
// downstream associations failure must not break the main recall path.
func TestRecall_PrimitivesNilSubListNoOp(t *testing.T) {
	c, _, ch, _ := newCore()
	ch.episResp = []core.RecallHit{{Tier: "episodic", Score: 0.9, ID: "e1"}}
	// primResp left nil — the fake's QueryEpisodicMulti returns
	// EpisodicMultiResult.Primitives == nil even with the flag on.

	out, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText:  "kubernetes",
		Primitives: true,
	})
	require.NoError(t, err)
	require.Len(t, out.Hits, 1)
	assert.Equal(t, "e1", out.Hits[0].ID)
}

// ---------------------------------------------------------------------
// P4 recurrent-settle expansion (Task 5, config A).
// ---------------------------------------------------------------------

// TestRecall_SettleBogusModeRejected pins the validation contract:
// Settle must be one of "off", "expand", "channel" (or omitted, which
// applies the server default). Any other value fails with ErrValidation
// so the HTTP/MCP boundary maps it to a 400.
func TestRecall_SettleBogusModeRejected(t *testing.T) {
	c, _, _, _ := newCore()
	_, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText: "kubernetes",
		Settle:    "bogus",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrValidation,
		"an invalid settle mode must surface as a validation error (400 at the boundary)")
}

// TestRecall_SettleExpandPlumbsToMultiQuery pins that a non-empty settle
// mode flips the settle flag on the chapterhouse multi-query and forwards
// the params passthrough.
func TestRecall_SettleExpandPlumbsToMultiQuery(t *testing.T) {
	c, _, ch, _ := newCore()
	_, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText:    "kubernetes",
		Settle:       "expand",
		SettleParams: core.SettleParams{Lambda: 0.7, TopM: 25},
	})
	require.NoError(t, err)
	require.Len(t, ch.multiQueries, 1)
	assert.True(t, ch.multiQueries[0].Settle,
		"a non-empty settle mode must set Settle=true on the multi-query")
	assert.InDelta(t, 0.7, ch.multiQueries[0].SettleParams.Lambda, 1e-9,
		"settle params must propagate to the multi-query")
	assert.EqualValues(t, 25, ch.multiQueries[0].SettleParams.TopM)
}

// TestRecall_SettleUnsetDefaultsToChannel pins the on-by-default contract:
// an omitted Settle now applies the server default (c.Settle == "channel"),
// so the multi-query settle flag flips true and chapterhouse runs spreading
// activation. This replaces the pre-flip "unset → off" contract — see the
// LongMemEval settle gate in docs/benchmarks.md.
func TestRecall_SettleUnsetDefaultsToChannel(t *testing.T) {
	c, _, ch, _ := newCore()
	// New() default: c.Settle == "channel", c.ActivationWeight == 0.40.
	_, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText: "kubernetes",
		// Settle intentionally omitted — must default to channel.
	})
	require.NoError(t, err)
	require.Len(t, ch.multiQueries, 1)
	assert.True(t, ch.multiQueries[0].Settle,
		"an omitted settle must apply the server default (channel) and flip the multi-query settle flag on")
}

// TestRecall_SettleUnsetAppliesDefaultActivationWeight pins that the server
// ActivationWeight default (0.40) rides along with the default channel mode
// when the caller left the weight unset. Asserted through the channel-fusion
// output score, the only weight-observable seam: one pool hit "e1" (RRF mass,
// no activation) and one expansion hit "xact" (rrf=0, activation=1.0), both
// reranked equally. With c.RerankWeight=0.5 and wActivation=0.40, rrfWeight =
// 1-0.5-0.40 = 0.10:
//
//	e1:   0.10*1.0 + 0.5*1.0 + 0.40*0.0 = 0.60
//	xact: 0.10*0.0 + 0.5*1.0 + 0.40*1.0 = 0.90
//
// xact wins at 0.90 only because the 0.40 default weight was applied — a zero
// weight would leave xact at 0.5 (below e1) and no activation channel at all.
func TestRecall_SettleUnsetAppliesDefaultActivationWeight(t *testing.T) {
	c, _, ch, _ := newCore()
	ch.episResp = []core.RecallHit{
		{Tier: "episodic", Score: 0.8, ID: "e1", Content: "episodic content"},
	}
	ch.expResp = []core.ExpansionHit{
		{ID: "xact", Activation: 1.0, Text: "expansion with high activation"},
	}
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Equal rerank scores so activation is the only differentiator.
		_, _ = w.Write([]byte(`{"scores":[
			{"id":"e1","score":0.5},
			{"id":"xact","score":0.5}
		]}`))
	}))
	defer srv.Close()
	c.Truthsayer = truthsayer.New(srv.URL).WithHTTPClient(srv.Client())
	// c.RerankWeight stays at the New() default 0.5; c.ActivationWeight 0.40.

	out, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText: "kubernetes",
		// Settle + ActivationWeight both omitted — server defaults apply.
	})
	require.NoError(t, err)
	require.Len(t, out.Hits, 2, "both hits surface (channel default engaged)")
	assert.Equal(t, "xact", out.Hits[0].ID,
		"xact must rank first: the default activation weight (0.40) tips it above e1's rrf mass")
	assert.InDelta(t, 0.90, out.Hits[0].Score, 1e-9, "xact score: 0.10*0 + 0.5*1 + 0.40*1 = 0.90 (proves w=0.40 applied)")
	assert.InDelta(t, 0.60, out.Hits[1].Score, 1e-9, "e1 score:   0.10*1 + 0.5*1 + 0.40*0 = 0.60")
}

// TestRecall_SettleExplicitOff pins the explicit opt-out at the wire level:
// Settle == "off" must leave the multi-query settle flag false so the
// chapterhouse handler stays on the no-expansion path. This is the pre-flip
// "unset" behavior, now reachable only by asking for it explicitly.
func TestRecall_SettleExplicitOff(t *testing.T) {
	c, _, ch, _ := newCore()
	_, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText: "kubernetes",
		Settle:    "off",
	})
	require.NoError(t, err)
	require.Len(t, ch.multiQueries, 1)
	assert.False(t, ch.multiQueries[0].Settle,
		"settle=off must leave the multi-query settle flag false (pre-P4 path)")
}

// TestRecall_SettleOffByteIdenticalToPreP4 is the regression test: with
// Settle == "off", recall output (hits + TierCounts) must match the pre-P4
// pipeline exactly — even when the fake has an expansion sub-list staged
// (it must NOT surface because the settle flag is off). Guards against an
// expansion or a stray "expansion" TierCounts key leaking into an explicitly
// disabled recall. (Post-flip: "off" is the explicit opt-out; an *omitted*
// settle now defaults to channel — see TestRecall_SettleUnsetDefaultsToChannel.)
func TestRecall_SettleOffByteIdenticalToPreP4(t *testing.T) {
	c, s, ch, _ := newCore()
	s.vectorHits["sess"] = []core.RecallHit{{Tier: "working", Score: 0.9, ID: "w1"}}
	ch.episResp = []core.RecallHit{{Tier: "episodic", Score: 0.5, ID: "e1"}}
	ch.semResp = []core.RecallHit{{Tier: "semantic", Score: 0.7, ID: "m1"}}
	// Stage an expansion sub-list that must NOT surface (settle off).
	ch.expResp = []core.ExpansionHit{{ID: "x1", Activation: 0.99, Text: "expansion text"}}

	out, err := c.Recall(context.Background(), core.RecallInput{
		SessionID: "sess", UserID: "u1", Workspace: "ws1",
		QueryText: "kubernetes",
		Settle:    "off", // explicit opt-out — the pre-P4 pipeline.
	})
	require.NoError(t, err)
	require.Len(t, out.Hits, 3, "only the three RRF-tier hits surface; expansion must not leak")
	assert.Equal(t, "w1", out.Hits[0].ID)
	assert.Equal(t, "e1", out.Hits[1].ID)
	assert.Equal(t, "m1", out.Hits[2].ID)
	// TierCounts must be byte-identical to the pre-P4 map — no "expansion" key.
	assert.Equal(t, map[string]int{
		"working": 1, "episodic": 1, "keyword": 0,
		"session_vector": 0, "semantic": 1, "primitives": 0,
	}, out.TierCounts, "settle-off TierCounts must not carry an expansion key")
}

// TestRecall_SettleChannelExplicitWeightRespected pins that an explicit
// ActivationWeight in channel mode is used verbatim — the server default
// (0.40) must NOT overwrite it. Same setup as the default-weight test but
// with an explicit 0.5, and RerankWeight lowered to 0.1 so the arithmetic
// (rrfWeight = 1-0.1-0.5 = 0.4) matches TestRecall_SettleChannelActivationLiftsByRawID.
func TestRecall_SettleChannelExplicitWeightRespected(t *testing.T) {
	c, _, ch, _ := newCore()
	ch.episResp = []core.RecallHit{
		{Tier: "episodic", Score: 0.8, ID: "e1", Content: "episodic content"},
	}
	ch.expResp = []core.ExpansionHit{
		{ID: "xact", Activation: 1.0, Text: "expansion with high activation"},
	}
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scores":[
			{"id":"e1","score":0.5},
			{"id":"xact","score":0.5}
		]}`))
	}))
	defer srv.Close()
	c.Truthsayer = truthsayer.New(srv.URL).WithHTTPClient(srv.Client())
	c.RerankWeight = 0.1

	out, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText:        "kubernetes",
		Settle:           "channel",
		ActivationWeight: 0.5, // explicit — must not be overwritten by the 0.40 default.
	})
	require.NoError(t, err)
	require.Len(t, out.Hits, 2)
	assert.Equal(t, "xact", out.Hits[0].ID)
	assert.InDelta(t, 0.60, out.Hits[0].Score, 1e-9,
		"xact score: 0.4*0 + 0.1*1 + 0.5*1 = 0.60 (explicit weight 0.5 applied, not the 0.40 default)")
	assert.InDelta(t, 0.50, out.Hits[1].Score, 1e-9, "e1 score: 0.4*1 + 0.1*1 + 0.5*0 = 0.50")
}

// TestRecall_SettleServerDefaultOffDisables pins the kill-switch: with the
// server default set to "off" (GHOLA_SETTLE=off), an omitted request Settle
// must NOT engage settle — the multi-query flag stays false. This is the
// production rollback path for the on-by-default flip.
func TestRecall_SettleServerDefaultOffDisables(t *testing.T) {
	c, _, ch, _ := newCore()
	c.Settle = "off" // simulate GHOLA_SETTLE=off wired in main.go.

	_, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText: "kubernetes",
		// Settle omitted — must follow the server default (off).
	})
	require.NoError(t, err)
	require.Len(t, ch.multiQueries, 1)
	assert.False(t, ch.multiQueries[0].Settle,
		"with server default off, an unset request must not engage settle (kill-switch)")
}

// TestRecall_SettleExpandStrongRerankHitEntersTopK pins config A's whole
// premise: a text-bearing expansion candidate with ZERO RRF mass can be
// pulled into the final top-K purely by cross-encoder score, and a weak
// expansion candidate cannot. Two expansion hits are staged; the stub
// reranker scores one high (above an RRF hit) and one low. The final
// order must place the strong expansion hit at the top and exclude the
// weak one under a tight limit.
func TestRecall_SettleExpandStrongRerankHitEntersTopK(t *testing.T) {
	c, _, ch, _ := newCore()
	// One real RRF hit from episodic.
	ch.episResp = []core.RecallHit{
		{Tier: "episodic", Score: 0.5, ID: "e1", Content: "episodic content"},
	}
	// Two expansion candidates, both text-bearing (rrf=0).
	ch.expResp = []core.ExpansionHit{
		{ID: "x-strong", Activation: 0.9, Text: "strong expansion text"},
		{ID: "x-weak", Activation: 0.1, Text: "weak expansion text"},
	}

	// Stub reranker: strong expansion hit scores highest, the RRF hit
	// middling, the weak expansion hit near zero.
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scores":[
			{"id":"x-strong","score":0.95},
			{"id":"e1","score":0.50},
			{"id":"x-weak","score":0.01}
		]}`))
	}))
	defer srv.Close()
	c.Truthsayer = truthsayer.New(srv.URL).WithHTTPClient(srv.Client())
	c.RerankWeight = 1.0 // pure rerank exposes the rrf=0 expansion path cleanly

	out, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText: "kubernetes",
		Limit:     2, // top-2 so the weak expansion hit is squeezed out
		Settle:    "expand",
	})
	require.NoError(t, err)
	require.Len(t, out.Hits, 2, "limited to top-2")
	assert.Equal(t, "x-strong", out.Hits[0].ID,
		"a strong-rerank expansion hit lands top despite rrf=0")
	assert.Equal(t, "expansion", out.Hits[0].Tier,
		"expansion provenance is carried on the Tier field")
	assert.Equal(t, "e1", out.Hits[1].ID, "the RRF hit takes the second slot")
	for _, h := range out.Hits {
		assert.NotEqual(t, "x-weak", h.ID,
			"a weak-rerank expansion hit must not enter the top-K")
	}
}

// TestRecall_SettleExpandDropsEmptyTextHits pins the Task 4 finding
// handling: expansion entries with empty text are dropped from the pool
// (they can neither be reranked nor consumed). Only the text-bearing
// expansion hit reaches the reranker as a candidate.
func TestRecall_SettleExpandDropsEmptyTextHits(t *testing.T) {
	c, _, ch, _ := newCore()
	ch.episResp = []core.RecallHit{
		{Tier: "episodic", Score: 0.5, ID: "e1", Content: "episodic content"},
	}
	ch.expResp = []core.ExpansionHit{
		{ID: "x-with-text", Activation: 0.9, Text: "has text"},
		{ID: "x-no-text", Activation: 0.8, Text: ""}, // dropped
	}

	var gotBody map[string]any
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scores":[]}`))
	}))
	defer srv.Close()
	c.Truthsayer = truthsayer.New(srv.URL).WithHTTPClient(srv.Client())

	out, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText: "kubernetes",
		Limit:     10,
		Settle:    "expand",
	})
	require.NoError(t, err)

	// The text-less expansion hit must never appear in the output.
	for _, h := range out.Hits {
		assert.NotEqual(t, "x-no-text", h.ID, "empty-text expansion hit must be dropped")
	}
	// Reranker candidates: e1 + x-with-text only (2), NOT x-no-text.
	cands, ok := gotBody["candidates"].([]any)
	require.True(t, ok)
	ids := map[string]bool{}
	for _, cv := range cands {
		ids[cv.(map[string]any)["id"].(string)] = true
	}
	assert.True(t, ids["x-with-text"], "text-bearing expansion hit is a rerank candidate")
	assert.True(t, ids["e1"], "the RRF hit is a rerank candidate")
	assert.False(t, ids["x-no-text"], "empty-text expansion hit is not sent to the reranker")
}

// TestRecall_SettleExpandDedupsAgainstPool pins the dedup contract: an
// expansion hit whose id already sits in the fused RRF pool must not be
// duplicated. The pool entry (with its real RRF mass) is kept; the
// expansion overlay only contributes activation (for Task 6). Here the
// same event_id appears both as an episodic RRF hit and as an expansion
// hit — the output must contain exactly one entry for it, and that entry
// must keep its episodic provenance (not be relabeled "expansion").
func TestRecall_SettleExpandDedupsAgainstPool(t *testing.T) {
	c, _, ch, _ := newCore()
	shared := "shared-evt"
	ch.episResp = []core.RecallHit{
		{Tier: "episodic", Score: 0.9, ID: shared, Content: "episodic content"},
	}
	ch.expResp = []core.ExpansionHit{
		{ID: shared, Activation: 0.9, Text: "expansion duplicate text"},
	}

	// No reranker: RRF-only path keeps this test focused on dedup.
	out, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText: "kubernetes",
		Limit:     10,
		Settle:    "expand",
	})
	require.NoError(t, err)

	n := 0
	for _, h := range out.Hits {
		if h.ID == shared {
			n++
			assert.Equal(t, "episodic", h.Tier,
				"the pooled RRF entry wins dedup; provenance stays episodic")
		}
	}
	assert.Equal(t, 1, n, "an expansion hit already in the pool must not duplicate")
}

// TestRecall_CrossEncoderRerankReorders pins the Stage 2/3 happy path:
// a stub truthsayer that scores an RRF-lower candidate higher than the
// RRF-top candidate flips the output ranking. Drives through the real
// truthsayer.Client + httptest.Server so the wire shape is exercised
// end-to-end.
//
// Post-grain-aware-dedup: sietch (working) and chapterhouse (episodic)
// produce event-grain hits keyed by event_id. To exercise cross-tier
// agreement on the same document we use the same event_id across the
// two tiers; otherwise the two events are separate documents.
func TestRecall_CrossEncoderRerankReorders(t *testing.T) {
	c, s, ch, _ := newCore()
	sidTop := "s-rrf-top"
	sidLow := "s-rrf-low"
	topID := "evt-top"
	// Two-tier agreement on topID: working tier rank-1, episodic
	// rank-1 -> highest RRF.
	s.vectorHits["sess"] = []core.RecallHit{
		{Tier: "working", Score: 0.9, ID: topID, SessionID: &sidTop, Content: "alpha"},
	}
	ch.episResp = []core.RecallHit{
		{Tier: "episodic", Score: 0.8, ID: topID, SessionID: &sidTop, Content: "alpha-ep"},
		{Tier: "episodic", Score: 0.5, ID: "evt-low", SessionID: &sidLow, Content: "beta-ep"},
	}

	// Stub truthsayer flips the order: evt-low scores higher than the
	// exemplar event for topID. (Recall picks the working-tier hit as
	// the exemplar for topID because raw 0.9 > 0.8.)
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scores":[
			{"id":"evt-low","score":0.95},
			{"id":"evt-top","score":0.10}
		]}`))
	}))
	defer srv.Close()
	c.Truthsayer = truthsayer.New(srv.URL).WithHTTPClient(srv.Client())
	c.RerankWeight = 1.0 // pure rerank — exposes the flip cleanly

	out, err := c.Recall(context.Background(), core.RecallInput{
		SessionID: "sess", UserID: "u1", Workspace: "ws1",
		QueryText: "kubernetes", Limit: 5,
		// Settle off: this test isolates rerank behavior. With the server
		// default (channel@0.40) the RerankWeight=1.0 here would push
		// rerank_weight+activation_weight to 1.40 and 400 at the boundary.
		Settle: "off",
	})
	require.NoError(t, err)
	require.Len(t, out.Hits, 2)
	// sidLow surfaces first because the stub's rerank score swamped RRF.
	assert.Equal(t, sidLow, *out.Hits[0].SessionID, "rerank flipped ranking")
	assert.Equal(t, sidTop, *out.Hits[1].SessionID)
}

// TestRecall_RerankFailureFallsBackToRRF verifies the graceful path:
// a deliberately-broken truthsayer URL must not error the Recall call;
// it logs a warning and returns the RRF-only ordering.
func TestRecall_RerankFailureFallsBackToRRF(t *testing.T) {
	c, s, ch, _ := newCore()
	sidTop := "s-rrf-top"
	sidLow := "s-rrf-low"
	s.vectorHits["sess"] = []core.RecallHit{
		{Tier: "working", Score: 0.9, ID: "w-top", SessionID: &sidTop, Content: "alpha"},
	}
	ch.episResp = []core.RecallHit{
		{Tier: "episodic", Score: 0.5, ID: "e-low", SessionID: &sidLow, Content: "beta-ep"},
	}

	// Broken truthsayer: stand up a server then close it so the URL
	// resolves but the listener is gone — Rerank fails with connection
	// refused. tight timeout keeps the test fast.
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {}))
	srv.Close()
	c.Truthsayer = truthsayer.New(srv.URL)
	c.RerankTimeout = 200 * time.Millisecond
	c.RerankWeight = 1.0

	out, err := c.Recall(context.Background(), core.RecallInput{
		SessionID: "sess", UserID: "u1", Workspace: "ws1",
		QueryText: "kubernetes", Limit: 5,
		// Settle off: isolates the rerank-failure fallback. RerankWeight=1.0
		// would collide with the channel default weight (sum > 1) otherwise.
		Settle: "off",
	})
	require.NoError(t, err, "recall must not fail when rerank is unreachable")
	require.Len(t, out.Hits, 2)
	// sidTop still wins (RRF order preserved despite weight=1.0 because
	// rerank failed and we returned hits unchanged).
	assert.Equal(t, sidTop, *out.Hits[0].SessionID, "RRF order preserved on rerank failure")
	assert.Equal(t, sidLow, *out.Hits[1].SessionID)
}

// TestRecall_RerankCandidateBodyPinsWireShape verifies the Recall call
// sends the expected JSON to truthsayer. Two cases:
//
//  1. SessionChunkText present (chapterhouse populated l1_chunk_text):
//     reranker sees the full role-prefixed session text, NOT the
//     single matching event's content. This is the load-bearing path
//     for bench R@5 — feeding single events drops the cross-encoder
//     to ~50% R@5 vs. ~58% with full session text.
//
//  2. SessionChunkText empty (open session / pre-migration / mid-tick):
//     reranker falls back to the exemplar event's Content. Lower
//     quality but recall still works.
func TestRecall_RerankCandidateBodyPinsWireShape(t *testing.T) {
	c, s, ch, _ := newCore()
	sid := "s-1"
	evtID := "evt-1"
	fullSession := "user: hello\nassistant: hi! the meeting is at 3pm\nuser: thanks"
	// Same event_id in both tiers — exemplar selection picks the
	// higher raw-score hit (working, 0.9 > episodic 0.5) for output
	// attribution. The episodic hit's SessionChunkText is collected
	// for the whole event/session key and surfaced on the exemplar.
	s.vectorHits["sess"] = []core.RecallHit{
		{Tier: "working", Score: 0.9, ID: evtID, SessionID: &sid, Content: "the meeting is at 3pm"},
	}
	ch.episResp = []core.RecallHit{
		// Episodic hit carries SessionChunkText — chapterhouse JOINed
		// episodic.sessions.l1_chunk_text into the response.
		{Tier: "episodic", Score: 0.5, ID: evtID, SessionID: &sid, Content: "the meeting is at 3pm episodic", SessionChunkText: fullSession},
	}

	var gotBody map[string]any
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scores":[]}`))
	}))
	defer srv.Close()
	c.Truthsayer = truthsayer.New(srv.URL).WithHTTPClient(srv.Client())

	_, err := c.Recall(context.Background(), core.RecallInput{
		SessionID: "sess", UserID: "u1", Workspace: "ws1",
		QueryText: "what time is the meeting", Limit: 5,
	})
	require.NoError(t, err)
	assert.Equal(t, "what time is the meeting", gotBody["query"])
	cands, ok := gotBody["candidates"].([]any)
	require.True(t, ok)
	require.Len(t, cands, 1, "same event in two tiers deduped to one rerank candidate")
	first := cands[0].(map[string]any)
	// Exemplar id is the event_id (same across tiers); the working-tier
	// row wins exemplar selection by raw score (0.9 > 0.5).
	assert.Equal(t, evtID, first["id"])
	// Rerank text comes from SessionChunkText, not Content. The
	// SessionChunkText travels with whichever hit chapterhouse
	// returned it on (the episodic hit here); Recall surfaces it for
	// the whole event-grain key.
	assert.Equal(t, fullSession, first["text"])
}

// TestRecall_RerankFallsBackToContentWhenSessionChunkEmpty pins the
// open-session / pre-migration path: when SessionChunkText is "" on
// every hit for a session, rerank input is the exemplar event's
// Content (current PR-C.1 behavior preserved).
func TestRecall_RerankFallsBackToContentWhenSessionChunkEmpty(t *testing.T) {
	c, s, ch, _ := newCore()
	sid := "s-1"
	evtID := "evt-1"
	s.vectorHits["sess"] = []core.RecallHit{
		{Tier: "working", Score: 0.9, ID: evtID, SessionID: &sid, Content: "the meeting is at 3pm"},
	}
	ch.episResp = []core.RecallHit{
		{Tier: "episodic", Score: 0.5, ID: evtID, SessionID: &sid, Content: "the meeting is at 3pm episodic"},
	}

	var gotBody map[string]any
	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"scores":[]}`))
	}))
	defer srv.Close()
	c.Truthsayer = truthsayer.New(srv.URL).WithHTTPClient(srv.Client())

	_, err := c.Recall(context.Background(), core.RecallInput{
		SessionID: "sess", UserID: "u1", Workspace: "ws1",
		QueryText: "what time is the meeting", Limit: 5,
	})
	require.NoError(t, err)
	cands, _ := gotBody["candidates"].([]any)
	require.Len(t, cands, 1)
	first := cands[0].(map[string]any)
	assert.Equal(t, evtID, first["id"])
	assert.Equal(t, "the meeting is at 3pm", first["text"], "fallback to exemplar Content")
}

// TestRecall_CwdDerivesWorkspace: an MCP agent rarely knows the
// workspace UUID, but it always knows cwd. When Workspace is empty and
// Cwd is set, Recall must derive the workspace via WorkspaceForCwd —
// the same mapping Record uses — and scope the chapterhouse tiers to it.
func TestRecall_CwdDerivesWorkspace(t *testing.T) {
	c, _, ch, _ := newCore()
	cwd := "/tmp/proj"

	_, err := c.Recall(context.Background(), core.RecallInput{
		UserID:    "u1",
		Cwd:       strPtr(cwd),
		QueryText: "kubernetes",
	})
	require.NoError(t, err)
	require.Len(t, ch.multiQueries, 1)
	assert.Equal(t, core.WorkspaceForCwd(cwd).String(), ch.multiQueries[0].WorkspaceID,
		"workspace derived from cwd reaches the chapterhouse query")
}

// TestRecall_NoWorkspaceNoCwd_Errors: neither an explicit workspace nor
// a cwd to derive one from -> validation error so the boundary returns
// 400 instead of an unscoped recall.
func TestRecall_NoWorkspaceNoCwd_Errors(t *testing.T) {
	c, _, _, _ := newCore()
	_, err := c.Recall(context.Background(), core.RecallInput{
		UserID:    "u1",
		QueryText: "kubernetes",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrValidation)
}

// TestRecall_CwdPointerToEmptyDoesNotDerive: a non-nil pointer to ""
// must NOT derive WorkspaceForCwd("") — that would collapse every
// empty-cwd recall to a single bogus workspace. Mirrors Record's guard.
func TestRecall_CwdPointerToEmptyDoesNotDerive(t *testing.T) {
	c, _, _, _ := newCore()
	empty := ""
	_, err := c.Recall(context.Background(), core.RecallInput{
		UserID:    "u1",
		Cwd:       &empty,
		QueryText: "kubernetes",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrValidation)
}

// TestRecall_ResolvesOpenSessionForSietchTier: the working tier needs a
// session id, but the MCP agent rarely has one. When IncludeSietch is
// requested without a session_id, Recall must scope it to the most-
// recent open session in the derived workspace — the same session
// Record would append to — and drive SearchVector/SearchFTS with it.
func TestRecall_ResolvesOpenSessionForSietchTier(t *testing.T) {
	c, s, _, _ := newCore()
	cwd := "/tmp/proj"
	ws := core.WorkspaceForCwd(cwd).String()
	s.sessions = []core.Session{{
		ID:          "sess-open",
		UserID:      "u1",
		WorkspaceID: ws,
		StartedAt:   time.Unix(1_700_000_000, 0).UTC(),
		// EndedAt nil -> still open
	}}

	_, err := c.Recall(context.Background(), core.RecallInput{
		UserID:        "u1",
		Cwd:           strPtr(cwd),
		QueryText:     "kubernetes",
		IncludeSietch: true,
	})
	require.NoError(t, err)
	assert.Contains(t, s.searchVectorSessions, "sess-open",
		"working-tier vector search scoped to the resolved open session")
	assert.Contains(t, s.searchFTSSessions, "sess-open",
		"working-tier FTS search scoped to the resolved open session")
}

// TestRecall_EmbedFailure_DegradesToLexical: when the embedder is down
// the lexical tiers (FTS) need no embedding, so recall must degrade to
// FTS-only instead of hard-failing. The multi call still fires but with
// an empty QueryEmbedding and rankings limited to ["vector","fts"]
// (session_vector needs an embedding and is dropped).
func TestRecall_EmbedFailure_DegradesToLexical(t *testing.T) {
	c, _, ch, emb := newCore()
	emb.err = errors.New("guild down")
	ch.kwResp = []core.RecallHit{{Tier: "keyword", Score: 0.5, ID: "k1"}}

	out, err := c.Recall(context.Background(), core.RecallInput{
		UserID:    "u1",
		Workspace: "ws1",
		QueryText: "kubernetes",
	})
	require.NoError(t, err, "embedder downtime must not fail the whole recall")
	assert.Contains(t, out.Degraded, "embed")
	require.Len(t, ch.multiQueries, 1)
	assert.Empty(t, ch.multiQueries[0].QueryEmbedding,
		"multi call must run with no embedding when embed failed")
	assert.ElementsMatch(t, []string{"vector", "fts"}, ch.multiQueries[0].Rankings,
		"session_vector ranking dropped without an embedding")
}

// TestRecall_TierFailure_Degrades: one tier failing must not sink the
// recall. The episodic multi call errors, the semantic tier succeeds —
// recall returns the semantic hits and reports "episodic" as degraded.
func TestRecall_TierFailure_Degrades(t *testing.T) {
	c, _, ch, _ := newCore()
	ch.multiErr = errors.New("episodic 503")
	ch.semResp = []core.RecallHit{{Tier: "semantic", Score: 0.7, ID: "m1"}}

	out, err := c.Recall(context.Background(), core.RecallInput{
		UserID:    "u1",
		Workspace: "ws1",
		QueryText: "kubernetes",
	})
	require.NoError(t, err, "one tier failing must not fail the recall")
	require.NotEmpty(t, out.Hits, "surviving semantic hits must come through")
	assert.Equal(t, "m1", out.Hits[0].ID)
	assert.Equal(t, []string{"episodic"}, out.Degraded)
}

// TestRecall_AllTiersFail_Errors: if every attempted tier fails there
// are no surviving hits, so recall must surface an error that names the
// all-fail condition and wraps an underlying tier error.
func TestRecall_AllTiersFail_Errors(t *testing.T) {
	c, _, ch, _ := newCore()
	ch.multiErr = errors.New("episodic 503")
	ch.semErr = errors.New("semantic 503")
	// No sietch session — only the two chapterhouse tiers are attempted.

	_, err := c.Recall(context.Background(), core.RecallInput{
		UserID:    "u1",
		Workspace: "ws1",
		QueryText: "kubernetes",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "all", "error must name the all-fail condition")
	assert.ErrorContains(t, err, "503", "error must wrap an underlying tier error")
}

// TestRecall_TierTimeout: a tier that exceeds TierTimeout is dropped
// and reported degraded — it must not stall or fail the recall. The
// semantic tier blocks until its context is cancelled; the episodic
// tier returns instantly. Recall must come back well under a second
// with episodic hits and "semantic" degraded.
func TestRecall_TierTimeout(t *testing.T) {
	c, _, ch, _ := newCore()
	c.TierTimeout = 10 * time.Millisecond
	ch.episResp = []core.RecallHit{{Tier: "episodic", Score: 0.7, ID: "e1"}}
	ch.semBlockUntilCtxDone = true

	start := time.Now()
	out, err := c.Recall(context.Background(), core.RecallInput{
		UserID:    "u1",
		Workspace: "ws1",
		QueryText: "kubernetes",
	})
	elapsed := time.Since(start)
	require.NoError(t, err, "a slow tier must not fail the recall")
	assert.Less(t, elapsed, time.Second, "recall must not block on the slow tier")
	assert.Contains(t, out.Degraded, "semantic")
	ids := make([]string, len(out.Hits))
	for i, h := range out.Hits {
		ids[i] = h.ID
	}
	assert.Contains(t, ids, "e1", "episodic hits must still surface")
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

func TestExpandSessionWorkspace_Happy(t *testing.T) {
	c, _, _, _ := newCore()
	// fakeChapterhouse's AddSessionWorkspace stub returns (true, nil)
	// by default — verify ghola passes the inputs through.
	added, err := c.ExpandSessionWorkspace(context.Background(), core.AddSessionWorkspaceInput{
		UserID:      "u1",
		SessionID:   "sess-1",
		WorkspaceID: "11111111-2222-3333-4444-555555555555",
	})
	require.NoError(t, err)
	assert.True(t, added)
}

func TestExpandSessionWorkspace_RejectsMissingFields(t *testing.T) {
	c, _, _, _ := newCore()
	_, err := c.ExpandSessionWorkspace(context.Background(), core.AddSessionWorkspaceInput{
		UserID:    "u1",
		SessionID: "sess-1",
		// WorkspaceID empty
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "workspace_id")
}

// ---------------------------------------------------------------------
// ConsolidateWorkspace — manual trigger for chapterhouse's episodic->
// semantic consolidation batch. Distinct from Consolidate above
// (sietch->episodic session flush, Pipeline A).
// ---------------------------------------------------------------------

func TestConsolidateWorkspace_ExplicitWorkspacePassesThrough(t *testing.T) {
	c, _, ch, _ := newCore()
	ws := "11111111-2222-3333-4444-555555555555"
	err := c.ConsolidateWorkspace(context.Background(), core.ConsolidateWorkspaceInput{
		Workspace: ws,
	})
	require.NoError(t, err)
	require.Len(t, ch.consolidateWorkspaces, 1)
	assert.Equal(t, ws, ch.consolidateWorkspaces[0])
}

// TestConsolidateWorkspace_DerivesWorkspaceFromCwd mirrors Recall's cwd
// ergonomics: an MCP agent rarely knows the workspace UUID but always
// knows cwd, so ConsolidateWorkspace derives it the same way Record and
// Recall do.
func TestConsolidateWorkspace_DerivesWorkspaceFromCwd(t *testing.T) {
	c, _, ch, _ := newCore()
	cwd := "/home/loganb/ghola"
	err := c.ConsolidateWorkspace(context.Background(), core.ConsolidateWorkspaceInput{
		Cwd: strPtr(cwd),
	})
	require.NoError(t, err)
	require.Len(t, ch.consolidateWorkspaces, 1)
	assert.Equal(t, core.WorkspaceForCwd(cwd).String(), ch.consolidateWorkspaces[0])
}

func TestConsolidateWorkspace_ExplicitWorkspaceWinsOverCwd(t *testing.T) {
	c, _, ch, _ := newCore()
	ws := "11111111-2222-3333-4444-555555555555"
	err := c.ConsolidateWorkspace(context.Background(), core.ConsolidateWorkspaceInput{
		Workspace: ws,
		Cwd:       strPtr("/some/other/project"),
	})
	require.NoError(t, err)
	require.Len(t, ch.consolidateWorkspaces, 1)
	assert.Equal(t, ws, ch.consolidateWorkspaces[0])
}

func TestConsolidateWorkspace_MissingWorkspaceAndCwd_ReturnsError(t *testing.T) {
	c, _, ch, _ := newCore()
	err := c.ConsolidateWorkspace(context.Background(), core.ConsolidateWorkspaceInput{})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrValidation)
	assert.Empty(t, ch.consolidateWorkspaces, "chapterhouse must not be called on a validation failure")
}

func TestConsolidateWorkspace_EmptyCwdDoesNotCollapseToBogusWorkspace(t *testing.T) {
	// Mirrors the Recall guard: a pointer-to-empty-string Cwd must not
	// derive WorkspaceForCwd("") — it must be treated as absent.
	c, _, ch, _ := newCore()
	err := c.ConsolidateWorkspace(context.Background(), core.ConsolidateWorkspaceInput{
		Cwd: strPtr(""),
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrValidation)
	assert.Empty(t, ch.consolidateWorkspaces)
}

func TestConsolidateWorkspace_InvalidWorkspaceUUID_ReturnsValidationError(t *testing.T) {
	c, _, ch, _ := newCore()
	err := c.ConsolidateWorkspace(context.Background(), core.ConsolidateWorkspaceInput{
		Workspace: "not-a-uuid",
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrValidation)
	assert.Empty(t, ch.consolidateWorkspaces, "chapterhouse must not be called with an invalid workspace")
}

func TestConsolidateWorkspace_PropagatesChapterhouseError(t *testing.T) {
	c, _, ch, _ := newCore()
	ch.consolidateErr = errors.New("mentat cluster (loud-fail): connection refused")
	err := c.ConsolidateWorkspace(context.Background(), core.ConsolidateWorkspaceInput{
		Workspace: "11111111-2222-3333-4444-555555555555",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection refused")
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
}

// Sanity: embedder failures surface.
// TestBackfillEmbeddings_FillsAll: the backfill pass drains every
// event that needs an embedding, calling SetEmbedding for each.
func TestBackfillEmbeddings_FillsAll(t *testing.T) {
	c, s, _, _ := newCore()
	s.needEmbedding["sess"] = []core.Event{
		{ID: "e1", SessionID: "sess", UserID: "u1", Text: strPtr("a")},
		{ID: "e2", SessionID: "sess", UserID: "u1", Text: strPtr("b")},
	}

	n, err := c.BackfillEmbeddings(context.Background(), "sess")
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	require.Len(t, s.setEmbeddings, 2)
	assert.Equal(t, "e1", s.setEmbeddings[0].EventID)
	assert.Equal(t, "e2", s.setEmbeddings[1].EventID)
}

// TestBackfillEmbeddings_EmbedderStillDown: if the embedder errors
// mid-pass nothing is written; the events stay in the backlog for the
// next tick.
func TestBackfillEmbeddings_EmbedderStillDown(t *testing.T) {
	c, s, _, emb := newCore()
	emb.err = errors.New("guild still down")
	s.needEmbedding["sess"] = []core.Event{
		{ID: "e1", SessionID: "sess", UserID: "u1", Text: strPtr("a")},
	}

	n, err := c.BackfillEmbeddings(context.Background(), "sess")
	require.Error(t, err)
	assert.Equal(t, 0, n)
	assert.Empty(t, s.setEmbeddings, "no embedding written when the embedder is down")
}

// TestRecord_EmbedderDown_StoresWithoutEmbedding: a record must never
// be lost because the embedder is unreachable. Record degrades to a
// no-embedding write; BackfillEmbeddings fills it in on recovery.
func TestRecord_EmbedderDown_StoresWithoutEmbedding(t *testing.T) {
	c, s, _, emb := newCore()
	emb.err = errors.New("guild down")

	ev, err := c.Record(context.Background(), core.RecordInput{
		SessionID: "sess", UserID: "u1",
		Event: core.Event{Type: "user", Text: strPtr("needs embedding")},
	})
	require.NoError(t, err, "embedder downtime must not fail the write")
	require.Len(t, s.events, 1)
	assert.Empty(t, s.events[0].Embedding, "event lands without an embedding")
	assert.Equal(t, ev.ID, s.current["sess"], "current pointer still advances")
}

// ---------------------------------------------------------------------
// GCSession — sietch file garbage collection.
// ---------------------------------------------------------------------

// markEndedRow seeds a fake session row whose ended_at is `ago` before
// the fixed test clock. Returns the session id.
func markEndedRow(c *core.Core, s *fakeSietch, ago time.Duration) string {
	id := "gc-sess"
	ended := c.Now().Add(-ago)
	s.sessionRows[id] = core.Session{ID: id, UserID: "u1", EndedAt: &ended}
	return id
}

func TestGCSession_RemovesDrainedEndedSession(t *testing.T) {
	c, s, _, _ := newCore()
	id := markEndedRow(c, s, 8*24*time.Hour) // ended 8 days ago, past 7d retention
	// No pending events, nothing awaiting embedding: maps are empty.

	removed, err := c.GCSession(context.Background(), id)
	require.NoError(t, err)
	assert.True(t, removed, "ended + drained + past retention -> remove")
	assert.Equal(t, []string{id}, s.removeSessions)
}

func TestGCSession_KeepsOpenSession(t *testing.T) {
	c, s, _, _ := newCore()
	// EndedAt nil: the session is still open.
	s.sessionRows["open"] = core.Session{ID: "open", UserID: "u1"}

	removed, err := c.GCSession(context.Background(), "open")
	require.NoError(t, err)
	assert.False(t, removed)
	assert.Empty(t, s.removeSessions)
}

func TestGCSession_KeepsRecentlyEnded(t *testing.T) {
	c, s, _, _ := newCore()
	id := markEndedRow(c, s, 24*time.Hour) // ended 1 day ago, inside 7d retention

	removed, err := c.GCSession(context.Background(), id)
	require.NoError(t, err)
	assert.False(t, removed, "still inside the retention window")
	assert.Empty(t, s.removeSessions)
}

func TestGCSession_KeepsUnconsolidated(t *testing.T) {
	c, s, _, _ := newCore()
	id := markEndedRow(c, s, 8*24*time.Hour)
	// A pending event past the watermark: not yet shipped to chapterhouse.
	s.pending[id] = []core.Event{{ID: "e1", SessionID: id, UserID: "u1"}}

	removed, err := c.GCSession(context.Background(), id)
	require.NoError(t, err)
	assert.False(t, removed, "unconsolidated events must not be GC'd")
	assert.Empty(t, s.removeSessions)
}

func TestGCSession_KeepsAwaitingBackfill(t *testing.T) {
	c, s, _, _ := newCore()
	id := markEndedRow(c, s, 8*24*time.Hour)
	// Belt-and-suspenders: an event that still needs an embedding.
	s.needEmbedding[id] = []core.Event{{ID: "e1", SessionID: id, UserID: "u1"}}

	removed, err := c.GCSession(context.Background(), id)
	require.NoError(t, err)
	assert.False(t, removed, "events awaiting backfill must not be GC'd")
	assert.Empty(t, s.removeSessions)
}

func TestGCSession_DisabledWhenRetentionZero(t *testing.T) {
	c, s, _, _ := newCore()
	c.SietchRetention = 0 // GC disabled
	id := markEndedRow(c, s, 8*24*time.Hour)

	removed, err := c.GCSession(context.Background(), id)
	require.NoError(t, err)
	assert.False(t, removed)
	// Retention-zero short-circuits before any store call, so nothing
	// is removed regardless of the session's eligibility.
	assert.Empty(t, s.removeSessions)
}

// TestGCSession_RemovesOrphanFile: a sietch file with no session row
// (recreated by conn()'s create-on-open after GC, e.g. a recall for a
// GC'd session id) surfaces as core.ErrSessionNotFound from GetSession.
// GCSession must treat it as an orphan — remove the file and report
// (true, nil) — not propagate the error and re-log it every tick.
func TestGCSession_RemovesOrphanFile(t *testing.T) {
	c, s, _, _ := newCore()
	id := "orphan-sess"
	s.getSessionErr[id] = core.ErrSessionNotFound

	removed, err := c.GCSession(context.Background(), id)
	require.NoError(t, err)
	assert.True(t, removed, "schema-only orphan -> remove")
	assert.Equal(t, []string{id}, s.removeSessions)
}

// ---------------------------------------------------------------------------
// Task 6 — three-way score fusion (config B / "channel" mode)
// ---------------------------------------------------------------------------

// TestRecall_SettleChannelZeroActivationWeightUsesDefault pins the post-flip
// contract: channel mode with ActivationWeight == 0 (unset) is no longer a
// rejection — the zero now means "use the server default", so c.ActivationWeight
// (0.40) is applied and the recall succeeds with settle engaged. (Pre-flip, a
// zero weight in channel mode was a hard 400; the on-by-default flip repurposes
// the zero as the "defer to server default" sentinel. An explicit bad weight
// that pushes the sum over 1 is still rejected — see
// TestRecall_SettleChannelWeightSumExceedsOneRejected.)
func TestRecall_SettleChannelZeroActivationWeightUsesDefault(t *testing.T) {
	c, _, ch, _ := newCore()
	// c.RerankWeight 0.5 + default ActivationWeight 0.40 = 0.90 <= 1 → valid.
	_, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText:        "kubernetes",
		Settle:           "channel",
		ActivationWeight: 0, // zero → apply the server default (0.40)
	})
	require.NoError(t, err,
		"channel mode with ActivationWeight=0 must default to the server weight, not reject")
	require.Len(t, ch.multiQueries, 1)
	assert.True(t, ch.multiQueries[0].Settle,
		"channel mode engages settle even when the weight defaults")
}

// TestRecall_SettleChannelWeightSumExceedsOneRejected pins the guard:
// c.RerankWeight + ActivationWeight > 1 must surface as ErrValidation.
// Default c.RerankWeight is 0.5 so ActivationWeight=0.6 pushes the sum
// to 1.1, which would yield a negative rrfWeight in FuseScores.
func TestRecall_SettleChannelWeightSumExceedsOneRejected(t *testing.T) {
	c, _, _, _ := newCore()
	// c.RerankWeight defaults to 0.5; 0.5 + 0.6 = 1.1 > 1.
	_, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText:        "kubernetes",
		Settle:           "channel",
		ActivationWeight: 0.6,
	})
	require.Error(t, err)
	assert.ErrorIs(t, err, core.ErrValidation,
		"rerank_weight+activation_weight > 1 must surface as a validation error (400)")
}

// TestRecall_SettleParamsValidation pins the settle_params boundary
// contract: when settle is on ("expand"/"channel"), non-zero tuning knobs
// are validated at the Recall boundary so a typo (lambda=-0.7,
// node_cap=1_000_000) surfaces as a 400 rather than being silently absorbed
// by chapterhouse's DefaultSettleParams substitution. Zero values remain
// valid (the "use server default" wire contract). Params are ignored
// entirely when settle is off ("off") — invalid knobs with settle off must
// NOT reject.
func TestRecall_SettleParamsValidation(t *testing.T) {
	cases := []struct {
		name    string
		settle  string
		params  core.SettleParams
		wantErr bool
		// wantInErr, for rejection cases, is a substring the error
		// message must contain — the offending field identifier as it
		// appears in production (e.g. "settle_params.lambda"). Empty for
		// acceptance cases.
		wantInErr string
	}{
		// Valid: all zero (server default), Settle on.
		{"expand zero params ok", "expand", core.SettleParams{}, false, ""},
		// Valid: fully-specified in-range params.
		{"expand valid params ok", "expand", core.SettleParams{
			Lambda: 0.7, Eps: 1e-6, MaxIters: 20, HopCap: 3, NodeCap: 2000, TopM: 25,
		}, false, ""},
		// Lambda: (0, 1) open interval.
		{"lambda negative rejected", "expand", core.SettleParams{Lambda: -0.7}, true, "settle_params.lambda"},
		{"lambda >= 1 rejected", "expand", core.SettleParams{Lambda: 1.0}, true, "settle_params.lambda"},
		{"lambda near-1 ok", "expand", core.SettleParams{Lambda: 0.999}, false, ""},
		// Eps: > 0.
		{"eps negative rejected", "expand", core.SettleParams{Eps: -1e-6}, true, "settle_params.eps"},
		{"eps tiny positive ok", "expand", core.SettleParams{Eps: 1e-9}, false, ""},
		// MaxIters / HopCap / TopM: positive.
		{"max_iters negative rejected", "expand", core.SettleParams{MaxIters: -1}, true, "settle_params.max_iters"},
		{"hop_cap negative rejected", "expand", core.SettleParams{HopCap: -1}, true, "settle_params.hop_cap"},
		{"top_m negative rejected", "expand", core.SettleParams{TopM: -1}, true, "settle_params.top_m"},
		// NodeCap: positive and <= ceiling.
		{"node_cap negative rejected", "expand", core.SettleParams{NodeCap: -1}, true, "settle_params.node_cap"},
		{"node_cap over ceiling rejected", "expand", core.SettleParams{NodeCap: core.MaxSettleNodeCap + 1}, true, "settle_params.node_cap"},
		{"node_cap at ceiling ok", "expand", core.SettleParams{NodeCap: core.MaxSettleNodeCap}, false, ""},
		// Ignored entirely when settle is off ("off"): invalid knobs must NOT reject.
		{"invalid params ignored when settle off", "off", core.SettleParams{
			Lambda: -0.7, Eps: -1, MaxIters: -5, NodeCap: core.MaxSettleNodeCap + 1,
		}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _, _, _ := newCore()
			_, err := c.Recall(context.Background(), core.RecallInput{
				UserID: "u1", Workspace: "ws1",
				QueryText:    "kubernetes",
				Settle:       tc.settle,
				SettleParams: tc.params,
			})
			if tc.wantErr {
				require.Error(t, err)
				assert.ErrorIs(t, err, core.ErrValidation,
					"out-of-range settle_params must surface as a validation error (400)")
				assert.Contains(t, err.Error(), tc.wantInErr,
					"error message must name the offending settle_params field")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestRecall_SettleExpandIgnoresActivationWeight pins mode isolation: in
// "expand" mode ActivationWeight is present in the input struct but must
// be completely ignored by the fusion path. The output must be identical
// to a plain expand recall with ActivationWeight=0 — the activation
// channel must NOT be applied when the mode is "expand".
func TestRecall_SettleExpandIgnoresActivationWeight(t *testing.T) {
	c, _, ch, _ := newCore()
	ch.episResp = []core.RecallHit{
		{Tier: "episodic", Score: 0.5, ID: "e1", Content: "episodic content"},
	}
	ch.expResp = []core.ExpansionHit{
		{ID: "x1", Activation: 0.9, Text: "expansion text"},
	}

	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Both candidates score equally so only the rrf+rerank blend
		// determines the output, not activation.
		_, _ = w.Write([]byte(`{"scores":[
			{"id":"e1","score":0.5},
			{"id":"x1","score":0.5}
		]}`))
	}))
	defer srv.Close()
	c.Truthsayer = truthsayer.New(srv.URL).WithHTTPClient(srv.Client())
	c.RerankWeight = 0.5

	// Recall with "expand" + non-zero ActivationWeight: activation must be ignored.
	withAW, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText:        "kubernetes",
		Settle:           "expand",
		ActivationWeight: 0.2, // must be a no-op in expand mode
	})
	require.NoError(t, err)

	// Recall with "expand" + zero ActivationWeight: must produce identical scores.
	withoutAW, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText:        "kubernetes",
		Settle:           "expand",
		ActivationWeight: 0,
	})
	require.NoError(t, err)

	require.Equal(t, len(withAW.Hits), len(withoutAW.Hits),
		"expand mode: hit count must be identical regardless of ActivationWeight")
	for i := range withAW.Hits {
		assert.Equal(t, withAW.Hits[i].ID, withoutAW.Hits[i].ID,
			"expand mode: hit order must be identical regardless of ActivationWeight")
		assert.InDelta(t, withAW.Hits[i].Score, withoutAW.Hits[i].Score, 1e-12,
			"expand mode: hit scores must be identical regardless of ActivationWeight")
	}
}

// TestRecall_SettleChannelActivationLiftsByRawID is the end-to-end
// re-keying regression + channel-mode lift test. It verifies that:
//
//  1. activationByID is keyed by RAW hit ID (not hitKey) — the expansion
//     hit's final score must reflect its activation score. Under the pre-
//     Step-0 bug (hitKey keying) the lookup in FuseScores would miss every
//     expansion entry and the channel mode would silently produce the same
//     result as expand mode.
//
//  2. Config B property: an expansion hit with high activation outranks an
//     equally-reranked pool hit that has no activation.
//
// Setup: one pool hit "e1" (episodic, RRF mass), one expansion hit "xact"
// (rrf=0, same rerank score as e1, activation=1.0). With channel mode and
// wActivation=0.3, xact's activation term must tip it above e1.
//
//   c.RerankWeight=0.3, wActivation=0.3.
//   Both hits get rerank score 0.5.
//   rrfMax = rrf(e1) = 1/61 ≈ 0.0164 (single-tier, k=60, rank=1 → 1/(60+1)).
//   xact: rrfByID=0.
//
//   rrfNorm(e1)   = 1.0   (it's the only nonzero rrf entry → max = itself)
//   rrfNorm(xact) = 0.0
//   rerankNorm both = 1.0 (same score, max=0.5/0.5=1)
//   actNorm(e1)   = 0.0  (absent from activation map)
//   actNorm(xact) = 1.0
//
//   e1:   (1-0.3-0.3)*1.0 + 0.3*1.0 + 0.3*0.0 = 0.4 + 0.3 = 0.70
//   xact: (1-0.3-0.3)*0.0 + 0.3*1.0 + 0.3*1.0 = 0.0 + 0.3 + 0.3 = 0.60
//
// Hmm — e1 still wins because its rrf mass (0.4) > xact's activation gain.
// Make wActivation higher so xact wins:
//
//   c.RerankWeight=0.1, wActivation=0.5.
//   e1:   (1-0.1-0.5)*1.0 + 0.1*1.0 + 0.5*0.0 = 0.4 + 0.1 = 0.50
//   xact: (1-0.1-0.5)*0.0 + 0.1*1.0 + 0.5*1.0 = 0.0 + 0.1 + 0.5 = 0.60
//   xact wins by 0.10. This is the re-keying regression proof.
func TestRecall_SettleChannelActivationLiftsByRawID(t *testing.T) {
	c, _, ch, _ := newCore()
	ch.episResp = []core.RecallHit{
		{Tier: "episodic", Score: 0.8, ID: "e1", Content: "episodic content"},
	}
	ch.expResp = []core.ExpansionHit{
		{ID: "xact", Activation: 1.0, Text: "expansion with high activation"},
	}

	srv := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Both candidates get the same rerank score — activation is the tie-breaker.
		_, _ = w.Write([]byte(`{"scores":[
			{"id":"e1","score":0.5},
			{"id":"xact","score":0.5}
		]}`))
	}))
	defer srv.Close()
	c.Truthsayer = truthsayer.New(srv.URL).WithHTTPClient(srv.Client())

	// c.RerankWeight=0.1, wActivation=0.5 → xact wins (see comment above).
	c.RerankWeight = 0.1

	out, err := c.Recall(context.Background(), core.RecallInput{
		UserID: "u1", Workspace: "ws1",
		QueryText:        "kubernetes",
		Settle:           "channel",
		ActivationWeight: 0.5,
	})
	require.NoError(t, err)
	require.Len(t, out.Hits, 2, "both hits must surface")
	assert.Equal(t, "xact", out.Hits[0].ID,
		"xact must rank first: activation (0.5) outweighs e1's rrf advantage when wActivation=0.5")
	assert.Equal(t, "e1", out.Hits[1].ID)
	assert.InDelta(t, 0.60, out.Hits[0].Score, 1e-9, "xact score: 0*0.4 + 0.1*1 + 0.5*1 = 0.60")
	assert.InDelta(t, 0.50, out.Hits[1].Score, 1e-9, "e1 score:   1.0*0.4 + 0.1*1 + 0.5*0 = 0.50")
}
