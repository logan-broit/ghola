package importlogs

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDeriveSessionID(t *testing.T) {
	raw := []byte(`{"some":"session"}`)
	require.Equal(t, DeriveSessionID("claude-code", raw), DeriveSessionID("claude-code", raw),
		"same input -> same uuid")
	require.NotEqual(t, DeriveSessionID("claude-code", raw), DeriveSessionID("openclaw", raw),
		"different tool -> different uuid")
}
