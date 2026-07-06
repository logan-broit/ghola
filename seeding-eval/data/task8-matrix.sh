#!/usr/bin/env bash
# Task 8: P4 run matrix {expand, channel@0.2} after Phase 0 baselines.
# Waits for the p4-phase0 tmux session to finish, sanity-checks the baselines,
# runs both settle configs, and emits the comparison table.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKTREE_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
DATA="${SCRIPT_DIR}"
VENV="${WORKTREE_ROOT}/seeding-eval/.venv"
LOG="${DATA}/task8.log"

log() { echo "[$(date '+%Y-%m-%dT%H:%M:%S')] $*" | tee -a "${LOG}"; }

# Read the bed identity from the phase0 driver (single source of truth).
WORKSPACE=$(grep -oP '^WORKSPACE="\K[^"]+' "${DATA}/phase0-driver.sh")
USER_ID=$(grep -oP '^USER_ID="\K[^"]+' "${DATA}/phase0-driver.sh")

log "waiting for p4-phase0 to finish..."
while tmux has-session -t p4-phase0 2>/dev/null; do sleep 120; done

for f in "${DATA}/results-baseline-noprim/report.json" \
         "${DATA}/results-baseline-prim/report.json" \
         "${DATA}/bridge-misses.txt"; do
  [[ -f "$f" ]] || { log "FATAL: missing ${f} — phase0 incomplete"; exit 1; }
done
log "baselines present; workspace=${WORKSPACE}"

run_cfg() { # <label> <extra args...>
  local label=$1; shift
  log "--- run: ${label} ---"
  "${VENV}/bin/seeding-eval-run" \
    --cases "${DATA}/cases.jsonl" \
    --workspace "${WORKSPACE}" \
    --user "${USER_ID}" \
    --out-dir "${DATA}/results-p4-${label}" \
    --event-buckets "${DATA}/event_buckets.json" \
    --k 20 --primitives "$@" 2>&1 | tee -a "${LOG}"
}

run_cfg expand  --settle expand
run_cfg channel --settle channel --activation-weight 0.2

log "--- comparison ---"
"${VENV}/bin/python" - "${DATA}" <<'PY' 2>&1 | tee -a "${LOG}"
import json, sys
from pathlib import Path
data = Path(sys.argv[1])
bridge = set(x.strip() for x in (data/"bridge-misses.txt").read_text().splitlines() if x.strip())

def stats(d):
    rep = json.loads((data/d/"report.json").read_text())
    p5 = rep.get("h2_p_at_5") or rep.get("p_at_5") or rep.get("metrics", {}).get("p_at_5")
    # bridge breakdown from per-case traces
    hits = 0; total = 0
    tr = data/d/"per-case-traces.jsonl"
    if tr.exists():
        for line in tr.open():
            r = json.loads(line)
            cid = r.get("case_id") or r.get("id")
            if cid in bridge:
                total += 1
                if r.get("p_at_5") or r.get("hit_at_5"):
                    hits += 1
    return p5, hits, total

rows = []
for label, d in [("baseline-prim","results-baseline-prim"),
                 ("expand","results-p4-expand"),
                 ("channel@0.2","results-p4-channel")]:
    try:
        p5, bh, bt = stats(d)
        rows.append((label, p5, bh, bt))
    except Exception as e:
        rows.append((label, f"ERR {e}", "-", "-"))

print(f"{'config':<15} {'P@5':<20} bridge-hits/total")
for label, p5, bh, bt in rows:
    print(f"{label:<15} {str(p5):<20} {bh}/{bt}")
PY
log "TASK8 DONE — verdict vs success bar is a human step"
