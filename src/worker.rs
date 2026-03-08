// pg_recall::worker — Autonomous background worker for Hebbian processing
//
// Implements a pgrx background worker that:
// 1. Automatically drains the co_activation_queue via process_co_activation_batch
// 2. Adapts polling interval based on queue activity (Active/Idle/Dormant)
// 3. Runs periodic association decay, pruning, and dormant archival
// 4. Drains in-flight work on graceful shutdown (SIGTERM)
//
// Requires shared_preload_libraries = 'pg_recall' in postgresql.conf.
// The target database is configured via the pg_recall.database GUC.

use pgrx::bgworkers::{BackgroundWorker, SignalWakeFlags};
use pgrx::prelude::*;
use std::time::{Duration, Instant};

// ---------------------------------------------------------------------------
// Adaptive polling state machine
// ---------------------------------------------------------------------------

#[derive(Debug, Clone, Copy, PartialEq)]
pub enum WorkerState {
    Active,
    Idle,
    Dormant,
}

impl WorkerState {
    pub fn name(&self) -> &'static str {
        match self {
            WorkerState::Active => "active",
            WorkerState::Idle => "idle",
            WorkerState::Dormant => "dormant",
        }
    }

    pub fn poll_interval_ms(&self) -> u64 {
        match self {
            WorkerState::Active => 100,
            WorkerState::Idle => 1_000,
            WorkerState::Dormant => 5_000,
        }
    }
}

pub struct StateMachine {
    pub state: WorkerState,
    last_activity: Instant,
}

impl StateMachine {
    pub fn new() -> Self {
        StateMachine {
            state: WorkerState::Active,
            last_activity: Instant::now(),
        }
    }

    /// Transition based on how many rows were processed in this cycle.
    pub fn transition(&mut self, rows_processed: i64) {
        if rows_processed > 0 {
            self.state = WorkerState::Active;
            self.last_activity = Instant::now();
            return;
        }
        let idle_secs = self.last_activity.elapsed().as_secs();
        match self.state {
            WorkerState::Active if idle_secs >= 30 => {
                self.state = WorkerState::Idle;
            }
            WorkerState::Idle if idle_secs >= 300 => {
                self.state = WorkerState::Dormant;
            }
            _ => {}
        }
    }

    pub fn poll_interval(&self) -> Duration {
        Duration::from_millis(self.state.poll_interval_ms())
    }
}

// ---------------------------------------------------------------------------
// Operational stats accumulator
// ---------------------------------------------------------------------------

pub struct StatsAccumulator {
    pub batches_processed: i64,
    pub rows_processed: i64,
    pub pairs_updated: i64,
    pub last_decay_at: Option<Instant>,
    pub last_archival_at: Option<Instant>,
}

impl StatsAccumulator {
    pub fn new() -> Self {
        StatsAccumulator {
            batches_processed: 0,
            rows_processed: 0,
            pairs_updated: 0,
            last_decay_at: None,
            last_archival_at: None,
        }
    }

    pub fn record_batch(&mut self, rows: i64) {
        if rows > 0 {
            self.batches_processed += 1;
            self.rows_processed += rows;
        }
    }
}

// ---------------------------------------------------------------------------
// Periodic maintenance jobs
// ---------------------------------------------------------------------------

const DECAY_INTERVAL: Duration = Duration::from_secs(3600); // 1 hour
const ARCHIVAL_INTERVAL: Duration = Duration::from_secs(21600); // 6 hours
const DRAIN_TIMEOUT: Duration = Duration::from_secs(30);

/// Run association decay and pruning (called inside a transaction).
fn run_decay_pruning() {
    // Decay: reduce weight of stale associations by 0.1%
    Spi::run(
        "UPDATE pg_recall.associations \
         SET weight = weight * 0.999 \
         WHERE updated_at < now() - interval '1 day'",
    )
    .unwrap_or_else(|e| log!("pg_recall worker: decay failed: {e}"));

    // Prune: remove associations below threshold
    let pruned = Spi::get_one::<i64>(
        "WITH deleted AS ( \
             DELETE FROM pg_recall.associations \
             WHERE weight < 0.001 \
             RETURNING 1 \
         ) SELECT count(*) FROM deleted",
    )
    .unwrap_or(Some(0))
    .unwrap_or(0);

    if pruned > 0 {
        log!(
            "pg_recall worker: decay/prune cycle complete, pruned {pruned} associations"
        );
    }
}

/// Archive dormant memories (called inside a transaction).
fn run_archival() {
    let archived = Spi::get_one::<i64>(
        "WITH updated AS ( \
             UPDATE pg_recall.mnemes \
             SET state = 'dormant' \
             WHERE state = 'active' \
               AND last_access < now() - interval '90 days' \
               AND confidence < 0.3 \
             RETURNING 1 \
         ) SELECT count(*) FROM updated",
    )
    .unwrap_or(Some(0))
    .unwrap_or(0);

    if archived > 0 {
        log!(
            "pg_recall worker: archived {archived} dormant memories"
        );
    }
}

// ---------------------------------------------------------------------------
// Stats table updates
// ---------------------------------------------------------------------------

/// Write stats to the worker_stats singleton row (called inside a transaction).
/// All values are passed by value to satisfy UnwindSafe bounds.
fn write_stats_row(
    state: &str,
    _queue_depth_hint: i64,
    batches: i64,
    rows: i64,
    pairs: i64,
    poll_ms: i32,
    update_decay_ts: bool,
) {
    // Get current queue depth
    let queue_depth = Spi::get_one::<i64>(
        "SELECT count(*) FROM pg_recall.co_activation_queue",
    )
    .unwrap_or(Some(0))
    .unwrap_or(0);

    let last_decay_clause = if update_decay_ts {
        "last_decay_at = now(),"
    } else {
        ""
    };

    Spi::run(&format!(
        "UPDATE pg_recall.worker_stats SET \
             state = '{state}', \
             queue_depth = {queue_depth}, \
             batches_processed = {batches}, \
             rows_processed = {rows}, \
             pairs_updated = {pairs}, \
             last_batch_at = now(), \
             {last_decay_clause} \
             poll_interval_ms = {poll_ms}, \
             updated_at = now() \
         WHERE id = 1",
    ))
    .unwrap_or_else(|e| log!("pg_recall worker: failed to update stats: {e}"));
}

// ---------------------------------------------------------------------------
// Queue drain (for graceful shutdown)
// ---------------------------------------------------------------------------

/// Drain the queue across multiple transactions until empty or timeout.
/// Returns total rows drained.
fn drain_via_transactions() -> i64 {
    let deadline = Instant::now() + DRAIN_TIMEOUT;
    let mut total: i64 = 0;
    loop {
        if Instant::now() >= deadline {
            log!("pg_recall worker: drain timeout reached, exiting");
            break;
        }
        let processed = BackgroundWorker::transaction(|| {
            Spi::get_one::<i64>(
                "SELECT pg_recall.process_co_activation_batch(100)",
            )
            .unwrap_or(Some(0))
            .unwrap_or(0)
        });
        if processed == 0 {
            break;
        }
        total += processed;
    }
    total
}

// ---------------------------------------------------------------------------
// Background worker entry point
// ---------------------------------------------------------------------------

#[pg_guard]
pub extern "C-unwind" fn worker_main(_arg: pg_sys::Datum) {
    // Attach signal handlers for SIGHUP (config reload) and SIGTERM (shutdown)
    BackgroundWorker::attach_signal_handlers(SignalWakeFlags::SIGHUP | SignalWakeFlags::SIGTERM);

    // Read the target database from GUC, defaulting to "postgres"
    let db_name = BackgroundWorker::transaction(|| {
        Spi::get_one::<String>(
            "SELECT COALESCE(current_setting('pg_recall.database', true), 'postgres')",
        )
        .unwrap_or(Some("postgres".to_string()))
        .unwrap_or_else(|| "postgres".to_string())
    });

    // Connect to the target database
    BackgroundWorker::connect_worker_to_spi(Some(&db_name), None);

    log!(
        "pg_recall worker: started, connected to database '{db_name}'"
    );

    let mut sm = StateMachine::new();
    let mut stats = StatsAccumulator::new();

    // Mark worker as running
    BackgroundWorker::transaction(|| {
        Spi::run(
            "UPDATE pg_recall.worker_stats SET \
                 state = 'active', \
                 started_at = now(), \
                 updated_at = now() \
             WHERE id = 1",
        )
        .unwrap_or_else(|e| log!("pg_recall worker: failed to set initial state: {e}"));
    });

    loop {
        // Check for termination signal
        if BackgroundWorker::sigterm_received() {
            log!("pg_recall worker: SIGTERM received, draining queue");
            let drain_total = drain_via_transactions();
            stats.rows_processed += drain_total;
            // Final stats update
            let poll_ms = sm.state.poll_interval_ms() as i32;
            let batches = stats.batches_processed;
            let rows = stats.rows_processed;
            let pairs = stats.pairs_updated;
            BackgroundWorker::transaction(move || {
                write_stats_row("shutdown", 0, batches, rows, pairs, poll_ms, false);
            });
            log!(
                "pg_recall worker: shutdown complete, processed {} total rows",
                stats.rows_processed
            );
            break;
        }

        // Process one batch cycle within a transaction
        let processed = BackgroundWorker::transaction(|| {
            Spi::get_one::<i64>(
                "SELECT pg_recall.process_co_activation_batch(100)",
            )
            .unwrap_or(Some(0))
            .unwrap_or(0)
        });

        stats.record_batch(processed);
        sm.transition(processed);

        // Run periodic maintenance if due
        let should_decay = match stats.last_decay_at {
            None => true,
            Some(last) => last.elapsed() >= DECAY_INTERVAL,
        };
        if should_decay {
            BackgroundWorker::transaction(|| {
                run_decay_pruning();
            });
            stats.last_decay_at = Some(Instant::now());
        }

        let should_archive = match stats.last_archival_at {
            None => true,
            Some(last) => last.elapsed() >= ARCHIVAL_INTERVAL,
        };
        if should_archive {
            BackgroundWorker::transaction(|| {
                run_archival();
            });
            stats.last_archival_at = Some(Instant::now());
        }

        // Update stats row
        let state_name = sm.state.name().to_string();
        let poll_ms = sm.state.poll_interval_ms() as i32;
        let batches = stats.batches_processed;
        let rows = stats.rows_processed;
        let pairs = stats.pairs_updated;
        let has_decay = stats.last_decay_at.is_some();
        BackgroundWorker::transaction(move || {
            write_stats_row(&state_name, 0, batches, rows, pairs, poll_ms, has_decay);
        });

        // Wait for the current poll interval (or a signal)
        BackgroundWorker::wait_latch(Some(sm.poll_interval()));
    }
}

// ---------------------------------------------------------------------------
// Unit tests (pure Rust, no Postgres)
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_state_machine_starts_active() {
        let sm = StateMachine::new();
        assert_eq!(sm.state, WorkerState::Active);
    }

    #[test]
    fn test_active_stays_active_with_work() {
        let mut sm = StateMachine::new();
        sm.transition(5);
        assert_eq!(sm.state, WorkerState::Active);
    }

    #[test]
    fn test_active_to_idle_after_30s() {
        let mut sm = StateMachine::new();
        // Simulate 31 seconds of no activity
        sm.last_activity = Instant::now() - Duration::from_secs(31);
        sm.transition(0);
        assert_eq!(sm.state, WorkerState::Idle);
    }

    #[test]
    fn test_active_stays_active_before_30s() {
        let mut sm = StateMachine::new();
        sm.last_activity = Instant::now() - Duration::from_secs(10);
        sm.transition(0);
        assert_eq!(sm.state, WorkerState::Active);
    }

    #[test]
    fn test_idle_to_dormant_after_5min() {
        let mut sm = StateMachine::new();
        sm.state = WorkerState::Idle;
        sm.last_activity = Instant::now() - Duration::from_secs(301);
        sm.transition(0);
        assert_eq!(sm.state, WorkerState::Dormant);
    }

    #[test]
    fn test_idle_stays_idle_before_5min() {
        let mut sm = StateMachine::new();
        sm.state = WorkerState::Idle;
        sm.last_activity = Instant::now() - Duration::from_secs(60);
        sm.transition(0);
        assert_eq!(sm.state, WorkerState::Idle);
    }

    #[test]
    fn test_dormant_to_active_with_work() {
        let mut sm = StateMachine::new();
        sm.state = WorkerState::Dormant;
        sm.last_activity = Instant::now() - Duration::from_secs(600);
        sm.transition(1);
        assert_eq!(sm.state, WorkerState::Active);
    }

    #[test]
    fn test_dormant_stays_dormant_without_work() {
        let mut sm = StateMachine::new();
        sm.state = WorkerState::Dormant;
        sm.last_activity = Instant::now() - Duration::from_secs(600);
        sm.transition(0);
        assert_eq!(sm.state, WorkerState::Dormant);
    }

    #[test]
    fn test_poll_intervals() {
        assert_eq!(WorkerState::Active.poll_interval_ms(), 100);
        assert_eq!(WorkerState::Idle.poll_interval_ms(), 1_000);
        assert_eq!(WorkerState::Dormant.poll_interval_ms(), 5_000);
    }

    #[test]
    fn test_state_names() {
        assert_eq!(WorkerState::Active.name(), "active");
        assert_eq!(WorkerState::Idle.name(), "idle");
        assert_eq!(WorkerState::Dormant.name(), "dormant");
    }

    #[test]
    fn test_stats_accumulator_record_batch() {
        let mut stats = StatsAccumulator::new();
        assert_eq!(stats.batches_processed, 0);
        assert_eq!(stats.rows_processed, 0);

        stats.record_batch(5);
        assert_eq!(stats.batches_processed, 1);
        assert_eq!(stats.rows_processed, 5);

        stats.record_batch(10);
        assert_eq!(stats.batches_processed, 2);
        assert_eq!(stats.rows_processed, 15);
    }

    #[test]
    fn test_stats_accumulator_ignores_zero() {
        let mut stats = StatsAccumulator::new();
        stats.record_batch(0);
        assert_eq!(stats.batches_processed, 0);
        assert_eq!(stats.rows_processed, 0);
    }
}
