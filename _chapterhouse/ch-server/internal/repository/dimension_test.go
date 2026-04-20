package repository_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
	"github.com/thinkwright/chapterhouse/ch-server/internal/testutil"
)

// TestDimensionAgnosticism locks in the substrate-level invariant
// from the design doc's CONSTRAINT: swappable_models_and_dimensions.
// The same migration SQL must install at any EMBEDDING_DIM the team
// ever wants to deploy (BGE-small 384, Qwen3 1024, Ada-002-shape
// 1536, …) with no SQL edits.
func TestDimensionAgnosticism(t *testing.T) {
	for _, dim := range []int{384, 1024, 1536} {
		dim := dim
		t.Run(fmt.Sprintf("dim=%d", dim), func(t *testing.T) {
			pg := testutil.NewEphemeralPostgres(t)
			t.Setenv("EMBEDDING_DIM", fmt.Sprintf("%d", dim))

			require.NoError(t, repository.ApplyMigrations(context.Background(), pg.Pool))

			got := testutil.ColumnVectorDim(t, pg.Pool, "episodic", "events", "embedding")
			require.Equal(t, dim, got,
				"episodic.events.embedding should follow EMBEDDING_DIM verbatim")

			// Sanity: insert a vector of the declared dim and read it
			// back, proving the column + HNSW index accept the shape.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			zeros := make([]string, dim)
			for i := range zeros {
				zeros[i] = "0"
			}
			literal := "[" + strings.Join(zeros, ",") + "]"

			_, err := pg.Pool.Exec(ctx, `
				INSERT INTO episodic.events (
					id, session_id, user_id, type, text, raw_event,
					embedding, created_at
				) VALUES (
					gen_random_uuid(), gen_random_uuid(), gen_random_uuid(),
					'user', 'hello', '{}'::jsonb, $1::vector, now()
				)`,
				literal,
			)
			require.NoError(t, err, "insert vector of declared dim")

			var count int
			require.NoError(t,
				pg.Pool.QueryRow(ctx, `SELECT count(*) FROM episodic.events`).Scan(&count))
			require.Equal(t, 1, count)
		})
	}
}
