package jsonlfamily_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/logan-broit/ghola/internal/importlogs"
	"github.com/logan-broit/ghola/internal/importlogs/adapters/jsonlfamily"
)

func TestJSONLFamily_ParsesCanonicalSession(t *testing.T) {
	a := jsonlfamily.New()
	got, err := a.Parse(importlogs.SessionFile{Path: "testdata/canonical.jsonl"})
	require.NoError(t, err)
	require.Equal(t, "claude-code", got.SourceTool)
	require.GreaterOrEqual(t, len(got.Events), 2)
	require.Equal(t, "user", got.Events[0].Type)
	require.False(t, got.Events[0].Timestamp.IsZero())

	// Tool-result splitting invariant: a user-typed envelope whose
	// nested message.content is an array carrying a tool_result block
	// MUST be re-classified as a "tool_result" normalized event, not
	// folded into the surrounding "user" turns. Without this the
	// downstream replay pipeline sees two consecutive user turns and
	// loses the assistant<->tool round-trip boundary.
	var sawToolResult bool
	for _, e := range got.Events {
		if e.Type == "tool_result" {
			sawToolResult = true
			require.Contains(t, e.Text, "vllm-qwen36",
				"tool_result event should preserve the tool's output text")
			break
		}
	}
	require.True(t, sawToolResult,
		"fixture should produce at least one tool_result event")
}
