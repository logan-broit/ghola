#!/usr/bin/env bash
# P4 activation-weight sweep on the repaired bed (v2 baselines as reference).
# w=0.45 already measured (v2-ch045). Bound: RerankWeight 0.5 => w < 0.5.
set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VENV="$(cd "${SCRIPT_DIR}/../.." && pwd)/seeding-eval/.venv"
DATA="${SCRIPT_DIR}"; LOG="${DATA}/weight-sweep.log"
WORKSPACE=$(grep -oP '^WORKSPACE="\K[^"]+' "${DATA}/phase0-driver.sh")
log(){ echo "[$(date '+%H:%M:%S')] $*" | tee -a "${LOG}"; }
for W in 0.25 0.30 0.35 0.40 0.49; do
  label="chw${W/0./}"
  log "--- w=${W} ---"
  "${VENV}/bin/seeding-eval-run" --cases "${DATA}/cases.jsonl" \
    --workspace "${WORKSPACE}" --user 00000000-0000-0000-0000-000000000001 \
    --out-dir "${DATA}/v2-${label}" --event-buckets "${DATA}/event_buckets.json" \
    --k 20 --primitives --settle channel --activation-weight "${W}" 2>&1 | tee -a "${LOG}"
done
log "--- sweep table ---"
"${VENV}/bin/python" - "${DATA}" <<'PY' 2>&1 | tee -a "${LOG}"
import json, sys
from pathlib import Path
data = Path(sys.argv[1])
bridge = set((data/"bridge-misses-v2.txt").read_text().split())
def row(d):
    h2=json.loads((data/d/"report.json").read_text())["h2"]
    hits=sum(1 for line in (data/d/"per-case-traces.jsonl").open()
             for r in [json.loads(line)]
             if r["variant"]=="none" and r["case_id"] in bridge and r["hit_p_at_5"])
    return h2["p_at_5_none"], h2["p_at_5_correct_era"], hits
print(f"{'w':<8} {'P@5none':<9} {'P@5corr':<9} bridge/{len(bridge)}")
for label,d in [("0(prim)","v2-prim"),("0.20","v2-ch020"),("0.25","v2-chw25"),
                ("0.30","v2-chw30"),("0.35","v2-chw35"),("0.40","v2-chw40"),
                ("0.45","v2-ch045"),("0.49","v2-chw49")]:
    try:
        p5n,p5c,b = row(d); print(f"{label:<8} {p5n:<9.3f} {p5c:<9.3f} {b}")
    except FileNotFoundError: print(f"{label:<8} MISSING")
PY
log "SWEEP DONE"
