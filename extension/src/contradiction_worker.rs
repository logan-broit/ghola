// pg_ghola::contradiction_worker -- Async contradiction detection worker
//
// Drains the contradiction_queue table and runs HNSW similarity scans
// to flag potential contradictions. Follows the same pattern as the
// Hebbian worker but with slower cadence (5s/30s/300s) since
// contradiction detection is not time-sensitive.

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
/// Returns (items_processed, candidates_found).
fn process_one_contradiction() -> (i64, i64) {
    let result = Spi::get_one::<pgrx::Uuid>(
        "WITH d AS ( \
             DELETE FROM semantic.contradiction_queue \
             WHERE id = (SELECT id FROM semantic.contradiction_queue ORDER BY id LIMIT 1) \
             RETURNING mneme_id \
         ) SELECT mneme_id FROM d"
    );

    match result {
        Ok(Some(mneme_id)) => {
            let candidates = Spi::get_one::<i64>(&format!(
                "SELECT semantic.flag_contradictions('{mneme_id}'::uuid, 0.85)"
            ))
            .unwrap_or(Some(0))
            .unwrap_or(0);

            if candidates > 0 {
                log!(
                    "pg_ghola contradiction worker: flagged {candidates} candidates for mneme {mneme_id}"
                );
            }
            (1, candidates)
        }
        _ => (0, 0),
    }
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

    loop {
        if BackgroundWorker::sigterm_received() {
            log!(
                "pg_ghola contradiction worker: SIGTERM received, \
                 shutdown complete, {} scans, {} candidates",
                total_scans, total_candidates
            );
            break;
        }

        let (processed, candidates) = BackgroundWorker::transaction(|| {
            process_one_contradiction()
        });

        if processed > 0 {
            total_scans += 1;
            total_candidates += candidates;
        }
        sm.transition(processed);

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
