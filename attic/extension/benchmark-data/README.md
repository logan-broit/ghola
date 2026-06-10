# Benchmark Reference Data

Binary COPY dumps of the pinned benchmark database. Used by `analysis/benchmark_restore.sh`.

## Files (gitignored)

- `ghola_mnemes_ref_20260411.bin.gz` -- 19,181 mnemes with embeddings, entities, clusters
- `ghola_clusters_ref_20260411.bin.gz` -- Cluster centroids
- `ghola_associations_ref_20260411.bin.gz` -- 11 supersedes associations (gating-time only)

## State captured

- access_count = 1 (uniform)
- No hebbian associations
- bench tags = `bench_00000000`
- Created from Iter 6 re-ingest (TEI nomic-embed-text-v1.5 CPU float32)

## Why binary COPY instead of pg_dump

pg_dump cannot export data for extension-member tables (tables created by CREATE EXTENSION).
The mnemes and associations tables are owned by the pg_ghola extension. COPY binary format
bypasses this limitation.

## Recreating

```bash
# Export from live database (after full reset)
./analysis/benchmark_reset.sh
kubectl exec -n ch-system memory-db-1 -- psql -U postgres -d memories \
    -c "COPY ghola.mnemes TO STDOUT WITH (FORMAT binary)" \
    | gzip -9 > benchmark-data/ghola_mnemes_ref_YYYYMMDD.bin.gz
# (repeat for associations and cluster_centroids)
```
