package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
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

// ---------------------------------------------------------------------
// /v1/episodic/ingest
// ---------------------------------------------------------------------

func TestEpisodicIngest_Happy(t *testing.T) {
	f := newEpisodicFixture(t)
	userID := uuid.New()
	sessionID := uuid.New()
	eventID := uuid.New()

	body := map[string]any{
		"session": map[string]any{
			"id":          sessionID,
			"user_id":     userID,
			"started_at":  time.Now().UTC().Format(time.RFC3339Nano),
			"event_count": 1,
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
// /v1/episodic/query
// ---------------------------------------------------------------------

func TestEpisodicQuery_Happy(t *testing.T) {
	f := newEpisodicFixture(t)
	userID := uuid.New()
	sessionID := uuid.New()
	eventID := uuid.New()

	// Seed one event so the query has something to return.
	ingestBody := map[string]any{
		"session": map[string]any{
			"id":          sessionID,
			"user_id":     userID,
			"started_at":  time.Now().UTC().Format(time.RFC3339Nano),
			"event_count": 1,
		},
		"events": []any{sampleEvent(eventID, sessionID, userID, "kubernetes pod scheduling")},
	}
	ingestReq := authedRequest(t, http.MethodPost, "/v1/episodic/ingest", ingestBody, userID)
	f.handler.Ingest(httptest.NewRecorder(), ingestReq)

	queryBody := map[string]any{
		"user_id":         userID,
		"query_text":      "kubernetes",
		"query_embedding": smallEmbedding(0.1),
		"limit":           10,
	}
	req := authedRequest(t, http.MethodPost, "/v1/episodic/query", queryBody, userID)
	rec := httptest.NewRecorder()
	f.handler.Query(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		Hits []map[string]any `json:"hits"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotEmpty(t, resp.Hits, "expected at least one hit")
	assert.Equal(t, eventID.String(), resp.Hits[0]["id"])
	assert.Equal(t, "episodic", resp.Hits[0]["tier"])
}

func TestEpisodicQuery_ACLRejectsOtherUser(t *testing.T) {
	f := newEpisodicFixture(t)
	owner := uuid.New()
	stranger := uuid.New()
	sessionID := uuid.New()
	eventID := uuid.New()

	// Owner ingests an event.
	f.handler.Ingest(httptest.NewRecorder(), authedRequest(t,
		http.MethodPost, "/v1/episodic/ingest",
		map[string]any{
			"session": map[string]any{
				"id":         sessionID,
				"user_id":    owner,
				"started_at": time.Now().UTC().Format(time.RFC3339Nano),
			},
			"events": []any{sampleEvent(eventID, sessionID, owner, "secret")},
		}, owner))

	// Stranger queries — owner's event must NOT appear.
	req := authedRequest(t, http.MethodPost, "/v1/episodic/query",
		map[string]any{
			"user_id":         stranger,
			"query_text":      "secret",
			"query_embedding": smallEmbedding(0.1),
			"limit":           10,
			"include_shared":  true,
		}, stranger)
	rec := httptest.NewRecorder()
	f.handler.Query(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var resp struct {
		Hits []map[string]any `json:"hits"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Empty(t, resp.Hits, "stranger must not see owner's event without a share")
}

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
				"id":         sessionID,
				"user_id":    owner,
				"started_at": time.Now().UTC().Format(time.RFC3339Nano),
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
				"id":         sessionID,
				"user_id":    owner,
				"started_at": time.Now().UTC().Format(time.RFC3339Nano),
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
				"id":         sessionID,
				"user_id":    userID,
				"started_at": time.Now().UTC().Format(time.RFC3339Nano),
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
// Helpers used above — kept at the bottom so the test bodies read top-
// down without forward-reference friction.
// ---------------------------------------------------------------------

// ensure imports are used when the Semantic tests in the sibling
// file toggle between happy + auth-deny stubs; harmless at the package
// level and keeps goimports from thrashing the file on save.
var _ = strings.Contains
var _ = fmt.Sprintf
