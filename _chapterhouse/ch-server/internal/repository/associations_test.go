package repository_test

import (
	"context"
	"math"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"
)

// seedEventForAssoc inserts a minimal episodic.events row with the
// given id (and a freshly-minted session_id wrapped around userID) so
// the FKs on semantic.associations.{src,dst}_event_id are satisfied.
// The associations table is the first place in repo tests that needs
// real events to exist, so the helper lives here rather than in the
// queue-only B4 test file.
func seedEventForAssoc(t *testing.T, repo *repository.Repository, eventID, userID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	sessionID := uuid.New()
	_, err := repo.Pool().Exec(ctx, `
		INSERT INTO episodic.sessions (id, user_id, started_at, event_count)
		VALUES ($1, $2, now(), 1)
	`, sessionID, userID)
	require.NoError(t, err)
	_, err = repo.Pool().Exec(ctx, `
		INSERT INTO episodic.events (id, session_id, user_id, type, text, raw_event, created_at)
		VALUES ($1, $2, $3, 'user', 'seed', '{}'::jsonb, now())
	`, eventID, sessionID, userID)
	require.NoError(t, err)
}

// newAssociationsRepo boots a fresh ephemeral Postgres + applies
// migrations and returns a Repository scoped to that pool. Each test
// gets its own DB so queue rows can't leak across tests.
func newAssociationsRepo(t *testing.T) *repository.Repository {
	t.Helper()
	pg := testutil.NewEphemeralPostgres(t)
	t.Setenv("EMBEDDING_DIM", "1024")
	require.NoError(t, repository.ApplyMigrations(t.Context(), pg.Pool))
	return repository.New(pg.Pool)
}

// TestEnqueueCoActivations_BulkInsert pins the bulk-insert path: every
// pair handed in lands as a row in semantic.co_activation_queue with
// the same (src, dst, workspace) tuple. enqueued_at is set by the DB
// default; the test asserts it's populated, not its precise value.
func TestEnqueueCoActivations_BulkInsert(t *testing.T) {
	repo := newAssociationsRepo(t)
	ctx := t.Context()

	workspaceID := uuid.New()
	pairs := []repository.CoActivationPair{
		{SrcEventID: uuid.New(), DstEventID: uuid.New(), WorkspaceID: workspaceID},
		{SrcEventID: uuid.New(), DstEventID: uuid.New(), WorkspaceID: workspaceID},
		{SrcEventID: uuid.New(), DstEventID: uuid.New(), WorkspaceID: workspaceID},
	}

	require.NoError(t, repo.EnqueueCoActivations(ctx, pairs))

	rows, err := repo.Pool().Query(ctx, `
		SELECT src_event_id, dst_event_id, workspace_id, enqueued_at
		FROM semantic.co_activation_queue
		ORDER BY src_event_id
	`)
	require.NoError(t, err)
	defer rows.Close()

	type row struct {
		src, dst, ws uuid.UUID
		enqueuedAt   time.Time
	}
	var got []row
	for rows.Next() {
		var r row
		require.NoError(t, rows.Scan(&r.src, &r.dst, &r.ws, &r.enqueuedAt))
		got = append(got, r)
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, 3, "all 3 pairs should land as queue rows")

	want := append([]repository.CoActivationPair(nil), pairs...)
	sort.Slice(want, func(i, j int) bool {
		return want[i].SrcEventID.String() < want[j].SrcEventID.String()
	})
	for i := range got {
		require.Equal(t, want[i].SrcEventID, got[i].src)
		require.Equal(t, want[i].DstEventID, got[i].dst)
		require.Equal(t, want[i].WorkspaceID, got[i].ws)
		require.False(t, got[i].enqueuedAt.IsZero(), "enqueued_at default must populate")
	}
}

// TestEnqueueCoActivations_EmptyNoOp pins the fast-path: an empty pairs
// slice must not hit the DB and must return nil. Asserted by checking
// that the queue stays empty afterwards.
func TestEnqueueCoActivations_EmptyNoOp(t *testing.T) {
	repo := newAssociationsRepo(t)
	ctx := t.Context()

	require.NoError(t, repo.EnqueueCoActivations(ctx, nil))
	require.NoError(t, repo.EnqueueCoActivations(ctx, []repository.CoActivationPair{}))

	var count int
	require.NoError(t, repo.Pool().QueryRow(ctx,
		`SELECT count(*) FROM semantic.co_activation_queue`,
	).Scan(&count))
	require.Equal(t, 0, count)
}

// TestDrainCoActivationQueue_ReturnsOldestN pre-populates 5 pairs with
// monotonic enqueued_at, drains with batchSize=3, and asserts the
// oldest 3 come back in lockstep with their row IDs.
func TestDrainCoActivationQueue_ReturnsOldestN(t *testing.T) {
	repo := newAssociationsRepo(t)
	ctx := t.Context()

	workspaceID := uuid.New()
	pairs := make([]repository.CoActivationPair, 5)
	for i := range pairs {
		pairs[i] = repository.CoActivationPair{
			SrcEventID:  uuid.New(),
			DstEventID:  uuid.New(),
			WorkspaceID: workspaceID,
		}
	}

	// Insert one at a time with an explicit enqueued_at so order is
	// deterministic regardless of clock resolution. We sidestep the
	// public Enqueue path here so this test is independent of its
	// implementation (it could not-yet-exist when this test runs).
	base := time.Now().Add(-1 * time.Hour)
	for i, p := range pairs {
		_, err := repo.Pool().Exec(ctx, `
			INSERT INTO semantic.co_activation_queue
				(src_event_id, dst_event_id, workspace_id, enqueued_at)
			VALUES ($1, $2, $3, $4)
		`, p.SrcEventID, p.DstEventID, p.WorkspaceID, base.Add(time.Duration(i)*time.Minute))
		require.NoError(t, err)
	}

	got, ids, err := repo.DrainCoActivationQueue(ctx, 3)
	require.NoError(t, err)
	require.Len(t, got, 3)
	require.Len(t, ids, 3, "ids must be returned in lockstep with pairs")

	// Oldest 3 pairs are the first 3 we inserted.
	for i := 0; i < 3; i++ {
		require.Equal(t, pairs[i].SrcEventID, got[i].SrcEventID,
			"drain must return oldest enqueued_at first; index %d", i)
		require.Equal(t, pairs[i].DstEventID, got[i].DstEventID)
		require.Equal(t, pairs[i].WorkspaceID, got[i].WorkspaceID)
	}

	// Drain does NOT delete: rows still present.
	var count int
	require.NoError(t, repo.Pool().QueryRow(ctx,
		`SELECT count(*) FROM semantic.co_activation_queue`,
	).Scan(&count))
	require.Equal(t, 5, count, "drain must not delete rows")
}

// TestDrainCoActivationQueue_EmptyReturnsNil pins the empty-queue case:
// no error, nil pairs, nil ids.
func TestDrainCoActivationQueue_EmptyReturnsNil(t *testing.T) {
	repo := newAssociationsRepo(t)
	ctx := t.Context()

	pairs, ids, err := repo.DrainCoActivationQueue(ctx, 10)
	require.NoError(t, err)
	require.Nil(t, pairs)
	require.Nil(t, ids)
}

// TestDeleteCoActivationQueueRows_ByID pre-populates 3 pairs, deletes
// the middle one by ID, and asserts the other two remain.
func TestDeleteCoActivationQueueRows_ByID(t *testing.T) {
	repo := newAssociationsRepo(t)
	ctx := t.Context()

	workspaceID := uuid.New()
	pairs := []repository.CoActivationPair{
		{SrcEventID: uuid.New(), DstEventID: uuid.New(), WorkspaceID: workspaceID},
		{SrcEventID: uuid.New(), DstEventID: uuid.New(), WorkspaceID: workspaceID},
		{SrcEventID: uuid.New(), DstEventID: uuid.New(), WorkspaceID: workspaceID},
	}
	ids := make([]int64, 3)
	for i, p := range pairs {
		require.NoError(t, repo.Pool().QueryRow(ctx, `
			INSERT INTO semantic.co_activation_queue
				(src_event_id, dst_event_id, workspace_id)
			VALUES ($1, $2, $3)
			RETURNING id
		`, p.SrcEventID, p.DstEventID, p.WorkspaceID).Scan(&ids[i]))
	}

	// Delete the middle row.
	require.NoError(t, repo.DeleteCoActivationQueueRows(ctx, []int64{ids[1]}))

	// Confirm exactly the outer two remain.
	rows, err := repo.Pool().Query(ctx, `
		SELECT src_event_id FROM semantic.co_activation_queue ORDER BY id
	`)
	require.NoError(t, err)
	defer rows.Close()

	var remaining []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		require.NoError(t, rows.Scan(&id))
		remaining = append(remaining, id)
	}
	require.NoError(t, rows.Err())
	require.Len(t, remaining, 2)
	require.Equal(t, pairs[0].SrcEventID, remaining[0])
	require.Equal(t, pairs[2].SrcEventID, remaining[1])
}

// TestDeleteCoActivationQueueRows_EmptyNoOp pins the empty-input
// fast-path: no error, no DB hit, queue contents unchanged.
func TestDeleteCoActivationQueueRows_EmptyNoOp(t *testing.T) {
	repo := newAssociationsRepo(t)
	ctx := t.Context()

	// Seed one row to confirm it survives the no-op delete.
	_, err := repo.Pool().Exec(ctx, `
		INSERT INTO semantic.co_activation_queue
			(src_event_id, dst_event_id, workspace_id)
		VALUES ($1, $2, $3)
	`, uuid.New(), uuid.New(), uuid.New())
	require.NoError(t, err)

	require.NoError(t, repo.DeleteCoActivationQueueRows(ctx, nil))
	require.NoError(t, repo.DeleteCoActivationQueueRows(ctx, []int64{}))

	var count int
	require.NoError(t, repo.Pool().QueryRow(ctx,
		`SELECT count(*) FROM semantic.co_activation_queue`,
	).Scan(&count))
	require.Equal(t, 1, count)
}

// ---------------------------------------------------------------------
// B5: UpsertAssociation + LookupAssociations
// ---------------------------------------------------------------------

// TestUpsertAssociation_InsertNew pins the insert branch: a fresh
// (src, dst, type) tuple lands as a new row with co_activations=1 and
// weight set by the formula at n=1, i.e. 1 - exp(-1/5.0) ~= 0.18127.
// The column default of 0.01 must NOT win — the upsert path is the
// authority for weight on insert too.
func TestUpsertAssociation_InsertNew(t *testing.T) {
	repo := newAssociationsRepo(t)
	ctx := t.Context()

	userID := uuid.New()
	src, dst := uuid.New(), uuid.New()
	seedEventForAssoc(t, repo, src, userID)
	seedEventForAssoc(t, repo, dst, userID)

	workspaceID := uuid.New()
	require.NoError(t, repo.UpsertAssociation(ctx, repository.Association{
		SrcEventID:      src,
		DstEventID:      dst,
		AssociationType: "hebbian",
		WorkspaceID:     workspaceID,
	}))

	var (
		gotWeight float64
		gotCo     int
		gotWS     uuid.UUID
		gotUpdAt  time.Time
	)
	require.NoError(t, repo.Pool().QueryRow(ctx, `
		SELECT weight, co_activations, workspace_id, updated_at
		FROM semantic.associations
		WHERE src_event_id = $1 AND dst_event_id = $2 AND association_type = 'hebbian'
	`, src, dst).Scan(&gotWeight, &gotCo, &gotWS, &gotUpdAt))

	require.Equal(t, 1, gotCo, "first upsert must set co_activations=1")
	want := 1 - math.Exp(-1.0/5.0)
	require.InDelta(t, want, gotWeight, 1e-9,
		"insert weight must come from the saturation formula at n=1, not the column default")
	require.Equal(t, workspaceID, gotWS)
	require.False(t, gotUpdAt.IsZero())
}

// TestUpsertAssociation_IncrementsExisting pins the update branch:
// upserting the same (src, dst, type) twice bumps co_activations to 2
// and recomputes weight via the formula. The input Association's
// Weight/CoActivations fields must be ignored — weight is always
// derived from the row's running co_activations counter.
func TestUpsertAssociation_IncrementsExisting(t *testing.T) {
	repo := newAssociationsRepo(t)
	ctx := t.Context()

	userID := uuid.New()
	src, dst := uuid.New(), uuid.New()
	seedEventForAssoc(t, repo, src, userID)
	seedEventForAssoc(t, repo, dst, userID)

	workspaceID := uuid.New()
	assoc := repository.Association{
		SrcEventID:      src,
		DstEventID:      dst,
		AssociationType: "hebbian",
		WorkspaceID:     workspaceID,
		// Deliberately bogus — must be ignored.
		Weight:        999.0,
		CoActivations: 999,
	}
	require.NoError(t, repo.UpsertAssociation(ctx, assoc))
	require.NoError(t, repo.UpsertAssociation(ctx, assoc))

	var (
		gotWeight float64
		gotCo     int
	)
	require.NoError(t, repo.Pool().QueryRow(ctx, `
		SELECT weight, co_activations
		FROM semantic.associations
		WHERE src_event_id = $1 AND dst_event_id = $2 AND association_type = 'hebbian'
	`, src, dst).Scan(&gotWeight, &gotCo))

	require.Equal(t, 2, gotCo, "second upsert must increment co_activations to 2")
	want := 1 - math.Exp(-2.0/5.0)
	require.InDelta(t, want, gotWeight, 1e-9,
		"weight after 2 upserts must be 1 - exp(-2/5)")
}

// TestUpsertAssociation_RecomputesWeightSaturating exercises the
// saturation curve by upserting 10 times. After 10 the formula gives
// 1 - exp(-2) ~= 0.86466. The point isn't the exact number — it's
// that the curve approaches 1 monotonically and never overshoots.
func TestUpsertAssociation_RecomputesWeightSaturating(t *testing.T) {
	repo := newAssociationsRepo(t)
	ctx := t.Context()

	userID := uuid.New()
	src, dst := uuid.New(), uuid.New()
	seedEventForAssoc(t, repo, src, userID)
	seedEventForAssoc(t, repo, dst, userID)

	workspaceID := uuid.New()
	assoc := repository.Association{
		SrcEventID:      src,
		DstEventID:      dst,
		AssociationType: "hebbian",
		WorkspaceID:     workspaceID,
	}
	for i := 0; i < 10; i++ {
		require.NoError(t, repo.UpsertAssociation(ctx, assoc))
	}

	var (
		gotWeight float64
		gotCo     int
	)
	require.NoError(t, repo.Pool().QueryRow(ctx, `
		SELECT weight, co_activations
		FROM semantic.associations
		WHERE src_event_id = $1 AND dst_event_id = $2 AND association_type = 'hebbian'
	`, src, dst).Scan(&gotWeight, &gotCo))

	require.Equal(t, 10, gotCo)
	want := 1 - math.Exp(-10.0/5.0) // ~0.86466
	require.InDelta(t, want, gotWeight, 1e-9)
	require.Less(t, gotWeight, 1.0, "saturation curve must stay strictly below 1")
}

// TestLookupAssociations_BulkByEventIDs pre-populates associations for
// 3 source events (one outgoing pair each), then bulk-fetches all 3
// in a single call and asserts the returned map is keyed by
// src_event_id with the right neighbor in each value slice.
func TestLookupAssociations_BulkByEventIDs(t *testing.T) {
	repo := newAssociationsRepo(t)
	ctx := t.Context()

	userID := uuid.New()
	workspaceID := uuid.New()

	srcs := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	dsts := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	for i := range srcs {
		seedEventForAssoc(t, repo, srcs[i], userID)
		seedEventForAssoc(t, repo, dsts[i], userID)
		require.NoError(t, repo.UpsertAssociation(ctx, repository.Association{
			SrcEventID:      srcs[i],
			DstEventID:      dsts[i],
			AssociationType: "hebbian",
			WorkspaceID:     workspaceID,
		}))
	}

	got, err := repo.LookupAssociations(ctx, srcs, "hebbian", workspaceID)
	require.NoError(t, err)
	require.Len(t, got, 3, "all 3 source events must appear as keys")

	for i, src := range srcs {
		neighbors, ok := got[src]
		require.True(t, ok, "missing key for src %d (%s)", i, src)
		require.Len(t, neighbors, 1, "src %d should have exactly 1 neighbor", i)
		require.Equal(t, src, neighbors[0].SrcEventID)
		require.Equal(t, dsts[i], neighbors[0].DstEventID)
		require.Equal(t, "hebbian", neighbors[0].AssociationType)
		require.Equal(t, workspaceID, neighbors[0].WorkspaceID)
		require.Equal(t, 1, neighbors[0].CoActivations)
		require.False(t, neighbors[0].UpdatedAt.IsZero())
	}
}

// TestLookupAssociations_FiltersByWorkspace pins the workspace
// boundary: the same (src, dst, type) tuple stored under workspace B
// must NOT come back when the lookup is scoped to workspace A. The
// PK includes workspace_id (migration 011), so the two rows can coexist
// and workspace isolation is enforced at both the storage and query layers.
func TestLookupAssociations_FiltersByWorkspace(t *testing.T) {
	repo := newAssociationsRepo(t)
	ctx := t.Context()

	userID := uuid.New()
	workspaceA, workspaceB := uuid.New(), uuid.New()

	// Use distinct src/dst pairs to keep this test's coverage orthogonal
	// to TestUpsertAssociation_WorkspaceIsolation (which deliberately uses
	// the same src/dst pair across workspaces to prove the PK fix).
	srcA, dstA := uuid.New(), uuid.New()
	srcB, dstB := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{srcA, dstA, srcB, dstB} {
		seedEventForAssoc(t, repo, id, userID)
	}

	require.NoError(t, repo.UpsertAssociation(ctx, repository.Association{
		SrcEventID: srcA, DstEventID: dstA,
		AssociationType: "hebbian", WorkspaceID: workspaceA,
	}))
	require.NoError(t, repo.UpsertAssociation(ctx, repository.Association{
		SrcEventID: srcB, DstEventID: dstB,
		AssociationType: "hebbian", WorkspaceID: workspaceB,
	}))

	gotA, err := repo.LookupAssociations(ctx, []uuid.UUID{srcA, srcB}, "hebbian", workspaceA)
	require.NoError(t, err)
	require.Len(t, gotA, 1, "only workspaceA's association should come back")
	_, ok := gotA[srcA]
	require.True(t, ok, "srcA must be in the result map")
	_, ok = gotA[srcB]
	require.False(t, ok, "srcB lives in workspaceB and must be filtered out")
}

// TestLookupAssociations_FiltersByAssociationType pre-populates two
// associations on the same (src, dst) pair under different types
// ('hebbian' and 'contradicts') and asserts a lookup with
// assocType="hebbian" returns only the hebbian row. The table's PK
// includes association_type (and workspace_id), so both rows can
// coexist on the same (src, dst, workspace).
func TestLookupAssociations_FiltersByAssociationType(t *testing.T) {
	repo := newAssociationsRepo(t)
	ctx := t.Context()

	userID := uuid.New()
	workspaceID := uuid.New()
	src, dst := uuid.New(), uuid.New()
	seedEventForAssoc(t, repo, src, userID)
	seedEventForAssoc(t, repo, dst, userID)

	require.NoError(t, repo.UpsertAssociation(ctx, repository.Association{
		SrcEventID: src, DstEventID: dst,
		AssociationType: "hebbian", WorkspaceID: workspaceID,
	}))
	require.NoError(t, repo.UpsertAssociation(ctx, repository.Association{
		SrcEventID: src, DstEventID: dst,
		AssociationType: "contradicts", WorkspaceID: workspaceID,
	}))

	got, err := repo.LookupAssociations(ctx, []uuid.UUID{src}, "hebbian", workspaceID)
	require.NoError(t, err)
	require.Len(t, got, 1)
	neighbors := got[src]
	require.Len(t, neighbors, 1, "only the hebbian row should come back")
	require.Equal(t, "hebbian", neighbors[0].AssociationType)
}

// TestLookupAssociations_EmptySrcsReturnsEmptyMap pins the
// empty-input contract: no src IDs in, an empty (non-nil) map out,
// and no DB hit needed. Spec: "Empty srcIDs returns empty map (not
// nil — easier for callers)."
func TestLookupAssociations_EmptySrcsReturnsEmptyMap(t *testing.T) {
	repo := newAssociationsRepo(t)
	ctx := t.Context()

	got, err := repo.LookupAssociations(ctx, nil, "hebbian", uuid.New())
	require.NoError(t, err)
	require.NotNil(t, got, "empty input must return a non-nil map")
	require.Len(t, got, 0)
}

// ---------------------------------------------------------------------
// LookupAssociationsByDst
// ---------------------------------------------------------------------

// TestLookupAssociationsByDst_BulkByDstIDs pre-populates three directed
// associations X->A, Y->B, Z->C, then bulk-fetches by dst IDs {A,B,C}
// and asserts the returned map is keyed by dst_event_id with the correct
// src in each value slice.
func TestLookupAssociationsByDst_BulkByDstIDs(t *testing.T) {
	repo := newAssociationsRepo(t)
	ctx := t.Context()

	userID := uuid.New()
	workspaceID := uuid.New()

	srcs := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	dsts := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	for i := range srcs {
		seedEventForAssoc(t, repo, srcs[i], userID)
		seedEventForAssoc(t, repo, dsts[i], userID)
		require.NoError(t, repo.UpsertAssociation(ctx, repository.Association{
			SrcEventID:      srcs[i],
			DstEventID:      dsts[i],
			AssociationType: "hebbian",
			WorkspaceID:     workspaceID,
		}))
	}

	got, err := repo.LookupAssociationsByDst(ctx, dsts, "hebbian", workspaceID)
	require.NoError(t, err)
	require.Len(t, got, 3, "all 3 dst events must appear as keys")

	for i, dst := range dsts {
		neighbors, ok := got[dst]
		require.True(t, ok, "missing key for dst %d (%s)", i, dst)
		require.Len(t, neighbors, 1, "dst %d should have exactly 1 incoming neighbor", i)
		require.Equal(t, srcs[i], neighbors[0].SrcEventID)
		require.Equal(t, dst, neighbors[0].DstEventID)
		require.Equal(t, "hebbian", neighbors[0].AssociationType)
		require.Equal(t, workspaceID, neighbors[0].WorkspaceID)
	}
}

// TestLookupAssociationsByDst_FiltersByWorkspace mirrors the src-lookup
// workspace filter test: a row stored under workspace B must NOT appear
// when the lookup is scoped to workspace A.
func TestLookupAssociationsByDst_FiltersByWorkspace(t *testing.T) {
	repo := newAssociationsRepo(t)
	ctx := t.Context()

	userID := uuid.New()
	workspaceA, workspaceB := uuid.New(), uuid.New()

	srcA, dstA := uuid.New(), uuid.New()
	srcB, dstB := uuid.New(), uuid.New()
	for _, id := range []uuid.UUID{srcA, dstA, srcB, dstB} {
		seedEventForAssoc(t, repo, id, userID)
	}

	require.NoError(t, repo.UpsertAssociation(ctx, repository.Association{
		SrcEventID: srcA, DstEventID: dstA,
		AssociationType: "hebbian", WorkspaceID: workspaceA,
	}))
	require.NoError(t, repo.UpsertAssociation(ctx, repository.Association{
		SrcEventID: srcB, DstEventID: dstB,
		AssociationType: "hebbian", WorkspaceID: workspaceB,
	}))

	gotA, err := repo.LookupAssociationsByDst(ctx, []uuid.UUID{dstA, dstB}, "hebbian", workspaceA)
	require.NoError(t, err)
	require.Len(t, gotA, 1, "only workspaceA's association should come back")
	_, ok := gotA[dstA]
	require.True(t, ok, "dstA must be in the result map")
	_, ok = gotA[dstB]
	require.False(t, ok, "dstB lives in workspaceB and must be filtered out")
}

// TestLookupAssociationsByDst_EmptyDstsReturnsEmptyMap pins the fast-path:
// empty input yields a non-nil empty map without hitting the DB.
func TestLookupAssociationsByDst_EmptyDstsReturnsEmptyMap(t *testing.T) {
	repo := newAssociationsRepo(t)
	ctx := t.Context()

	got, err := repo.LookupAssociationsByDst(ctx, nil, "hebbian", uuid.New())
	require.NoError(t, err)
	require.NotNil(t, got, "empty input must return a non-nil map")
	require.Len(t, got, 0)
}

// ---------------------------------------------------------------------
// Workspace-isolation upsert bug: associations PK was missing workspace_id
// ---------------------------------------------------------------------

// TestUpsertAssociation_WorkspaceIsolation is the regression test for the
// workspace-capture bug: when two workspaces share the same (src, dst,
// association_type) tuple, the ON CONFLICT on the old PK
// (src_event_id, dst_event_id, association_type) causes workspace B's
// upsert to increment workspace A's row rather than inserting its own.
//
// The fix (migration 011) adds workspace_id to the PK so each workspace
// owns its associations independently.
//
// Assertions:
//   - After upserts from workspace A and workspace B, TWO rows exist.
//   - Each row has co_activations=1 and its own workspace_id.
//   - LookupAssociations scoped to workspace B returns the pair.
func TestUpsertAssociation_WorkspaceIsolation(t *testing.T) {
	repo := newAssociationsRepo(t)
	ctx := t.Context()

	userID := uuid.New()
	src, dst := uuid.New(), uuid.New()
	seedEventForAssoc(t, repo, src, userID)
	seedEventForAssoc(t, repo, dst, userID)

	workspaceA := uuid.New()
	workspaceB := uuid.New()

	// Upsert the same (src, dst, 'hebbian') pair under each workspace.
	require.NoError(t, repo.UpsertAssociation(ctx, repository.Association{
		SrcEventID: src, DstEventID: dst,
		AssociationType: "hebbian", WorkspaceID: workspaceA,
	}))
	require.NoError(t, repo.UpsertAssociation(ctx, repository.Association{
		SrcEventID: src, DstEventID: dst,
		AssociationType: "hebbian", WorkspaceID: workspaceB,
	}))

	// Two distinct rows must exist — one per workspace.
	var rowCount int
	require.NoError(t, repo.Pool().QueryRow(ctx, `
		SELECT count(*) FROM semantic.associations
		WHERE src_event_id = $1 AND dst_event_id = $2 AND association_type = 'hebbian'
	`, src, dst).Scan(&rowCount))
	require.Equal(t, 2, rowCount,
		"each workspace must own a distinct association row; "+
			"count=1 means workspace B captured workspace A's row (bug)")

	// Each row must have co_activations=1 — neither should have been incremented
	// by the other workspace's upsert.
	type wsRow struct {
		workspaceID   uuid.UUID
		coActivations int
	}
	rows, err := repo.Pool().Query(ctx, `
		SELECT workspace_id, co_activations FROM semantic.associations
		WHERE src_event_id = $1 AND dst_event_id = $2 AND association_type = 'hebbian'
		ORDER BY workspace_id
	`, src, dst)
	require.NoError(t, err)
	defer rows.Close()

	var got []wsRow
	for rows.Next() {
		var r wsRow
		require.NoError(t, rows.Scan(&r.workspaceID, &r.coActivations))
		got = append(got, r)
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, 2)
	for _, r := range got {
		require.Equal(t, 1, r.coActivations,
			"workspace %s should have co_activations=1, not %d "+
				"(>1 means another workspace's upsert incremented this row)",
			r.workspaceID, r.coActivations)
	}

	// LookupAssociations scoped to workspace B must return the pair.
	gotB, err := repo.LookupAssociations(ctx, []uuid.UUID{src}, "hebbian", workspaceB)
	require.NoError(t, err)
	require.Len(t, gotB, 1, "workspaceB lookup must return 1 src key")
	neighbors, ok := gotB[src]
	require.True(t, ok, "src must be present in workspaceB's result map")
	require.Len(t, neighbors, 1)
	require.Equal(t, workspaceB, neighbors[0].WorkspaceID,
		"neighbor must belong to workspaceB, not workspaceA")
	require.Equal(t, 1, neighbors[0].CoActivations)
}

// TestUpsertAssociation_SameWorkspaceIncrements confirms that the
// standard increment path still works after the PK fix: two upserts
// from the same workspace on the same pair yield one row with
// co_activations=2.
func TestUpsertAssociation_SameWorkspaceIncrements(t *testing.T) {
	repo := newAssociationsRepo(t)
	ctx := t.Context()

	userID := uuid.New()
	src, dst := uuid.New(), uuid.New()
	seedEventForAssoc(t, repo, src, userID)
	seedEventForAssoc(t, repo, dst, userID)

	workspaceID := uuid.New()
	assoc := repository.Association{
		SrcEventID: src, DstEventID: dst,
		AssociationType: "hebbian", WorkspaceID: workspaceID,
	}
	require.NoError(t, repo.UpsertAssociation(ctx, assoc))
	require.NoError(t, repo.UpsertAssociation(ctx, assoc))

	var rowCount, co int
	require.NoError(t, repo.Pool().QueryRow(ctx, `
		SELECT count(*), max(co_activations) FROM semantic.associations
		WHERE src_event_id = $1 AND dst_event_id = $2
		  AND association_type = 'hebbian' AND workspace_id = $3
	`, src, dst, workspaceID).Scan(&rowCount, &co))
	require.Equal(t, 1, rowCount, "same workspace must yield one row, not two")
	require.Equal(t, 2, co, "two upserts in the same workspace must set co_activations=2")
}
