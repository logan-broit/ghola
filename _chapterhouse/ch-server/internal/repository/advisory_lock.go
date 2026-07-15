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

// advisoryLockAcquireTimeout bounds how long TryWorkspaceConsolidationLock
// waits for a pooled connection. The manual-trigger caller
// (handler/semantic.go's Consolidate) passes a context.WithoutCancel'd,
// deadline-free context so a client disconnect can't abort a run already in
// flight — but that also means, without a bound here, pool exhaustion would
// block the acquire indefinitely instead of surfacing as a fast, retryable
// error.
const advisoryLockAcquireTimeout = 5 * time.Second

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
// the lock. If pg_advisory_unlock itself errors, release does NOT return
// the connection to the pool — a "healthy" pooled connection secretly still
// holding the lock would 409 the workspace until the pool happened to
// recycle it — it hijacks the connection out of pool management and closes
// it directly instead, guaranteeing Postgres frees the lock immediately.
func (r *Repository) TryWorkspaceConsolidationLock(
	ctx context.Context, workspaceID uuid.UUID,
) (release func(), acquired bool, err error) {
	acquireCtx, cancel := context.WithTimeout(ctx, advisoryLockAcquireTimeout)
	defer cancel()
	conn, err := r.pool.Acquire(acquireCtx)
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
			_, unlockErr := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, key)
			disposeAdvisoryLockConn(unlockErr,
				func() {
					// Hijack takes the connection out of pool management;
					// closing the physical connection guarantees Postgres
					// frees any session-scoped lock it still held, however
					// pg_advisory_unlock failed.
					pc := conn.Hijack()
					_ = pc.Close(unlockCtx)
				},
				conn.Release,
			)
		})
	}
	return release, true, nil
}

// disposeAdvisoryLockConn decides how the lock-holding connection is
// returned after the unlock attempt. A nil unlockErr means the connection
// is known lock-free, so it goes back to the pool via release(). A non-nil
// unlockErr means the connection cannot be trusted to be lock-free: a
// healthy-looking connection returned to the pool while it still holds the
// workspace's session-scoped advisory lock would 409 every future
// consolidation run against that workspace until the pool's health-check /
// lifetime logic happened to recycle it away — so destroy() runs instead
// and the connection is torn down immediately.
//
// Expressed as a free function over two callables (not a *pgxpool.Conn
// method) so the branch selection is unit-testable without a live
// connection.
func disposeAdvisoryLockConn(unlockErr error, destroy func(), release func()) {
	if unlockErr != nil {
		destroy()
		return
	}
	release()
}
