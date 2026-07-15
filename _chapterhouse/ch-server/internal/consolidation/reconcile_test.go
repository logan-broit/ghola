package consolidation_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/consolidation"
)

type fakePooler struct {
	calls   []uuid.UUID
	failFor map[uuid.UUID]bool
}

func (f *fakePooler) PoolSessionToL1(_ context.Context, _ uuid.UUID, sessionID uuid.UUID) error {
	f.calls = append(f.calls, sessionID)
	if f.failFor[sessionID] {
		return errors.New("pool boom")
	}
	return nil
}

func TestReconcile_PoolsEveryMissingSession(t *testing.T) {
	repo := newSemRepo(t)
	ctx := context.Background()
	// Seed 3 closed sessions with NULL l1_embedding.
	ws := uuid.New()
	var ids []uuid.UUID
	for i := 0; i < 3; i++ {
		sid := uuid.New()
		_, err := repo.Pool().Exec(ctx, `
			INSERT INTO episodic.sessions (id, user_id, started_at, ended_at, event_count)
			VALUES ($1, $2, now(), now(), 0)`, sid, ws)
		require.NoError(t, err)
		ids = append(ids, sid)
	}
	fp := &fakePooler{failFor: map[uuid.UUID]bool{ids[1]: true}}
	n, err := consolidation.Reconcile(ctx, repo, fp, 100)
	require.NoError(t, err, "one bad session must not abort the batch")
	require.Equal(t, 2, n, "2 of 3 pooled successfully")
	require.Len(t, fp.calls, 3, "all 3 attempted")
}
