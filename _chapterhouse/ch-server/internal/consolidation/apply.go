package consolidation

import (
	"context"
	"fmt"

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
// Reads the existing level-1 set ONCE up front (snapshot) so the match
// decisions within a run are stable and cheap; the writes then apply.
func ApplyClusters(ctx context.Context, repo *repository.Repository, workspaceID uuid.UUID, assigns []ClusterAssignment) (int, error) {
	existing, err := repo.WorkspaceLevel1Mnemes(ctx, workspaceID)
	if err != nil {
		return 0, fmt.Errorf("read existing mnemes: %w", err)
	}
	byID := make(map[uuid.UUID]repository.Level1Mneme, len(existing))
	for _, m := range existing {
		byID[m.ID] = m
	}

	written := 0
	for _, a := range assigns {
		if len(a.MemberIDs) == 0 {
			continue
		}
		matchID := BestOverlapMatch(a.MemberIDs, existing)
		if matchID == uuid.Nil {
			if _, err := repo.InsertMneme(ctx, workspaceID, a.Centroid, a.MemberIDs); err != nil {
				return written, err
			}
			written++
			continue
		}
		if sameMembers(byID[matchID].MemberIDs, a.MemberIDs) {
			continue // idempotent: unchanged membership, no reinforce
		}
		if err := repo.ReinforceMneme(ctx, matchID, a.Centroid, a.MemberIDs); err != nil {
			return written, err
		}
		written++
	}
	return written, nil
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
