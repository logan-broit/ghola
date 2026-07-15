//go:build integration_smoke

// OPERATOR-GATED: this file only compiles/runs under `-tags
// integration_smoke`; it never runs as part of the default `go test
// ./...`. It requires a live compose stack (DATABASE_URL + MENTAT_URL)
// and writes rows into a fresh scratch workspace. Do NOT run without
// operator approval — see scripts/consolidation-smoke.sh, the intended
// entrypoint.
package consolidation_test

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/consolidation"
	"github.com/thinkwright/chapterhouse/ch-server/internal/mentat"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
)

// smokeSessionCount is N: the number of synthetic sessions seeded into
// the scratch workspace. Comfortably above the pipeline's default
// min_cluster_size (3, see Deps.MinClusterSize / pipeline.go) so the real
// mentat HDBSCAN reliably finds one non-noise cluster instead of noise.
const smokeSessionCount = 5

// noopPooler is a no-op consolidation.SessionPooler for the smoke run.
// Reconcile's ClosedSessionsMissingL1 read is NOT workspace-scoped — on a
// live stack it may surface real, unrelated closed sessions still missing
// an L1 vector. This test's own seeded sessions already carry a populated
// l1_embedding (see seedSmokeWorkspace), so Reconcile never needs a real
// pooler for them; no-op-succeeding for anything else is strictly safer
// than wiring a real mentat Pool call against production data this test
// does not own.
type noopPooler struct{}

func (noopPooler) PoolSessionToL1(context.Context, uuid.UUID, uuid.UUID) error { return nil }

// smokeVec returns a dim-length vector that points in (almost) the same
// direction for every idx: all components are 1.0 except one, which is
// nudged by a tiny amount keyed on idx. mentat's HDBSCAN clusters under
// cosine distance on L2-normalized vectors (see mentat/clustering.py), so
// this whole seeded batch lands at ~0 mutual distance — one dense,
// non-noise cluster — while still giving every row a distinct value
// (never bit-identical rows).
func smokeVec(dim, idx int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		v[i] = 1.0
	}
	if dim > 0 {
		v[idx%dim] += 0.001
	}
	return v
}

// smokeVecLit renders smokeVec(dim, idx) as a pgvector text literal for
// seeding episodic.events.embedding directly (mirrors pipeline_test.go's
// vecLit — the repo's own vector-literal helper is unexported).
func smokeVecLit(dim, idx int) string {
	v := smokeVec(dim, idx)
	var b strings.Builder
	b.WriteByte('[')
	for i, x := range v {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.FormatFloat(float64(x), 'f', -1, 32))
	}
	b.WriteByte(']')
	return b.String()
}

// seedSmokeWorkspace inserts n closed synthetic sessions into ws, each
// with a user + assistant event (embedded, tagged, texted) and a pooled
// L1 vector, all pointing in the same direction (smokeVec) so the real
// mentat clustering call groups the whole batch into one cluster with
// live, content-bearing member events for the enrichment step to select
// representatives from.
func seedSmokeWorkspace(t *testing.T, ctx context.Context, repo *repository.Repository, ws uuid.UUID, dim, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		sid := uuid.New()
		_, err := repo.Pool().Exec(ctx, `
			INSERT INTO episodic.sessions
			  (id, user_id, started_at, ended_at, event_count, cwd, git_branch)
			VALUES ($1, $2, now(), now(), 2, '/tmp/consolidation-smoke', 'integration-smoke')`,
			sid, ws)
		require.NoError(t, err)

		for j, typ := range []string{"user", "assistant"} {
			_, err := repo.Pool().Exec(ctx, `
				INSERT INTO episodic.events
				  (id, session_id, user_id, type, text, raw_event, embedding,
				   tags, entities, created_at)
				VALUES ($1, $2, $3, $4, $5, '{}'::jsonb, ($6::text)::vector,
				        $7, $8, now())`,
				uuid.New(), sid, ws, typ,
				fmt.Sprintf("consolidation smoke event text session=%d role=%s", i, typ),
				smokeVecLit(dim, i*2+j),
				[]string{"integration-smoke", "consolidation"}, []string{"smoke"})
			require.NoError(t, err)
		}

		require.NoError(t, repo.UpdateSessionL1(ctx, sid, smokeVec(dim, i),
			fmt.Sprintf("smoke chunk %d", i)))
	}
}

// TestConsolidationSmoke seeds N synthetic sessions (member events +
// embeddings) into a fresh scratch workspace against a live DB, runs the
// real consolidation.RunWorkspace pipeline against a real mentat client,
// and asserts the pipeline actually produced content: (a) a level-1 mneme
// with non-empty representatives + tags + span_start, and (b) a semantic
// query near the seeded cluster returns a hit with a non-empty TopExcerpt.
//
// Run via scripts/consolidation-smoke.sh against the compose stack —
// OPERATOR-GATED, requires DATABASE_URL + MENTAT_URL pointed at a live,
// running stack. Never run as part of the default suite.
func TestConsolidationSmoke(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Fatal("DATABASE_URL must be set to a live stack (see scripts/consolidation-smoke.sh)")
	}
	mentatURL := os.Getenv("MENTAT_URL")
	if mentatURL == "" {
		t.Fatal("MENTAT_URL must be set to a live mentat (see scripts/consolidation-smoke.sh)")
	}
	dim := 1024
	if raw := os.Getenv("EMBEDDING_DIM"); raw != "" {
		v, err := strconv.Atoi(raw)
		require.NoError(t, err, "EMBEDDING_DIM must be an integer")
		dim = v
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dbURL)
	require.NoError(t, err, "connect to live DATABASE_URL")
	defer pool.Close()
	require.NoError(t, pool.Ping(ctx), "ping live DATABASE_URL")

	repo := repository.New(pool)

	// Fresh random scratch workspace so re-runs (and the real, running
	// stack) never collide with this test's rows. Cleanup is optional —
	// a scratch UUID is inert and never overlaps real workspace data.
	ws := uuid.New()

	seedSmokeWorkspace(t, ctx, repo, ws, dim, smokeSessionCount)

	d := consolidation.Deps{
		Repo:   repo,
		Mentat: mentat.NewClient(mentatURL, nil),
		Pooler: noopPooler{},
	}

	require.NoError(t, consolidation.RunWorkspace(ctx, d, ws), "RunWorkspace against the live stack")

	// (a) mnemes exist with non-empty representatives + tags + span_start.
	var (
		reps      []byte
		tags      []string
		spanStart *time.Time
	)
	err = pool.QueryRow(ctx, `
		SELECT representatives, tags, span_start
		FROM semantic.mnemes
		WHERE workspace_id = $1 AND level = 1 AND state = 'active'`, ws).
		Scan(&reps, &tags, &spanStart)
	require.NoError(t, err, "expected an enriched level-1 mneme for the seeded cluster")
	require.NotEmpty(t, reps, "representatives populated")
	require.NotEmpty(t, tags, "aggregated tags populated")
	require.NotNil(t, spanStart, "span_start populated")

	// (b) QueryMnemesByEmbedding near the seeded cluster returns a hit
	// with a non-empty TopExcerpt.
	hits, err := repo.QueryMnemesByEmbedding(ctx, ws, smokeVec(dim, 0), 5)
	require.NoError(t, err)
	require.NotEmpty(t, hits, "semantic query returns at least one hit near the seeded cluster")
	require.NotEmpty(t, hits[0].TopExcerpt, "top hit carries a non-empty excerpt")
}
