package consolidation_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

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

func TestLLM_Label_MultibyteTruncation(t *testing.T) {
	// "日" is 3 bytes in UTF-8; 40 runes = 120 bytes, well past the
	// 80-byte cap, and a naive line[:80] would split rune 27 (bytes
	// 78-81). Regression guard for the byte-slice truncation defect.
	long := strings.Repeat("日", 40)
	srv := fakeChat(t, long, 200)
	defer srv.Close()
	c := consolidation.NewLLMClient(srv.URL, "test-model", "")
	got, err := c.Label(context.Background(), []string{"excerpt"})
	require.NoError(t, err)
	require.True(t, utf8.ValidString(got), "label must be valid UTF-8, got %q", got)
	require.LessOrEqual(t, len(got), 80)
}

func TestLLM_Digest_ReturnsParagraph(t *testing.T) {
	srv := fakeChat(t, "The project is mid-refactor of the consolidation pipeline.", 200)
	defer srv.Close()
	c := consolidation.NewLLMClient(srv.URL, "test-model", "")
	got, err := c.Digest(context.Background(), []string{"label one", "label two"})
	require.NoError(t, err)
	require.Equal(t, "The project is mid-refactor of the consolidation pipeline.", got)
}

func TestLLM_MalformedJSON_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{not valid json"))
	}))
	defer srv.Close()
	c := consolidation.NewLLMClient(srv.URL, "test-model", "")
	_, err := c.Label(context.Background(), []string{"x"})
	require.Error(t, err, "malformed 200 body must surface as an error, not a panic or empty label")
}

func TestLLM_EmptyChoices_ReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{}})
	}))
	defer srv.Close()
	c := consolidation.NewLLMClient(srv.URL, "test-model", "")
	_, err := c.Label(context.Background(), []string{"x"})
	require.Error(t, err, "empty choices must surface as an error")
}

func TestLLM_AuthHeader_BearerWhenAPIKeySet(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "ok"}}},
		})
	}))
	defer srv.Close()
	c := consolidation.NewLLMClient(srv.URL, "test-model", "secret-key")
	_, err := c.Label(context.Background(), []string{"x"})
	require.NoError(t, err)
	require.Equal(t, "Bearer secret-key", gotAuth)
}

func TestLLM_AuthHeader_AbsentWhenAPIKeyEmpty(t *testing.T) {
	var gotAuth string
	seen := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = true
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": "ok"}}},
		})
	}))
	defer srv.Close()
	c := consolidation.NewLLMClient(srv.URL, "test-model", "")
	_, err := c.Label(context.Background(), []string{"x"})
	require.NoError(t, err)
	require.True(t, seen, "handler must have been called")
	require.Empty(t, gotAuth, "no Authorization header when apiKey is empty")
}
