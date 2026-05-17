// Package consolidation runs the offline consolidation cycle: drain
// the co-activation queue (Pipeline A's by-product) and fold each
// pair into semantic.associations as a strengthened Hebbian link.
//
// The package is intentionally thin — all SQL lives in the repository
// layer; consolidation is orchestration only. The worker entrypoint
// (cmd/worker, see C3) calls DrainAndStrengthen on a tick.
//
// Spec parity: extension/src/hebbian.rs::drain_co_activation_queue.
package consolidation

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/thinkwright/chapterhouse/ch-server/internal/repository"
)

// DrainAndStrengthen drains up to batchSize pairs from
// semantic.co_activation_queue, upserts each into
// semantic.associations (incrementing co_activations and recomputing
// weight via the saturation formula), and deletes the consumed queue
// rows — all inside a single transaction.
//
// Idempotency: if any step fails the whole tx rolls back, so the
// queue rows survive and the next tick replays them. UpsertAssociation
// is itself idempotent (ON CONFLICT DO UPDATE), so a drained-but-not-
// deleted retry would only be at risk of double-counting; the single
// tx prevents that case from existing.
//
// Returns the number of pairs processed (== queue rows consumed).
// Empty queue is a fast no-op: no transaction, no error.
func DrainAndStrengthen(
	ctx context.Context, repo *repository.Repository, batchSize int,
) (int, error) {
	if batchSize <= 0 {
		return 0, nil
	}

	var processed int
	err := repo.WithTxRaw(ctx, func(tx pgx.Tx) error {
		pairs, ids, err := repo.DrainCoActivationQueueTx(ctx, tx, batchSize)
		if err != nil {
			return fmt.Errorf("drain queue: %w", err)
		}
		if len(pairs) == 0 {
			// Nothing to do — let the empty tx commit cleanly.
			return nil
		}

		for i, p := range pairs {
			assoc := repository.Association{
				SrcEventID:      p.SrcEventID,
				DstEventID:      p.DstEventID,
				AssociationType: "hebbian",
				WorkspaceID:     p.WorkspaceID,
			}
			if err := repo.UpsertAssociationTx(ctx, tx, assoc); err != nil {
				return fmt.Errorf("upsert association %d: %w", i, err)
			}
		}

		if err := repo.DeleteCoActivationQueueRowsTx(ctx, tx, ids); err != nil {
			return fmt.Errorf("delete queue rows: %w", err)
		}

		processed = len(pairs)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return processed, nil
}
