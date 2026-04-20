package handler_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

// The semantic handlers wrap the ghola extension's `semantic.recall`
// and `semantic.update_confidence` functions. The ghola extension
// lives in the ghcr.io/logan-broit/ghola:0.2.0 image, not in the
// plain pgvector/pgvector:pg17 we use for episodic tests. For Phase
// 3.3 / 3.7 we install a minimal SQL shim: a `semantic` schema with
// a `mnemes` table and an `update_confidence` function that mirrors
// the extension's surface at the SQL level. This lets the handler
// tests verify request/response shape without needing the full
// cognitive-primitive extension loaded.

type semanticFixture struct {
	pg      *testutil.EphemeralPostgres
	repo    *repository.Repository
	handler *handler.SemanticHandler
}

func newSemanticFixture(t *testing.T) *semanticFixture {
	t.Helper()
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "8")
	require.NoError(t, repository.ApplyMigrations(context.Background(), pg.Pool))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, err := pg.Pool.Exec(ctx, `
		CREATE SCHEMA IF NOT EXISTS semantic;

		CREATE TABLE semantic.mnemes (
			id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			workspace_id          uuid NOT NULL,
			concept               text NOT NULL,
			content               text NOT NULL,
			embedding             vector(8),
			confidence            double precision NOT NULL DEFAULT 0.5,
			access_count          integer NOT NULL DEFAULT 0,
			last_access           timestamptz NOT NULL DEFAULT now(),
			created_at            timestamptz NOT NULL DEFAULT now(),
			state                 text NOT NULL DEFAULT 'active',
			memory_type           text NOT NULL DEFAULT 'factual',
			tags                  text[] NOT NULL DEFAULT '{}',
			entities              text[] NOT NULL DEFAULT '{}',
			source_episodic_ids   uuid[] NOT NULL DEFAULT '{}',
			contributor_user_ids  uuid[] NOT NULL DEFAULT '{}'
		);

		CREATE OR REPLACE FUNCTION semantic.update_confidence(
			mid uuid, evidence double precision
		) RETURNS double precision LANGUAGE SQL AS $$
			UPDATE semantic.mnemes
			SET confidence = GREATEST(0.025, evidence)
			WHERE id = mid
			RETURNING confidence;
		$$;

		-- Minimal stub of the ghola extension's recall_result composite
		-- type + recall(...) function. The production version (from
		-- the pg_ghola extension) applies ACT-R + Hebbian + Bayesian
		-- scoring; this stub just returns candidate rows with fixed
		-- placeholder scores so handler tests can verify request /
		-- response plumbing without booting a ghola-extension image.
		CREATE TYPE semantic.recall_result AS (
			mneme_id       uuid,
			score          double precision,
			content_match  double precision,
			activation     double precision,
			hebbian_boost  double precision,
			confidence     double precision,
			concept        text,
			content        text
		);

		CREATE OR REPLACE FUNCTION semantic.recall(
			ws        uuid,
			qtext     text,
			qembed    vector,
			limit_n   int,
			min_conf  double precision
		) RETURNS SETOF semantic.recall_result LANGUAGE SQL AS $$
			SELECT id, 0.5::double precision, 0.5::double precision,
			       0.0::double precision, 0.0::double precision,
			       confidence, concept, content
			FROM semantic.mnemes
			WHERE workspace_id = ws
			  AND confidence >= min_conf
			ORDER BY 1 - (embedding <=> qembed) DESC
			LIMIT limit_n;
		$$;
	`)
	require.NoError(t, err)

	repo := repository.New(pg.Pool)
	return &semanticFixture{
		pg:      pg,
		repo:    repo,
		handler: handler.NewSemanticHandler(repo),
	}
}

func authedSemanticRequest(t *testing.T, method, path string, body any, userID uuid.UUID) *http.Request {
	t.Helper()
	buf := new(bytes.Buffer)
	require.NoError(t, json.NewEncoder(buf).Encode(body))
	req := httptest.NewRequest(method, path, buf)
	req.Header.Set("Content-Type", "application/json")
	if userID != uuid.Nil {
		ctx := auth.WithContext(req.Context(), &auth.Context{UserID: userID})
		req = req.WithContext(ctx)
	}
	return req
}

// ---------------------------------------------------------------------
// /v1/semantic/query
// ---------------------------------------------------------------------

func TestSemanticQuery_Happy(t *testing.T) {
	f := newSemanticFixture(t)
	userID := uuid.New()
	workspace := uuid.New()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := f.pg.Pool.Exec(ctx, `
		INSERT INTO semantic.mnemes (workspace_id, concept, content, embedding)
		VALUES ($1, 'k8s', 'pod scheduling', '[0.1,0.1,0.1,0.1,0.1,0.1,0.1,0.1]'::vector)
	`, workspace)
	require.NoError(t, err)

	body := map[string]any{
		"workspace_id":    workspace,
		"query_text":      "k8s",
		"query_embedding": []float64{0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1, 0.1},
		"limit":           10,
	}
	req := authedSemanticRequest(t, http.MethodPost, "/v1/semantic/query", body, userID)
	rec := httptest.NewRecorder()
	f.handler.Query(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		Hits []map[string]any `json:"hits"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.NotEmpty(t, resp.Hits)
	assert.Equal(t, "semantic", resp.Hits[0]["tier"])
	assert.Equal(t, "k8s", resp.Hits[0]["concept"])
}

func TestSemanticQuery_Unauthenticated(t *testing.T) {
	f := newSemanticFixture(t)

	req := authedSemanticRequest(t, http.MethodPost, "/v1/semantic/query",
		map[string]any{"workspace_id": uuid.New()}, uuid.Nil)
	rec := httptest.NewRecorder()
	f.handler.Query(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// ---------------------------------------------------------------------
// /v1/semantic/feedback
// ---------------------------------------------------------------------

func TestSemanticFeedback_Happy(t *testing.T) {
	f := newSemanticFixture(t)
	userID := uuid.New()
	workspace := uuid.New()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var id uuid.UUID
	require.NoError(t, f.pg.Pool.QueryRow(ctx, `
		INSERT INTO semantic.mnemes (workspace_id, concept, content, embedding, confidence)
		VALUES ($1, 'k8s', 'pod scheduling', '[0.1,0.1,0.1,0.1,0.1,0.1,0.1,0.1]'::vector, 0.5)
		RETURNING id
	`, workspace).Scan(&id))

	body := map[string]any{
		"mneme_id": id,
		"evidence": 0.95,
	}
	req := authedSemanticRequest(t, http.MethodPost, "/v1/semantic/feedback", body, userID)
	rec := httptest.NewRecorder()
	f.handler.Feedback(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp struct {
		MnemeID    string  `json:"mneme_id"`
		Confidence float64 `json:"confidence"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, id.String(), resp.MnemeID)
	assert.Greater(t, resp.Confidence, 0.5)
}

func TestSemanticFeedback_Unauthenticated(t *testing.T) {
	f := newSemanticFixture(t)

	req := authedSemanticRequest(t, http.MethodPost, "/v1/semantic/feedback",
		map[string]any{"mneme_id": uuid.New(), "evidence": 0.5}, uuid.Nil)
	rec := httptest.NewRecorder()
	f.handler.Feedback(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

// ---------------------------------------------------------------------
// /v1/semantic/list
// ---------------------------------------------------------------------

func TestSemanticList_Happy(t *testing.T) {
	f := newSemanticFixture(t)
	userID := uuid.New()
	workspace := uuid.New()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for i := 0; i < 3; i++ {
		_, err := f.pg.Pool.Exec(ctx, `
			INSERT INTO semantic.mnemes (workspace_id, concept, content, embedding)
			VALUES ($1, 'c', 'x', '[0.1,0.1,0.1,0.1,0.1,0.1,0.1,0.1]'::vector)
		`, workspace)
		require.NoError(t, err)
	}

	body := map[string]any{
		"workspace_id": workspace,
		"limit":        10,
	}
	req := authedSemanticRequest(t, http.MethodPost, "/v1/semantic/list", body, userID)
	rec := httptest.NewRecorder()
	f.handler.List(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var resp struct {
		Mnemes []map[string]any `json:"mnemes"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Len(t, resp.Mnemes, 3)
}

func TestSemanticList_Unauthenticated(t *testing.T) {
	f := newSemanticFixture(t)

	req := authedSemanticRequest(t, http.MethodPost, "/v1/semantic/list",
		map[string]any{"workspace_id": uuid.New()}, uuid.Nil)
	rec := httptest.NewRecorder()
	f.handler.List(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
