package core_test

import (
	"context"
	"encoding/json"
	stdhttp "net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/logan-broit/ghola/internal/core"
	"github.com/logan-broit/ghola/internal/sietch"
	"github.com/logan-broit/ghola/internal/truthsayer"
)

func TestRecallProvenance_FusionKeepsOnlyMatchingEventSource(t *testing.T) {
	for _, name := range []string{"matching", "reranked", "different-session", "missing-session", "different-event", "semantic", "session-vector"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			c, _, ch, _ := newCore()
			store, err := sietch.Open(t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			c.Sietch = store
			sid, eventID := core.NewID(), core.NewID()
			require.NoError(t, store.OpenSession(ctx, core.Session{ID: sid, UserID: "u1", WorkspaceID: "ws1", StartedAt: time.Now()}))
			_, err = store.RecordEvent(ctx, core.Event{ID: eventID, SessionID: sid, UserID: "u1", Type: "assistant",
				Text: strPtr("provenancefixture local text"), Embedding: []float32{0.1, 0.2, 0.3}, CreatedAt: time.Now(),
				RawEvent: json.RawMessage(`{"producer":"pi-ghola/1","actor":"pi","verification":"unverified","pi":{"entry_id":"a1b2c3d4"}}`)})
			require.NoError(t, err)
			remote := core.RecallHit{Tier: "episodic", ID: eventID, SessionID: &sid, Score: 2, Content: "remote exemplar"}
			switch name {
			case "different-session":
				remote.SessionID = strPtr(core.NewID())
			case "missing-session":
				remote.SessionID = nil
			case "different-event":
				remote.ID = core.NewID()
			case "semantic":
				remote.Tier = "semantic"
			case "session-vector":
				remote.Tier = "session_vector"
			}
			if name == "semantic" {
				ch.semResp = []core.RecallHit{remote}
			} else if name == "session-vector" {
				ch.svResp = []core.RecallHit{remote}
			} else {
				ch.episResp = []core.RecallHit{remote}
			}
			if name == "reranked" {
				server := httptest.NewServer(stdhttp.HandlerFunc(func(w stdhttp.ResponseWriter, r *stdhttp.Request) {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{"scores": []map[string]any{{"id": eventID, "score": 0.75}}})
				}))
				defer server.Close()
				c.Truthsayer = truthsayer.New(server.URL).WithHTTPClient(server.Client())
			}
			result, err := c.Recall(ctx, core.RecallInput{UserID: "u1", Workspace: "ws1", SessionID: sid,
				QueryText: "provenancefixture", Limit: 10, IncludeSietch: true, IncludeEpisode: true, IncludeSemant: true, Settle: "off"})
			require.NoError(t, err)
			found := false
			for _, hit := range result.Hits {
				if hit.Content != remote.Content {
					continue
				}
				found = true
				require.Equal(t, remote.Tier, hit.Tier, "provenance must not change native tier attribution")
				if name == "matching" || name == "reranked" {
					require.NotNil(t, hit.Provenance, "working source must survive another tier winning the exemplar")
					require.Equal(t, "a1b2c3d4", hit.Provenance.Pi.EntryID)
					require.Equal(t, "unverified", hit.Provenance.Verification)
				} else {
					require.Nil(t, hit.Provenance, "never borrow source metadata across identities or grains")
				}
			}
			require.True(t, found)
		})
	}
}
