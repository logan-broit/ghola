package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSessionWorkspace_200 pins the happy-path wire shape:
// {added: true} on a freshly tagged session.
func TestSessionWorkspace_200(t *testing.T) {
	f := newEpisodicFixture(t)
	owner, sessionID, workspaceID := uuid.New(), uuid.New(), uuid.New()

	_, err := f.pg.Pool.Exec(context.Background(), `
		INSERT INTO episodic.sessions (id, user_id, started_at, event_count)
		VALUES ($1, $2, now(), 0)`, sessionID, owner)
	require.NoError(t, err)

	req := authedRequest(t, http.MethodPost, "/v1/episodic/session_workspace",
		map[string]any{
			"user_id":      owner,
			"session_id":   sessionID,
			"workspace_id": workspaceID,
		}, owner)
	rec := httptest.NewRecorder()
	f.handler.AddSessionWorkspace(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp struct {
		Added bool `json:"added"`
	}
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.True(t, resp.Added)
}

// TestSessionWorkspace_403_OtherUser pins that a caller cannot tag a
// session they don't own — same per-user ACL the repo enforces.
func TestSessionWorkspace_403_OtherUser(t *testing.T) {
	f := newEpisodicFixture(t)
	owner, stranger, sessionID, workspaceID := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	_, err := f.pg.Pool.Exec(context.Background(), `
		INSERT INTO episodic.sessions (id, user_id, started_at, event_count)
		VALUES ($1, $2, now(), 0)`, sessionID, owner)
	require.NoError(t, err)

	req := authedRequest(t, http.MethodPost, "/v1/episodic/session_workspace",
		map[string]any{
			"user_id":      stranger,
			"session_id":   sessionID,
			"workspace_id": workspaceID,
		}, stranger)
	rec := httptest.NewRecorder()
	f.handler.AddSessionWorkspace(rec, req)
	assert.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
}

// TestSessionWorkspace_409_SessionMissing pins that a missing session
// row produces 409 with a body the calling agent can act on.
func TestSessionWorkspace_409_SessionMissing(t *testing.T) {
	f := newEpisodicFixture(t)
	user := uuid.New()

	req := authedRequest(t, http.MethodPost, "/v1/episodic/session_workspace",
		map[string]any{
			"user_id":      user,
			"session_id":   uuid.New(),
			"workspace_id": uuid.New(),
		}, user)
	rec := httptest.NewRecorder()
	f.handler.AddSessionWorkspace(rec, req)
	assert.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
}
