package consolidation_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/thinkwright/chapterhouse/ch-server/internal/consolidation"
	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
)

func TestBestOverlapMatch(t *testing.T) {
	a, b, c, d, e := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	m1 := repository.Level1Mneme{ID: uuid.New(), MemberIDs: []uuid.UUID{a, b}}
	m2 := repository.Level1Mneme{ID: uuid.New(), MemberIDs: []uuid.UUID{a, b, c}}

	cases := []struct {
		name     string
		cluster  []uuid.UUID
		existing []repository.Level1Mneme
		wantID   uuid.UUID // uuid.Nil means "no match, insert"
	}{
		{"no existing -> insert", []uuid.UUID{a, b}, nil, uuid.Nil},
		{"no overlap -> insert", []uuid.UUID{d, e}, []repository.Level1Mneme{m1}, uuid.Nil},
		{"single overlap -> match", []uuid.UUID{a, d}, []repository.Level1Mneme{m1}, m1.ID},
		{"largest overlap wins", []uuid.UUID{a, b, c}, []repository.Level1Mneme{m1, m2}, m2.ID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := consolidation.BestOverlapMatch(tc.cluster, tc.existing)
			require.Equal(t, tc.wantID, got)
		})
	}
}

// Deterministic tie-break: equal overlap resolves by smallest mneme UUID.
func TestBestOverlapMatch_TieBreakByUUID(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	lo := repository.Level1Mneme{ID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), MemberIDs: []uuid.UUID{a}}
	hi := repository.Level1Mneme{ID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), MemberIDs: []uuid.UUID{b}}
	// cluster overlaps each by exactly 1 -> tie -> smallest id wins.
	got := consolidation.BestOverlapMatch([]uuid.UUID{a, b}, []repository.Level1Mneme{hi, lo})
	require.Equal(t, lo.ID, got)
}
