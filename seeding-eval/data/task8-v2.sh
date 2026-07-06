#!/usr/bin/env bash
# Task 8 v2: full P4 matrix on the REPAIRED bed (25,784 edges; the v1 run at
# 8,842 edges is invalidated by the workspace-PK capture bug, see
# P42-FORENSICS.md). Five configs; bridge set re-derived with the corrected
# criterion (none-variant miss under BOTH baselines).
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VENV="$(cd "${SCRIPT_DIR}/../.." && pwd)/seeding-eval/.venv"
DATA="${SCRIPT_DIR}"
LOG="${DATA}/task8-v2.log"
WORKSPACE=$(grep -oP '^WORKSPACE="\K[^"]+' "${DATA}/phase0-driver.sh")
USER_ID="00000000-0000-0000-0000-000000000001"
log(){ echo "[$(date '+%H:%M:%S')] $*" | tee -a "${LOG}"; }

run(){ local label=$1; shift
  log "--- ${label} ---"
  "${VENV}/bin/seeding-eval-run" --cases "${DATA}/cases.jsonl" \
    --workspace "${WORKSPACE}" --user "${USER_ID}" \
    --out-dir "${DATA}/v2-${label}" --event-buckets "${DATA}/event_buckets.json" \
    --k 20 "$@" 2>&1 | tee -a "${LOG}"
}

run noprim
run prim    --primitives
run expand  --primitives --settle expand
run ch020   --primitives --settle channel --activation-weight 0.2
run ch045   --primitives --settle channel --activation-weight 0.45

log "--- comparison ---"
"${VENV}/bin/python" - "${DATA}" <<'PY' 2>&1 | tee -a "${LOG}"
import json, sys
from pathlib import Path
data = Path(sys.argv[1])
def none_hits(d):
    out={}
    for line in (data/d/"per-case-traces.jsonl").open():
        r=json.loads(line)
        if r["variant"]=="none": out[r["case_id"]]=bool(r["hit_p_at_5"])
    return out
bn=none_hits("v2-noprim"); bp=none_hits("v2-prim")
bridge=sorted(c for c in bn if not bn[c] and not bp.get(c,False))
(data/"bridge-misses-v2.txt").write_text("\n".join(bridge)+"\n")
print(f"bridge set (v2 bed): {len(bridge)} of {len(bn)}")
print(f"{'config':<10} {'P@5none':<9} {'P@5corr':<9} bridge/{len(bridge)}")
for label in ["noprim","prim","expand","ch020","ch045"]:
    h2=json.loads((data/f"v2-{label}"/"report.json").read_text())["h2"]
    nh=none_hits(f"v2-{label}")
    bh=sum(1 for c in bridge if nh.get(c,False))
    print(f"{label:<10} {h2['p_at_5_none']:<9.3f} {h2['p_at_5_correct_era']:<9.3f} {bh}")
PY
log "TASK8-V2 DONE"
