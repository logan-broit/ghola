package repository

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
)

// AdvisoryLockKey derives a stable 64-bit Postgres advisory-lock key from
// a workspace UUID: its first 8 bytes read big-endian as a signed int64.
// Deterministic per workspace, so the nightly job and the manual trigger
// contend on the same key. pg_try_advisory_lock takes a bigint (int64),
// and reinterpreting the unsigned big-endian value as signed is a lossless
// bit-pattern cast — collisions are astronomically unlikely across the v4
// UUID space and, if one ever occurred, would only serialize two unrelated
// workspaces (a correctness-safe over-approximation of the guard).
func AdvisoryLockKey(id uuid.UUID) int64 {
	return int64(binary.BigEndian.Uint64(id[:8]))
}

// TryWorkspaceConsolidationLock attempts to acquire a session-scoped
// Postgres advisory lock for the workspace on a DEDICATED pooled
// connection held for the caller's run. This is the concurrency guard for
// consolidation: two RunWorkspace calls on one workspace (nightly job +
// manual trigger, or two manual triggers) must not both cluster/insert and
// produce duplicate mnemes or double reinforcement.
//
// Returns:
//   - (release, true, nil)  — lock held; the caller owns it until it calls
//     release exactly once (idempotent), which unlocks and returns the
//     connection to the pool. Defer it.
//   - (nil, false, nil)     — lock is busy (another run holds it); the
//     connection has already been returned. Caller should back off
//     (ErrConsolidationBusy at the consolidation layer).
//   - (nil, false, err)     — a real DB error acquiring the connection or
//     issuing pg_try_advisory_lock.
//
// The lock is session-scoped: it lives on the acquired connection and is
// released only by pg_advisory_unlock on that same connection (or when it
// closes). release therefore unlocks BEFORE returning the connection to the
// pool, so a reused connection never carries a stale lock. It uses a
// detached, short-deadline context so a cancelled run context still frees
// the lock.
func (r *Repository) TryWorkspaceConsolidationLock(
	ctx context.Context, workspaceID uuid.UUID,
) (release func(), acquired bool, err error) {
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquire advisory-lock connection: %w", err)
	}

	key := AdvisoryLockKey(workspaceID)
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&locked); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("pg_try_advisory_lock: %w", err)
	}
	if !locked {
		conn.Release()
		return nil, false, nil
	}

	var once sync.Once
	release = func() {
		once.Do(func() {
			unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			// Best-effort: on failure the lock is freed when the pool
			// eventually closes/recycles the connection.
			_, _ = conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, key)
			conn.Release()
		})
	}
	return release, true, nil
}
