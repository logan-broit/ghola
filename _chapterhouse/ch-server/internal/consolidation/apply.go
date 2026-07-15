package consolidation

import (
	"context"
	"fmt"
	"sort"

	"github.com/google/uuid"

	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
)

// ClusterAssignment is one non-noise cluster from a mentat run: its
// member session ids and its (L2-normalized) centroid embedding.
type ClusterAssignment struct {
	MemberIDs []uuid.UUID
	Centroid  []float32
}

// ApplyClusters upserts one mneme per assignment for a workspace, using
// overlap-reinforcement identity. Returns the number of rows *written*
// (inserts + membership-changed reinforcements). A reinforcement whose
// membership is unchanged is skipped so re-running the same night is a
// no-op — the design's idempotency contract.
//
// Reads the existing level-1 set once, then maintains a WORKING in-memory
// view of the mneme set as it applies: a reinforcement rewrites the matched
// mneme's MemberIDs in the working view; an insert appends the new mneme
// (with its returned id). Each subsequent BestOverlapMatch therefore sees
// CURRENT membership. This is what makes a cluster split correct: when one
// existing mneme owns members that a re-cluster splits across several new
// assignments, the first assignment claims (reinforces) it and the rest see
// its shrunken membership and fall through to insert — instead of every
// assignment matching the stale snapshot and last-write-wins collapsing N
// clusters into one row (whose losers the enrich step then can't resolve).
//
// Assignments are applied in a stable order (smallest member UUID first) so
// which cluster wins the reinforce vs which insert is deterministic,
// independent of mentat's arbitrary label numbering.
//
// Precondition: assigns must be pairwise member-disjoint (as the pipeline's
// label-partitioning in groupClusters guarantees — each session id maps to
// at most one non-noise label). This function does not enforce disjointness:
// overlapping assignments passed in one call can both match the same
// existing mneme and reinforce it in sequence, so the result silently merges
// them into one row via reinforce-with-superset semantics (the second
// reinforcement's MemberIDs simply replace the first's) rather than
// producing two mnemes.
func ApplyClusters(ctx context.Context, repo *repository.Repository, workspaceID uuid.UUID, assigns []ClusterAssignment) (int, error) {
	existing, err := repo.WorkspaceLevel1Mnemes(ctx, workspaceID)
	if err != nil {
		return 0, fmt.Errorf("read existing mnemes: %w", err)
	}
	// Working view of the level-1 set, mutated as we apply so a split's
	// later cluster sees the membership its earlier cluster already claimed.
	work := make([]repository.Level1Mneme, len(existing))
	copy(work, existing)
	idx := make(map[uuid.UUID]int, len(work))
	for i, m := range work {
		idx[m.ID] = i
	}

	ordered := make([]ClusterAssignment, len(assigns))
	copy(ordered, assigns)
	sort.Slice(ordered, func(i, j int) bool {
		return lessUUID(minMember(ordered[i].MemberIDs), minMember(ordered[j].MemberIDs))
	})

	written := 0
	for _, a := range ordered {
		if len(a.MemberIDs) == 0 {
			continue
		}
		matchID := BestOverlapMatch(a.MemberIDs, work)
		if matchID == uuid.Nil {
			newID, err := repo.InsertMneme(ctx, workspaceID, a.Centroid, a.MemberIDs)
			if err != nil {
				return written, err
			}
			work = append(work, repository.Level1Mneme{ID: newID, MemberIDs: a.MemberIDs})
			idx[newID] = len(work) - 1
			written++
			continue
		}
		i := idx[matchID]
		if sameMembers(work[i].MemberIDs, a.MemberIDs) {
			continue // idempotent: unchanged membership, no reinforce
		}
		if err := repo.ReinforceMneme(ctx, matchID, a.Centroid, a.MemberIDs); err != nil {
			return written, err
		}
		work[i].MemberIDs = a.MemberIDs
		written++
	}
	return written, nil
}

// minMember returns the smallest UUID in a member set (uuid.Nil for empty).
// Used only to give ApplyClusters a stable, label-independent apply order.
func minMember(members []uuid.UUID) uuid.UUID {
	smallest := uuid.Nil
	for i, m := range members {
		if i == 0 || lessUUID(m, smallest) {
			smallest = m
		}
	}
	return smallest
}

func sameMembers(a, b []uuid.UUID) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[uuid.UUID]struct{}, len(a))
	for _, x := range a {
		set[x] = struct{}{}
	}
	for _, x := range b {
		if _, ok := set[x]; !ok {
			return false
		}
	}
	return true
}
