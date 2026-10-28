package repository_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// enrichRep mirrors the JSON shape the consolidation layer marshals into
// the representatives column; the repository stores it opaquely as jsonb.
type enrichRep struct {
	EventID string  `json:"event_id"`
	Excerpt string  `json:"excerpt"`
	Score   float64 `json:"score"`
}

// TestUpdateMnemeEnrichment_RoundTrip seeds a mneme, writes the
// selection-first content columns, and reads them back: representatives
// decode to the expected array, tags/entities/span round-trip, and label
// stays NULL when passed nil (the LLM step owns it).
func TestUpdateMnemeEnrichment_RoundTrip(t *testing.T) {
	repo := newSemRepo(t)
	ctx := context.Background()
	ws := uuid.New()

	id, err := repo.InsertMneme(ctx, ws, vec1024(0.1), []uuid.UUID{uuid.New()})
	require.NoError(t, err)

	reps := []enrichRep{
		{EventID: uuid.NewString(), Excerpt: "first excerpt", Score: 0.9},
		{EventID: uuid.NewString(), Excerpt: "second excerpt", Score: 0.7},
	}
	repsJSON, err := json.Marshal(reps)
	require.NoError(t, err)
	metaJSON := []byte(`{"source":"consolidation-test"}`)

	spanStart := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	spanEnd := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	tags := []string{"go", "db"}
	entities := []string{"pgvector", "hdbscan"}

	// label nil -> column stays NULL.
	require.NoError(t, repo.UpdateMnemeEnrichment(ctx, id, nil, repsJSON, tags, entities, spanStart, spanEnd, metaJSON))

	var (
		label            *string
		gotReps, gotMeta []byte
		gotTags, gotEnt  []string
		gotStart, gotEnd time.Time
	)
	require.NoError(t, repo.Pool().QueryRow(ctx, `
		SELECT label, representatives, tags, entities, span_start, span_end, meta
		FROM semantic.mnemes WHERE id = $1`, id).Scan(
		&label, &gotReps, &gotTags, &gotEnt, &gotStart, &gotEnd, &gotMeta))

	require.Nil(t, label, "label is NULL when passed nil")
	require.ElementsMatch(t, tags, gotTags)
	require.ElementsMatch(t, entities, gotEnt)
	require.True(t, gotStart.Equal(spanStart), "span_start round-trips")
	require.True(t, gotEnd.Equal(spanEnd), "span_end round-trips")

	var decoded []enrichRep
	require.NoError(t, json.Unmarshal(gotReps, &decoded))
	require.Equal(t, reps, decoded, "representatives decode to the stored array")

	var meta map[string]string
	require.NoError(t, json.Unmarshal(gotMeta, &meta))
	require.Equal(t, "consolidation-test", meta["source"])
}

// TestUpdateMnemeEnrichment_LabelCoalescePreservesExisting proves the
// COALESCE($2, label) semantics: a later enrichment pass with a nil label
// must not clobber a label the LLM step already set.
func TestUpdateMnemeEnrichment_LabelCoalescePreservesExisting(t *testing.T) {
	repo := newSemRepo(t)
	ctx := context.Background()
	ws := uuid.New()

	id, err := repo.InsertMneme(ctx, ws, vec1024(0.1), []uuid.UUID{uuid.New()})
	require.NoError(t, err)
	span := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)

	want := "consolidation cluster"
	require.NoError(t, repo.UpdateMnemeEnrichment(ctx, id, &want, []byte(`[]`), nil, nil, span, span, []byte(`{}`)))
	// Re-enrich with nil label -> COALESCE leaves the existing label intact.
	require.NoError(t, repo.UpdateMnemeEnrichment(ctx, id, nil, []byte(`[]`), []string{"go"}, nil, span, span, []byte(`{}`)))

	var label *string
	require.NoError(t, repo.Pool().QueryRow(ctx, `SELECT label FROM semantic.mnemes WHERE id = $1`, id).Scan(&label))
	require.NotNil(t, label)
	require.Equal(t, want, *label)
}
