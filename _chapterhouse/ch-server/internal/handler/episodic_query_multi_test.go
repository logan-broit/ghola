package handler_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/handler"
)

// TestMultiRankingRequest_RankingsRoundTrip ensures the multi-ranking
// request's Rankings discriminator field marshals to the documented JSON
// shape (`rankings: ["vector","fts"]`) and unmarshals back losslessly.
// Presence of this field is what A3 will use to switch /v1/episodic/query
// from legacy hybrid mode into multi-ranking mode.
func TestMultiRankingRequest_RankingsRoundTrip(t *testing.T) {
	req := handler.MultiRankingRequest{
		UserID:      uuid.New(),
		WorkspaceID: uuid.New(),
		QueryText:   "hello world",
		Limit:       10,
		Rankings:    []string{"vector", "fts"},
	}

	buf, err := json.Marshal(req)
	require.NoError(t, err)

	// JSON should carry the snake-case discriminator.
	if !strings.Contains(string(buf), `"rankings":["vector","fts"]`) {
		t.Fatalf("rankings key missing or wrong shape: %s", buf)
	}

	var back handler.MultiRankingRequest
	require.NoError(t, json.Unmarshal(buf, &back))
	assert.Equal(t, req.Rankings, back.Rankings)
	assert.Equal(t, req.QueryText, back.QueryText)
	assert.Equal(t, req.Limit, back.Limit)
}

// TestMultiRankingRequest_RankingsOmitemptyWhenAbsent guards the legacy
// vs multi-ranking discriminator: a request without Rankings must NOT
// emit a "rankings" key, so A3's "field present?" branch sees absence.
func TestMultiRankingRequest_RankingsOmitemptyWhenAbsent(t *testing.T) {
	req := handler.MultiRankingRequest{
		UserID:      uuid.New(),
		WorkspaceID: uuid.New(),
		QueryText:   "hello world",
		Limit:       10,
	}
	buf, err := json.Marshal(req)
	require.NoError(t, err)
	assert.NotContains(t, string(buf), "rankings")
}

// TestMultiRankingResponse_SubListsRoundTrip verifies the per-tier
// sub-lists `vector`, `fts`, `session_vector` round-trip through JSON
// with their exact lower-snake keys. These three keys are the recall
// fan-out's contract — any drift here breaks ghola.Recall in A5/A6.
func TestMultiRankingResponse_SubListsRoundTrip(t *testing.T) {
	eventID := uuid.New()
	sessionID := uuid.New()

	resp := handler.MultiRankingResponse{
		Vector: []handler.MultiRankingHit{{
			EventID:   &eventID,
			SessionID: &sessionID,
			Tier:      "vector",
		}},
		FTS: []handler.MultiRankingHit{{
			EventID:   &eventID,
			SessionID: &sessionID,
			Tier:      "fts",
		}},
		SessionVector: []handler.MultiRankingHit{{
			SessionID: &sessionID,
			Tier:      "session_vector",
		}},
	}

	buf, err := json.Marshal(resp)
	require.NoError(t, err)

	s := string(buf)
	assert.Contains(t, s, `"vector":`)
	assert.Contains(t, s, `"fts":`)
	assert.Contains(t, s, `"session_vector":`)

	var back handler.MultiRankingResponse
	require.NoError(t, json.Unmarshal(buf, &back))
	require.Len(t, back.Vector, 1)
	require.Len(t, back.FTS, 1)
	require.Len(t, back.SessionVector, 1)
	assert.Equal(t, "vector", back.Vector[0].Tier)
	assert.Equal(t, "fts", back.FTS[0].Tier)
	assert.Equal(t, "session_vector", back.SessionVector[0].Tier)
	require.NotNil(t, back.Vector[0].EventID)
	assert.Equal(t, eventID, *back.Vector[0].EventID)
}

// TestMultiRankingHit_OmitemptyOnSharedFields ensures the shared hit
// struct's `event_id`, `session_id`, `tier`, `score` keys all carry
// omit-on-zero semantics — session-vector tier has no event_id, and
// within a sub-list `tier` may be redundant. A zero-valued hit must
// NOT emit any of those four keys so empty tiers don't litter the
// wire with zero-uuid / zero-score noise.
//
// `score` is a value-type struct, so plain `,omitempty` is a no-op;
// A3 fixes the contract by either swapping in `,omitzero` (Go 1.24+)
// or making the field a pointer with `,omitempty`. This test pins
// whichever path A3 takes.
func TestMultiRankingHit_OmitemptyOnSharedFields(t *testing.T) {
	hit := handler.MultiRankingHit{}
	buf, err := json.Marshal(hit)
	require.NoError(t, err)
	s := string(buf)
	assert.NotContains(t, s, `"event_id"`, "event_id must be omitempty")
	assert.NotContains(t, s, `"session_id"`, "session_id must be omitempty")
	assert.NotContains(t, s, `"tier"`, "tier must be omitempty")
	assert.NotContains(t, s, `"score"`, "score must be omitted on zero-value hit")
}

// TestMultiRankingHit_PopulatedScoreAndChunkRoundTrip locks the wire
// shape on the two value-typed fields a non-empty hit fills in: Score
// and SessionChunkText. A3 is the first commit that actually populates
// these, so the round-trip contract gets pinned here alongside the
// handler work.
func TestMultiRankingHit_PopulatedScoreAndChunkRoundTrip(t *testing.T) {
	eventID := uuid.New()
	hit := handler.MultiRankingHit{
		EventID: &eventID,
		Tier:    "vector",
		Score: handler.ScoreBreakdown{
			Semantic: 0.91,
			FTS:      0.0,
			Merged:   0.91,
		},
		SessionChunkText: "user: hello\nassistant: hi",
	}
	buf, err := json.Marshal(hit)
	require.NoError(t, err)
	s := string(buf)
	// The score key must appear when the breakdown is non-zero.
	assert.Contains(t, s, `"score":`)
	assert.Contains(t, s, `"session_chunk_text":"user: hello\nassistant: hi"`)

	var back handler.MultiRankingHit
	require.NoError(t, json.Unmarshal(buf, &back))
	assert.InDelta(t, 0.91, back.Score.Semantic, 1e-9)
	assert.InDelta(t, 0.91, back.Score.Merged, 1e-9)
	assert.Equal(t, "user: hello\nassistant: hi", back.SessionChunkText)
}

// ---------------------------------------------------------------------
// /v1/episodic/query — multi-ranking handler (httptest + real PG)
//
// All tests below exercise the multi-ranking path through the same
// httptest fixture the rest of episodic_test.go uses (real ephemeral
// Postgres, real ingest, real query). Per-tier sub-lists must surface
// for each ranking the caller requested; tiers the caller did not ask
// for must be absent (or empty arrays) on the wire. An absent or
// empty `rankings` list is rejected with 400 (A8 deleted the legacy
// hybrid mode that used to handle empty `rankings`).
// ---------------------------------------------------------------------

// TestEpisodicQuery_MultiRanking pins the happy path: request all
// three rankings, all three sub-list fields surface, each is a
// non-nil array (may be empty if no matches for a tier — e.g.
// session_vector before mentat populates l1_embedding).
func TestEpisodicQuery_MultiRanking(t *testing.T) {
	f := newEpisodicFixture(t)
	userID := uuid.New()
	sessionID := uuid.New()
	eventID := uuid.New()
	workspaceID := uuid.New()

	// Seed one event so the vector + fts tiers have something to score.
	ingestBody := map[string]any{
		"session": map[string]any{
			"id":           sessionID,
			"user_id":      userID,
			"workspace_id": workspaceID,
			"started_at":   time.Now().UTC().Format(time.RFC3339Nano),
			"event_count":  1,
		},
		"events": []any{sampleEvent(eventID, sessionID, userID, "kubernetes pod scheduling")},
	}
	ingestReq := authedRequest(t, http.MethodPost, "/v1/episodic/ingest", ingestBody, userID)
	ingestRec := httptest.NewRecorder()
	f.handler.Ingest(ingestRec, ingestReq)
	require.Equal(t, http.StatusOK, ingestRec.Code, "ingest body=%s", ingestRec.Body.String())

	// Populate l1_embedding so the session_vector tier has a match.
	parts := make([]string, 8)
	for i := range parts {
		parts[i] = "0.1"
	}
	embLit := "[" + strings.Join(parts, ",") + "]"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := f.pg.Pool.Exec(ctx, `
		UPDATE episodic.sessions
		SET l1_embedding = ($1::text)::vector,
		    l1_chunk_text = 'user: kubernetes pod scheduling'
		WHERE id = $2`, embLit, sessionID)
	require.NoError(t, err)

	queryBody := map[string]any{
		"user_id":         userID,
		"workspace_id":    workspaceID,
		"query_text":      "kubernetes",
		"query_embedding": smallEmbedding(0.1),
		"limit":           10,
		"rankings":        []string{"vector", "fts", "session_vector"},
	}
	req := authedRequest(t, http.MethodPost, "/v1/episodic/query", queryBody, userID)
	rec := httptest.NewRecorder()
	f.handler.Query(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// Decode into the public response shape so the wire-shape assertion
	// is meaningful (vs. a generic map[string]any).
	var resp handler.MultiRankingResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	// Each requested tier must surface as a non-nil array. (Empty is
	// acceptable, but nil is a wire bug — clients iterate without nil-
	// checking.)
	require.NotNil(t, resp.Vector, "vector sub-list must be present (non-nil)")
	require.NotNil(t, resp.FTS, "fts sub-list must be present (non-nil)")
	require.NotNil(t, resp.SessionVector, "session_vector sub-list must be present (non-nil)")

	// The vector and fts tiers must each return at least one hit (the
	// seeded event) — defense-in-depth that the fan-out actually ran.
	require.NotEmpty(t, resp.Vector, "vector tier must hit the seeded event")
	require.NotEmpty(t, resp.FTS, "fts tier must hit the seeded event")
	require.NotEmpty(t, resp.SessionVector, "session_vector tier must hit the seeded session")

	// Per-hit invariants: tier name matches the bucket; event-grain
	// tiers populate event_id; session_vector tier populates session_id
	// (and may omit event_id since the unit is the session).
	for _, h := range resp.Vector {
		assert.Equal(t, "vector", h.Tier)
		require.NotNil(t, h.EventID, "vector hit must populate event_id")
	}
	for _, h := range resp.FTS {
		assert.Equal(t, "fts", h.Tier)
		require.NotNil(t, h.EventID, "fts hit must populate event_id")
	}
	for _, h := range resp.SessionVector {
		assert.Equal(t, "session_vector", h.Tier)
		require.NotNil(t, h.SessionID, "session_vector hit must populate session_id")
	}
}

// TestEpisodicQuery_MultiRanking_PartialRankings pins the per-tier
// opt-in: requesting only `vector` must populate the vector sub-list
// and leave fts / session_vector absent (or as empty arrays — both
// are wire-acceptable per omitempty on the response).
func TestEpisodicQuery_MultiRanking_PartialRankings(t *testing.T) {
	f := newEpisodicFixture(t)
	userID := uuid.New()
	sessionID := uuid.New()
	eventID := uuid.New()
	workspaceID := uuid.New()

	ingestBody := map[string]any{
		"session": map[string]any{
			"id":           sessionID,
			"user_id":      userID,
			"workspace_id": workspaceID,
			"started_at":   time.Now().UTC().Format(time.RFC3339Nano),
			"event_count":  1,
		},
		"events": []any{sampleEvent(eventID, sessionID, userID, "kubernetes pod scheduling")},
	}
	ingestReq := authedRequest(t, http.MethodPost, "/v1/episodic/ingest", ingestBody, userID)
	ingestRec := httptest.NewRecorder()
	f.handler.Ingest(ingestRec, ingestReq)
	require.Equal(t, http.StatusOK, ingestRec.Code, "ingest body=%s", ingestRec.Body.String())

	queryBody := map[string]any{
		"user_id":         userID,
		"workspace_id":    workspaceID,
		"query_embedding": smallEmbedding(0.1),
		"limit":           10,
		"rankings":        []string{"vector"},
	}
	req := authedRequest(t, http.MethodPost, "/v1/episodic/query", queryBody, userID)
	rec := httptest.NewRecorder()
	f.handler.Query(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// Wire-level: only `vector` should appear at the top level. fts +
	// session_vector must be omitted (omitempty on nil slices). Anchor
	// the assertions to the leading `{"…":` shape so the inner score
	// breakdown (`"fts":0` inside a hit) doesn't false-positive.
	body := rec.Body.String()
	assert.Contains(t, body, `{"vector":`)
	assert.NotContains(t, body, `,"fts":[`)
	assert.NotContains(t, body, `,"session_vector":[`)

	var resp handler.MultiRankingResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Vector, "vector tier must hit the seeded event")
	assert.Empty(t, resp.FTS, "fts tier was not requested; must be empty")
	assert.Empty(t, resp.SessionVector, "session_vector tier was not requested; must be empty")
}

// TestEpisodicQuery_MultiRanking_RejectsEmptyRankings pins the
// validation: an explicit empty list is invalid input (caller asked
// for new mode but supplied no tiers). 400 Bad Request, no fan-out.
func TestEpisodicQuery_MultiRanking_RejectsEmptyRankings(t *testing.T) {
	f := newEpisodicFixture(t)
	userID := uuid.New()
	workspaceID := uuid.New()

	queryBody := map[string]any{
		"user_id":         userID,
		"workspace_id":    workspaceID,
		"query_embedding": smallEmbedding(0.1),
		"limit":           10,
		"rankings":        []string{},
	}
	req := authedRequest(t, http.MethodPost, "/v1/episodic/query", queryBody, userID)
	rec := httptest.NewRecorder()
	f.handler.Query(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"empty rankings list must be rejected, body=%s", rec.Body.String())
}

// TestEpisodicQuery_MultiRanking_RejectsUnknownRanking pins the
// validation on tier names: an unknown member ("primitives" lands in
// D1, behind a flag) is rejected with 400. Mirrors the empty-list
// rejection above so the caller sees a single contract.
func TestEpisodicQuery_MultiRanking_RejectsUnknownRanking(t *testing.T) {
	f := newEpisodicFixture(t)
	userID := uuid.New()
	workspaceID := uuid.New()

	queryBody := map[string]any{
		"user_id":         userID,
		"workspace_id":    workspaceID,
		"query_embedding": smallEmbedding(0.1),
		"limit":           10,
		"rankings":        []string{"vector", "primitives"},
	}
	req := authedRequest(t, http.MethodPost, "/v1/episodic/query", queryBody, userID)
	rec := httptest.NewRecorder()
	f.handler.Query(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"unknown ranking must be rejected, body=%s", rec.Body.String())
}

// ---------------------------------------------------------------------
// D1: primitives flag — Hebbian-boosted fourth ranking
//
// `primitives: true` adds a fourth sub-list (`primitives`) to the
// response. Candidates are the union of vector + fts top-K event ids.
// Per-candidate boost = sum(weight) over associations whose dst is
// also in the candidate set. Sorted descending by boost; zero-boost
// entries are dropped (no in-set neighbors == no surface).
// ---------------------------------------------------------------------

// seedPrimitivesFixture stands up three events in one session/workspace
// and ingests them through the handler so the multi-ranking fan-out has
// matching vector + fts candidates. Returns the event IDs in seed order
// (A, B, C) so tests can wire associations between them.
func seedPrimitivesFixture(t *testing.T, f *episodicHandlerFixture) (
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
			sampleEvent(eventB, sessionID, userID, "kubernetes pod scheduling deeper"),
			sampleEvent(eventC, sessionID, userID, "kubernetes pod scheduling further"),
		},
	}
	ingestReq := authedRequest(t, http.MethodPost, "/v1/episodic/ingest", ingestBody, userID)
	ingestRec := httptest.NewRecorder()
	f.handler.Ingest(ingestRec, ingestReq)
	require.Equal(t, http.StatusOK, ingestRec.Code, "ingest body=%s", ingestRec.Body.String())
	return
}

// TestEpisodicQuery_PrimitivesTrue_ReturnsFourthRanking pins the
// happy path. With associations A->B (w=0.5) and A->C (w=0.7) and a
// candidate set that includes all three events, A's boost is the sum
// of in-set neighbor weights (1.2). B and C have no outgoing
// associations to other candidates so their boost is zero and they
// drop out of the primitives list. Result: a one-element primitives
// list with A on top.
func TestEpisodicQuery_PrimitivesTrue_ReturnsFourthRanking(t *testing.T) {
	f := newEpisodicFixture(t)
	userID, workspaceID, _, eventA, eventB, eventC := seedPrimitivesFixture(t, f)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Seed associations directly via SQL so we can control the weights
	// without going through the saturation formula. UpsertAssociation
	// would always start at the n=1 weight (~0.18); we want explicit
	// 0.5 and 0.7 so the sort order is unambiguous.
	_, err := f.pg.Pool.Exec(ctx, `
		INSERT INTO semantic.associations
			(src_event_id, dst_event_id, association_type, weight, co_activations, workspace_id)
		VALUES
			($1, $2, 'hebbian', 0.5, 1, $4),
			($1, $3, 'hebbian', 0.7, 1, $4)
	`, eventA, eventB, eventC, workspaceID)
	require.NoError(t, err)

	queryBody := map[string]any{
		"user_id":         userID,
		"workspace_id":    workspaceID,
		"query_text":      "kubernetes",
		"query_embedding": smallEmbedding(0.1),
		"limit":           10,
		"rankings":        []string{"vector", "fts", "session_vector"},
		"primitives":      true,
	}
	req := authedRequest(t, http.MethodPost, "/v1/episodic/query", queryBody, userID)
	rec := httptest.NewRecorder()
	f.handler.Query(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var resp handler.MultiRankingResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	require.NotNil(t, resp.Vector, "vector must be present")
	require.NotNil(t, resp.FTS, "fts must be present")
	require.NotNil(t, resp.Primitives, "primitives must be present when flag is true")
	prims := *resp.Primitives
	require.NotEmpty(t, prims, "primitives must surface A (boost > 0)")

	// A is the only event with in-set neighbors -> only A appears, with
	// boost = 0.5 + 0.7 = 1.2. Sort order is descending by boost.
	require.NotNil(t, prims[0].EventID)
	assert.Equal(t, eventA, *prims[0].EventID,
		"highest boost belongs to A (in-set neighbors B + C)")
	assert.InDelta(t, 1.2, prims[0].Score.Merged, 1e-9,
		"primitive score = sum of in-set neighbor weights")
	assert.Equal(t, "primitives", prims[0].Tier)
	require.NotNil(t, prims[0].Text, "primitive hit must carry the event text")
	assert.Equal(t, "kubernetes pod scheduling", *prims[0].Text)

	// Sort invariant: every following entry has score <= the one before.
	for i := 1; i < len(prims); i++ {
		assert.LessOrEqual(t,
			prims[i].Score.Merged,
			prims[i-1].Score.Merged,
			"primitives must be sorted by boost descending")
	}
}

// TestEpisodicQuery_PrimitivesFalse_NoPrimitivesField pins the wire
// contract when the flag is omitted (or set to false explicitly): the
// response carries vector / fts / session_vector but the `primitives`
// key is absent (omitempty drops the nil slice).
func TestEpisodicQuery_PrimitivesFalse_NoPrimitivesField(t *testing.T) {
	f := newEpisodicFixture(t)
	userID, workspaceID, _, _, _, _ := seedPrimitivesFixture(t, f)

	queryBody := map[string]any{
		"user_id":         userID,
		"workspace_id":    workspaceID,
		"query_text":      "kubernetes",
		"query_embedding": smallEmbedding(0.1),
		"limit":           10,
		"rankings":        []string{"vector", "fts"},
		"primitives":      false,
	}
	req := authedRequest(t, http.MethodPost, "/v1/episodic/query", queryBody, userID)
	rec := httptest.NewRecorder()
	f.handler.Query(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	body := rec.Body.String()
	assert.NotContains(t, body, `"primitives"`,
		"primitives key must be omitted when flag is false")

	var resp handler.MultiRankingResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Nil(t, resp.Primitives,
		"primitives pointer must remain nil when flag is false")
}

// TestEpisodicQuery_PrimitivesTrue_NoAssociations_EmptyPrimitivesList
// pins the empty-state behavior: when the flag is on but no
// associations exist for any candidate, the field is still present
// (the caller asked for it) and serializes as an empty array.
func TestEpisodicQuery_PrimitivesTrue_NoAssociations_EmptyPrimitivesList(t *testing.T) {
	f := newEpisodicFixture(t)
	userID, workspaceID, _, _, _, _ := seedPrimitivesFixture(t, f)

	// Sanity: no associations seeded.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var count int
	require.NoError(t, f.pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM semantic.associations WHERE workspace_id = $1`,
		workspaceID,
	).Scan(&count))
	require.Zero(t, count, "test setup invariant: no associations expected")

	queryBody := map[string]any{
		"user_id":         userID,
		"workspace_id":    workspaceID,
		"query_text":      "kubernetes",
		"query_embedding": smallEmbedding(0.1),
		"limit":           10,
		"rankings":        []string{"vector", "fts"},
		"primitives":      true,
	}
	req := authedRequest(t, http.MethodPost, "/v1/episodic/query", queryBody, userID)
	rec := httptest.NewRecorder()
	f.handler.Query(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	body := rec.Body.String()
	assert.Contains(t, body, `"primitives":[]`,
		"primitives key must serialize as an explicit empty array when flag is true")

	var resp handler.MultiRankingResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Primitives, "primitives pointer must be set when flag is true")
	assert.Empty(t, *resp.Primitives,
		"no in-set boosts -> primitives is empty")
}

