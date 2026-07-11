package consolidation_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/consolidation"
)

func TestParseWorkspaces(t *testing.T) {
	valid := uuid.New().String()
	got := consolidation.ParseWorkspaces(valid + ",not-a-uuid, " + uuid.Nil.String())
	require.Len(t, got, 2, "keeps valid uuids, drops garbage")
	require.Empty(t, consolidation.ParseWorkspaces(""))
	require.Empty(t, consolidation.ParseWorkspaces("   "))
}

func TestNextRunDelay(t *testing.T) {
	// 01:00 now, target hour 2 -> ~1h.
	now := time.Date(2026, 7, 6, 1, 0, 0, 0, time.Local)
	d := consolidation.NextRunDelay(now, 2)
	require.InDelta(t, time.Hour.Seconds(), d.Seconds(), 1)
	// 03:00 now, target hour 2 -> ~23h (tomorrow).
	now2 := time.Date(2026, 7, 6, 3, 0, 0, 0, time.Local)
	d2 := consolidation.NextRunDelay(now2, 2)
	require.InDelta(t, (23 * time.Hour).Seconds(), d2.Seconds(), 1)
}
