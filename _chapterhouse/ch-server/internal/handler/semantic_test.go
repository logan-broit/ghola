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
	"github.com/thinkwright/chapterhouse/ch-server/internal/mentat"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/semantic"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"
)

// semanticFixture wires the v0.3 read path end-to-end against an
// ephemeral Postgres + a fake mentat HTTP server. The migration runner
// owns schema creation; tests INSERT mnemes against the v0.3 shape
// directly.
type semanticFixture struct {
	pg      *testutil.EphemeralPostgres
	mentat  *httptest.Server
	repo    *repository.Repository
	handler *handler.SemanticHandler
}

// newSemanticFixture spins up a Postgres + a fake mentat that returns
// `pooled = first event embedding`. That deterministic identity pool
// means tests can put a known vector in `query_embedding` and reason
// directly about cosine ordering against the seeded mnemes.
func newSemanticFixture(t *testing.T) *semanticFixture {
	t.Helper()
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "8")
	require.NoError(t, repository.ApplyMigrations(context.Background(), pg.Pool))

	// Fake mentat: echo the (last) event's embedding back as `pooled`.
	// We pick the *last* event so the query embedding wins over any
	// recent_context the test might prepend; it matches the production
	// invariant that the query is the strongest pull on the probe vector.
	mentatSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pool" {
			http.NotFound(w, r)
			return
		}
		var req mentat.PoolRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		require.NotEmpty(t, req.Events)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(mentat.PoolResponse{
			Embedding: req.Events[len(req.Events)-1].Embedding,
		})
	}))
	t.Cleanup(mentatSrv.Close)

	repo := repository.New(pg.Pool)
	mentatClient := mentat.NewClient(mentatSrv.URL, nil)
	q := semantic.NewQuerier(repo, mentatClient, nil)

	return &semanticFixture{
		pg:      pg,
		mentat:  mentatSrv,
		repo:    repo,
		handler: handler.NewSemanticHandler(q),
	}
}

// insertMneme writes a v0.3-shaped row directly. Returns the new id.
func (f *semanticFixture) insertMneme(t *testing.T, ctx context.Context, workspace uuid.UUID, emb []float64) uuid.UUID {
	t.Helper()
	parts := make([]string, len(emb))
	for i, x := range emb {
		parts[i] = fmt.Sprintf("%g", x)
	}
	lit := "[" + strings.Join(parts, ",") + "]"

	var id uuid.UUID
	require.NoError(t, f.pg.Pool.QueryRow(ctx, `
		INSERT INTO semantic.mnemes (workspace_id, level, embedding)
		VALUES ($1, 1, ($2::text)::vector)
		RETURNING id`, workspace, lit).Scan(&id))
	return id
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

// TestSemanticQuery_ReturnsHitsForSimilarEmbedding pins the
// cosine-ordering behavior end-to-end. Two mnemes are seeded with
// known embeddings; the query embedding is closer to one than the
// other; the response must order them by similarity (highest score
// first). The fake mentat returns pooled = query_embedding so the
// repository's cosine ordering is exercised directly.
func TestSemanticQuery_ReturnsHitsForSimilarEmbedding(t *testing.T) {
	f := newSemanticFixture(t)
	userID := uuid.New()
	workspace := uuid.New()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	near := []float64{1, 0, 0, 0, 0, 0, 0, 0}
	far := []float64{0, 1, 0, 0, 0, 0, 0, 0}
	nearID := f.insertMneme(t, ctx, workspace, near)
	farID := f.insertMneme(t, ctx, workspace, far)

	body := map[string]any{
		"workspace_id":    workspace,
		"query_embedding": []float32{0.99, 0.01, 0, 0, 0, 0, 0, 0},
		"limit":           10,
	}
	req := authedSemanticRequest(t, http.MethodPost, "/v1/semantic/query", body, userID)
	rec := httptest.NewRecorder()
	f.handler.Query(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		Hits []struct {
			MnemeID uuid.UUID `json:"mneme_id"`
			Score   float64   `json:"score"`
			Level   int       `json:"level"`
			Tier    string    `json:"tier"`
		} `json:"hits"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Hits, 2)
	assert.Equal(t, nearID, resp.Hits[0].MnemeID, "nearer mneme must rank first")
	assert.Equal(t, farID, resp.Hits[1].MnemeID)
	assert.Greater(t, resp.Hits[0].Score, resp.Hits[1].Score)
	assert.Equal(t, "semantic", resp.Hits[0].Tier)
	assert.Equal(t, 1, resp.Hits[0].Level)
}

// TestSemanticQuery_Unauthenticated pins the auth gate. No mentat
// call should fire; the handler must short-circuit on missing
// auth.Context before the body is even read.
func TestSemanticQuery_Unauthenticated(t *testing.T) {
	f := newSemanticFixture(t)

	req := authedSemanticRequest(t, http.MethodPost, "/v1/semantic/query",
		map[string]any{"workspace_id": uuid.New()}, uuid.Nil)
	rec := httptest.NewRecorder()
	f.handler.Query(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}
