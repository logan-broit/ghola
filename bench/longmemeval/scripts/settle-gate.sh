#!/usr/bin/env bash
# LME R@k settle gate: baseline (settle off) vs channel@0.40 on the same
# stack, same day. No-regression bar for the settle default-on decision:
# R@5 >= 99.4% (docs/benchmarks.md, reranker-on deterministic baseline).
# Retrieval-only — local compute (guild + truthsayer GPU), no reader quota.
#
# Run from anywhere; HARNESS overrides the deployed-harness location.
# Reuses the already-indexed LME corpus (workspace uuid5 derivation is
# deterministic); does NOT re-index.
set -euo pipefail
HARNESS="${HARNESS:-$HOME/longmemeval-ghola}"
cd "$HARNESS"
STAMP=$(date -u +%Y%m%dT%H%M%SZ)
LOG="results/settle-gate-${STAMP}.log"
log(){ echo "[$(date '+%H:%M:%S')] $*" | tee -a "$LOG"; }

run_one(){ # <label> [KEY=VAL ...]
  local label=$1; shift
  local out="results/settle-gate-${STAMP}-${label}.jsonl"
  log "--- retrieve: ${label} ($*) ---"
  env GHOLA_V2_DELEGATE=1 "$@" .venv/bin/python run.py retrieve \
    --backend ghola_v2 --dataset s 2>&1 | tee -a "$LOG"
  # retrieve names its own output; claim the newest result file so the
  # two configs can't be confused afterwards.
  mv "$(ls -t results/ghola_v2_s_*.jsonl | head -1)" "$out"
  log "--- evaluate: ${label} ---"
  .venv/bin/python run.py evaluate --run "$out" 2>&1 | tee -a "$LOG"
}

run_one baseline
run_one channel BENCH_SETTLE=channel BENCH_ACTIVATION_WEIGHT=0.40

log "GATE DONE — verdict vs the 99.4% R@5 bar is a human step"
