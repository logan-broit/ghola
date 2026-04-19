#!/bin/bash
# Restore the pinned reference database from binary COPY dumps.
# Usage: ./analysis/benchmark_restore.sh [data_dir]
#
# SAFETY: Only deletes mnemes tagged with bench_00000000 before restoring.
# Real user memories are NEVER modified by this script.
#
# Restores mnemes, associations, and cluster_centroids from compressed
# binary COPY files. The extension and schema must already exist.
#
# Default data directory: benchmark-data/

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DATA_DIR="${1:-$SCRIPT_DIR/../benchmark-data}"
BENCH_TAG="bench_00000000"

for f in ghola_mnemes_ref_20260412.bin.gz ghola_clusters_ref_20260412.bin.gz; do
    if [ ! -f "$DATA_DIR/$f" ]; then
        echo "ERROR: Required file not found: $DATA_DIR/$f"
        exit 1
    fi
done

echo "Restoring from: $DATA_DIR"
echo "SAFETY: Only clearing benchmark-tagged data (tag: $BENCH_TAG)"

# Count real memories before
REAL_COUNT=$(kubectl exec -n ch-system memory-db-1 -- psql -U postgres -d memories -t -A -c "
SELECT count(*) FROM ghola.mnemes WHERE NOT (tags @> ARRAY['$BENCH_TAG']::text[]);")
echo "Real memories present: $REAL_COUNT (will be preserved)"

# Clear ONLY benchmark data
echo "Clearing benchmark data..."
kubectl exec -n ch-system memory-db-1 -- psql -U postgres -d memories -c "
BEGIN;
TRUNCATE ghola.co_activation_queue;
-- Delete benchmark associations
DELETE FROM ghola.associations
WHERE src_id IN (SELECT id FROM ghola.mnemes WHERE tags @> ARRAY['$BENCH_TAG']::text[]);
-- Delete benchmark contradiction candidates
DELETE FROM ghola.contradiction_candidates
WHERE mneme_a IN (SELECT id FROM ghola.mnemes WHERE tags @> ARRAY['$BENCH_TAG']::text[])
   OR mneme_b IN (SELECT id FROM ghola.mnemes WHERE tags @> ARRAY['$BENCH_TAG']::text[]);
-- Delete benchmark mnemes
DELETE FROM ghola.mnemes WHERE tags @> ARRAY['$BENCH_TAG']::text[];
-- Clear benchmark cluster centroids (these are workspace-scoped, bench uses 00000000-...-0001)
DELETE FROM ghola.cluster_centroids WHERE workspace_id = '00000000-0000-0000-0000-000000000001';
-- Clear queues (will be re-populated by COPY triggers)
TRUNCATE ghola.gating_queue;
TRUNCATE ghola.contradiction_queue;
COMMIT;"

# Restore mnemes
# Note: dumps have 77-byte kubectl stderr prefix ("Defaulted container...")
# that must be stripped before PostgreSQL can parse the PGCOPY header.
echo "Restoring mnemes (this takes a few minutes)..."
gunzip -c "$DATA_DIR/ghola_mnemes_ref_20260412.bin.gz" | tail -c +78 | \
    kubectl exec -i -n ch-system memory-db-1 -- \
    psql -U postgres -d memories -c "COPY ghola.mnemes FROM STDIN WITH (FORMAT binary)"

# Restore cluster_centroids
echo "Restoring cluster_centroids..."
gunzip -c "$DATA_DIR/ghola_clusters_ref_20260412.bin.gz" | tail -c +78 | \
    kubectl exec -i -n ch-system memory-db-1 -- \
    psql -U postgres -d memories -c "COPY ghola.cluster_centroids FROM STDIN WITH (FORMAT binary)"

# Restore associations (optional, only supersedes)
if [ -f "$DATA_DIR/ghola_associations_ref_20260412.bin.gz" ]; then
    echo "Restoring associations..."
    gunzip -c "$DATA_DIR/ghola_associations_ref_20260412.bin.gz" | tail -c +78 | \
        kubectl exec -i -n ch-system memory-db-1 -- \
        psql -U postgres -d memories -c "COPY ghola.associations FROM STDIN WITH (FORMAT binary)"
fi

# Clear worker queues (COPY binary fires INSERT triggers, re-populating queues)
echo "Clearing worker queues (triggered by COPY)..."
kubectl exec -n ch-system memory-db-1 -- psql -U postgres -d memories -c "
TRUNCATE ghola.gating_queue;
TRUNCATE ghola.contradiction_queue;
TRUNCATE ghola.co_activation_queue;"

# Verify
echo "Verifying..."
kubectl exec -n ch-system memory-db-1 -- psql -U postgres -d memories -t -A -c "
SELECT 'bench_mnemes: ' || count(*) FILTER (WHERE tags @> ARRAY['$BENCH_TAG']::text[]) ||
       ', real_mnemes: ' || count(*) FILTER (WHERE NOT (tags @> ARRAY['$BENCH_TAG']::text[])) ||
       ', total: ' || count(*)
FROM ghola.mnemes;"

REAL_AFTER=$(kubectl exec -n ch-system memory-db-1 -- psql -U postgres -d memories -t -A -c "
SELECT count(*) FROM ghola.mnemes WHERE NOT (tags @> ARRAY['$BENCH_TAG']::text[]);")

if [ "$REAL_COUNT" != "$REAL_AFTER" ]; then
    echo "WARNING: Real memory count changed! Before: $REAL_COUNT, After: $REAL_AFTER"
    exit 1
fi

echo "Restore complete. Real memories preserved ($REAL_AFTER)."
