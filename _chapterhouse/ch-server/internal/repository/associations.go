package repository

// associations.go — Hebbian co-activation primitives, repo layer.
//
// Convention deviation note (vs. the seeding-eval-harness plan B3): the
// plan suggested a per-domain `AssociationsRepo` struct with its own
// constructor. The existing repo idiom (episodic.go, semantic.go) is to
// attach methods directly to *Repository so all SQL paths share the
// same *pgxpool.Pool + WithTx machinery. We follow the existing idiom
// — every B4/B5/C2 stub on this file is a method on *Repository. The
// plan's CoActivationPair / Association value types are kept verbatim.
//
// B4 has landed the queue ops (Enqueue/Drain/Delete). B5 still has
// stubs for UpsertAssociation / LookupAssociations. C2's tx-aware
// variants are deferred — the existing WithTx / BeginTx wrappers on
// *Repository (see repository.go) are likely sufficient, but naming
// is left to be revisited at C2.

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// pgxQuerier is the implicit interface satisfied by both
// *pgxpool.Pool and pgx.Tx — the parts of pgx we need to share SQL
// bodies between the non-tx and tx-aware variants of the queue ops.
// Keeping this internal to associations.go avoids a stray dependency
// surface across the rest of the repo package.
type pgxQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// pgxExecer is the Exec-shaped half of pgxQuerier. Same rationale.
type pgxExecer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// CoActivationPair is one (src, dst, workspace) tuple buffered in
// semantic.co_activation_queue by the ingest path. The consolidation
// worker drains these in batches, folds each pair into
// semantic.associations (incrementing co_activations + recomputing
// weight), then deletes the queue rows.
//
// Order matters: src is the event the recall query lights up first;
// dst is the event we want to surface alongside it. Hebbian links are
// directional — a pair (A, B) does not imply (B, A) is queued.
type CoActivationPair struct {
	SrcEventID  uuid.UUID
	DstEventID  uuid.UUID
	WorkspaceID uuid.UUID
}

// Association mirrors one row of semantic.associations. Weight is the
// activation-strength scalar consumed by the fusion-stage Hebbian
// boost; co_activations is the raw count we recompute weight from
// (`1 - exp(-co_activations / 5.0)`, see B5).
//
// AssociationType is one of {'hebbian','contradicts','supersedes',
// 'supports'} — the CHECK constraint on the table enforces the set.
// v0.4 only writes 'hebbian'; the other kinds are reserved.
type Association struct {
	SrcEventID      uuid.UUID
	DstEventID      uuid.UUID
	AssociationType string
	Weight          float64
	CoActivations   int
	WorkspaceID     uuid.UUID
	UpdatedAt       time.Time
}

// EnqueueCoActivations bulk-inserts co-activation pairs into
// semantic.co_activation_queue. Called by the ingest path (Pipeline A)
// inside the same tx that writes events, so a successful POST /ingest
// guarantees both the events and their co-activation work-items land
// atomically.
//
// Empty input is a fast no-op: returns nil without touching the DB.
// Non-empty inserts go through pgx.Batch — one round trip per call,
// matching the bulk-upsert idiom used by IngestEpisodicBatch.
func (r *Repository) EnqueueCoActivations(ctx context.Context, pairs []CoActivationPair) error {
	if len(pairs) == 0 {
		return nil
	}

	const enqueueSQL = `
		INSERT INTO semantic.co_activation_queue
			(src_event_id, dst_event_id, workspace_id)
		VALUES ($1, $2, $3)
	`

	batch := &pgx.Batch{}
	for _, p := range pairs {
		batch.Queue(enqueueSQL, p.SrcEventID, p.DstEventID, p.WorkspaceID)
	}

	br := r.pool.SendBatch(ctx, batch)
	defer br.Close()

	for i := range pairs {
		if _, err := br.Exec(); err != nil {
			return fmt.Errorf("enqueue co-activation pair %d: %w", i, err)
		}
	}
	return nil
}

// DrainCoActivationQueue pulls up to batchSize rows out of
// semantic.co_activation_queue oldest-first and returns the pairs in
// lockstep with their queue row IDs. The caller (consolidation worker)
// folds each pair into semantic.associations via UpsertAssociation,
// then deletes the queue rows by ID via DeleteCoActivationQueueRows.
//
// Returning ids alongside pairs (rather than re-deriving them) keeps
// the worker loop transactional without round-tripping a SELECT-then-
// match-by-tuple step.
//
// Empty queue returns (nil, nil, nil) — not an error.
func (r *Repository) DrainCoActivationQueue(
	ctx context.Context, batchSize int,
) ([]CoActivationPair, []int64, error) {
	return drainCoActivationQueue(ctx, r.pool, batchSize)
}

// DrainCoActivationQueueTx is the tx-aware variant of
// DrainCoActivationQueue. The consolidation worker (C2) needs to
// drain + upsert + delete inside one transaction so a partial failure
// rolls back cleanly and the same rows can be replayed on the next
// tick. Same return contract.
func (r *Repository) DrainCoActivationQueueTx(
	ctx context.Context, tx pgx.Tx, batchSize int,
) ([]CoActivationPair, []int64, error) {
	return drainCoActivationQueue(ctx, tx, batchSize)
}

// drainCoActivationQueue is the shared body parameterized by a pgx
// query runner — pgxpool.Pool and pgx.Tx both satisfy the implicit
// interface (Query). Keeping the SQL in one place avoids drift
// between the tx-aware and non-tx variants.
func drainCoActivationQueue(
	ctx context.Context, q pgxQuerier, batchSize int,
) ([]CoActivationPair, []int64, error) {
	rows, err := q.Query(ctx, `
		SELECT id, src_event_id, dst_event_id, workspace_id
		FROM semantic.co_activation_queue
		ORDER BY enqueued_at ASC
		LIMIT $1
	`, batchSize)
	if err != nil {
		return nil, nil, fmt.Errorf("drain co_activation_queue: %w", err)
	}
	defer rows.Close()

	var (
		pairs []CoActivationPair
		ids   []int64
	)
	for rows.Next() {
		var (
			id   int64
			pair CoActivationPair
		)
		if err := rows.Scan(&id, &pair.SrcEventID, &pair.DstEventID, &pair.WorkspaceID); err != nil {
			return nil, nil, fmt.Errorf("scan co_activation_queue row: %w", err)
		}
		ids = append(ids, id)
		pairs = append(pairs, pair)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate co_activation_queue rows: %w", err)
	}
	return pairs, ids, nil
}

// DeleteCoActivationQueueRows removes the queue rows whose IDs the
// caller is acknowledging. Paired with DrainCoActivationQueue: the
// worker drains, processes, then deletes.
//
// Empty input is a fast no-op: returns nil without touching the DB.
func (r *Repository) DeleteCoActivationQueueRows(ctx context.Context, ids []int64) error {
	return deleteCoActivationQueueRows(ctx, r.pool, ids)
}

// DeleteCoActivationQueueRowsTx is the tx-aware variant. See C2 in
// internal/consolidation/drain.go for the drain+upsert+delete cycle.
func (r *Repository) DeleteCoActivationQueueRowsTx(
	ctx context.Context, tx pgx.Tx, ids []int64,
) error {
	return deleteCoActivationQueueRows(ctx, tx, ids)
}

func deleteCoActivationQueueRows(ctx context.Context, q pgxExecer, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	if _, err := q.Exec(ctx, `
		DELETE FROM semantic.co_activation_queue
		WHERE id = ANY($1::bigint[])
	`, ids); err != nil {
		return fmt.Errorf("delete co_activation_queue rows: %w", err)
	}
	return nil
}

// hebbianSaturationK is the saturation constant in the weight
// formula `1 - exp(-co_activations / k)`. k=5 is locked from the
// design doc decision log: at n=5 the curve is at ~63%, at n=10
// it's at ~86%, and it never exceeds 1. Hardcoded for v0.4 — promote
// to config if the consolidation worker grows tuning knobs.
const hebbianSaturationK = 5.0

// UpsertAssociation folds one co-activation event into
// semantic.associations. INSERT … ON CONFLICT (src_event_id,
// dst_event_id, association_type) DO UPDATE SET co_activations =
// co_activations + 1, weight = 1 - exp(-co_activations / 5.0),
// updated_at = now().
//
// The Weight / CoActivations / UpdatedAt fields on the input are
// ignored on both branches — weight is always derived from the
// running co_activations counter. They're carried on the type so
// LookupAssociations can hand the same shape back without an extra
// read model.
//
// Initial-row weight (insert branch) uses the formula at n=1
// (~0.18127), not the column default of 0.01. The default is a
// safety net for any rows that bypass this path; the formula is the
// source of truth and must agree with the update branch.
func (r *Repository) UpsertAssociation(ctx context.Context, assoc Association) error {
	return upsertAssociation(ctx, r.pool, assoc)
}

// UpsertAssociationTx is the tx-aware variant. See C2 in
// internal/consolidation/drain.go for the drain+upsert+delete cycle.
func (r *Repository) UpsertAssociationTx(
	ctx context.Context, tx pgx.Tx, assoc Association,
) error {
	return upsertAssociation(ctx, tx, assoc)
}

func upsertAssociation(ctx context.Context, q pgxExecer, assoc Association) error {
	const upsertSQL = `
		INSERT INTO semantic.associations
			(src_event_id, dst_event_id, association_type, weight, co_activations, workspace_id, updated_at)
		VALUES
			($1, $2, $3, $4, 1, $5, now())
		ON CONFLICT (src_event_id, dst_event_id, association_type) DO UPDATE SET
			co_activations = semantic.associations.co_activations + 1,
			weight         = 1 - exp(-(semantic.associations.co_activations + 1)::float / $6),
			updated_at     = now()
	`
	initialWeight := 1 - math.Exp(-1.0/hebbianSaturationK)
	if _, err := q.Exec(ctx, upsertSQL,
		assoc.SrcEventID,
		assoc.DstEventID,
		assoc.AssociationType,
		initialWeight,
		assoc.WorkspaceID,
		hebbianSaturationK,
	); err != nil {
		return fmt.Errorf("upsert association: %w", err)
	}
	return nil
}

// LookupAssociations bulk-fetches associations whose src_event_id is
// in srcIDs, filtered to the given association_type and workspace_id.
// Returns a map keyed by src_event_id; a src with no associations is
// omitted from the map (callers expect map-miss == "no Hebbian
// neighbors", not "empty slice").
//
// Workspace scoping is non-negotiable: associations are
// workspace-scoped at the data model, and the fusion path must not
// see neighbors from a different scope even if the event UUIDs match.
//
// Empty srcIDs returns an empty (non-nil) map without hitting the DB
// — easier for callers than a nil-vs-empty distinction.
func (r *Repository) LookupAssociations(
	ctx context.Context,
	srcIDs []uuid.UUID,
	assocType string,
	workspaceID uuid.UUID,
) (map[uuid.UUID][]Association, error) {
	out := make(map[uuid.UUID][]Association)
	if len(srcIDs) == 0 {
		return out, nil
	}

	const lookupSQL = `
		SELECT src_event_id, dst_event_id, association_type, weight, co_activations, workspace_id, updated_at
		FROM semantic.associations
		WHERE src_event_id = ANY($1::uuid[])
		  AND association_type = $2
		  AND workspace_id = $3
	`
	rows, err := r.pool.Query(ctx, lookupSQL, srcIDs, assocType, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("lookup associations: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var a Association
		if err := rows.Scan(
			&a.SrcEventID,
			&a.DstEventID,
			&a.AssociationType,
			&a.Weight,
			&a.CoActivations,
			&a.WorkspaceID,
			&a.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan association row: %w", err)
		}
		out[a.SrcEventID] = append(out[a.SrcEventID], a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate association rows: %w", err)
	}
	return out, nil
}
