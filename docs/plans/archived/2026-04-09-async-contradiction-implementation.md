# Async Contradiction Worker Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Move contradiction detection from a synchronous INSERT trigger to an asynchronous background worker with its own queue and adaptive polling (5s/30s/300s).

**Architecture:** New `contradiction_queue` table, lightweight enqueue trigger, second pgrx background worker reusing the existing StateMachine/StatsAccumulator pattern with different cadence constants.

**Tech Stack:** Rust, pgrx 0.17.0, PostgreSQL 18, cargo-pgrx

---

### Task 1: Add contradiction_queue table and stats table to schema

**Files:**
- Modify: `src/schema.rs`

**Step 1: Add the contradiction_queue and contradiction_worker_stats tables**

In `src/schema.rs`, add to the `CREATE TABLE` SQL string (after the `co_activation_queue` table definition):

```sql
CREATE TABLE IF NOT EXISTS contradiction_queue (
    id           bigserial PRIMARY KEY,
    workspace_id uuid NOT NULL,
    mneme_id     uuid NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS contradiction_worker_stats (
    id                serial PRIMARY KEY,
    state             text NOT NULL DEFAULT 'stopped',
    queue_depth       bigint NOT NULL DEFAULT 0,
    scans_completed   bigint NOT NULL DEFAULT 0,
    candidates_found  bigint NOT NULL DEFAULT 0,
    last_scan_at      timestamptz,
    poll_interval_ms  integer NOT NULL DEFAULT 5000,
    started_at        timestamptz,
    updated_at        timestamptz DEFAULT now()
);

INSERT INTO contradiction_worker_stats (id) VALUES (1) ON CONFLICT DO NOTHING;
```

**Step 2: Modify the contradiction_check_trigger to enqueue instead of scan**

Replace the existing trigger function body. Find the `contradiction_check_trigger` definition in `src/schema.rs` (or `src/contradiction.rs` if the DDL is there) and change it from:

```sql
CREATE OR REPLACE FUNCTION contradiction_check_trigger()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    PERFORM @extschema@.flag_contradictions(NEW.id, 0.85);
    RETURN NEW;
END;
$$;
```

To:

```sql
CREATE OR REPLACE FUNCTION contradiction_check_trigger()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO @extschema@.contradiction_queue (workspace_id, mneme_id)
    VALUES (NEW.workspace_id, NEW.id);
    RETURN NEW;
END;
$$;
```

**Step 3: Run tests to verify existing tests still compile**

Run: `cd ~/pg_ghola && cargo pgrx test pg18 2>&1 | tail -20`

Some integration tests that expected synchronous contradiction flagging will now fail -- that's expected and will be fixed in Task 5.

**Step 4: Commit**

```bash
git add src/schema.rs
git commit -m "feat: add contradiction_queue table and async enqueue trigger"
```

---

### Task 2: Create contradiction_worker module

**Files:**
- Create: `src/contradiction_worker.rs`
- Modify: `src/lib.rs` (add module declaration)

**Step 1: Create src/contradiction_worker.rs**

```rust
// pg_ghola::contradiction_worker — Async contradiction detection worker
//
// Drains the contradiction_queue table and runs HNSW similarity scans
// to flag potential contradictions. Follows the same pattern as the
// Hebbian worker but with slower cadence (5s/30s/300s) since
// contradiction detection is not time-sensitive.
//
// Neuroscience basis:
// - Detection (enqueue): <1s (hippocampal mismatch signal)
// - Scanning (this worker): seconds (hippocampal comparison)
// - Resolution: hours-days (mPFC schema integration, sleep consolidation)

use pgrx::bgworkers::{BackgroundWorker, SignalWakeFlags};
use pgrx::prelude::*;
use std::time::{Duration, Instant};

use crate::PG_GHOLA_DATABASE;

// ---------------------------------------------------------------------------
// Contradiction worker state machine (slower cadence than Hebbian)
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Copy, PartialEq)]
pub enum ContradictionWorkerState {
    Active,   // 5s poll
    Idle,     // 30s poll
    Dormant,  // 300s poll
}

impl ContradictionWorkerState {
    pub fn name(&self) -> &'static str {
        match self {
            Self::Active => "active",
            Self::Idle => "idle",
            Self::Dormant => "dormant",
        }
    }

    pub fn poll_interval_ms(&self) -> u64 {
        match self {
            Self::Active => 5_000,
            Self::Idle => 30_000,
            Self::Dormant => 300_000,
        }
    }
}

pub struct ContradictionStateMachine {
    pub state: ContradictionWorkerState,
    last_activity: Instant,
}

impl ContradictionStateMachine {
    pub fn new() -> Self {
        Self {
            state: ContradictionWorkerState::Active,
            last_activity: Instant::now(),
        }
    }

    pub fn transition(&mut self, items_processed: i64) {
        if items_processed > 0 {
            self.state = ContradictionWorkerState::Active;
            self.last_activity = Instant::now();
            return;
        }
        let idle_secs = self.last_activity.elapsed().as_secs();
        match self.state {
            ContradictionWorkerState::Active if idle_secs >= 30 => {
                self.state = ContradictionWorkerState::Idle;
            }
            ContradictionWorkerState::Idle if idle_secs >= 300 => {
                self.state = ContradictionWorkerState::Dormant;
            }
            _ => {}
        }
    }

    pub fn poll_interval(&self) -> Duration {
        Duration::from_millis(self.state.poll_interval_ms())
    }
}

// ---------------------------------------------------------------------------
// Queue processing
// ---------------------------------------------------------------------------

/// Process one item from the contradiction queue.
/// Returns 1 if an item was processed, 0 if queue was empty.
fn process_one_contradiction() -> i64 {
    // Dequeue one item
    let row = Spi::get_two::<i64, pgrx::Uuid>(
        "DELETE FROM ghola.contradiction_queue \
         WHERE id = (SELECT id FROM ghola.contradiction_queue ORDER BY id LIMIT 1) \
         RETURNING id, mneme_id",
    );

    match row {
        Ok((Some(_queue_id), Some(mneme_id))) => {
            // Run the existing contradiction detection logic
            let candidates = Spi::get_one::<i64>(&format!(
                "SELECT ghola.flag_contradictions('{mneme_id}'::uuid, 0.85)"
            ))
            .unwrap_or(Some(0))
            .unwrap_or(0);

            if candidates > 0 {
                log!(
                    "pg_ghola contradiction worker: flagged {candidates} candidates for mneme {mneme_id}"
                );
            }
            1
        }
        _ => 0,
    }
}

// ---------------------------------------------------------------------------
// Stats updates
// ---------------------------------------------------------------------------

fn write_contradiction_stats(
    state: &str,
    scans: i64,
    candidates: i64,
    poll_ms: i32,
) {
    let queue_depth = Spi::get_one::<i64>(
        "SELECT count(*) FROM ghola.contradiction_queue",
    )
    .unwrap_or(Some(0))
    .unwrap_or(0);

    Spi::run(&format!(
        "UPDATE ghola.contradiction_worker_stats SET \
             state = '{state}', \
             queue_depth = {queue_depth}, \
             scans_completed = {scans}, \
             candidates_found = {candidates}, \
             last_scan_at = now(), \
             poll_interval_ms = {poll_ms}, \
             updated_at = now() \
         WHERE id = 1",
    ))
    .unwrap_or_else(|e| log!("pg_ghola contradiction worker: stats update failed: {e}"));
}

// ---------------------------------------------------------------------------
// Background worker entry point
// ---------------------------------------------------------------------------

#[pg_guard]
#[no_mangle]
pub extern "C-unwind" fn contradiction_worker_main(_arg: pg_sys::Datum) {
    BackgroundWorker::attach_signal_handlers(SignalWakeFlags::SIGHUP | SignalWakeFlags::SIGTERM);

    let db_name = PG_GHOLA_DATABASE
        .get()
        .and_then(|cs| cs.to_str().ok().map(|s| s.to_string()))
        .unwrap_or_else(|| "memories".to_string());

    BackgroundWorker::connect_worker_to_spi(Some(&db_name), None);

    log!(
        "pg_ghola contradiction worker: started, connected to database '{db_name}'"
    );

    let mut sm = ContradictionStateMachine::new();
    let mut total_scans: i64 = 0;
    let mut total_candidates: i64 = 0;

    // Mark worker as running
    BackgroundWorker::transaction(|| {
        Spi::run(
            "UPDATE ghola.contradiction_worker_stats SET \
                 state = 'active', \
                 started_at = now(), \
                 updated_at = now() \
             WHERE id = 1",
        )
        .unwrap_or_else(|e| log!("pg_ghola contradiction worker: init failed: {e}"));
    });

    loop {
        if BackgroundWorker::sigterm_received() {
            log!("pg_ghola contradiction worker: SIGTERM received, shutting down");
            let state_name = sm.state.name().to_string();
            let poll_ms = sm.state.poll_interval_ms() as i32;
            let scans = total_scans;
            let candidates = total_candidates;
            BackgroundWorker::transaction(move || {
                write_contradiction_stats(&state_name, scans, candidates, poll_ms);
            });
            log!(
                "pg_ghola contradiction worker: shutdown complete, {} scans, {} candidates",
                total_scans, total_candidates
            );
            break;
        }

        // Process one queue item
        let processed = BackgroundWorker::transaction(|| {
            process_one_contradiction()
        });

        if processed > 0 {
            total_scans += 1;
        }
        sm.transition(processed);

        // Update stats periodically (every scan or every poll when idle)
        let state_name = sm.state.name().to_string();
        let poll_ms = sm.state.poll_interval_ms() as i32;
        let scans = total_scans;
        let candidates = total_candidates;
        BackgroundWorker::transaction(move || {
            write_contradiction_stats(&state_name, scans, candidates, poll_ms);
        });

        BackgroundWorker::wait_latch(Some(sm.poll_interval()));
    }
}

// ---------------------------------------------------------------------------
// Unit tests
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_contradiction_state_machine_starts_active() {
        let sm = ContradictionStateMachine::new();
        assert_eq!(sm.state, ContradictionWorkerState::Active);
    }

    #[test]
    fn test_contradiction_active_to_idle() {
        let mut sm = ContradictionStateMachine::new();
        sm.last_activity = Instant::now() - Duration::from_secs(31);
        sm.transition(0);
        assert_eq!(sm.state, ContradictionWorkerState::Idle);
    }

    #[test]
    fn test_contradiction_idle_to_dormant() {
        let mut sm = ContradictionStateMachine::new();
        sm.state = ContradictionWorkerState::Idle;
        sm.last_activity = Instant::now() - Duration::from_secs(301);
        sm.transition(0);
        assert_eq!(sm.state, ContradictionWorkerState::Dormant);
    }

    #[test]
    fn test_contradiction_dormant_to_active() {
        let mut sm = ContradictionStateMachine::new();
        sm.state = ContradictionWorkerState::Dormant;
        sm.last_activity = Instant::now() - Duration::from_secs(600);
        sm.transition(1);
        assert_eq!(sm.state, ContradictionWorkerState::Active);
    }

    #[test]
    fn test_contradiction_poll_intervals() {
        assert_eq!(ContradictionWorkerState::Active.poll_interval_ms(), 5_000);
        assert_eq!(ContradictionWorkerState::Idle.poll_interval_ms(), 30_000);
        assert_eq!(ContradictionWorkerState::Dormant.poll_interval_ms(), 300_000);
    }
}
```

**Step 2: Add module to lib.rs**

In `src/lib.rs`, add after `pub mod contradiction;`:

```rust
pub mod contradiction_worker;
```

**Step 3: Run unit tests**

Run: `cd ~/pg_ghola && cargo test 2>&1 | tail -10`
Expected: All unit tests pass (the new state machine tests + existing ones)

**Step 4: Commit**

```bash
git add src/contradiction_worker.rs src/lib.rs
git commit -m "feat: add contradiction_worker module with async queue processing"
```

---

### Task 3: Register second background worker in _PG_init

**Files:**
- Modify: `src/lib.rs:47-68`

**Step 1: Add second BackgroundWorkerBuilder**

After the existing Hebbian worker registration (line 67), add:

```rust
    BackgroundWorkerBuilder::new("pg_ghola Contradiction Worker")
        .set_function("contradiction_worker_main")
        .set_library("pg_ghola")
        .set_argument(0i32.into_datum())
        .enable_spi_access()
        .set_start_time(BgWorkerStartTime::RecoveryFinished)
        .set_restart_time(Some(Duration::from_secs(10)))
        .load();
```

**Step 2: Run full pgrx test suite**

Run: `cd ~/pg_ghola && cargo pgrx test pg18 2>&1 | tail -30`

This will start a test Postgres instance with both workers. Check for startup errors.

**Step 3: Commit**

```bash
git add src/lib.rs
git commit -m "feat: register contradiction worker as second background worker"
```

---

### Task 4: Fix the DELETE-RETURNING dequeue pattern

**Files:**
- Modify: `src/contradiction_worker.rs`

The `process_one_contradiction` function uses `Spi::get_two` with a `DELETE ... RETURNING` which may not work cleanly with pgrx SPI. Test and fix if needed.

**Step 1: Test the dequeue pattern in pgrx integration test**

Add to `src/integration_tests.rs`:

```rust
#[pg_test]
fn test_contradiction_queue_enqueue_and_dequeue() {
    // Create schema
    Spi::run("SELECT ghola.recall_result_stub()").expect("schema init");

    let ws = "30000000-0000-0000-0000-000000000001";

    // Insert a test mneme (fires the trigger which enqueues)
    let mneme_id = insert_mneme(ws, "test_contra", "test contradiction queue", 0.10);

    // Verify queue has an entry
    let queue_count = Spi::get_one::<i64>(
        "SELECT count(*) FROM ghola.contradiction_queue"
    ).expect("query").expect("null");

    assert!(queue_count >= 1, "expected at least 1 queue entry, got {queue_count}");

    // Dequeue one item
    let dequeued = Spi::get_one::<i64>(&format!(
        "WITH d AS ( \
             DELETE FROM ghola.contradiction_queue \
             WHERE id = (SELECT id FROM ghola.contradiction_queue ORDER BY id LIMIT 1) \
             RETURNING mneme_id \
         ) SELECT count(*) FROM d"
    )).expect("query").expect("null");

    assert_eq!(dequeued, 1, "expected to dequeue 1 item");
}
```

**Step 2: Run the integration test**

Run: `cd ~/pg_ghola && cargo pgrx test pg18 test_contradiction_queue_enqueue_and_dequeue 2>&1 | tail -20`

**Step 3: Adjust dequeue SQL in contradiction_worker.rs if needed**

If `Spi::get_two` doesn't work with DELETE RETURNING, use the CTE pattern:

```rust
fn process_one_contradiction() -> i64 {
    let result = Spi::get_one::<pgrx::Uuid>(
        "WITH d AS ( \
             DELETE FROM ghola.contradiction_queue \
             WHERE id = (SELECT id FROM ghola.contradiction_queue ORDER BY id LIMIT 1) \
             RETURNING mneme_id \
         ) SELECT mneme_id FROM d"
    );

    match result {
        Ok(Some(mneme_id)) => {
            let candidates = Spi::get_one::<i64>(&format!(
                "SELECT ghola.flag_contradictions('{mneme_id}'::uuid, 0.85)"
            ))
            .unwrap_or(Some(0))
            .unwrap_or(0);

            if candidates > 0 {
                log!(
                    "pg_ghola contradiction worker: flagged {candidates} candidates for mneme {mneme_id}"
                );
            }
            1
        }
        _ => 0,
    }
}
```

**Step 4: Commit**

```bash
git add src/contradiction_worker.rs src/integration_tests.rs
git commit -m "feat: test and fix contradiction queue dequeue pattern"
```

---

### Task 5: Update integration tests for async behavior

**Files:**
- Modify: `src/integration_tests.rs`

**Step 1: Find and update tests that expect synchronous contradiction detection**

The existing tests may call INSERT and immediately check `contradiction_candidates`. With async detection, the INSERT only enqueues -- candidates appear after the worker processes the queue. Update tests to either:

a) Call `process_one_contradiction()` manually after INSERT (simulating the worker), or
b) Call `flag_contradictions()` directly (testing the detection logic independent of the trigger)

**Step 2: Run full test suite**

Run: `cd ~/pg_ghola && cargo pgrx test pg18 2>&1 | tail -30`
Expected: All tests pass

**Step 3: Commit**

```bash
git add src/integration_tests.rs
git commit -m "test: update integration tests for async contradiction detection"
```

---

### Task 6: Build, deploy, and verify

**Step 1: Build the Docker image**

```bash
cd ~/pg_ghola
docker build --no-cache -f Dockerfile.cnpg -t cnpg-pg18-ghola:18.1-ghola-0.0.3 .
```

**Step 2: Transfer and import**

```bash
docker save cnpg-pg18-ghola:18.1-ghola-0.0.3 | ssh nuc "cat > /tmp/cnpg-ghola-0.0.3.tar"
# On NUC:
ssh nuc "sudo k3s ctr images import /tmp/cnpg-ghola-0.0.3.tar && rm /tmp/cnpg-ghola-0.0.3.tar"
```

**Step 3: Update ArgoCD manifest**

Update `~/ai/homelab-k3s/chapterhouse/postgres-cluster.yaml`:
```yaml
imageName: docker.io/library/cnpg-pg18-ghola:18.1-ghola-0.0.3
```

Commit and push for ArgoCD sync.

**Step 4: Verify both workers start**

```bash
kubectl logs -n ch-system memory-db-1 --tail=20 | grep "pg_ghola"
# Should see:
# pg_ghola worker: started, connected to database 'memories'
# pg_ghola contradiction worker: started, connected to database 'memories'
```

**Step 5: Test INSERT enqueues to contradiction_queue**

```bash
kubectl exec -n ch-system memory-db-1 -- psql -U postgres -d memories -c "
  SELECT count(*) FROM ghola.contradiction_queue;
  -- Should be 0 (worker drains it)

  SELECT * FROM ghola.contradiction_worker_stats;
  -- Should show state='active' or 'idle', scans_completed >= 0
"
```

**Step 6: Commit results**

```bash
git add -A
git commit -m "deploy: pg_ghola 0.0.3 with async contradiction worker"
```

---

## Task Dependency Summary

```
Task 1 (schema: queue table + trigger change)
  └→ Task 2 (contradiction_worker module)
       └→ Task 3 (register in _PG_init)
            └→ Task 4 (fix dequeue pattern)
                 └→ Task 5 (update integration tests)
                      └→ Task 6 (build + deploy)
```
