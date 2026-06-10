// ghola::consolidation_worker — v2 background consolidation loop
//
// Responsibilities:
// 1. Drain semantic.co_activation_queue via process_co_activation_batch.
// 2. Hourly decay of semantic.associations (weight *= 0.999 on stale
//    rows; prune weight < 0.001).
// 3. Six-hourly archival of semantic.mnemes (active + low-confidence +
//    no recent access -> archived).
// 4. Graceful drain on SIGTERM.
//
// Neuroscience analog: sleep consolidation — offline reorganization of
// memory traces.
//
// v2 drops everything related to clusters, working-memory expiration,
// state-tier cleanup, config table reads, and the worker_stats table.
// Those concerns either moved to Go-side Pipelines A/B or were
// retired in the greenfield rewrite.

use pgrx::bgworkers::{BackgroundWorker, SignalWakeFlags};
use pgrx::prelude::*;
use std::time::{Duration, Instant};

use crate::GHOLA_DATABASE;

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
// Periodic maintenance cadence
// ---------------------------------------------------------------------------

const DECAY_INTERVAL: Duration = Duration::from_secs(3600); // 1h
const ARCHIVAL_INTERVAL: Duration = Duration::from_secs(21_600); // 6h
const DRAIN_TIMEOUT: Duration = Duration::from_secs(30);

// ---------------------------------------------------------------------------
// Association decay / pruning
// ---------------------------------------------------------------------------

fn run_decay_pruning() {
    // Decay: reduce weight of stale associations by 0.1%.
    Spi::run(
        "UPDATE semantic.associations \
         SET weight = weight * 0.999, updated_at = now() \
         WHERE updated_at < now() - interval '1 day'",
    )
    .unwrap_or_else(|e| log!("ghola consolidation worker: decay failed: {e}"));

    // Prune: drop associations below the floor.
    let pruned = Spi::get_one::<i64>(
        "WITH deleted AS ( \
             DELETE FROM semantic.associations \
             WHERE weight < 0.001 \
             RETURNING 1 \
         ) SELECT count(*) FROM deleted",
    )
    .unwrap_or(Some(0))
    .unwrap_or(0);

    if pruned > 0 {
        log!(
            "ghola consolidation worker: decay/prune cycle pruned {pruned} associations"
        );
    }
}

// ---------------------------------------------------------------------------
// Archival
// ---------------------------------------------------------------------------

fn run_archival() {
    // v2 state values are just 'active' / 'archived' (no 'dormant').
    let archived = Spi::get_one::<i64>(
        "WITH updated AS ( \
             UPDATE semantic.mnemes \
             SET state = 'archived' \
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
            "ghola consolidation worker: archived {archived} stale low-confidence mnemes"
        );
    }
}

// ---------------------------------------------------------------------------
// Co-activation drain
// ---------------------------------------------------------------------------

fn drain_via_transactions() -> i64 {
    let mut total: i64 = 0;
    let start = Instant::now();

    loop {
        if start.elapsed() >= DRAIN_TIMEOUT {
            log!(
                "ghola consolidation worker: drain timeout after {}s, {total} rows processed",
                DRAIN_TIMEOUT.as_secs()
            );
            break;
        }

        let processed = BackgroundWorker::transaction(|| {
            Spi::get_one::<i64>("SELECT semantic.process_co_activation_batch(100)")
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
#[no_mangle]
pub extern "C-unwind" fn consolidation_worker_main(_arg: pg_sys::Datum) {
    BackgroundWorker::attach_signal_handlers(SignalWakeFlags::SIGHUP | SignalWakeFlags::SIGTERM);

    let db_name = GHOLA_DATABASE
        .get()
        .and_then(|cs| cs.to_str().ok().map(|s| s.to_string()))
        .unwrap_or_else(|| "memories".to_string());

    BackgroundWorker::connect_worker_to_spi(Some(&db_name), None);

    log!(
        "ghola consolidation worker: started, connected to database '{db_name}'"
    );

    let mut sm = StateMachine::new();
    let mut last_decay = Instant::now();
    let mut last_archival = Instant::now();

    loop {
        if BackgroundWorker::sigterm_received() {
            log!("ghola consolidation worker: SIGTERM received, draining");
            let drained = drain_via_transactions();
            log!(
                "ghola consolidation worker: shutdown complete, drained {drained} rows"
            );
            break;
        }

        // Per-cycle: drain one batch.
        let processed = BackgroundWorker::transaction(|| {
            Spi::get_one::<i64>("SELECT semantic.process_co_activation_batch(100)")
                .unwrap_or(Some(0))
                .unwrap_or(0)
        });

        sm.transition(processed);

        // Periodic maintenance.
        if last_decay.elapsed() >= DECAY_INTERVAL {
            BackgroundWorker::transaction(|| run_decay_pruning());
            last_decay = Instant::now();
        }

        if last_archival.elapsed() >= ARCHIVAL_INTERVAL {
            BackgroundWorker::transaction(|| run_archival());
            last_archival = Instant::now();
        }

        BackgroundWorker::wait_latch(Some(sm.poll_interval()));
    }
}

// ---------------------------------------------------------------------------
// Unit tests (state machine pure logic)
// ---------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn state_machine_starts_active() {
        let sm = StateMachine::new();
        assert_eq!(sm.state, WorkerState::Active);
    }

    #[test]
    fn active_to_idle_after_30s_of_no_work() {
        let mut sm = StateMachine::new();
        sm.last_activity = Instant::now() - Duration::from_secs(31);
        sm.transition(0);
        assert_eq!(sm.state, WorkerState::Idle);
    }

    #[test]
    fn idle_to_dormant_after_300s_of_no_work() {
        let mut sm = StateMachine::new();
        sm.state = WorkerState::Idle;
        sm.last_activity = Instant::now() - Duration::from_secs(301);
        sm.transition(0);
        assert_eq!(sm.state, WorkerState::Dormant);
    }

    #[test]
    fn work_resets_to_active() {
        let mut sm = StateMachine::new();
        sm.state = WorkerState::Dormant;
        sm.last_activity = Instant::now() - Duration::from_secs(600);
        sm.transition(5);
        assert_eq!(sm.state, WorkerState::Active);
    }

    #[test]
    fn poll_intervals() {
        assert_eq!(WorkerState::Active.poll_interval_ms(), 100);
        assert_eq!(WorkerState::Idle.poll_interval_ms(), 1_000);
        assert_eq!(WorkerState::Dormant.poll_interval_ms(), 5_000);
    }
}
