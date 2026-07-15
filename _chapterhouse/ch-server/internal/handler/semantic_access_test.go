package handler_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSemanticQuery_FiresAccessTracking pins Task 20 at the handler
// boundary: a semantic query fires the HOLA access bump for the mnemes it
// returned, without blocking the response on that DB write. The bump runs
// in a fire-and-forget goroutine (detached context), so the response must
// return promptly and the access_count reaches 1 only eventually — proving
// the handler -> Querier.TouchMnemes -> repo passthrough is wired.
func TestSemanticQuery_FiresAccessTracking(t *testing.T) {
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

	start := time.Now()
	f.handler.Query(rec, req)
	elapsed := time.Since(start)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	// The touch goroutine gets a 5s context ceiling; the response path does
	// no DB work past the recall query, so it must return well under that.
	// A generous 2s bound proves we don't synchronously wait on the bump
	// while staying CI-robust.
	assert.Less(t, elapsed, 2*time.Second, "response must not block on access tracking")

	// The access_count reaches 1 through the fire-and-forget goroutine.
	require.Eventually(t, func() bool {
		var n int
		if err := f.pg.Pool.QueryRow(ctx,
			`SELECT access_count FROM semantic.mnemes WHERE id = $1`, id).Scan(&n); err != nil {
			return false
		}
		return n == 1
	}, 3*time.Second, 25*time.Millisecond,
		"access_count must reach 1 via the handler's fire-and-forget touch")
}
