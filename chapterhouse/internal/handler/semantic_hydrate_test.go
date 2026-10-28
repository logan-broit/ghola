package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSemanticQuery_WireResponseCarriesContent pins Task 18 at the wire
// boundary: a labelled mneme with a representative excerpt must serialize
// `label` + `content` (content = label + "\n" + top excerpt) so ghola's
// recall path finally sees readable text on semantic hits.
func TestSemanticQuery_WireResponseCarriesContent(t *testing.T) {
	f := newSemanticFixture(t)
	userID := uuid.New()
	workspace := uuid.New()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Seed a labelled mneme with a representative excerpt directly.
	var id uuid.UUID
	require.NoError(t, f.pg.Pool.QueryRow(ctx, `
		INSERT INTO semantic.mnemes (workspace_id, level, embedding, label, representatives)
		VALUES ($1, 1, ($2::text)::vector, $3, $4::jsonb)
		RETURNING id`,
		workspace, "[1,0,0,0,0,0,0,0]", "cluster label",
		`[{"excerpt":"the top excerpt"}]`).Scan(&id))

	body := map[string]any{
		"workspace_id":    workspace,
		"query_embedding": []float32{1, 0, 0, 0, 0, 0, 0, 0},
		"limit":           10,
	}
	req := authedSemanticRequest(t, http.MethodPost, "/v1/semantic/query", body, userID)
	rec := httptest.NewRecorder()
	f.handler.Query(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		Hits []struct {
			MnemeID uuid.UUID `json:"mneme_id"`
			Label   string    `json:"label"`
			Content string    `json:"content"`
		} `json:"hits"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Hits, 1)
	assert.Equal(t, id, resp.Hits[0].MnemeID)
	assert.Equal(t, "cluster label", resp.Hits[0].Label)
	assert.Equal(t, "cluster label\nthe top excerpt", resp.Hits[0].Content)
}

// TestSemanticQuery_Level2DigestSurfacesParagraphAsContent pins the
// level-2 case the consolidation design turns on: a workspace digest
// carries its paragraph in `label` with NO representatives (see
// repository.InsertDigestMneme). Hydration must surface that paragraph as
// content — the excerpt jsonb-path resolves to "" on a null representatives
// column, so content is the label alone (no stray leading newline).
func TestSemanticQuery_Level2DigestSurfacesParagraphAsContent(t *testing.T) {
	f := newSemanticFixture(t)
	userID := uuid.New()
	workspace := uuid.New()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	paragraph := "This workspace spent the week on consolidation: clustering, enrichment, recall hydration."
	var id uuid.UUID
	require.NoError(t, f.pg.Pool.QueryRow(ctx, `
		INSERT INTO semantic.mnemes (workspace_id, level, embedding, label)
		VALUES ($1, 2, ($2::text)::vector, $3)
		RETURNING id`,
		workspace, "[1,0,0,0,0,0,0,0]", paragraph).Scan(&id))

	body := map[string]any{
		"workspace_id":    workspace,
		"query_embedding": []float32{1, 0, 0, 0, 0, 0, 0, 0},
		"limit":           10,
	}
	req := authedSemanticRequest(t, http.MethodPost, "/v1/semantic/query", body, userID)
	rec := httptest.NewRecorder()
	f.handler.Query(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		Hits []struct {
			MnemeID uuid.UUID `json:"mneme_id"`
			Level   int       `json:"level"`
			Content string    `json:"content"`
		} `json:"hits"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Hits, 1)
	assert.Equal(t, id, resp.Hits[0].MnemeID)
	assert.Equal(t, 2, resp.Hits[0].Level)
	assert.Equal(t, paragraph, resp.Hits[0].Content,
		"digest paragraph surfaces as content, with no excerpt and no leading newline")
}

// TestSemanticQuery_BoundsOversizedMultibyteContent drives an over-cap
// multibyte label through the real hydrated-hit path (a seeded mneme,
// not a unit call on semanticHitContent in isolation) and pins that the
// wire `content` is bounded at semanticContentCap (800 bytes) and never
// invalid UTF-8. "世" is 3 bytes, so 400 runes (1200 bytes) both exceeds
// the cap and does not land on a byte boundary at 800.
func TestSemanticQuery_BoundsOversizedMultibyteContent(t *testing.T) {
	f := newSemanticFixture(t)
	userID := uuid.New()
	workspace := uuid.New()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	long := strings.Repeat("世", 400)
	var id uuid.UUID
	require.NoError(t, f.pg.Pool.QueryRow(ctx, `
		INSERT INTO semantic.mnemes (workspace_id, level, embedding, label)
		VALUES ($1, 2, ($2::text)::vector, $3)
		RETURNING id`,
		workspace, "[1,0,0,0,0,0,0,0]", long).Scan(&id))

	body := map[string]any{
		"workspace_id":    workspace,
		"query_embedding": []float32{1, 0, 0, 0, 0, 0, 0, 0},
		"limit":           10,
	}
	req := authedSemanticRequest(t, http.MethodPost, "/v1/semantic/query", body, userID)
	rec := httptest.NewRecorder()
	f.handler.Query(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		Hits []struct {
			MnemeID uuid.UUID `json:"mneme_id"`
			Content string    `json:"content"`
		} `json:"hits"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Hits, 1)
	assert.Equal(t, id, resp.Hits[0].MnemeID)
	assert.True(t, utf8.ValidString(resp.Hits[0].Content),
		"bounded content must be valid UTF-8, got %q", resp.Hits[0].Content)
	assert.LessOrEqual(t, len(resp.Hits[0].Content), 800)
	assert.NotEmpty(t, resp.Hits[0].Content)
}

// TestSemanticQuery_ContentLessMnemeOmitsLabelAndContentKeys pins the
// omitempty contract on the wire: a pre-consolidation (content-less)
// mneme — no label, no representatives — must serialize a hit with the
// `label` and `content` keys ABSENT entirely, not present-and-empty. This
// is what keeps the wire byte-identical to the pre-consolidation shape
// for old rows; a regression to non-pointer/non-omitempty fields would
// instead emit `"label":"","content":""`.
func TestSemanticQuery_ContentLessMnemeOmitsLabelAndContentKeys(t *testing.T) {
	f := newSemanticFixture(t)
	userID := uuid.New()
	workspace := uuid.New()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	id := f.insertMneme(t, ctx, workspace, []float64{1, 0, 0, 0, 0, 0, 0, 0})

	body := map[string]any{
		"workspace_id":    workspace,
		"query_embedding": []float32{1, 0, 0, 0, 0, 0, 0, 0},
		"limit":           10,
	}
	req := authedSemanticRequest(t, http.MethodPost, "/v1/semantic/query", body, userID)
	rec := httptest.NewRecorder()
	f.handler.Query(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		Hits []map[string]json.RawMessage `json:"hits"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	require.Len(t, resp.Hits, 1)
	assert.Equal(t, id.String(), strings.Trim(string(resp.Hits[0]["mneme_id"]), `"`))
	_, hasLabel := resp.Hits[0]["label"]
	_, hasContent := resp.Hits[0]["content"]
	assert.False(t, hasLabel, "label key must be absent entirely for a content-less mneme")
	assert.False(t, hasContent, "content key must be absent entirely for a content-less mneme")
}
