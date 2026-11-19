package core_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/logan-broit/ghola/internal/core"
	"github.com/logan-broit/ghola/internal/sietch"
)

const replayUser = "00000000-0000-4000-8000-000000000011"
const replayWorkspace = "00000000-0000-4000-8000-000000000021"

type unavailableReplayEmbedder struct{}

func (unavailableReplayEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, errors.New("fixture: models disabled")
}

func replayCore(t *testing.T) (*core.Core, *sietch.Store, string) {
	t.Helper()
	root := t.TempDir()
	store, err := sietch.Open(root)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	c := core.New(store, newFakeChapterhouse(), unavailableReplayEmbedder{})
	c.Now = func() time.Time { return time.Unix(1_700_000_000, 0).UTC() }
	return c, store, root
}

// Decode the real wire shape so the initial test catches silent omission of
// unsupported fields instead of failing to compile against the old DTO.
func replayStartInput(t *testing.T, id string) core.SessionStartInput {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"session_id": id, "user_id": replayUser, "workspace_id": replayWorkspace,
		"cwd": "/fixture/project", "agent_kind": "pi", "git_branch": "main", "source_device": "fixture",
	})
	require.NoError(t, err)
	var in core.SessionStartInput
	require.NoError(t, json.Unmarshal(body, &in))
	return in
}

func TestPiIngestSessionReplay(t *testing.T) {
	ctx := context.Background()
	c, store, _ := replayCore(t)
	id := core.NewID()
	in := replayStartInput(t, id)
	first, err := c.SessionStart(ctx, in)
	require.NoError(t, err)
	require.Equal(t, id, first.ID, "caller episode ID must be honored")
	ended := first.StartedAt.Add(time.Minute)
	require.NoError(t, store.MarkEnded(ctx, id, ended))
	require.NoError(t, store.CloseSession(ctx, id))
	c.Now = func() time.Time { return ended.Add(time.Hour) }
	again, err := c.SessionStart(ctx, in)
	require.NoError(t, err)
	require.Equal(t, first.ID, again.ID)
	require.Equal(t, first.StartedAt, again.StartedAt)
	require.Equal(t, &ended, again.EndedAt, "replay must not reopen a closed episode")
	sessions, err := store.ListSessions(ctx, replayUser)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
}

func TestPiIngestSessionConflicts(t *testing.T) {
	for _, field := range []string{"user", "workspace", "cwd", "agent", "branch", "device"} {
		t.Run(field, func(t *testing.T) {
			c, store, _ := replayCore(t)
			in := replayStartInput(t, core.NewID())
			first, err := c.SessionStart(context.Background(), in)
			require.NoError(t, err)
			changed := "changed"
			switch field {
			case "user":
				in.UserID = "00000000-0000-4000-8000-000000000012"
			case "workspace":
				in.WorkspaceID = "00000000-0000-4000-8000-000000000022"
			case "cwd":
				in.Cwd = &changed
			case "agent":
				in.AgentKind = &changed
			case "branch":
				in.GitBranch = &changed
			case "device":
				in.SourceDevice = &changed
			}
			_, err = c.SessionStart(context.Background(), in)
			require.ErrorIs(t, err, core.ErrReplayConflict)
			stored, err := store.GetSession(context.Background(), first.ID)
			require.NoError(t, err)
			require.Equal(t, first, stored)
		})
	}
}

func TestPiIngestSessionConcurrentReplay(t *testing.T) {
	c, store, _ := replayCore(t)
	id := core.NewID()
	in := replayStartInput(t, id)
	type result struct {
		session core.Session
		err     error
	}
	results := make(chan result, 12)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() { s, err := c.SessionStart(context.Background(), in); results <- result{s, err} })
	}
	wg.Wait()
	close(results)
	for r := range results {
		require.NoError(t, r.err)
		require.Equal(t, id, r.session.ID)
	}
	sessions, err := store.ListSessions(context.Background(), replayUser)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
}

func TestPiIngestSessionRejectsMalformedIDBeforeStorage(t *testing.T) {
	for _, id := range []string{"../escape", "/tmp/escape", "bad?mode=memory", "not-an-id"} {
		t.Run(id, func(t *testing.T) {
			c, _, root := replayCore(t)
			_, err := c.SessionStart(context.Background(), replayStartInput(t, id))
			require.ErrorIs(t, err, core.ErrValidation)
			files, err := os.ReadDir(root)
			require.NoError(t, err)
			require.Empty(t, files)
		})
	}
}

func replayRecordInput(t *testing.T, sid, key, text string) core.RecordInput {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"session_id": sid, "user_id": replayUser, "source_key": key,
		"event": map[string]any{"type": "assistant", "text": text, "raw_event": map[string]any{"pi_entry": key}},
	})
	require.NoError(t, err)
	var in core.RecordInput
	require.NoError(t, json.Unmarshal(body, &in))
	return in
}

func TestPiIngestRecordReplay(t *testing.T) {
	ctx := context.Background()
	c, store, _ := replayCore(t)
	sess, err := c.SessionStart(ctx, replayStartInput(t, core.NewID()))
	require.NoError(t, err)
	text := strings.Repeat("full transcript fixture ", 10000)
	in := replayRecordInput(t, sess.ID, "pi:entry:a", text)
	a, err := c.Record(ctx, in)
	require.NoError(t, err)
	b, err := c.Record(ctx, replayRecordInput(t, sess.ID, "pi:entry:b", "later response"))
	require.NoError(t, err)
	require.NoError(t, store.SetBookmark(ctx, sess.ID, a.ID, "remember"))
	before, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	c.Now = func() time.Time { return before.StartedAt.Add(time.Hour) }
	again, err := c.Record(ctx, in)
	require.NoError(t, err)
	require.Equal(t, a.ID, again.ID)
	require.Equal(t, text, *again.Text)
	require.Equal(t, "remember", *again.BookmarkLabel)
	current, err := store.CurrentEvent(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, b.ID, current, "replaying A must not rewind the pointer from B")
	after, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, 2, after.EventCount)
}

func TestPiIngestRecordConflictAndCanonicalJSON(t *testing.T) {
	ctx := context.Background()
	c, store, _ := replayCore(t)
	sess, err := c.SessionStart(ctx, replayStartInput(t, core.NewID()))
	require.NoError(t, err)
	in := replayRecordInput(t, sess.ID, "pi:canonical", "original")
	in.Event.RawEvent = json.RawMessage(`{"b":2,"a":9007199254740993}`)
	first, err := c.Record(ctx, in)
	require.NoError(t, err)
	in.Event.RawEvent = json.RawMessage(`{ "a": 9007199254740993, "b": 2 }`)
	again, err := c.Record(ctx, in)
	require.NoError(t, err)
	require.Equal(t, first.ID, again.ID, "JSON key order and whitespace are not payload changes")
	in.Event.RawEvent = json.RawMessage(`{"a":9007199254740992,"b":2}`)
	_, err = c.Record(ctx, in)
	require.ErrorIs(t, err, core.ErrReplayConflict, "large integer identity must not be rounded")
	in.Event.RawEvent = json.RawMessage(`{"b":2,"a":9007199254740993}`)
	changed := "changed"
	in.Event.Text = &changed
	_, err = c.Record(ctx, in)
	require.ErrorIs(t, err, core.ErrReplayConflict)
	stored, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, 1, stored.EventCount)
}

func TestPiIngestRecordConcurrentReplay(t *testing.T) {
	c, store, _ := replayCore(t)
	sess, err := c.SessionStart(context.Background(), replayStartInput(t, core.NewID()))
	require.NoError(t, err)
	in := replayRecordInput(t, sess.ID, "pi:concurrent", "once")
	type result struct {
		event core.Event
		err   error
	}
	results := make(chan result, 12)
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() { e, err := c.Record(context.Background(), in); results <- result{e, err} })
	}
	wg.Wait()
	close(results)
	var id string
	for r := range results {
		require.NoError(t, r.err)
		if id == "" {
			id = r.event.ID
		}
		require.Equal(t, id, r.event.ID)
	}
	stored, err := store.GetSession(context.Background(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, 1, stored.EventCount)
}

type replayEmbedFunc func(context.Context, string) ([]float32, error)

func (f replayEmbedFunc) Embed(ctx context.Context, text string) ([]float32, error) {
	return f(ctx, text)
}

func TestPiIngestForgottenReplayDoesNotReprocessText(t *testing.T) {
	ctx := context.Background()
	c, store, _ := replayCore(t)
	sess, err := c.SessionStart(ctx, replayStartInput(t, core.NewID()))
	require.NoError(t, err)
	text := "suppressed transcript fixture"
	in := replayRecordInput(t, sess.ID, "pi:forgotten", text)
	in.Event.RawEvent, err = json.Marshal(map[string]string{"text": text})
	require.NoError(t, err)
	first, err := c.Record(ctx, in)
	require.NoError(t, err)
	require.NoError(t, store.SoftForget(ctx, sess.ID, []string{first.ID}))
	called := false
	c.Embedder = replayEmbedFunc(func(context.Context, string) ([]float32, error) {
		called = true
		return nil, errors.New("unexpected reprocessing of forgotten text")
	})
	again, err := c.Record(ctx, in)
	require.NoError(t, err)
	require.False(t, called, "replay must be resolved before embedding")
	require.Equal(t, first.ID, again.ID)
	body, err := json.Marshal(again)
	require.NoError(t, err)
	require.NotContains(t, string(body), text, "do not return retained raw/tool payloads on forgotten replay")
	hits, err := store.SearchFTS(ctx, sess.ID, "suppressed", 10)
	require.NoError(t, err)
	require.Empty(t, hits)
}

func TestPiIngestReceiptSurvivesGC(t *testing.T) {
	for _, kind := range []string{"creation", "forgotten_record", "legacy_record_receipt", "legacy"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			c, store, _ := replayCore(t)
			id := core.NewID()
			if strings.HasPrefix(kind, "legacy") {
				id = ""
			}
			sess, err := c.SessionStart(ctx, replayStartInput(t, id))
			require.NoError(t, err)
			if kind == "forgotten_record" || kind == "legacy_record_receipt" {
				e, err := c.Record(ctx, replayRecordInput(t, sess.ID, "pi:forgotten", "retained receipt"))
				require.NoError(t, err)
				require.NoError(t, store.SoftForget(ctx, sess.ID, []string{e.ID}))
			}
			require.NoError(t, c.SessionEnd(ctx, sess.ID))
			c.Now = func() time.Time { return sess.StartedAt.Add(8 * 24 * time.Hour) }
			removed, err := c.GCSession(ctx, sess.ID)
			require.NoError(t, err)
			require.Equal(t, kind == "legacy", removed)
			ids, err := store.ActiveSessionIDs(ctx)
			require.NoError(t, err)
			if kind == "legacy" {
				require.Empty(t, ids)
			} else {
				require.Equal(t, []string{sess.ID}, ids)
			}
		})
	}
}

func TestPiIngestRecordRollbackDoesNotConsumeKey(t *testing.T) {
	ctx := context.Background()
	c, store, _ := replayCore(t)
	sess, err := c.SessionStart(ctx, replayStartInput(t, core.NewID()))
	require.NoError(t, err)
	in := replayRecordInput(t, sess.ID, "pi:rollback", "valid after repair")
	missing := core.NewID()
	in.ParentID = &missing
	_, err = c.Record(ctx, in)
	require.Error(t, err, "real SQLite must reject the missing parent")
	in.ParentID = nil
	_, err = c.Record(ctx, in)
	require.NoError(t, err, "a rolled-back append must not consume its source key")
	s, err := store.GetSession(ctx, sess.ID)
	require.NoError(t, err)
	require.Equal(t, 1, s.EventCount)
}

func TestPiIngestRecordRequiresStableExistingSession(t *testing.T) {
	for _, kind := range []string{"implicit", "unknown", "foreign_owner", "caller_event_id"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			c, store, _ := replayCore(t)
			sess, err := c.SessionStart(ctx, replayStartInput(t, core.NewID()))
			require.NoError(t, err)
			in := replayRecordInput(t, sess.ID, "pi:invalid", "not stored")
			want := core.ErrValidation
			switch kind {
			case "implicit":
				in.SessionID = ""
				cwd := "/fixture"
				in.Cwd = &cwd
			case "unknown":
				in.SessionID = core.NewID()
				want = core.ErrSessionNotFound
			case "foreign_owner":
				in.UserID = "00000000-0000-4000-8000-000000000012"
				want = core.ErrReplayConflict
			case "caller_event_id":
				in.Event.ID = core.NewID()
			}
			_, err = c.Record(ctx, in)
			require.ErrorIs(t, err, want)
			ids, err := store.ActiveSessionIDs(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{sess.ID}, ids, "rejected requests must not create orphan databases")
		})
	}
}

func TestPiIngestRecordIDsFollowAppendOrder(t *testing.T) {
	ctx := context.Background()
	c, store, _ := replayCore(t)
	sess, err := c.SessionStart(ctx, replayStartInput(t, core.NewID()))
	require.NoError(t, err)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	c.Embedder = replayEmbedFunc(func(_ context.Context, text string) ([]float32, error) {
		if text == "slow" {
			close(entered)
			<-release
		}
		return nil, errors.New("fixture: models disabled")
	})
	type result struct {
		event core.Event
		err   error
	}
	resultCh := make(chan result, 1)
	slowInput := replayRecordInput(t, sess.ID, "pi:slow", "slow")
	go func() {
		e, err := c.Record(ctx, slowInput)
		resultCh <- result{e, err}
	}()
	<-entered
	fast, err := c.Record(ctx, replayRecordInput(t, sess.ID, "pi:fast", "fast"))
	unblock()
	slow := <-resultCh
	require.NoError(t, err)
	require.NoError(t, slow.err)
	require.Greater(t, slow.event.ID, fast.ID, "allocate source-key event IDs at append, not before a slow embed")
	pending, err := store.PendingEvents(ctx, sess.ID, fast.ID)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, slow.event.ID, pending[0].ID)
}

func TestPiIngestRecordValidation(t *testing.T) {
	for _, kind := range []string{"blank_key", "long_key", "nul_key", "invalid_id", "embedding", "type", "raw_json"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			c, store, _ := replayCore(t)
			sess, err := c.SessionStart(ctx, replayStartInput(t, core.NewID()))
			require.NoError(t, err)
			in := replayRecordInput(t, sess.ID, "pi:invalid", "not stored")
			switch kind {
			case "blank_key":
				in.SourceKey = " "
			case "long_key":
				in.SourceKey = strings.Repeat("x", 513)
			case "nul_key":
				in.SourceKey = "pi:\x00"
			case "invalid_id":
				in.SessionID = "bad?mode=memory"
			case "embedding":
				in.Event.Embedding = []float32{1}
			case "type":
				in.Event.Type = "finding"
			case "raw_json":
				in.Event.RawEvent = json.RawMessage(`{`)
			}
			_, err = c.Record(ctx, in)
			require.ErrorIs(t, err, core.ErrValidation)
			stored, err := store.GetSession(ctx, sess.ID)
			require.NoError(t, err)
			require.Zero(t, stored.EventCount)
			ids, err := store.ActiveSessionIDs(ctx)
			require.NoError(t, err)
			require.Equal(t, []string{sess.ID}, ids)
		})
	}
}

func TestPiIngestRecordKeyIsSessionScoped(t *testing.T) {
	ctx := context.Background()
	c, _, _ := replayCore(t)
	ids := make(map[string]bool)
	for range 2 {
		sess, err := c.SessionStart(ctx, replayStartInput(t, core.NewID()))
		require.NoError(t, err)
		e, err := c.Record(ctx, replayRecordInput(t, sess.ID, "pi:same-key", "different episode"))
		require.NoError(t, err)
		ids[e.ID] = true
	}
	require.Len(t, ids, 2)
}

func TestPiIngestSessionDoesNotAdoptLegacyID(t *testing.T) {
	c, store, _ := replayCore(t)
	legacy, err := c.SessionStart(context.Background(), replayStartInput(t, ""))
	require.NoError(t, err)
	_, err = c.SessionStart(context.Background(), replayStartInput(t, legacy.ID))
	require.ErrorIs(t, err, core.ErrReplayConflict)
	stored, err := store.GetSession(context.Background(), legacy.ID)
	require.NoError(t, err)
	require.Equal(t, legacy, stored)
}
