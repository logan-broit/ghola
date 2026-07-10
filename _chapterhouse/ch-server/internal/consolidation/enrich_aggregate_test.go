package consolidation_test

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/consolidation"
)

func TestExcerpt_BoundsTo500(t *testing.T) {
	long := strings.Repeat("x", 900)
	got := consolidation.Excerpt(long)
	require.LessOrEqual(t, len(got), 500)
	require.Equal(t, strings.Repeat("x", 500), got)
	require.Equal(t, "short", consolidation.Excerpt("short"))
}

func TestAggregate_TagsUnionTopN_AndSpan(t *testing.T) {
	t0 := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)
	reps := []consolidation.Rep{
		{EventID: uuid.New(), SessionID: uuid.New(), Excerpt: "a", CreatedAt: t0, Tags: []string{"go", "db"}, Entities: []string{"pgvector"}},
		{EventID: uuid.New(), SessionID: uuid.New(), Excerpt: "b", CreatedAt: t1, Tags: []string{"go", "test"}, Entities: []string{"pgvector", "hdbscan"}},
	}
	agg := consolidation.Aggregate(reps, 5)
	require.Equal(t, t0, agg.SpanStart)
	require.Equal(t, t1, agg.SpanEnd)
	// "go" appears twice -> first in frequency order.
	require.Equal(t, "go", agg.Tags[0])
	require.ElementsMatch(t, []string{"go", "db", "test"}, agg.Tags)
	require.ElementsMatch(t, []string{"pgvector", "hdbscan"}, agg.Entities)
}

func TestAggregate_EmptyRepsGuard(t *testing.T) {
	agg := consolidation.Aggregate(nil, 5)
	require.Empty(t, agg.Tags)
	require.Empty(t, agg.Entities)
	require.True(t, agg.SpanStart.IsZero())
}
