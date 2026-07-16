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

// smokeBlobSize is N per blob: the number of synthetic sessions seeded
// into each of two direction blobs (8 total). Comfortably above the
// pipeline's default min_cluster_size (3, see Deps.MinClusterSize /
// pipeline.go) so the real mentat HDBSCAN reliably finds two non-noise
// clusters instead of noise.
//
// Two blobs, not one: HDBSCAN's default allow_single_cluster=false never
// emits a cluster when the *entire* corpus is a single dense blob — there
// is no density contrast for anything to be a cluster relative to.
// Confirmed empirically against the live mentat service: a single
// 5-point blob labels every point -1 (noise); two 4-point blobs label
// [0,0,0,0,1,1,1,1]. Seeding two well-separated blobs gives HDBSCAN the
// contrast it needs to emit real clusters (see also
// docs/consolidation.md's note on this).
const smokeBlobSize = 4

// noopPooler is a no-op consolidation.SessionPooler for the smoke run.
// Reconcile's ClosedSessionsMissingL1 read is NOT workspace-scoped — on a
// live stack it may surface real, unrelated closed sessions still missing
// an L1 vector. This test's own seeded sessions already carry a populated
// l1_embedding (see seedSmokeBlob), so Reconcile never needs a real
// pooler for them; no-op-succeeding for anything else is strictly safer
// than wiring a real mentat Pool call against production data this test
// does not own.
type noopPooler struct{}

func (noopPooler) PoolSessionToL1(context.Context, uuid.UUID, uuid.UUID) error { return nil }

// smokeVec returns a dim-length vector for one of two well-separated
// direction blobs (blob 0 or blob 1), nudged by a tiny idx-keyed amount
// so no two rows in the same blob are bit-identical. mentat's HDBSCAN
// clusters under cosine distance on L2-normalized vectors (see
// mentat/clustering.py): blob 0 points in the all-ones direction, blob 1
// flips the sign of the vector's second half, so the two blobs sit at
// ~0 cosine similarity (well-separated) while every row within a blob
// lands at ~0 mutual distance from its blobmates (dense, non-noise).
func smokeVec(dim, blob, idx int) []float32 {
	v := make([]float32, dim)
	for i := range v {
		if blob != 0 && i >= dim/2 {
			v[i] = -1.0
		} else {
			v[i] = 1.0
		}
	}
	if dim > 0 {
		v[idx%dim] += 0.001
	}
	return v
}

// smokeVecLit renders smokeVec(dim, blob, idx) as a pgvector text literal
// for seeding episodic.events.embedding directly (mirrors
// pipeline_test.go's vecLit — the repo's own vector-literal helper is
// unexported).
func smokeVecLit(dim, blob, idx int) string {
	v := smokeVec(dim, blob, idx)
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

// smokeBlobTag returns the distinguishing tag seeded onto every event in
// a blob, so the test can identify which enriched mneme owns which
// blob's content by its aggregated tags (see Aggregate in enrich.go).
func smokeBlobTag(blob int) string {
	return fmt.Sprintf("smoke-blob-%d", blob)
}

// containsTag reports whether tags contains want.
func containsTag(tags []string, want string) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}

// seedSmokeBlob inserts n closed synthetic sessions into ws for one
// direction blob, each with a user + assistant event (embedded, tagged,
// texted) and a pooled L1 vector, all pointing in blob's direction
// (smokeVec) so the real mentat clustering call groups this blob into
// its own cluster with live, content-bearing member events for the
// enrichment step to select representatives from. Every event also
// carries smokeBlobTag(blob) so the resulting mneme's aggregated tags
// (see Aggregate in enrich.go) identify which blob produced it.
func seedSmokeBlob(t *testing.T, ctx context.Context, repo *repository.Repository, ws uuid.UUID, dim, blob, n int) {
	t.Helper()
	blobTag := smokeBlobTag(blob)
	for i := 0; i < n; i++ {
		sid := uuid.New()
		// Distinct user_id from the scratch workspace id: workspace scoping
		// goes through episodic.session_workspaces (migration 006), so the
		// smoke exercises the real WorkspaceSessionL1s join rather than the
		// old user_id==workspace coincidence.
		uid := uuid.New()
		_, err := repo.Pool().Exec(ctx, `
			INSERT INTO episodic.sessions
			  (id, user_id, started_at, ended_at, event_count, cwd, git_branch)
			VALUES ($1, $2, now(), now(), 2, '/tmp/consolidation-smoke', 'integration-smoke')`,
			sid, uid)
		require.NoError(t, err)
		_, err = repo.Pool().Exec(ctx, `
			INSERT INTO episodic.session_workspaces (session_id, workspace_id)
			VALUES ($1, $2)`, sid, ws)
		require.NoError(t, err)

		for j, typ := range []string{"user", "assistant"} {
			_, err := repo.Pool().Exec(ctx, `
				INSERT INTO episodic.events
				  (id, session_id, user_id, type, text, raw_event, embedding,
				   tags, entities, created_at)
				VALUES ($1, $2, $3, $4, $5, '{}'::jsonb, ($6::text)::vector,
				        $7, $8, now())`,
				uuid.New(), sid, uid, typ,
				fmt.Sprintf("consolidation smoke event text blob=%d session=%d role=%s", blob, i, typ),
				smokeVecLit(dim, blob, i*2+j),
				[]string{"integration-smoke", "consolidation", blobTag}, []string{"smoke"})
			require.NoError(t, err)
		}

		require.NoError(t, repo.UpdateSessionL1(ctx, sid, smokeVec(dim, blob, i),
			fmt.Sprintf("smoke chunk blob=%d %d", blob, i)))
	}
}

// TestConsolidationSmoke seeds two well-separated direction blobs of
// synthetic sessions (member events + embeddings; smokeBlobSize each, 8
// total) into a fresh scratch workspace against a live DB, runs the real
// consolidation.RunWorkspace pipeline against a real mentat client, and
// asserts the pipeline actually produced content: (a) two enriched
// level-1 mnemes exist (one per blob), each with non-empty
// representatives + tags + span_start, and (b) a semantic query near
// blob A's direction returns a hit whose top result is blob A's mneme
// (not blob B's) with a non-empty TopExcerpt.
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

	seedSmokeBlob(t, ctx, repo, ws, dim, 0, smokeBlobSize)
	seedSmokeBlob(t, ctx, repo, ws, dim, 1, smokeBlobSize)

	d := consolidation.Deps{
		Repo:   repo,
		Mentat: mentat.NewClient(mentatURL, nil),
		Pooler: noopPooler{},
	}

	require.NoError(t, consolidation.RunWorkspace(ctx, d, ws), "RunWorkspace against the live stack")

	// (a) two enriched level-1 mnemes exist (one per seeded blob), each
	// with non-empty representatives + tags + span_start.
	rows, err := pool.Query(ctx, `
		SELECT id, representatives, tags, span_start
		FROM semantic.mnemes
		WHERE workspace_id = $1 AND level = 1 AND state = 'active'`, ws)
	require.NoError(t, err, "query enriched level-1 mnemes")
	defer rows.Close()

	var blobAID uuid.UUID
	count := 0
	for rows.Next() {
		var (
			id        uuid.UUID
			reps      []byte
			tags      []string
			spanStart *time.Time
		)
		require.NoError(t, rows.Scan(&id, &reps, &tags, &spanStart))
		require.NotEmpty(t, reps, "representatives populated")
		require.NotEmpty(t, tags, "aggregated tags populated")
		require.NotNil(t, spanStart, "span_start populated")
		count++
		if containsTag(tags, smokeBlobTag(0)) {
			blobAID = id
		}
	}
	require.NoError(t, rows.Err())
	require.Equal(t, 2, count, "expected one enriched level-1 mneme per seeded blob")
	require.NotEqual(t, uuid.Nil, blobAID, "blob A's mneme identified by its aggregated tag")

	// (b) QueryMnemesByEmbedding near blob A's direction returns a hit
	// whose top result is blob A's mneme (not blob B's), with a
	// non-empty TopExcerpt.
	hits, err := repo.QueryMnemesByEmbedding(ctx, ws, smokeVec(dim, 0, 0), 5)
	require.NoError(t, err)
	require.NotEmpty(t, hits, "semantic query returns at least one hit near blob A")
	require.Equal(t, blobAID, hits[0].ID, "top hit is blob A's mneme, not blob B's")
	require.NotEmpty(t, hits[0].TopExcerpt, "top hit carries a non-empty excerpt")
}
