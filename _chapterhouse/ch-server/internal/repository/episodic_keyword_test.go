package repository_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"
)

// TestQueryEpisodicKeyword_RanksUniqueTermFirst pins the load-bearing
// invariant for the keyword tier: an event whose text contains a
// query-unique term must rank above events that don't contain the
// term at all. ts_rank_cd handles term frequency + cover density;
// this test gates that ghola's RRF fan-out can trust the ranked list.
//
// Seeded with three events:
//
//	ev-A: "we deployed kubernetes pods today" — exact match
//	ev-B: "tooling around the gitops workflow" — irrelevant
//	ev-C: "kubernetes is great"               — exact match, less density
//
// Query "kubernetes" should return ev-A and ev-C (both match), ranked
// by ts_rank_cd. ev-B doesn't match the tsquery and is excluded.
func TestQueryEpisodicKeyword_RanksUniqueTermFirst(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "1024")
	require.NoError(t, repository.ApplyMigrations(t.Context(), pg.Pool))

	repo := repository.New(pg.Pool)

	userID := uuid.New()
	workspaceID := uuid.New()
	sessionID := uuid.New()

	// Sessions row first (the FK on episodic.events.session_id).
	_, err := pg.Pool.Exec(t.Context(), `
		INSERT INTO episodic.sessions (id, user_id, started_at, event_count)
		VALUES ($1, $2, now(), 3)`, sessionID, userID)
	require.NoError(t, err)

	// Workspace scoping is required after migration 006: queries filter
	// by workspace_id, so the session must be a member of the workspace
	// the test queries with.
	_, err = pg.Pool.Exec(t.Context(), `
		INSERT INTO episodic.session_workspaces (session_id, workspace_id)
		VALUES ($1, $2)`, sessionID, workspaceID)
	require.NoError(t, err)

	type seed struct {
		id   uuid.UUID
		text string
	}
	evA := seed{uuid.New(), "we deployed kubernetes pods today"}
	evB := seed{uuid.New(), "tooling around the gitops workflow"}
	evC := seed{uuid.New(), "kubernetes is great"}

	for i, e := range []seed{evA, evB, evC} {
		_, err := pg.Pool.Exec(t.Context(), `
			INSERT INTO episodic.events (id, session_id, user_id, type, text, raw_event, created_at)
			VALUES ($1, $2, $3, 'user', $4, '{}'::jsonb, now() + make_interval(secs => $5::float))`,
			e.id, sessionID, userID, e.text, float64(i)*0.001)
		require.NoError(t, err)
	}

	hits, err := repo.QueryEpisodicKeyword(t.Context(), repository.EpisodicKeywordParams{
		UserID:      userID,
		WorkspaceID: workspaceID,
		QueryText:   "kubernetes",
		Limit:       10,
	})
	require.NoError(t, err)
	require.Len(t, hits, 2, "only kubernetes-bearing events match")

	gotIDs := []uuid.UUID{hits[0].Event.ID, hits[1].Event.ID}
	// Both kubernetes events appear; gitops event excluded.
	assert.Contains(t, gotIDs, evA.id)
	assert.Contains(t, gotIDs, evC.id)
	assert.NotContains(t, gotIDs, evB.id)

	// Ranking is fts-only; Semantic must be 0, Merged == FTS, both >0.
	for i, h := range hits {
		assert.Equal(t, 0.0, h.Semantic, "hit %d", i)
		assert.Equal(t, h.FTS, h.Merged, "hit %d", i)
		assert.Greater(t, h.FTS, 0.0, "hit %d has non-zero FTS score", i)
	}
}

// TestQueryEpisodicKeyword_ScopedToCaller verifies that another
// user's matching event isn't returned. Recall path's auth contract
// is per-user; the keyword tier must honor the same boundary.
func TestQueryEpisodicKeyword_ScopedToCaller(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "1024")
	require.NoError(t, repository.ApplyMigrations(t.Context(), pg.Pool))

	repo := repository.New(pg.Pool)

	owner := uuid.New()
	other := uuid.New()
	ownerSession := uuid.New()
	otherSession := uuid.New()
	// Both sessions live in the same workspace — the user_id boundary
	// is what must keep them apart. This is the harder case: a shared
	// workspace_id can't accidentally rescue the per-user filter.
	workspaceID := uuid.New()

	for _, s := range []struct {
		id, user uuid.UUID
	}{{ownerSession, owner}, {otherSession, other}} {
		_, err := pg.Pool.Exec(t.Context(), `
			INSERT INTO episodic.sessions (id, user_id, started_at, event_count)
			VALUES ($1, $2, now(), 1)`, s.id, s.user)
		require.NoError(t, err)
		_, err = pg.Pool.Exec(t.Context(), `
			INSERT INTO episodic.session_workspaces (session_id, workspace_id)
			VALUES ($1, $2)`, s.id, workspaceID)
		require.NoError(t, err)
	}

	insertEvent := func(user, sess uuid.UUID, text string) uuid.UUID {
		id := uuid.New()
		_, err := pg.Pool.Exec(t.Context(), `
			INSERT INTO episodic.events (id, session_id, user_id, type, text, raw_event, created_at)
			VALUES ($1, $2, $3, 'user', $4, '{}'::jsonb, now())`,
			id, sess, user, text)
		require.NoError(t, err)
		return id
	}
	ownerEv := insertEvent(owner, ownerSession, "kubernetes pods")
	_ = insertEvent(other, otherSession, "kubernetes pods") // must be excluded

	hits, err := repo.QueryEpisodicKeyword(t.Context(), repository.EpisodicKeywordParams{
		UserID:      owner,
		WorkspaceID: workspaceID,
		QueryText:   "kubernetes",
		Limit:       10,
	})
	require.NoError(t, err)
	require.Len(t, hits, 1, "other user's event must not leak")
	assert.Equal(t, ownerEv, hits[0].Event.ID)
}

// TestQueryEpisodicKeyword_LongQueryWebsearchSemantics pins the
// natural-language behavior of the FTS tier.
//
// The motivation: with plainto_tsquery, queries are compiled to a
// strict AND-of-all-stems tsquery — even the literal token "or" is
// treated as a stop word and folded into the AND chain. So a query
// like "deadlock or timeout" matches only events that contain BOTH
// stems. On real corpora (issue bodies, multi-sentence questions),
// this degenerates to "find the issue itself and nothing else"
// (FTS top-20 = 1 across bridge-32 spot-checks).
//
// websearch_to_tsquery parses the same input as a search-engine-style
// query: lowercase/uppercase "or" between tokens compiles to a real
// tsquery disjunction (`A | B`), and punctuation is sanitized rather
// than ignored or rejected. This is a strict superset of plainto's
// single-keyword behavior, plus the natural-language operators that
// matter on long inputs.
//
// This test seeds three events whose relevant terms are split across
// multiple events, then queries with a stop-word-laden, "X or Y"-shaped
// natural-language body. The assertion: under websearch, the two
// events that match either disjunct come back. Under plainto this
// would compile to `'deadlock' & 'timeout'` and return zero events.
func TestQueryEpisodicKeyword_LongQueryWebsearchSemantics(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "1024")
	require.NoError(t, repository.ApplyMigrations(t.Context(), pg.Pool))

	repo := repository.New(pg.Pool)

	userID := uuid.New()
	workspaceID := uuid.New()
	sessionID := uuid.New()

	_, err := pg.Pool.Exec(t.Context(), `
		INSERT INTO episodic.sessions (id, user_id, started_at, event_count)
		VALUES ($1, $2, now(), 3)`, sessionID, userID)
	require.NoError(t, err)
	_, err = pg.Pool.Exec(t.Context(), `
		INSERT INTO episodic.session_workspaces (session_id, workspace_id)
		VALUES ($1, $2)`, sessionID, workspaceID)
	require.NoError(t, err)

	type seed struct {
		id   uuid.UUID
		text string
	}
	// Query parses to `('bug' & 'deadlock') | 'timeout'` under
	// websearch's precedence (& binds tighter than |). Seeds chosen so:
	//   evA matches the LHS — has both "bug" and "deadlock".
	//   evB matches the RHS — has "timeout".
	//   evC has none of the relevant stems — must be excluded.
	evA := seed{uuid.New(), "filed a bug about a postgres deadlock during recovery"}
	evB := seed{uuid.New(), "we observed a request timeout reading from the upstream service"}
	evC := seed{uuid.New(), "tooling around the gitops workflow"}

	for i, e := range []seed{evA, evB, evC} {
		_, err := pg.Pool.Exec(t.Context(), `
			INSERT INTO episodic.events (id, session_id, user_id, type, text, raw_event, created_at)
			VALUES ($1, $2, $3, 'user', $4, '{}'::jsonb, now() + make_interval(secs => $5::float))`,
			e.id, sessionID, userID, e.text, float64(i)*0.001)
		require.NoError(t, err)
	}

	// Natural-language query with explicit disjunction. Stop words
	// ("the", "a") and punctuation are tolerated by both functions, but
	// the lowercase "or" is the discriminator: under plainto it's a
	// stop word that gets folded into the AND chain
	// (`'deadlock' & 'timeout'` — zero hits, no event contains both
	// stems). Under websearch it parses as a tsquery disjunction
	// (`'deadlock' | 'timeout'`), picking up both relevant events while
	// excluding the noise event.
	queryBody := `the bug: deadlock or timeout`

	hits, err := repo.QueryEpisodicKeyword(t.Context(), repository.EpisodicKeywordParams{
		UserID:      userID,
		WorkspaceID: workspaceID,
		QueryText:   queryBody,
		Limit:       10,
	})
	require.NoError(t, err)

	gotIDs := make(map[uuid.UUID]bool)
	for _, h := range hits {
		gotIDs[h.Event.ID] = true
	}
	// Both disjunct-matching events come back; the irrelevant noise
	// event does not. This is exactly the websearch behavior — under
	// plainto's AND-of-all-stems compilation no event matches because
	// none contain BOTH "deadlock" and "timeout".
	assert.True(t, gotIDs[evA.id], "deadlock event matches via websearch disjunction")
	assert.True(t, gotIDs[evB.id], "timeout event matches via websearch disjunction")
	assert.False(t, gotIDs[evC.id], "irrelevant event must not match")

	// Per-tier contract still holds: Semantic stays zero, Merged tracks FTS.
	for i, h := range hits {
		assert.Equal(t, 0.0, h.Semantic, "hit %d", i)
		assert.Equal(t, h.FTS, h.Merged, "hit %d", i)
		assert.Greater(t, h.FTS, 0.0, "hit %d has non-zero FTS score", i)
	}
}
