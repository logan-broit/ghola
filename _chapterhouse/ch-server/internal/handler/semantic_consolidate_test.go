package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/auth"
	"github.com/thinkwright/chapterhouse/ch-server/internal/consolidation"
	"github.com/thinkwright/chapterhouse/ch-server/internal/handler"
	"github.com/thinkwright/chapterhouse/ch-server/internal/semantic"
)

// TestSemanticConsolidate_ValidWorkspace_ReturnsOK pins the happy path:
// a valid {workspace} body with a wired (fake) runner returns 200 +
// {"status":"ok"}, and the runner receives the parsed workspace id.
func TestSemanticConsolidate_ValidWorkspace_ReturnsOK(t *testing.T) {
	h := handler.NewSemanticHandler(&semantic.Querier{})
	userID := uuid.New()
	workspace := uuid.New()

	var gotWorkspace uuid.UUID
	h = h.WithConsolidateRunner(func(_ context.Context, ws uuid.UUID) error {
		gotWorkspace = ws
		return nil
	})

	req := authedSemanticRequest(t, http.MethodPost, "/v1/semantic/consolidate",
		map[string]any{"workspace": workspace}, userID)
	rec := httptest.NewRecorder()
	h.Consolidate(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, workspace, gotWorkspace)

	var resp map[string]string
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.Equal(t, "ok", resp["status"])
}

// TestSemanticConsolidate_MissingWorkspace_ReturnsBadRequest pins the
// zero-value guard: an omitted (or explicit-nil) workspace must 400
// before the runner is ever invoked.
func TestSemanticConsolidate_MissingWorkspace_ReturnsBadRequest(t *testing.T) {
	h := handler.NewSemanticHandler(&semantic.Querier{})
	called := false
	h = h.WithConsolidateRunner(func(_ context.Context, _ uuid.UUID) error {
		called = true
		return nil
	})

	req := authedSemanticRequest(t, http.MethodPost, "/v1/semantic/consolidate",
		map[string]any{}, uuid.New())
	rec := httptest.NewRecorder()
	h.Consolidate(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.False(t, called, "runner must not fire when workspace is missing")
}

// TestSemanticConsolidate_InvalidJSON_ReturnsBadRequest pins the decode
// failure path (e.g. workspace is not a UUID string).
func TestSemanticConsolidate_InvalidJSON_ReturnsBadRequest(t *testing.T) {
	h := handler.NewSemanticHandler(&semantic.Querier{})
	h = h.WithConsolidateRunner(func(_ context.Context, _ uuid.UUID) error {
		return nil
	})

	req := authedSemanticRequest(t, http.MethodPost, "/v1/semantic/consolidate",
		map[string]any{"workspace": "not-a-uuid"}, uuid.New())
	rec := httptest.NewRecorder()
	h.Consolidate(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// TestSemanticConsolidate_RunnerError_ReturnsInternalError pins the
// loud-fail path: a runner error (e.g. mentat-down) surfaces as 500,
// not a silently swallowed 200.
func TestSemanticConsolidate_RunnerError_ReturnsInternalError(t *testing.T) {
	h := handler.NewSemanticHandler(&semantic.Querier{})
	h = h.WithConsolidateRunner(func(_ context.Context, _ uuid.UUID) error {
		return errors.New("mentat cluster (loud-fail): connection refused")
	})

	req := authedSemanticRequest(t, http.MethodPost, "/v1/semantic/consolidate",
		map[string]any{"workspace": uuid.New()}, uuid.New())
	rec := httptest.NewRecorder()
	h.Consolidate(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

// TestSemanticConsolidate_RunnerNotConfigured_ReturnsServiceUnavailable
// pins the nil-runner default: a deployment that never wired a runner (no
// WithConsolidateRunner call) must not panic — it 503s (the feature is
// unconfigured, not a server fault), so callers can distinguish a
// deploy-time gap from a genuine 500.
func TestSemanticConsolidate_RunnerNotConfigured_ReturnsServiceUnavailable(t *testing.T) {
	h := handler.NewSemanticHandler(&semantic.Querier{})

	req := authedSemanticRequest(t, http.MethodPost, "/v1/semantic/consolidate",
		map[string]any{"workspace": uuid.New()}, uuid.New())
	rec := httptest.NewRecorder()
	h.Consolidate(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

// TestSemanticConsolidate_BusyLock_ReturnsConflict pins the concurrency
// guard's HTTP mapping: when the runner reports the workspace is already
// consolidating (consolidation.ErrConsolidationBusy from the advisory
// lock), the handler must return 409 Conflict — a retryable signal — not
// a 500 that reads as a server fault.
func TestSemanticConsolidate_BusyLock_ReturnsConflict(t *testing.T) {
	h := handler.NewSemanticHandler(&semantic.Querier{})
	h = h.WithConsolidateRunner(func(_ context.Context, _ uuid.UUID) error {
		return consolidation.ErrConsolidationBusy
	})

	req := authedSemanticRequest(t, http.MethodPost, "/v1/semantic/consolidate",
		map[string]any{"workspace": uuid.New()}, uuid.New())
	rec := httptest.NewRecorder()
	h.Consolidate(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code, "body=%s", rec.Body.String())
}

// TestSemanticConsolidate_RunDetachedFromRequestContext pins Finding 1a:
// the run must NOT be aborted when the client disconnects or its request
// times out. The handler detaches the run from the request context
// (context.WithoutCancel), so cancelling the request context leaves the
// runner's context live — the batch completes server-side and an
// idempotent re-trigger converges. The runner's context must still carry
// request-scoped values (auth), proving it's a WithoutCancel derivation,
// not a bare context.Background().
func TestSemanticConsolidate_RunDetachedFromRequestContext(t *testing.T) {
	userID := uuid.New()
	h := handler.NewSemanticHandler(&semantic.Querier{})

	started := make(chan struct{})
	release := make(chan struct{})
	var runCtx context.Context
	h = h.WithConsolidateRunner(func(ctx context.Context, _ uuid.UUID) error {
		runCtx = ctx
		close(started)
		<-release // hold the run open so we can cancel the request mid-flight
		return nil
	})

	base := authedSemanticRequest(t, http.MethodPost, "/v1/semantic/consolidate",
		map[string]any{"workspace": uuid.New()}, userID)
	ctx, cancel := context.WithCancel(base.Context())
	req := base.WithContext(ctx)

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.Consolidate(rec, req)
		close(done)
	}()

	<-started
	cancel() // client disconnect / timeout: the request context is now done
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, runCtx.Err(), "run context must survive request cancellation")
	assert.Equal(t, userID, auth.UserIDFromContext(runCtx),
		"detached run context must still carry request-scoped auth values")

	close(release)
	<-done
}

// TestSemanticConsolidate_Unauthenticated_ReturnsUnauthorized mirrors
// TestSemanticQuery_Unauthenticated: the auth gate must short-circuit
// before the body is read or the runner is invoked.
func TestSemanticConsolidate_Unauthenticated_ReturnsUnauthorized(t *testing.T) {
	h := handler.NewSemanticHandler(&semantic.Querier{})
	called := false
	h = h.WithConsolidateRunner(func(_ context.Context, _ uuid.UUID) error {
		called = true
		return nil
	})

	req := authedSemanticRequest(t, http.MethodPost, "/v1/semantic/consolidate",
		map[string]any{"workspace": uuid.New()}, uuid.Nil)
	rec := httptest.NewRecorder()
	h.Consolidate(rec, req)

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.False(t, called)
}
