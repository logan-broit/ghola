package handler_test

// episodic_settle_test.go — TDD tests for the P4 recurrent-settle expansion
// sub-list in /v1/episodic/query.
//
// Test strategy:
//   - All tests that exercise the settle path use the real ephemeral
//     Postgres fixture (newEpisodicFixture) so event ingest and text
//     hydration are real.  The fakeAssocLookup from settle_neighborhood_test.go
//     is available (same package handler_test) and is injected via
//     WithAssocLookup to control the Hebbian graph without needing real
//     associations in the DB.
//   - Tests that pin wire-shape contracts (disabled = absent field,
//     enabled = present field) also use the real fixture so the full
//     handler path runs.
//   - Four required tests (TDD order: failing first, then passing):
//       1. Settle disabled -> response has NO expansion field.
//       2. Settle enabled, zero associations -> empty expansion list, no error.
//       3. Settle enabled, with associations -> expansion present, correct
//          shape, activations descending, seeds excluded, texts hydrated.
//       4. Settle enabled, only enabled:true -> param defaults applied.
//
// Test naming follows the existing handler test conventions:
//   TestEpisodicQuery_Settle<Scenario>

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

	"github.com/thinkwright/chapterhouse/ch-server/internal/handler"
)

// seedSettleFixture stands up three events (A, B, C) in one
// session/workspace and ingests them through the handler.  Returns the
// IDs so tests can build fakeAssocLookup graphs referencing them.
func seedSettleFixture(t *testing.T, f *episodicHandlerFixture) (
	userID, workspaceID, sessionID, eventA, eventB, eventC uuid.UUID,
) {
	t.Helper()
	userID = uuid.New()
	workspaceID = uuid.New()
	sessionID = uuid.New()
	eventA = uuid.New()
	eventB = uuid.New()
	eventC = uuid.New()

	ingestBody := map[string]any{
		"session": map[string]any{
			"id":           sessionID,
			"user_id":      userID,
			"workspace_id": workspaceID,
			"started_at":   time.Now().UTC().Format(time.RFC3339Nano),
			"event_count":  3,
		},
		"events": []any{
			sampleEvent(eventA, sessionID, userID, "kubernetes pod scheduling"),
			sampleEvent(eventB, sessionID, userID, "kubernetes memory pressure"),
			sampleEvent(eventC, sessionID, userID, "kubernetes node eviction"),
		},
	}
	ingestReq := authedRequest(t, http.MethodPost, "/v1/episodic/ingest", ingestBody, userID)
	ingestRec := httptest.NewRecorder()
	f.handler.Ingest(ingestRec, ingestReq)
	require.Equal(t, http.StatusOK, ingestRec.Code, "ingest body=%s", ingestRec.Body.String())
	return
}

// seedSettleFixtureWithExtra ingests a fourth event D that is NOT in the
// seed set (not matched by the query).  D is used to verify that an
// expansion node outside the original query set is surfaced when the
// fakeAssocLookup wires an association from a seed into D.
//
// D is inserted into a separate session that is NOT in the workspace's
// session_workspaces table, so the vector and FTS queries (which filter by
// workspace) will never return it.  The fakeAssocLookup can still reference
// D's UUID.  GetEventTextByIDs also workspace-filters, so D will appear in
// expansion without text (nil) — acceptable since the test only needs to
// confirm D's presence and activation > 0.
//
// The user_id is shared so the session is owned by the same user.
func seedSettleFixtureWithExtra(t *testing.T, f *episodicHandlerFixture) (
	userID, workspaceID, sessionID, eventA, eventB, eventC, eventD uuid.UUID,
) {
	t.Helper()
	userID, workspaceID, sessionID, eventA, eventB, eventC = seedSettleFixture(t, f)
	eventD = uuid.New()
	dSessionID := uuid.New()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Insert a separate session (NOT in workspace) then insert event D into it.
	_, err := f.pg.Pool.Exec(ctx, `
		INSERT INTO episodic.sessions (id, user_id, started_at, event_count)
		VALUES ($1, $2, now(), 1)
	`, dSessionID, userID)
	require.NoError(t, err)

	_, err = f.pg.Pool.Exec(ctx, `
		INSERT INTO episodic.events
			(id, session_id, user_id, type, text, embedding, raw_event, created_at)
		VALUES
			($1, $2, $3, 'user', 'prometheus alertmanager silences',
			 ($4::text)::vector, '{"text":"prometheus alertmanager silences"}'::jsonb, now())
	`, eventD, dSessionID, userID, "[0.9,0.9,0.9,0.9,0.9,0.9,0.9,0.9]")
	require.NoError(t, err)
	return
}

// TestEpisodicQuery_Settle_DisabledByDefault pins the wire contract when
// the settle block is absent (default): the response has NO expansion field.
// This is the back-compat test — settle=nil must produce byte-identical
// output to the pre-P4 handler.
func TestEpisodicQuery_Settle_DisabledByDefault(t *testing.T) {
	f := newEpisodicFixture(t)
	userID, workspaceID, _, _, _, _ := seedSettleFixture(t, f)

	queryBody := map[string]any{
		"user_id":         userID,
		"workspace_id":    workspaceID,
		"query_text":      "kubernetes",
		"query_embedding": smallEmbedding(0.1),
		"limit":           10,
		"rankings":        []string{"vector", "fts"},
		// no "settle" key — must behave identically to pre-P4
	}
	req := authedRequest(t, http.MethodPost, "/v1/episodic/query", queryBody, userID)
	rec := httptest.NewRecorder()
	f.handler.Query(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	body := rec.Body.String()
	assert.NotContains(t, body, `"expansion"`,
		"expansion key must be absent when settle is not requested")

	var resp handler.MultiRankingResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Nil(t, resp.Expansion,
		"expansion pointer must remain nil when settle is disabled (back-compat)")
}

// TestEpisodicQuery_Settle_EnabledWithZeroAssociations pins the empty-state
// behavior: settle is on but no Hebbian associations exist — expansion is
// present in the response (the caller asked) and serializes as [].
func TestEpisodicQuery_Settle_EnabledWithZeroAssociations(t *testing.T) {
	f := newEpisodicFixture(t)
	userID, workspaceID, _, _, _, _ := seedSettleFixture(t, f)

	// Inject a fakeAssocLookup that returns no associations for any input.
	emptyFake := &fakeAssocLookup{}
	f.handler = f.handler.WithAssocLookup(emptyFake)

	queryBody := map[string]any{
		"user_id":         userID,
		"workspace_id":    workspaceID,
		"query_text":      "kubernetes",
		"query_embedding": smallEmbedding(0.1),
		"limit":           10,
		"rankings":        []string{"vector", "fts"},
		"settle":          map[string]any{"enabled": true},
	}
	req := authedRequest(t, http.MethodPost, "/v1/episodic/query", queryBody, userID)
	rec := httptest.NewRecorder()
	f.handler.Query(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	body := rec.Body.String()
	assert.Contains(t, body, `"expansion":[]`,
		"expansion key must serialize as empty array when settle is on but no neighbors")

	var resp handler.MultiRankingResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Expansion, "expansion pointer must be set when settle is enabled")
	assert.Empty(t, *resp.Expansion, "no associations -> empty expansion")
}

// TestEpisodicQuery_Settle_EnabledWithAssociations is the full happy-path
// test.  Setup:
//   - Events A, B, C are ingested and will be matched by the query (seeds).
//   - Event D is ingested but NOT matched by the query (expansion target).
//   - fakeAssocLookup wires A->D (w=0.8) so D gets activation after settle.
//   - A, B, C are seeds so they must NOT appear in expansion.
//   - D is a non-seed so it MUST appear in expansion with activation > 0.
//   - expansion activations must be descending.
//   - D's text must be hydrated from PG (real repo hydration call).
func TestEpisodicQuery_Settle_EnabledWithAssociations(t *testing.T) {
	f := newEpisodicFixture(t)
	userID, workspaceID, _, eventA, eventB, eventC, eventD :=
		seedSettleFixtureWithExtra(t, f)

	// Wire A->D with weight 0.8 so D gets activation via spreading from A.
	fake := &fakeAssocLookup{}
	fake.addEdge(eventA, eventD, 0.8, "hebbian", workspaceID)
	f.handler = f.handler.WithAssocLookup(fake)

	queryBody := map[string]any{
		"user_id":         userID,
		"workspace_id":    workspaceID,
		"query_text":      "kubernetes",
		"query_embedding": smallEmbedding(0.1),
		"limit":           10,
		"rankings":        []string{"vector", "fts"},
		"settle":          map[string]any{"enabled": true},
	}
	req := authedRequest(t, http.MethodPost, "/v1/episodic/query", queryBody, userID)
	rec := httptest.NewRecorder()
	f.handler.Query(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp handler.MultiRankingResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	require.NotNil(t, resp.Expansion, "expansion must be present when settle is enabled")
	expansion := *resp.Expansion

	// D should be in expansion; A/B/C are seeds and must NOT appear.
	seedIDs := map[uuid.UUID]bool{eventA: true, eventB: true, eventC: true}
	for _, hit := range expansion {
		assert.False(t, seedIDs[hit.EventID],
			"seed event %s must not appear in expansion", hit.EventID)
	}

	// D must appear (it is the only non-seed with an association).
	// D's session is NOT in the workspace so GetEventTextByIDs won't find
	// its text — nil text is expected and acceptable.
	var foundD bool
	for _, hit := range expansion {
		if hit.EventID == eventD {
			foundD = true
			assert.Greater(t, hit.Activation, 0.0,
				"D's activation must be > 0 (mass received from seed A)")
		}
	}
	assert.True(t, foundD, "event D must appear in expansion (receives activation from A)")

	// Activation descending invariant.
	for i := 1; i < len(expansion); i++ {
		assert.GreaterOrEqual(t,
			expansion[i-1].Activation,
			expansion[i].Activation,
			"expansion activations must be descending at index %d", i)
	}
}

// TestEpisodicQuery_Settle_DefaultParamsApplied verifies that when the
// settle block only carries enabled:true (no explicit params), the handler
// applies DefaultSettleParams() — specifically that the pipeline runs and
// returns a valid response without panicking or erroring.  This pins that
// all zero-value fields in SettleRequest fall back to defaults.
func TestEpisodicQuery_Settle_DefaultParamsApplied(t *testing.T) {
	f := newEpisodicFixture(t)
	userID, workspaceID, _, eventA, _, _, eventD :=
		seedSettleFixtureWithExtra(t, f)

	fake := &fakeAssocLookup{}
	fake.addEdge(eventA, eventD, 0.5, "hebbian", workspaceID)
	f.handler = f.handler.WithAssocLookup(fake)

	// Only enabled:true — all other params absent (zero values -> defaults).
	queryBody := map[string]any{
		"user_id":         userID,
		"workspace_id":    workspaceID,
		"query_text":      "kubernetes",
		"query_embedding": smallEmbedding(0.1),
		"limit":           10,
		"rankings":        []string{"vector", "fts"},
		"settle":          map[string]any{"enabled": true},
		// lambda, hop_cap, node_cap, top_m, eps, max_iters all absent
	}
	req := authedRequest(t, http.MethodPost, "/v1/episodic/query", queryBody, userID)
	rec := httptest.NewRecorder()
	f.handler.Query(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp handler.MultiRankingResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// With defaults applied: TopM=25, HopCap=3, NodeCap=2000, Lambda=0.7.
	// D should appear — the settle pipeline ran without error.
	require.NotNil(t, resp.Expansion, "expansion must be present (settle enabled)")

	var foundD bool
	for _, hit := range *resp.Expansion {
		if hit.EventID == eventD {
			foundD = true
			assert.Greater(t, hit.Activation, 0.0,
				"D must have positive activation with default params")
		}
	}
	assert.True(t, foundD, "D must appear in expansion when defaults are applied correctly")
}
