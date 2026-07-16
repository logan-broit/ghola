package backfill

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"
)

func TestSegment_SplitsOnGapsOverThreshold(t *testing.T) {
	base := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	events := []evt{
		{ID: "a", CreatedAt: base},
		{ID: "b", CreatedAt: base.Add(1 * time.Minute)}, // same episode
		{ID: "c", CreatedAt: base.Add(5 * time.Hour)},   // gap 5h-1m > 4h -> new episode
		{ID: "d", CreatedAt: base.Add(5*time.Hour + time.Minute)},
		{ID: "e", CreatedAt: base.Add(12 * time.Hour)}, // gap ~7h > 4h -> new episode
	}
	got := segment(events, 4*time.Hour)
	require.Len(t, got, 3, "three episodes")
	assert.Equal(t, []string{"a", "b"}, ids(got[0]))
	assert.Equal(t, []string{"c", "d"}, ids(got[1]))
	assert.Equal(t, []string{"e"}, ids(got[2]))
}

func TestSegment_EmptyIsNil(t *testing.T) {
	assert.Nil(t, segment(nil, 4*time.Hour))
}

func ids(g []evt) []string {
	out := make([]string, len(g))
	for i, e := range g {
		out[i] = e.ID
	}
	return out
}

func TestExecute_SegmentsSessionByGap(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "1024")
	ctx := context.Background()
	require.NoError(t, repository.ApplyMigrations(ctx, pg.Pool))

	sid := uuid.NewString()
	uid := uuid.NewString()
	wid := uuid.NewString()
	base := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

	// Original session with a non-null l1_embedding (to prove Execute
	// clears it) and one workspace.
	_, err := pg.Pool.Exec(ctx, `
		INSERT INTO episodic.sessions (id, user_id, started_at, event_count, cwd, l1_embedding)
		VALUES ($1::uuid, $2::uuid, $3, 5, '/tmp/dog',
		        ('[' || array_to_string(array_fill(0.1::float4, ARRAY[1024]), ',') || ']')::vector)`,
		sid, uid, base)
	require.NoError(t, err)
	_, err = pg.Pool.Exec(ctx,
		`INSERT INTO episodic.session_workspaces (session_id, workspace_id) VALUES ($1::uuid, $2::uuid)`,
		sid, wid)
	require.NoError(t, err)

	// Five events across three episodes (gaps > 4h between episodes).
	times := []time.Time{
		base, base.Add(time.Minute),
		base.Add(5 * time.Hour), base.Add(5*time.Hour + time.Minute),
		base.Add(12 * time.Hour),
	}
	var eventIDs []string
	for _, ts := range times {
		eid := uuid.NewString()
		eventIDs = append(eventIDs, eid)
		_, err := pg.Pool.Exec(ctx, `
			INSERT INTO episodic.events (id, session_id, user_id, type, text, raw_event, created_at)
			VALUES ($1::uuid, $2::uuid, $3::uuid, 'user', 'hi', '{}'::jsonb, $4)`,
			eid, sid, uid, ts)
		require.NoError(t, err)
	}

	plan, err := BuildPlan(ctx, pg.Pool, sid, 4*time.Hour)
	require.NoError(t, err)
	require.Len(t, plan.Segments, 3, "three episodes")
	require.True(t, plan.Segments[2].IsOriginal, "final segment keeps the original id")
	require.Equal(t, sid, plan.Segments[2].NewID)

	backupPath := filepath.Join(t.TempDir(), "backup.json")
	require.NoError(t, Execute(ctx, pg.Pool, plan, backupPath, ""))

	// Three sessions now, all closed.
	var sessions, open int
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE ended_at IS NULL)
		   FROM episodic.sessions`).Scan(&sessions, &open))
	assert.Equal(t, 3, sessions, "one row per segment")
	assert.Equal(t, 0, open, "every segment is closed")

	// Event counts sum to 5 and no event points at a missing session.
	var eventCountSum, dangling int
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT coalesce(sum(event_count),0) FROM episodic.sessions`).Scan(&eventCountSum))
	assert.Equal(t, 5, eventCountSum)
	require.NoError(t, pg.Pool.QueryRow(ctx, `
		SELECT count(*) FROM episodic.events e
		 WHERE NOT EXISTS (SELECT 1 FROM episodic.sessions s WHERE s.id = e.session_id)`).
		Scan(&dangling))
	assert.Equal(t, 0, dangling, "no orphaned events")

	// Original l1_embedding cleared; every session mirrors the workspace.
	var l1Null bool
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT l1_embedding IS NULL FROM episodic.sessions WHERE id = $1::uuid`, sid).Scan(&l1Null))
	assert.True(t, l1Null, "original l1_embedding cleared -> reconciler re-pools")
	var wsRows int
	require.NoError(t, pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM episodic.session_workspaces WHERE workspace_id = $1::uuid`, wid).Scan(&wsRows))
	assert.Equal(t, 3, wsRows, "workspace mirrored onto every segment")

	// Backup captured every event against its original session id.
	raw, err := os.ReadFile(backupPath)
	require.NoError(t, err)
	var backup map[string]string
	require.NoError(t, json.Unmarshal(raw, &backup))
	assert.Len(t, backup, 5)
	for _, eid := range eventIDs {
		assert.Equal(t, sid, backup[eid], "backup maps event -> original session")
	}
}
