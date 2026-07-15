package consolidation

import (
	"bytes"

	"github.com/google/uuid"

	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
)

// BestOverlapMatch returns the id of the existing level-1 mneme with the
// largest member_ids intersection with clusterMembers, or uuid.Nil when
// no existing mneme shares any member (caller should insert).
//
// Mirrors mentat/mnemes.py: "&& then ORDER BY count(unnest ∩) DESC LIMIT
// 1". Ties break deterministically by smallest mneme UUID (Postgres LIMIT
// 1 on an unordered tie was nondeterministic; we make the Go port stable —
// see internal/primitives/settle.go for the repo's UUID-sorted
// determinism precedent).
func BestOverlapMatch(clusterMembers []uuid.UUID, existing []repository.Level1Mneme) uuid.UUID {
	want := make(map[uuid.UUID]struct{}, len(clusterMembers))
	for _, m := range clusterMembers {
		want[m] = struct{}{}
	}

	bestID := uuid.Nil
	bestCount := 0
	for _, m := range existing {
		count := 0
		for _, mem := range m.MemberIDs {
			if _, ok := want[mem]; ok {
				count++
			}
		}
		if count == 0 {
			continue
		}
		if count > bestCount || (count == bestCount && lessUUID(m.ID, bestID)) {
			bestCount = count
			bestID = m.ID
		}
	}
	return bestID
}

// lessUUID reports whether a sorts before b by raw 16-byte order. bestID
// starting at uuid.Nil is handled naturally: any real id compares greater
// than Nil is false for the first hit because bestCount is 0 there, so the
// count>bestCount branch always takes the first candidate.
func lessUUID(a, b uuid.UUID) bool {
	return bytes.Compare(a[:], b[:]) < 0
}
