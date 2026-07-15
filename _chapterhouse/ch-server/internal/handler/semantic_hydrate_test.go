package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
