#!/bin/bash
# Restore the pinned reference database from binary COPY dumps.
# Usage: ./analysis/benchmark_restore.sh [data_dir]
#
# Restores mnemes, associations, and cluster_centroids from compressed
# binary COPY files. The extension and schema must already exist.
#
# Default data directory: benchmark-data/

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
DATA_DIR="${1:-$SCRIPT_DIR/../benchmark-data}"

for f in ghola_mnemes_ref_20260412.bin.gz ghola_clusters_ref_20260412.bin.gz; do
    if [ ! -f "$DATA_DIR/$f" ]; then
        echo "ERROR: Required file not found: $DATA_DIR/$f"
        exit 1
    fi
done

echo "Restoring from: $DATA_DIR"

# Clear existing data
echo "Clearing existing data..."
kubectl exec -n ch-system memory-db-1 -- psql -U postgres -d memories -c "
BEGIN;
TRUNCATE ghola.co_activation_queue;
TRUNCATE ghola.contradiction_queue;
TRUNCATE ghola.gating_queue;
TRUNCATE ghola.contradiction_candidates;
TRUNCATE ghola.cluster_centroids CASCADE;
TRUNCATE ghola.associations CASCADE;
TRUNCATE ghola.mnemes CASCADE;
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
SELECT 'mnemes: ' || count(*) ||
       ', avg_ac: ' || avg(access_count)::numeric(10,2) ||
       ', assoc: ' || (SELECT count(*) FROM ghola.associations) ||
       ', embeddings: ' || count(*) FILTER (WHERE embedding IS NOT NULL)
FROM ghola.mnemes;"

echo "Restore complete."
