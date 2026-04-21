package replay_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/replay"
)

// fakeVLLM is an httptest.Server that echoes a canned chat.completions
// reply with the given assistant text.
func fakeVLLM(t *testing.T, replyText string, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if status != 0 && status != 200 {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		resp := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"role": "assistant", "content": replyText}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestMentat_Distill_ValidJSON(t *testing.T) {
	srv := fakeVLLM(t, `{"concept":"CNPG runs Postgres","content":"Chapterhouse provisions Postgres clusters via the CloudNativePG operator.","memory_type":"factual","entities":["CNPG","Postgres"]}`, 200)

	c := &replay.MentatClient{BaseURL: srv.URL, Model: "test-model"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m, err := c.Distill(ctx, replay.DistillInput{
		E1: "CNPG", E2: "Postgres",
		Turns: []string{"we use CNPG for postgres"},
	})
	require.NoError(t, err)
	assert.Equal(t, "CNPG runs Postgres", m.Concept)
	assert.Equal(t, "factual", m.MemoryType)
	assert.ElementsMatch(t, []string{"CNPG", "Postgres"}, m.Entities)
}

func TestMentat_Distill_StripsMarkdownFence(t *testing.T) {
	srv := fakeVLLM(t, "```json\n{\"concept\":\"x\",\"content\":\"y\",\"memory_type\":\"factual\"}\n```", 200)

	c := &replay.MentatClient{BaseURL: srv.URL, Model: "m"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m, err := c.Distill(ctx, replay.DistillInput{E1: "x", E2: "y", Turns: []string{"t"}})
	require.NoError(t, err)
	assert.Equal(t, "x", m.Concept)
}

func TestMentat_Distill_RejectsMalformed(t *testing.T) {
	// No JSON object at all.
	srv := fakeVLLM(t, "I refuse to answer in JSON.", 200)

	c := &replay.MentatClient{BaseURL: srv.URL, Model: "m"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	m, err := c.Distill(ctx, replay.DistillInput{E1: "a", E2: "b", Turns: []string{"t"}})
	require.Error(t, err)
	assert.Nil(t, m)
	assert.Contains(t, err.Error(), "no JSON object")
}

func TestMentat_Distill_RejectsInvalidMemoryType(t *testing.T) {
	srv := fakeVLLM(t, `{"concept":"c","content":"x","memory_type":"bogus"}`, 200)

	c := &replay.MentatClient{BaseURL: srv.URL, Model: "m"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := c.Distill(ctx, replay.DistillInput{E1: "a", E2: "b", Turns: []string{"t"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "memory_type")
}

func TestMentat_Distill_SurfacesHTTPError(t *testing.T) {
	srv := fakeVLLM(t, "", 500)

	c := &replay.MentatClient{BaseURL: srv.URL, Model: "m"}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := c.Distill(ctx, replay.DistillInput{E1: "a", E2: "b", Turns: []string{"t"}})
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "500"))
}
