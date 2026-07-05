#!/usr/bin/env bash
# Phase 0 driver: regenerate the seeding-eval bed for P4 recurrent-settle.
#
# Usage: bash seeding-eval/data/phase0-driver.sh
# Run from the worktree root: /home/loganb/ghola/.claude/worktrees/p4
#
# All artifacts land under seeding-eval/data/. Large files (.jsonl,
# results dirs) are gitignored; README, this script, and bridge-misses.txt
# (written once eval completes) stay committed.
#
# Eval-bed identity
#   Workspace: e256f781-c7cc-42c4-8539-ed4e944158c0  (fresh; never re-used)
#   User:      00000000-0000-0000-0000-000000000001   (default dev user)
#   Source:    vercel/next.js, strategy=merged-prs, n=50
#
# Services assumed live on localhost:
#   ghola        :7421
#   chapterhouse :8080
#   postgres     :5432 (docker-compose-postgres-1)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKTREE_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
DATA_DIR="${SCRIPT_DIR}"
LOG="${DATA_DIR}/phase0.log"
VENV="${WORKTREE_ROOT}/seeding-eval/.venv"
CACHE_DIR="${DATA_DIR}/cache"
BUNDLE_DIR="${DATA_DIR}/bundle-dir"
BUNDLE="${BUNDLE_DIR}/bundle.jsonl"
CASES="${DATA_DIR}/cases.jsonl"
EVENT_BUCKETS="${DATA_DIR}/event_buckets.json"
RESULTS_NOPRIM="${DATA_DIR}/results-baseline-noprim"
RESULTS_PRIM="${DATA_DIR}/results-baseline-prim"

WORKSPACE="a4f8bdd2-65c0-44b6-990d-ef2d1f8dc479"
USER_ID="00000000-0000-0000-0000-000000000001"
REPO="vercel/next.js"
N_RESOLVED=750
STRATEGY="merged-prs"

CHAPTERHOUSE_URL="http://localhost:8080"
GHOLA_URL="http://localhost:7421"
POSTGRES_CONTAINER="docker-compose-postgres-1"
RESUME_STATE="${DATA_DIR}/import-logs-imported.txt"

export CHAPTERHOUSE_API_KEY="${CHAPTERHOUSE_API_KEY:-}"
export GITHUB_TOKEN="${GITHUB_TOKEN:-$(gh auth token 2>/dev/null || echo '')}"

log() {
    local ts
    ts="$(date '+%Y-%m-%dT%H:%M:%S')"
    echo "[${ts}] $*" | tee -a "${LOG}"
}

die() {
    log "FATAL: $*"
    exit 1
}

log "=== Phase 0 driver start ==="
log "worktree: ${WORKTREE_ROOT}"
log "workspace: ${WORKSPACE}"
log "user: ${USER_ID}"
log "repo: ${REPO} strategy=${STRATEGY} n=${N_RESOLVED}"

mkdir -p "${CACHE_DIR}" "${BUNDLE_DIR}" "${RESULTS_NOPRIM}" "${RESULTS_PRIM}"

# ---------------------------------------------------------------------------
# (a) seeding-extract: GitHub corpus → bundle JSONL
# ---------------------------------------------------------------------------
log "--- Step A: seeding-extract ---"

if [[ -f "${BUNDLE}" ]]; then
    NLINES=$(wc -l < "${BUNDLE}")
    log "bundle already exists (${NLINES} lines) — skipping extraction (cache-first)"
else
    log "running seeding-extract (may take 5-15 min on cold cache)..."
    "${VENV}/bin/seeding-extract" \
        --repo "${REPO}" \
        --strategy "${STRATEGY}" \
        --n-resolved "${N_RESOLVED}" \
        --cache-dir "${CACHE_DIR}" \
        --bundle-out "${BUNDLE}" \
        2>&1 | tee -a "${LOG}"
fi

BUNDLE_LINES=$(wc -l < "${BUNDLE}")
log "bundle: ${BUNDLE_LINES} thread records"
[[ "${BUNDLE_LINES}" -gt 0 ]] || die "bundle is empty — extraction failed"

# ---------------------------------------------------------------------------
# (b) Build cases.jsonl and event_buckets.json from the extracted cache
# ---------------------------------------------------------------------------
log "--- Step B: build cases + event_buckets ---"

"${VENV}/bin/python" - <<'PYEOF' 2>&1 | tee -a "${LOG}"
import json, uuid, sys
from pathlib import Path
from seeding_eval.bundle import NS_EVENT, build_bundle, write_bundle
from seeding_eval.cases import build_cases
from seeding_eval.modules import primary_bucket
import dataclasses

REPO = "vercel/next.js"
DATA = Path(__file__).resolve().parent if hasattr(__file__, '__file__') else Path("seeding-eval/data")
PYEOF

# Use python with explicit paths since heredoc __file__ doesn't work cleanly
"${VENV}/bin/python" - "${DATA_DIR}" "${CACHE_DIR}" "${REPO}" "${CASES}" "${EVENT_BUCKETS}" <<'PYEOF' 2>&1 | tee -a "${LOG}"
import json, uuid, sys, dataclasses
from pathlib import Path

data_dir   = Path(sys.argv[1])
cache_dir  = Path(sys.argv[2])
repo       = sys.argv[3]
cases_path = Path(sys.argv[4])
eb_path    = Path(sys.argv[5])

from seeding_eval.bundle import NS_EVENT, build_bundle, write_bundle
from seeding_eval.cases import build_cases, EvalCase
from seeding_eval.modules import primary_bucket

# Load cached extracts
def _repo_slug(r):
    return r.replace("/", "__")

repo_dir = cache_dir / _repo_slug(repo)

issues  = json.loads((repo_dir / "issues.json").read_text())
links   = json.loads((repo_dir / "links.json").read_text())
prs     = json.loads((repo_dir / "prs.json").read_text()) if (repo_dir / "prs.json").exists() else {}
commits = json.loads((repo_dir / "commits.json").read_text()) if (repo_dir / "commits.json").exists() else {}

extracts = {"issues": issues, "links": links, "prs": prs, "commits": commits}

# Build and write cases JSONL
cases = build_cases(extracts, repo=repo)
with cases_path.open("w") as f:
    for c in cases:
        d = dataclasses.asdict(c)
        d["ground_truth_event_ids"] = list(d["ground_truth_event_ids"])
        d["module_path_buckets"]    = list(d["module_path_buckets"])
        f.write(json.dumps(d) + "\n")
n_held_out = sum(1 for c in cases if c.held_out)
print(f"cases: {len(cases)} total, {n_held_out} held-out → {cases_path}")

# Build event_buckets.json
event_buckets = {}
for issue in issues:
    issue_num = str(issue["number"])
    link = links.get(issue_num) or {}
    if link.get("pr") is None:
        continue
    if link.get("files"):
        issue_eid = str(uuid.uuid5(NS_EVENT, f"{repo}/issue/{issue_num}"))
        event_buckets[issue_eid] = primary_bucket(link["files"])
        pr_eid = str(uuid.uuid5(NS_EVENT, f"{repo}/pr/{link['pr']}"))
        event_buckets[pr_eid] = primary_bucket(link["files"])
    for sha in link.get("commits", []):
        commit = commits.get(sha) or {}
        if commit.get("files"):
            commit_eid = str(uuid.uuid5(NS_EVENT, f"{repo}/commit/{sha}"))
            event_buckets[commit_eid] = primary_bucket(commit["files"])

eb_path.write_text(json.dumps(event_buckets, indent=2))
print(f"event_buckets: {len(event_buckets)} entries → {eb_path}")
PYEOF

log "cases and event_buckets written"

# ---------------------------------------------------------------------------
# (c) import-logs: ingest bundle into chapterhouse
# ---------------------------------------------------------------------------
log "--- Step C: import-logs ingest ---"
log "ingesting ${BUNDLE_LINES} threads into workspace ${WORKSPACE}..."

# Auth: lift the API key from the running ghola container's env at exec time
# (never written to disk or logs; import-logs reads it from the process env).
CHAPTERHOUSE_API_KEY=$(docker inspect docker-compose-ghola-1 \
    --format '{{range .Config.Env}}{{println .}}{{end}}' \
    | grep '^CHAPTERHOUSE_API_KEY=' | cut -d= -f2-)
if [ -z "${CHAPTERHOUSE_API_KEY}" ]; then
    # Local stack runs AUTH_PROVIDER=default + CHAPTERHOUSE_REQUIRE_KEY=false:
    # the server ignores the Bearer key; import-logs only requires non-empty.
    CHAPTERHOUSE_API_KEY="local-dev-noauth"
    log "container key empty; using local-dev placeholder (server auth is off)"
fi
export CHAPTERHOUSE_API_KEY

cd "${WORKTREE_ROOT}"
go run ./cmd/import-logs \
    "--source=github:${BUNDLE_DIR}" \
    "--workspace=${WORKSPACE}" \
    "--user=${USER_ID}" \
    "--chapterhouse-url=${CHAPTERHOUSE_URL}" \
    "--resume-state=${RESUME_STATE}" \
    "--resume=false" \
    2>&1 | tee -a "${LOG}"

log "ingest complete"

# ---------------------------------------------------------------------------
# (d) Wait for co_activation_queue to drain
# ---------------------------------------------------------------------------
log "--- Step D: wait for co_activation_queue to drain ---"

MAX_WAIT=600   # 10 minutes
ELAPSED=0
POLL=10

while true; do
    QUEUE_DEPTH=$(docker exec "${POSTGRES_CONTAINER}" \
        psql -U memory_api -d memories -tc \
        "SELECT count(*) FROM semantic.co_activation_queue;" \
        2>/dev/null | tr -d ' ')
    QUEUE_DEPTH="${QUEUE_DEPTH:-unknown}"
    log "co_activation_queue depth: ${QUEUE_DEPTH}"
    if [[ "${QUEUE_DEPTH}" == "0" ]]; then
        log "queue drained"
        break
    fi
    if [[ "${ELAPSED}" -ge "${MAX_WAIT}" ]]; then
        log "WARNING: queue did not drain after ${MAX_WAIT}s (depth=${QUEUE_DEPTH}) — proceeding anyway"
        break
    fi
    sleep "${POLL}"
    ELAPSED=$((ELAPSED + POLL))
done

# ---------------------------------------------------------------------------
# (e) Run baselines
# ---------------------------------------------------------------------------
log "--- Step E: baselines ---"

CASES_LINES=$(wc -l < "${CASES}")
log "running noprim baseline (${CASES_LINES} cases, k=20)..."
"${VENV}/bin/seeding-eval-run" \
    --cases "${CASES}" \
    --workspace "${WORKSPACE}" \
    --user "${USER_ID}" \
    --ghola-base-url "${GHOLA_URL}" \
    --k 20 \
    --event-buckets "${EVENT_BUCKETS}" \
    --out-dir "${RESULTS_NOPRIM}" \
    2>&1 | tee -a "${LOG}"
log "noprim baseline done → ${RESULTS_NOPRIM}"

log "running prim baseline (${CASES_LINES} cases, k=20, --primitives)..."
"${VENV}/bin/seeding-eval-run" \
    --cases "${CASES}" \
    --workspace "${WORKSPACE}" \
    --user "${USER_ID}" \
    --ghola-base-url "${GHOLA_URL}" \
    --k 20 \
    --event-buckets "${EVENT_BUCKETS}" \
    --primitives \
    --out-dir "${RESULTS_PRIM}" \
    2>&1 | tee -a "${LOG}"
log "prim baseline done → ${RESULTS_PRIM}"

# ---------------------------------------------------------------------------
# (f) Derive bridge-miss set
# ---------------------------------------------------------------------------
log "--- Step F: derive bridge-miss set ---"

"${VENV}/bin/python" - \
    "${RESULTS_NOPRIM}" "${RESULTS_PRIM}" \
    "${DATA_DIR}/bridge-misses.txt" \
    <<'PYEOF' 2>&1 | tee -a "${LOG}"
import json, sys
from pathlib import Path

noprim_dir = Path(sys.argv[1])
prim_dir   = Path(sys.argv[2])
out_path   = Path(sys.argv[3])

def load_traces(d):
    p = d / "per-case-traces.jsonl"
    if not p.exists():
        return []
    with p.open() as f:
        return [json.loads(l) for l in f if l.strip()]

noprim_traces = load_traces(noprim_dir)
prim_traces   = load_traces(prim_dir)

def ground_truth_in_top5(trace):
    """Return True if any ground-truth event_id appears in top-5 hits."""
    gt_ids = set(trace.get("ground_truth_event_ids") or [])
    hits = trace.get("hits") or []
    top5_ids = {h.get("id") for h in hits[:5]}
    return bool(gt_ids & top5_ids)

# Index by (case_id, variant) for both baselines
def index_by_case_variant(traces):
    idx = {}
    for t in traces:
        key = (t.get("case_id"), t.get("variant"))
        idx[key] = t
    return idx

noprim_idx = index_by_case_variant(noprim_traces)
prim_idx   = index_by_case_variant(prim_traces)

# Collect all case_ids
all_cases = set()
for (cid, variant) in noprim_idx:
    if variant in ("none", None):
        all_cases.add(cid)

# Miss = NO ground truth in top-5 under BOTH baselines (variant="none")
misses = []
for cid in sorted(all_cases):
    noprim_t = noprim_idx.get((cid, "none"))
    prim_t   = prim_idx.get((cid, "none"))
    noprim_hit = ground_truth_in_top5(noprim_t) if noprim_t else False
    prim_hit   = ground_truth_in_top5(prim_t)   if prim_t   else False
    if not noprim_hit and not prim_hit:
        misses.append(cid)

out_path.write_text("\n".join(misses) + ("\n" if misses else ""))
print(f"bridge-misses: {len(misses)} cases miss under BOTH baselines → {out_path}")
print(f"total held-out cases evaluated: {len(all_cases)}")
PYEOF

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
log "--- Summary ---"

# Extract P@5 numbers from report JSONs
"${VENV}/bin/python" - "${RESULTS_NOPRIM}" "${RESULTS_PRIM}" "${DATA_DIR}/bridge-misses.txt" <<'PYEOF' 2>&1 | tee -a "${LOG}"
import json, sys
from pathlib import Path

def p_at_5_from_report(d):
    p = Path(d) / "report.json"
    if not p.exists():
        return "N/A"
    try:
        r = json.loads(p.read_text())
        # Try common keys
        for key in ("h2", "H2", "metrics"):
            if key in r:
                sub = r[key]
                for pkey in ("p_at_5_none", "p_at_5", "p5"):
                    if pkey in sub:
                        return f"{sub[pkey]:.3f}"
        return json.dumps(r.get("h2", r))[:80]
    except Exception as e:
        return f"parse error: {e}"

noprim_p5 = p_at_5_from_report(sys.argv[1])
prim_p5   = p_at_5_from_report(sys.argv[2])
misses_f  = Path(sys.argv[3])
miss_count = len([l for l in misses_f.read_text().splitlines() if l.strip()]) if misses_f.exists() else "?"

print(f"BASELINE noprim P@5={noprim_p5}  prim P@5={prim_p5}  bridge-misses={miss_count}")
PYEOF

log "=== Phase 0 driver complete ==="
