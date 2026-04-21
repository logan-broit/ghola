package replay_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/replay"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"
)

type stubDistiller struct {
	got  []replay.DistillInput
	reply *replay.Mneme
	err  error
}

func (s *stubDistiller) Distill(ctx context.Context, in replay.DistillInput) (*replay.Mneme, error) {
	s.got = append(s.got, in)
	if s.err != nil {
		return nil, s.err
	}
	return s.reply, nil
}

type stubEmbedder struct{ vec []float32 }

func (s stubEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	return s.vec, nil
}

// TestWorker_EndToEnd: synthetic 24h load with 3 sessions tagging the
// same pair triggers one Upsert call -> one new mneme row.
func TestWorker_EndToEnd(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	applyMigrations(t, pg.Pool)
	installSemanticStub(t, pg.Pool)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		s := seedSession(t, pg.Pool)
		// Seed a user event with matching text.
		_, err := pg.Pool.Exec(ctx, `
			INSERT INTO episodic.events
			  (id, session_id, user_id, type, text, raw_event, entities, created_at, ingested_at)
			VALUES
			  ($1, $2, $3, 'user', 'we use CNPG to provision Postgres', '{}'::jsonb, $4, $5, $5)
		`, uuid.New(), s, uuid.New(), []string{"CNPG", "Postgres"}, now)
		require.NoError(t, err)
	}

	w := &replay.Worker{
		Pool: pg.Pool,
		Mentat: &stubDistiller{reply: &replay.Mneme{
			Concept: "CNPG + Postgres",
			Content: "Chapterhouse uses the CloudNativePG operator.",
			MemoryType: "factual",
			Entities: []string{"CNPG", "Postgres"},
		}},
		Embedder: stubEmbedder{vec: []float32{1, 0, 0, 0}},
		Cfg: replay.WorkerConfig{
			Window:      24 * time.Hour,
			MinSupport:  3,
			WorkspaceID: uuid.New(),
		},
	}

	require.NoError(t, w.RunOnce(ctx))

	var count int
	require.NoError(t, pg.Pool.QueryRow(ctx, `SELECT count(*) FROM semantic.mnemes`).Scan(&count))
	assert.Equal(t, 1, count, "exactly one mneme per detected pair")
}

// TestWorker_DistillerErrorSkipsPair: a failing LLM call must not
// crash the run — just log + move on.
func TestWorker_DistillerErrorSkipsPair(t *testing.T) {
	pg := testutil.NewEphemeralPostgres(t)
	applyMigrations(t, pg.Pool)
	installSemanticStub(t, pg.Pool)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		s := seedSession(t, pg.Pool)
		_, err := pg.Pool.Exec(ctx, `
			INSERT INTO episodic.events
			  (id, session_id, user_id, type, text, raw_event, entities, created_at, ingested_at)
			VALUES
			  ($1, $2, $3, 'user', 'anything', '{}'::jsonb, $4, $5, $5)
		`, uuid.New(), s, uuid.New(), []string{"A", "B"}, now)
		require.NoError(t, err)
	}

	w := &replay.Worker{
		Pool:     pg.Pool,
		Mentat:   &stubDistiller{err: assert.AnError},
		Embedder: stubEmbedder{vec: []float32{1, 0, 0, 0}},
		Cfg:      replay.WorkerConfig{WorkspaceID: uuid.New()},
	}

	require.NoError(t, w.RunOnce(ctx), "RunOnce must swallow per-pair errors")

	var count int
	require.NoError(t, pg.Pool.QueryRow(ctx, `SELECT count(*) FROM semantic.mnemes`).Scan(&count))
	assert.Equal(t, 0, count, "failed distill → no mneme")
}
