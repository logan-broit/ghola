package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/auth"
	"github.com/thinkwright/chapterhouse/ch-server/internal/embedding"
	"github.com/thinkwright/chapterhouse/ch-server/internal/handler"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"
)

// episodicHandlerFixture bundles the per-test Postgres instance and
// an initialized EpisodicHandler so the endpoint-level tests stay
// focused on request/response shape.
type episodicHandlerFixture struct {
	pg      *testutil.EphemeralPostgres
	repo    *repository.Repository
	handler *handler.EpisodicHandler
}

func newEpisodicFixture(t *testing.T) *episodicHandlerFixture {
	t.Helper()
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "8")
	require.NoError(t, repository.ApplyMigrations(context.Background(), pg.Pool))

	repo := repository.New(pg.Pool)
	return &episodicHandlerFixture{
		pg:      pg,
		repo:    repo,
		handler: handler.NewEpisodicHandler(repo),
	}
}

// newEpisodicFixtureWithEmbedder is the same as newEpisodicFixture but
// wires an embedding.Provider into the handler so the server-side
// backstop fires. Tests that don't pass an embedder exercise the
// "embedder unconfigured" code path (NULL embeddings pass through).
func newEpisodicFixtureWithEmbedder(t *testing.T, emb embedding.Provider) *episodicHandlerFixture {
	t.Helper()
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "8")
	require.NoError(t, repository.ApplyMigrations(context.Background(), pg.Pool))

	repo := repository.New(pg.Pool)
	return &episodicHandlerFixture{
		pg:      pg,
		repo:    repo,
		handler: handler.NewEpisodicHandler(repo).WithEmbedder(emb),
	}
}

// authedRequest builds a POST request with an auth.Context in its
// context value so the handler sees the caller. Phase 3.8 will wire
// the middleware that populates this from a Bearer token; for now the
// tests inject directly.
func authedRequest(t *testing.T, method, path string, body any, userID uuid.UUID) *http.Request {
	t.Helper()
	buf := new(bytes.Buffer)
	if body != nil {
		require.NoError(t, json.NewEncoder(buf).Encode(body))
	}
	req := httptest.NewRequest(method, path, buf)
	req.Header.Set("Content-Type", "application/json")
	if userID != uuid.Nil {
		ctx := auth.WithContext(req.Context(), &auth.Context{UserID: userID})
		req = req.WithContext(ctx)
	}
	return req
}

func smallEmbedding(fill float64) []float64 {
	v := make([]float64, 8)
	for i := range v {
		v[i] = fill
	}
	return v
}

func sampleEvent(id, sessionID, userID uuid.UUID, text string) map[string]any {
	return map[string]any{
		"id":         id,
		"session_id": sessionID,
		"user_id":    userID,
		"type":       "user",
		"role":       "user",
		"text":       text,
		"raw_event":  map[string]any{"text": text},
		"embedding":  smallEmbedding(0.1),
		"entities":   []string{},
		"tags":       []string{},
		"created_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
}

// sampleEventNoEmbedding is the same as sampleEvent but omits the
// embedding field — exercising the server-side backstop path where a
// future ingester forgets to embed at the wire boundary.
func sampleEventNoEmbedding(id, sessionID, userID uuid.UUID, text string) map[string]any {
	return map[string]any{
		"id":         id,
		"session_id": sessionID,
		"user_id":    userID,
		"type":       "user",
		"role":       "user",
		"text":       text,
		"raw_event":  map[string]any{"text": text},
		"entities":   []string{},
		"tags":       []string{},
		"created_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
}

// ---------------------------------------------------------------------
// /v1/episodic/ingest
// ---------------------------------------------------------------------

func TestEpisodicIngest_Happy(t *testing.T) {
	f := newEpisodicFixture(t)
	userID := uuid.New()
	sessionID := uuid.New()
	eventID := uuid.New()
	workspaceID := uuid.New()

	body := map[string]any{
		"session": map[string]any{
			"id":           sessionID,
			"user_id":      userID,
			"workspace_id": workspaceID,
			"started_at":   time.Now().UTC().Format(time.RFC3339Nano),
			"event_count":  1,
		},
		"events": []any{sampleEvent(eventID, sessionID, userID, "hello world")},
	}

	req := authedRequest(t, http.MethodPost, "/v1/episodic/ingest", body, userID)
	rec := httptest.NewRecorder()
	f.handler.Ingest(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		SessionID string `json:"session_id"`
		Inserted  int    `json:"inserted"`
		Updated   int    `json:"updated"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, sessionID.String(), resp.SessionID)
	assert.Equal(t, 1, resp.Inserted)
	assert.Equal(t, 0, resp.Updated)

	// A re-POST of the same batch should become an UPDATE, not INSERT,
	// because Pipeline A retries must be idempotent.
	req2 := authedRequest(t, http.MethodPost, "/v1/episodic/ingest", body, userID)
	rec2 := httptest.NewRecorder()
	f.handler.Ingest(rec2, req2)
	require.Equal(t, http.StatusOK, rec2.Code, "body=%s", rec2.Body.String())

	require.NoError(t, json.NewDecoder(rec2.Body).Decode(&resp))
	assert.Equal(t, 0, resp.Inserted)
	assert.Equal(t, 1, resp.Updated, "second POST should update, not insert")
}

func TestEpisodicIngest_Unauthenticated(t *testing.T) {
	f := newEpisodicFixture(t)

	req := authedRequest(t, http.MethodPost, "/v1/episodic/ingest",
		map[string]any{}, uuid.Nil)
	rec := httptest.NewRecorder()
	f.handler.Ingest(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// ---------------------------------------------------------------------
// Server-side embedding backstop on /v1/episodic/ingest
//
// chapterhouse owns the invariant "every episodic event has an
// embedding" (after ingest). If the configured embedder is wired AND a
// caller submits events with embedding=null, the handler fills them in
// before persistence. If the embedder is unconfigured, NULL passes
// through (preserves test ergonomics + lighter-client paths). If the
// embedder errors, the entire batch fails — never silently store NULLs.
// ---------------------------------------------------------------------

func TestEpisodicIngest_FillsMissingEmbedding(t *testing.T) {
	mock := testutil.NewMockEmbeddingProvider(8)
	f := newEpisodicFixtureWithEmbedder(t, mock)

	userID := uuid.New()
	sessionID := uuid.New()
	eventID := uuid.New()
	workspaceID := uuid.New()

	body := map[string]any{
		"session": map[string]any{
			"id":           sessionID,
			"user_id":      userID,
			"workspace_id": workspaceID,
			"started_at":   time.Now().UTC().Format(time.RFC3339Nano),
			"event_count":  1,
		},
		"events": []any{sampleEventNoEmbedding(eventID, sessionID, userID, "hello world")},
	}

	req := authedRequest(t, http.MethodPost, "/v1/episodic/ingest", body, userID)
	rec := httptest.NewRecorder()
	f.handler.Ingest(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// The mock embedder must have been called exactly once with the
	// event text — not on a synthetic placeholder.
	mock.Mu.Lock()
	calls := append([]string(nil), mock.Calls...)
	mock.Mu.Unlock()
	require.Equal(t, []string{"hello world"}, calls,
		"mock embedder must see the event text exactly once")

	// And the persisted row has a non-NULL embedding (the backstop fired).
	var embedNull bool
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, f.pg.Pool.QueryRow(ctx,
		`SELECT embedding IS NULL FROM episodic.events WHERE id = $1`,
		eventID,
	).Scan(&embedNull))
	assert.False(t, embedNull, "backstop must populate embedding before persistence")
}

func TestEpisodicIngest_PreservesSuppliedEmbedding(t *testing.T) {
	mock := testutil.NewMockEmbeddingProvider(8)
	f := newEpisodicFixtureWithEmbedder(t, mock)

	userID := uuid.New()
	sessionID := uuid.New()
	eventID := uuid.New()
	workspaceID := uuid.New()

	// Caller supplies an embedding with sentinel values 0.42 — the
	// backstop must NOT overwrite it.
	supplied := make([]float64, 8)
	for i := range supplied {
		supplied[i] = 0.42
	}
	ev := sampleEventNoEmbedding(eventID, sessionID, userID, "hello world")
	ev["embedding"] = supplied

	body := map[string]any{
		"session": map[string]any{
			"id":           sessionID,
			"user_id":      userID,
			"workspace_id": workspaceID,
			"started_at":   time.Now().UTC().Format(time.RFC3339Nano),
		},
		"events": []any{ev},
	}

	req := authedRequest(t, http.MethodPost, "/v1/episodic/ingest", body, userID)
	rec := httptest.NewRecorder()
	f.handler.Ingest(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// Embedder must NOT have been called — the supplied vector is canon.
	mock.Mu.Lock()
	calls := append([]string(nil), mock.Calls...)
	mock.Mu.Unlock()
	assert.Empty(t, calls, "embedder must not be called when embedding is supplied")

	// Persisted vector must equal what the caller sent.
	var stored []float32
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, f.pg.Pool.QueryRow(ctx,
		`SELECT embedding::real[] FROM episodic.events WHERE id = $1`,
		eventID,
	).Scan(&stored))
	require.Len(t, stored, 8)
	for i, v := range stored {
		assert.InDelta(t, 0.42, v, 1e-5, "stored[%d] must match supplied", i)
	}
}

func TestEpisodicIngest_PartiallyFilledBatch(t *testing.T) {
	mock := testutil.NewMockEmbeddingProvider(8)
	f := newEpisodicFixtureWithEmbedder(t, mock)

	userID := uuid.New()
	sessionID := uuid.New()
	workspaceID := uuid.New()

	embeddedID := uuid.New()
	missingID := uuid.New()

	embedded := sampleEventNoEmbedding(embeddedID, sessionID, userID, "already embedded")
	supplied := make([]float64, 8)
	for i := range supplied {
		supplied[i] = 0.99
	}
	embedded["embedding"] = supplied

	missing := sampleEventNoEmbedding(missingID, sessionID, userID, "needs backstop")

	body := map[string]any{
		"session": map[string]any{
			"id":           sessionID,
			"user_id":      userID,
			"workspace_id": workspaceID,
			"started_at":   time.Now().UTC().Format(time.RFC3339Nano),
		},
		"events": []any{embedded, missing},
	}

	req := authedRequest(t, http.MethodPost, "/v1/episodic/ingest", body, userID)
	rec := httptest.NewRecorder()
	f.handler.Ingest(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// Embedder called only on the missing-embedding event's text.
	mock.Mu.Lock()
	calls := append([]string(nil), mock.Calls...)
	mock.Mu.Unlock()
	assert.Equal(t, []string{"needs backstop"}, calls,
		"backstop must only embed events with nil embedding")

	// Both rows persisted with non-NULL embeddings.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, id := range []uuid.UUID{embeddedID, missingID} {
		var embedNull bool
		require.NoError(t, f.pg.Pool.QueryRow(ctx,
			`SELECT embedding IS NULL FROM episodic.events WHERE id = $1`, id,
		).Scan(&embedNull))
		assert.False(t, embedNull, "event %s must have non-NULL embedding", id)
	}
}

func TestEpisodicIngest_EmbedderUnconfigured_AcceptsNullEmbedding(t *testing.T) {
	// No embedder wired. The handler must preserve the legacy path —
	// accept NULL embeddings rather than 500.
	f := newEpisodicFixture(t)

	userID := uuid.New()
	sessionID := uuid.New()
	eventID := uuid.New()
	workspaceID := uuid.New()

	body := map[string]any{
		"session": map[string]any{
			"id":           sessionID,
			"user_id":      userID,
			"workspace_id": workspaceID,
			"started_at":   time.Now().UTC().Format(time.RFC3339Nano),
		},
		"events": []any{sampleEventNoEmbedding(eventID, sessionID, userID, "no embedder, no problem")},
	}

	req := authedRequest(t, http.MethodPost, "/v1/episodic/ingest", body, userID)
	rec := httptest.NewRecorder()
	f.handler.Ingest(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var embedNull bool
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, f.pg.Pool.QueryRow(ctx,
		`SELECT embedding IS NULL FROM episodic.events WHERE id = $1`,
		eventID,
	).Scan(&embedNull))
	assert.True(t, embedNull, "without embedder, NULL must pass through")
}

func TestEpisodicIngest_EmbedderError_FailsRequest(t *testing.T) {
	// Embedder configured but returns an error. Contract: refuse the
	// entire ingest. Never silently store NULL embeddings when the
	// operator explicitly wired an embedder.
	embErr := errors.New("guild blew up")
	f := newEpisodicFixtureWithEmbedder(t, testutil.NewErrorEmbeddingProvider(embErr))

	userID := uuid.New()
	sessionID := uuid.New()
	eventID := uuid.New()
	workspaceID := uuid.New()

	body := map[string]any{
		"session": map[string]any{
			"id":           sessionID,
			"user_id":      userID,
			"workspace_id": workspaceID,
			"started_at":   time.Now().UTC().Format(time.RFC3339Nano),
		},
		"events": []any{sampleEventNoEmbedding(eventID, sessionID, userID, "doomed")},
	}

	req := authedRequest(t, http.MethodPost, "/v1/episodic/ingest", body, userID)
	rec := httptest.NewRecorder()
	f.handler.Ingest(rec, req)
	require.Equal(t, http.StatusInternalServerError, rec.Code,
		"embedder failure must surface as 500, body=%s", rec.Body.String())

	// And no rows persisted (transactional — embed failure happens
	// before the DB upsert).
	var count int
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, f.pg.Pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM episodic.events WHERE id = $1`, eventID,
	).Scan(&count))
	assert.Equal(t, 0, count, "no events must persist when embedder fails")
}

func TestEpisodicIngest_SkipsEventsWithoutText(t *testing.T) {
	// Tool-call events (Text == nil) cannot be embedded. The backstop
	// must skip them — leave embedding NULL, do not synthesize text,
	// do not fail the request. Mirrors what import-logs does upstream.
	mock := testutil.NewMockEmbeddingProvider(8)
	f := newEpisodicFixtureWithEmbedder(t, mock)

	userID := uuid.New()
	sessionID := uuid.New()
	workspaceID := uuid.New()
	eventID := uuid.New()

	// Note: type must remain in the validated set; just drop the text field.
	ev := map[string]any{
		"id":         eventID,
		"session_id": sessionID,
		"user_id":    userID,
		"type":       "tool_result",
		"raw_event":  map[string]any{"tool": "noop"},
		"entities":   []string{},
		"tags":       []string{},
		"created_at": time.Now().UTC().Format(time.RFC3339Nano),
	}

	body := map[string]any{
		"session": map[string]any{
			"id":           sessionID,
			"user_id":      userID,
			"workspace_id": workspaceID,
			"started_at":   time.Now().UTC().Format(time.RFC3339Nano),
		},
		"events": []any{ev},
	}

	req := authedRequest(t, http.MethodPost, "/v1/episodic/ingest", body, userID)
	rec := httptest.NewRecorder()
	f.handler.Ingest(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	mock.Mu.Lock()
	calls := append([]string(nil), mock.Calls...)
	mock.Mu.Unlock()
	assert.Empty(t, calls, "events without text must not be embedded")

	var embedNull bool
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, f.pg.Pool.QueryRow(ctx,
		`SELECT embedding IS NULL FROM episodic.events WHERE id = $1`, eventID,
	).Scan(&embedNull))
	assert.True(t, embedNull, "text-less event must persist with NULL embedding")
}

// ---------------------------------------------------------------------
// /v1/episodic/query
// ---------------------------------------------------------------------

// Legacy hybrid-mode handler tests (TestEpisodicQuery_Happy,
// TestEpisodicQuery_ACLRejectsOtherUser, TestEpisodicQuery_TagsAnyFilter,
// TestEpisodicQueryKeyword_TagsAnyFilter) were removed in A8 alongside
// the legacy /query path. Multi-ranking equivalents — including
// tags_any pass-through to the per-tier rankers — live in
// episodic_query_multi_test.go. ACL boundaries are still exercised at
// the repo layer (workspace_isolation_test.go).

// ---------------------------------------------------------------------
// /v1/episodic/share
// ---------------------------------------------------------------------

func TestEpisodicShare_Happy(t *testing.T) {
	f := newEpisodicFixture(t)
	owner := uuid.New()
	sessionID := uuid.New()

	// Seed a session so the scope_id references something real.
	f.handler.Ingest(httptest.NewRecorder(), authedRequest(t,
		http.MethodPost, "/v1/episodic/ingest",
		map[string]any{
			"session": map[string]any{
				"id":           sessionID,
				"user_id":      owner,
				"workspace_id": uuid.New(),
				"started_at":   time.Now().UTC().Format(time.RFC3339Nano),
			},
			"events": []any{},
		}, owner))

	body := map[string]any{
		"owner_user_id": owner,
		"target":        "team",
		"scope_type":    "session",
		"scope_id":      sessionID,
	}
	req := authedRequest(t, http.MethodPost, "/v1/episodic/share", body, owner)
	rec := httptest.NewRecorder()
	f.handler.Share(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	_, err := uuid.Parse(resp.ID)
	assert.NoError(t, err, "share id must be a UUID")
}

func TestEpisodicShare_NonOwnerForbidden(t *testing.T) {
	f := newEpisodicFixture(t)
	owner := uuid.New()
	stranger := uuid.New()
	sessionID := uuid.New()

	// Owner ingests.
	f.handler.Ingest(httptest.NewRecorder(), authedRequest(t,
		http.MethodPost, "/v1/episodic/ingest",
		map[string]any{
			"session": map[string]any{
				"id":           sessionID,
				"user_id":      owner,
				"workspace_id": uuid.New(),
				"started_at":   time.Now().UTC().Format(time.RFC3339Nano),
			},
			"events": []any{},
		}, owner))

	// Stranger tries to share owner's session.
	req := authedRequest(t, http.MethodPost, "/v1/episodic/share",
		map[string]any{
			"owner_user_id": stranger, // even if they claim it in the body
			"target":        "team",
			"scope_type":    "session",
			"scope_id":      sessionID,
		}, stranger)
	rec := httptest.NewRecorder()
	f.handler.Share(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code,
		"non-owner attempting to share must get 403")
}

// ---------------------------------------------------------------------
// /v1/episodic/forget
// ---------------------------------------------------------------------

func TestEpisodicForget_Happy(t *testing.T) {
	f := newEpisodicFixture(t)
	userID := uuid.New()
	sessionID := uuid.New()
	eventID := uuid.New()

	f.handler.Ingest(httptest.NewRecorder(), authedRequest(t,
		http.MethodPost, "/v1/episodic/ingest",
		map[string]any{
			"session": map[string]any{
				"id":           sessionID,
				"user_id":      userID,
				"workspace_id": uuid.New(),
				"started_at":   time.Now().UTC().Format(time.RFC3339Nano),
			},
			"events": []any{sampleEvent(eventID, sessionID, userID, "sensitive data")},
		}, userID))

	req := authedRequest(t, http.MethodPost, "/v1/episodic/forget",
		map[string]any{
			"user_id":    userID,
			"event_ids":  []uuid.UUID{eventID},
		}, userID)
	rec := httptest.NewRecorder()
	f.handler.Forget(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp struct {
		Forgotten int `json:"forgotten"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, 1, resp.Forgotten)

	// Verify: text column flipped to the sentinel; embedding nulled.
	var text string
	var embedNull bool
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, f.pg.Pool.QueryRow(ctx,
		`SELECT text, embedding IS NULL FROM episodic.events WHERE id = $1`,
		eventID,
	).Scan(&text, &embedNull))
	assert.Equal(t, "[forgotten]", text)
	assert.True(t, embedNull)
}

func TestEpisodicForget_Unauthenticated(t *testing.T) {
	f := newEpisodicFixture(t)

	req := authedRequest(t, http.MethodPost, "/v1/episodic/forget",
		map[string]any{"event_ids": []string{uuid.New().String()}}, uuid.Nil)
	rec := httptest.NewRecorder()
	f.handler.Forget(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// ---------------------------------------------------------------------
// Co-activation enqueue on /v1/episodic/ingest (Task C1)
//
// After the events upsert succeeds, the ingest handler enqueues every
// pair (i, j with i<j) of incoming events into semantic.co_activation_queue
// for the consolidation worker to fold into semantic.associations. Three
// invariants pinned below:
//   - n>=2 events -> n*(n-1)/2 queue rows, each tagged with the session's
//     workspace_id.
//   - n<=1 events -> queue stays empty (no DB hit, no log).
//   - enqueue failure -> ingest still succeeds (events landed); the
//     enqueue error is best-effort, logged but not surfaced.
// ---------------------------------------------------------------------

func TestEpisodicIngest_EnqueuesCoActivations(t *testing.T) {
	f := newEpisodicFixture(t)
	userID := uuid.New()
	sessionID := uuid.New()
	workspaceID := uuid.New()
	eventA := uuid.New()
	eventB := uuid.New()
	eventC := uuid.New()

	body := map[string]any{
		"session": map[string]any{
			"id":           sessionID,
			"user_id":      userID,
			"workspace_id": workspaceID,
			"started_at":   time.Now().UTC().Format(time.RFC3339Nano),
		},
		"events": []any{
			sampleEvent(eventA, sessionID, userID, "first"),
			sampleEvent(eventB, sessionID, userID, "second"),
			sampleEvent(eventC, sessionID, userID, "third"),
		},
	}

	req := authedRequest(t, http.MethodPost, "/v1/episodic/ingest", body, userID)
	rec := httptest.NewRecorder()
	f.handler.Ingest(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// 3 events -> 3 pairs (AB, AC, BC).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var count int
	require.NoError(t, f.pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM semantic.co_activation_queue`,
	).Scan(&count))
	assert.Equal(t, 3, count, "n=3 events must produce n*(n-1)/2 = 3 pairs")

	// Each row must carry the session's workspace_id.
	var wsCount int
	require.NoError(t, f.pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM semantic.co_activation_queue WHERE workspace_id = $1`,
		workspaceID,
	).Scan(&wsCount))
	assert.Equal(t, 3, wsCount, "every queued pair must carry session.workspace_id")

	// And the pair shape — (A,B), (A,C), (B,C) — matches the lexicographic
	// upper-triangle iteration the impl is contractually required to use.
	type pair struct{ src, dst uuid.UUID }
	rows, err := f.pg.Pool.Query(ctx,
		`SELECT src_event_id, dst_event_id FROM semantic.co_activation_queue ORDER BY id`)
	require.NoError(t, err)
	defer rows.Close()
	var got []pair
	for rows.Next() {
		var p pair
		require.NoError(t, rows.Scan(&p.src, &p.dst))
		got = append(got, p)
	}
	require.NoError(t, rows.Err())
	assert.ElementsMatch(t, []pair{
		{src: eventA, dst: eventB},
		{src: eventA, dst: eventC},
		{src: eventB, dst: eventC},
	}, got)
}

func TestEpisodicIngest_SingleEvent_NoEnqueue(t *testing.T) {
	f := newEpisodicFixture(t)
	userID := uuid.New()
	sessionID := uuid.New()
	workspaceID := uuid.New()
	eventID := uuid.New()

	body := map[string]any{
		"session": map[string]any{
			"id":           sessionID,
			"user_id":      userID,
			"workspace_id": workspaceID,
			"started_at":   time.Now().UTC().Format(time.RFC3339Nano),
		},
		"events": []any{sampleEvent(eventID, sessionID, userID, "lonely")},
	}

	req := authedRequest(t, http.MethodPost, "/v1/episodic/ingest", body, userID)
	rec := httptest.NewRecorder()
	f.handler.Ingest(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// No pairs to enqueue from a single event.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	require.NoError(t, f.pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM semantic.co_activation_queue`,
	).Scan(&count))
	assert.Equal(t, 0, count, "n=1 event must not enqueue any pairs")
}

// erroringEnqueuer is a fake-implementing-the-interface (not a mock-of-an-
// HTTP-client) used by TestEpisodicIngest_EnqueueFailure_DoesNotFailIngest
// to exercise the best-effort error-handling branch in the ingest handler.
type erroringEnqueuer struct {
	calls int
	err   error
}

func (e *erroringEnqueuer) EnqueueCoActivations(_ context.Context, _ []repository.CoActivationPair) error {
	e.calls++
	return e.err
}

func TestEpisodicIngest_EnqueueFailure_DoesNotFailIngest(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "8")
	require.NoError(t, repository.ApplyMigrations(context.Background(), pg.Pool))

	repo := repository.New(pg.Pool)
	fake := &erroringEnqueuer{err: errors.New("queue blew up")}
	h := handler.NewEpisodicHandler(repo).WithCoActivationEnqueuer(fake)

	userID := uuid.New()
	sessionID := uuid.New()
	workspaceID := uuid.New()
	eventA := uuid.New()
	eventB := uuid.New()

	body := map[string]any{
		"session": map[string]any{
			"id":           sessionID,
			"user_id":      userID,
			"workspace_id": workspaceID,
			"started_at":   time.Now().UTC().Format(time.RFC3339Nano),
		},
		"events": []any{
			sampleEvent(eventA, sessionID, userID, "alpha"),
			sampleEvent(eventB, sessionID, userID, "beta"),
		},
	}

	req := authedRequest(t, http.MethodPost, "/v1/episodic/ingest", body, userID)
	rec := httptest.NewRecorder()
	h.Ingest(rec, req)

	// Ingest still returned 200 — enqueue is best-effort.
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// Fake was called (so the failure path was actually exercised).
	assert.Equal(t, 1, fake.calls,
		"handler must invoke the enqueuer once per ingest with >1 events")

	// Events still landed in episodic.events — the upsert is the source
	// of truth; the queue is downstream.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var eventCount int
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM episodic.events WHERE session_id = $1`,
		sessionID,
	).Scan(&eventCount))
	assert.Equal(t, 2, eventCount, "events must persist even when enqueue fails")
}

// ---------------------------------------------------------------------
// Helpers used above — kept at the bottom so the test bodies read top-
// down without forward-reference friction.
// ---------------------------------------------------------------------

// ensure imports are used when the Semantic tests in the sibling
// file toggle between happy + auth-deny stubs; harmless at the package
// level and keeps goimports from thrashing the file on save.
var _ = strings.Contains
var _ = fmt.Sprintf
