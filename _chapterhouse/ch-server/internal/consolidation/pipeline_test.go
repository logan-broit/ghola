package consolidation_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/consolidation"
	"github.com/thinkwright/chapterhouse/ch-server/internal/mentat"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
)

// fakeEmbedder returns a fixed vector for any text — enough for the
// digest write path (InsertDigestMneme needs a non-empty 1024-vec).
type fakeEmbedder struct{ vec []float32 }

func (f fakeEmbedder) Embed(_ context.Context, _ string) ([]float32, error) { return f.vec, nil }

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// vecLit renders a []float32 as a pgvector text literal for seeding
// episodic.events.embedding directly (the repo's own helper is unexported).
func vecLit(v []float32) string {
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

// seedWorkspace inserts n closed sessions (each with 2 embedded, tagged
// user/assistant events) into the workspace and pools an L1 vector onto
// each so the cluster + enrichment steps have material to work with.
func seedWorkspace(t *testing.T, repo *repository.Repository, ws uuid.UUID, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		sid := uuid.New()
		_, err := repo.Pool().Exec(ctx, `
			INSERT INTO episodic.sessions
			  (id, user_id, started_at, ended_at, event_count, cwd, git_branch)
			VALUES ($1, $2, now(), now(), 2, '/home/loganb/ghola', 'feat/consolidation')`,
			sid, ws)
		require.NoError(t, err)

		for j := 0; j < 2; j++ {
			eid := uuid.New()
			typ := "user"
			if j%2 == 1 {
				typ = "assistant"
			}
			fill := float32(i+1)*0.1 + float32(j)*0.01
			_, err := repo.Pool().Exec(ctx, `
				INSERT INTO episodic.events
				  (id, session_id, user_id, type, text, raw_event, embedding,
				   tags, entities, created_at)
				VALUES ($1, $2, $3, $4, $5, '{}'::jsonb, ($6::text)::vector,
				        $7, $8, now())`,
				eid, sid, ws, typ,
				"event text for session "+strconv.Itoa(i)+" blob "+strconv.Itoa(j),
				vecLit(vec1024(fill)),
				[]string{"go", "consolidation"}, []string{"ghola"})
			require.NoError(t, err)
		}

		require.NoError(t, repo.UpdateSessionL1(ctx, sid, vec1024(float32(i+1)*0.1), "chunk text"))
	}
}

// fakeMentat returns a single all-zero-label clustering (every session in
// one cluster) sized to the request's id count.
func fakeMentat(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			IDs []string `json:"ids"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		labels := make([]int, len(req.IDs)) // all 0 => one cluster
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"labels":   labels,
			"outliers": []string{},
		})
	}))
}

// fakeLLM answers any chat-completion with a fixed line (label) / paragraph
// (digest) — both endpoints share /v1/chat/completions.
func fakeLLM(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": "consolidation pipeline work"}},
			},
		})
	}))
}

func countMnemes(t *testing.T, repo *repository.Repository, ws uuid.UUID, level int, state string) int {
	t.Helper()
	var n int
	err := repo.Pool().QueryRow(context.Background(), `
		SELECT count(*) FROM semantic.mnemes
		WHERE workspace_id = $1 AND level = $2 AND state = $3`, ws, level, state).Scan(&n)
	require.NoError(t, err)
	return n
}

func TestRunWorkspace_BuildsContentMnemes(t *testing.T) {
	repo := newSemRepo(t)
	ctx := context.Background()
	ws := uuid.New()
	seedWorkspace(t, repo, ws, 3)

	mSrv := fakeMentat(t)
	defer mSrv.Close()
	lSrv := fakeLLM(t)
	defer lSrv.Close()

	d := consolidation.Deps{
		Repo:     repo,
		Mentat:   mentat.NewClient(mSrv.URL, nil),
		Pooler:   &fakePooler{},
		LLM:      consolidation.NewLLMClient(lSrv.URL, "test-model", ""),
		Embedder: fakeEmbedder{vec: vec1024(0.2)},
		Logger:   discardLogger(),
	}

	require.NoError(t, consolidation.RunWorkspace(ctx, d, ws))

	// One level-1 content mneme, fully enriched.
	var (
		label     *string
		reps      []byte
		tags      []string
		spanStart *time.Time
		meta      []byte
	)
	err := repo.Pool().QueryRow(ctx, `
		SELECT label, representatives, tags, span_start, meta
		FROM semantic.mnemes
		WHERE workspace_id = $1 AND level = 1 AND state = 'active'`, ws).
		Scan(&label, &reps, &tags, &spanStart, &meta)
	require.NoError(t, err)
	require.NotEmpty(t, reps, "representatives populated")
	require.NotContains(t, string(reps), "null", "representatives is a real array")
	require.NotEmpty(t, tags, "aggregated tags populated")
	require.NotNil(t, spanStart, "span_start populated")
	require.NotNil(t, label, "LLM configured => label written")
	require.NotEmpty(t, *label)
	require.Contains(t, string(meta), "session_count", "meta carries session_count")

	// Exactly one active level-2 digest exists.
	require.Equal(t, 1, countMnemes(t, repo, ws, 2, "active"))
	require.Equal(t, 0, countMnemes(t, repo, ws, 2, "archived"))

	// Re-run archives the prior digest and writes a fresh one.
	require.NoError(t, consolidation.RunWorkspace(ctx, d, ws))
	require.Equal(t, 1, countMnemes(t, repo, ws, 2, "active"), "still exactly one active digest")
	require.Equal(t, 1, countMnemes(t, repo, ws, 2, "archived"), "prior digest archived")
}

func TestRunWorkspace_LLMUnset_SkipsLabelsNotFail(t *testing.T) {
	repo := newSemRepo(t)
	ctx := context.Background()
	ws := uuid.New()
	seedWorkspace(t, repo, ws, 3)

	mSrv := fakeMentat(t)
	defer mSrv.Close()

	d := consolidation.Deps{
		Repo:   repo,
		Mentat: mentat.NewClient(mSrv.URL, nil),
		Pooler: &fakePooler{},
		LLM:    nil, // skip label/digest
		Logger: discardLogger(),
	}

	require.NoError(t, consolidation.RunWorkspace(ctx, d, ws))

	// Enrichment still ran (representatives written) but label is NULL.
	var (
		label *string
		reps  []byte
	)
	err := repo.Pool().QueryRow(ctx, `
		SELECT label, representatives FROM semantic.mnemes
		WHERE workspace_id = $1 AND level = 1 AND state = 'active'`, ws).
		Scan(&label, &reps)
	require.NoError(t, err)
	require.NotEmpty(t, reps, "free enrichment runs without an LLM")
	require.Nil(t, label, "no LLM => label stays NULL")

	// No digest written when LLM is unset.
	require.Equal(t, 0, countMnemes(t, repo, ws, 2, "active"))
}

func TestRunWorkspace_MentatDown_LoudFails(t *testing.T) {
	repo := newSemRepo(t)
	ctx := context.Background()
	ws := uuid.New()
	seedWorkspace(t, repo, ws, 2)

	mSrv := fakeMentat(t)
	mSrv.Close() // mentat is down

	d := consolidation.Deps{
		Repo:   repo,
		Mentat: mentat.NewClient(mSrv.URL, nil),
		Pooler: &fakePooler{},
		Logger: discardLogger(),
	}

	err := consolidation.RunWorkspace(ctx, d, ws)
	require.Error(t, err, "mentat-down aborts the run (nightly retry recovers)")
}
