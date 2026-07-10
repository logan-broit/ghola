package consolidation_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/consolidation"
)

func fakeChat(t *testing.T, reply string, status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": reply}}},
		})
	}))
}

func TestLLM_Label_OneLine(t *testing.T) {
	srv := fakeChat(t, "Refactor the consolidation worker pipeline\n", 200)
	defer srv.Close()
	c := consolidation.NewLLMClient(srv.URL, "test-model", "")
	got, err := c.Label(context.Background(), []string{"excerpt one", "excerpt two"})
	require.NoError(t, err)
	require.Equal(t, "Refactor the consolidation worker pipeline", got, "trims + collapses to one line")
	require.LessOrEqual(t, len(got), 80)
}

func TestLLM_Down_ReturnsError(t *testing.T) {
	srv := fakeChat(t, "", 500)
	defer srv.Close()
	c := consolidation.NewLLMClient(srv.URL, "test-model", "")
	_, err := c.Label(context.Background(), []string{"x"})
	require.Error(t, err, "LLM 5xx surfaces as error; the PIPELINE decides to skip")
}

func TestNewLLMClient_EmptyURL_IsNil(t *testing.T) {
	require.Nil(t, consolidation.NewLLMClient("", "m", ""))
}
