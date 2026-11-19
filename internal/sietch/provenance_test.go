package sietch_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/logan-broit/ghola/internal/core"
)

func TestRecallProvenance_Allowlist(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sess := mkSession("u1")
	require.NoError(t, s.OpenSession(ctx, sess))
	ev := mkEvent(sess, "provenancefixture complete text", []float32{1, 0})
	ev.RawEvent = json.RawMessage(`{
		"producer":"pi-ghola/1", "actor":"pi", "verification":"unverified",
		"captured_at":"2026-09-17T03:00:00.000Z", "annotation":false,
		"pi":{"session_id":"00000000-0000-4000-8000-000000000031", "entry_id":"a1b2c3d4",
			"parent_entry_id":"abcd1234", "previous_episode_id":"00000000-0000-4000-8000-000000000041",
			"journal_persisted":false, "timestamp":"2026-09-17T02:59:00Z", "stop_reason":"aborted",
			"parent_session_file":"/private/path-not-returned", "provider_payload":"private-marker"},
		"repository":{"revision":"0123456789012345678901234567890123456789", "dirty":false, "credentials":"private-marker"},
		"thinking":"private-marker", "tool_output":{"blob":"private-marker"}
	}`)
	_, err := s.RecordEvent(ctx, ev)
	require.NoError(t, err)
	want := `{"producer":"pi-ghola/1","actor":"pi","verification":"unverified",
		"captured_at":"2026-09-17T03:00:00.000Z","annotation":false,
		"pi":{"session_id":"00000000-0000-4000-8000-000000000031","entry_id":"a1b2c3d4",
			"parent_entry_id":"abcd1234","previous_episode_id":"00000000-0000-4000-8000-000000000041",
			"journal_persisted":false,"timestamp":"2026-09-17T02:59:00Z","stop_reason":"aborted"},
		"repository":{"revision":"0123456789012345678901234567890123456789","dirty":false}}`
	searches := map[string]func() ([]core.RecallHit, error){
		"fts":    func() ([]core.RecallHit, error) { return s.SearchFTS(ctx, sess.ID, "provenancefixture", 1) },
		"vector": func() ([]core.RecallHit, error) { return s.SearchVector(ctx, sess.ID, []float32{1, 0}, 1) },
	}
	for name, search := range searches {
		t.Run(name, func(t *testing.T) {
			hits, err := search()
			require.NoError(t, err)
			require.Len(t, hits, 1)
			require.Equal(t, ev.ID, hits[0].ID)
			require.Equal(t, *ev.Text, hits[0].Content)
			wire, err := json.Marshal(hits[0])
			require.NoError(t, err)
			var hit map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(wire, &hit))
			require.Contains(t, hit, "provenance")
			require.JSONEq(t, want, string(hit["provenance"]))
			require.NotContains(t, string(wire), "private-marker")
			require.NotContains(t, string(wire), "private/path")
			require.NotContains(t, string(wire), "raw_event")
			require.Less(t, len(hit["provenance"]), 4096)
		})
	}
	pending, err := s.PendingEvents(ctx, sess.ID, "")
	require.NoError(t, err)
	require.Equal(t, ev.RawEvent, pending[0].RawEvent, "projection must not rewrite the captured record")
	require.NoError(t, s.SoftForget(ctx, sess.ID, []string{ev.ID}))
	for _, search := range searches {
		hits, err := search()
		require.NoError(t, err)
		require.Empty(t, hits, "forgotten records cannot expose provenance")
	}
}

func TestRecallProvenance_UnknownMetadataDoesNotBreakRecall(t *testing.T) {
	for name, raw := range map[string]string{
		"legacy":             `{"model":"legacy","actor":"do-not-infer"}`,
		"other-producer":     `{"producer":"other/1","actor":"do-not-infer"}`,
		"malformed":          `{"producer":`,
		"wrong-shape":        `[]`,
		"wrong-field-type":   `{"producer":"pi-ghola/1","pi":{"journal_persisted":"false"}}`,
		"oversize":           `{"producer":"pi-ghola/1","hidden":"` + strings.Repeat("x", 16384) + `"}`,
		"unicode-byte-bound": `{"producer":"pi-ghola/1","hidden":"` + strings.Repeat("中", 6000) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s := newTestStore(t)
			sess := mkSession("u1")
			require.NoError(t, s.OpenSession(ctx, sess))
			ev := mkEvent(sess, "unknownfixture still searchable", []float32{1, 0})
			ev.RawEvent = json.RawMessage(raw)
			_, err := s.RecordEvent(ctx, ev)
			require.NoError(t, err)
			fts, err := s.SearchFTS(ctx, sess.ID, "unknownfixture", 1)
			require.NoError(t, err)
			vec, err := s.SearchVector(ctx, sess.ID, []float32{1, 0}, 1)
			require.NoError(t, err)
			for _, hits := range [][]core.RecallHit{fts, vec} {
				require.Len(t, hits, 1)
				require.Equal(t, *ev.Text, hits[0].Content)
				wire, err := json.Marshal(hits[0])
				require.NoError(t, err)
				require.NotContains(t, string(wire), "provenance")
			}
		})
	}
}

func TestRecallProvenance_InvalidFieldsRemainUnknown(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sess := mkSession("u1")
	require.NoError(t, s.OpenSession(ctx, sess))
	ev := mkEvent(sess, "invalidfixture", nil)
	ev.RawEvent = json.RawMessage(`{"producer":"pi-ghola/1","actor":"` + strings.Repeat("x", 129) + `",
		"verification":"verified","captured_at":"not-a-time",
		"pi":{"session_id":"not-a-uuid", "entry_id":"` + strings.Repeat("x", 129) + `",
		"parent_entry_id":"<instructions>","previous_episode_id":"../bad", "timestamp":"bad", "stop_reason":"success"},
		"repository":{"revision":"not-a-commit","dirty":null}}`)
	_, err := s.RecordEvent(ctx, ev)
	require.NoError(t, err)
	hits, err := s.SearchFTS(ctx, sess.ID, "invalidfixture", 1)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	wire, err := json.Marshal(hits[0])
	require.NoError(t, err)
	var hit map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wire, &hit))
	require.Contains(t, hit, "provenance")
	require.JSONEq(t, `{"producer":"pi-ghola/1"}`, string(hit["provenance"]))
}

func TestRecallProvenance_VectorKeepsSelectedEventMetadata(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	sess := mkSession("u1")
	require.NoError(t, s.OpenSession(ctx, sess))
	for i, vector := range [][]float32{{1, 0}, {0, 1}} {
		ev := mkEvent(sess, "vectorfixture", vector)
		ev.RawEvent = json.RawMessage(`{"producer":"pi-ghola/1","actor":"` + []string{"selected", "other"}[i] + `"}`)
		_, err := s.RecordEvent(ctx, ev)
		require.NoError(t, err)
	}
	hits, err := s.SearchVector(ctx, sess.ID, []float32{1, 0}, 1)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	wire, err := json.Marshal(hits[0])
	require.NoError(t, err)
	require.Contains(t, string(wire), `"actor":"selected"`)
	require.NotContains(t, string(wire), `"actor":"other"`)
}
