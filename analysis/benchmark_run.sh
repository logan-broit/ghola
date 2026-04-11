#!/bin/bash
# Run a retrieve-only benchmark with co-activation reset.
# Usage: ./analysis/benchmark_run.sh [num_runs]
#
# Resets co-activation state before EACH run, runs retrieve-only, evaluates.
# For multi-run averaging, pass num_runs > 1 (default: 1).
#
# Results are saved to ~/longmemeval-ghola/results/ and evaluated inline.

set -euo pipefail

NUM_RUNS=${1:-1}
WORKSPACE="00000000-0000-0000-0000-000000000001"
RESULTS_DIR="$HOME/longmemeval-ghola/results"
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "=== Benchmark: $NUM_RUNS run(s), retrieve-only, workspace=$WORKSPACE ==="

for i in $(seq 1 "$NUM_RUNS"); do
    echo ""
    echo "--- Run $i/$NUM_RUNS ---"

    # Reset ALL retrieval-time state (co-activation + hebbian associations)
    echo "Resetting retrieval-time state..."
    kubectl exec -n ch-system memory-db-1 -- psql -U postgres -d memories -q -c "
    BEGIN;
    TRUNCATE ghola.co_activation_queue;
    DELETE FROM ghola.associations WHERE association_type = 'hebbian';
    UPDATE ghola.mnemes SET access_count = 1, last_access = created_at;
    COMMIT;" 2>/dev/null

    # Retrieve
    echo "Running retrieve..."
    cd "$HOME/longmemeval-ghola"
    RESULT_FILE=$(.venv/bin/python run.py retrieve \
        --backend ghola_mcp --dataset s \
        --workspace-id "$WORKSPACE" 2>&1 | grep -oP 'results/\S+\.jsonl')

    # Evaluate
    echo "Evaluating $RESULT_FILE..."
    .venv/bin/python run.py evaluate --run "$RESULT_FILE"
done

echo ""
echo "=== All runs complete ==="
