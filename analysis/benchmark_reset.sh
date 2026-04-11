#!/bin/bash
# Reset ALL retrieval-time state for a clean retrieve-only benchmark run.
# Usage: ./analysis/benchmark_reset.sh
#
# Resets:
#   - access_count (ACT-R base-level activation) to 1
#   - last_access to created_at (ACT-R decay baseline)
#   - co_activation_queue (pending co-activation events)
#   - hebbian associations (retrieval-time co-activation weights)
#
# Preserves:
#   - mneme content, embeddings, entities, clusters, tsvectors
#   - supersedes associations (gating-time knowledge updates)
#
# Run this BEFORE every retrieve-only benchmark to eliminate rich-get-richer drift.

set -euo pipefail

echo "Resetting retrieval-time state..."
kubectl exec -n ch-system memory-db-1 -- psql -U postgres -d memories -c "
BEGIN;
TRUNCATE ghola.co_activation_queue;
DELETE FROM ghola.associations WHERE association_type = 'hebbian';
UPDATE ghola.mnemes SET access_count = 1, last_access = created_at;
COMMIT;"

echo "Verifying..."
kubectl exec -n ch-system memory-db-1 -- psql -U postgres -d memories -t -A -c "
SELECT 'mnemes: ' || count(*) ||
       ', avg_ac: ' || avg(access_count)::numeric(10,2) ||
       ', assoc: ' || (SELECT count(*) FROM ghola.associations)
FROM ghola.mnemes;"

echo "Reset complete."
