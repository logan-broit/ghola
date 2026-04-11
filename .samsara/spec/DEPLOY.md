# Deploy Pipeline

## Build

```bash
cd ~/pg_ghola
docker build --no-cache -f Dockerfile.cnpg -t cnpg-pg18-ghola:18.1-ghola-0.0.5 .
```

## Transfer and Import

```bash
docker save cnpg-pg18-ghola:18.1-ghola-0.0.5 -o /tmp/cnpg-ghola-0.0.5.tar
sudo -u claude scp /tmp/cnpg-ghola-0.0.5.tar nuc:/tmp/
sudo -u claude ssh nuc 'sudo /usr/local/bin/k3s ctr images import /tmp/cnpg-ghola-0.0.5.tar && rm /tmp/cnpg-ghola-0.0.5.tar'
```

## Restart Pod

```bash
kubectl delete pod -n ch-system memory-db-1
# Wait for pod to come back
sleep 60
kubectl get pods -n ch-system memory-db-1
```

## Recreate Recall Functions

CRITICAL: After every pod restart, recall functions must be manually recreated.
The extension .so has the symbols but CREATE EXTENSION only runs once.

```bash
kubectl exec -n ch-system memory-db-1 -- psql -U postgres -d memories -c "
CREATE OR REPLACE FUNCTION ghola.recall_inner(
    workspace_id uuid, query_text TEXT, query_embedding_text TEXT,
    limit_n INT DEFAULT 10, min_confidence double precision DEFAULT 0.0,
    w_semantic double precision DEFAULT 0.6, w_fts double precision DEFAULT 0.4,
    w_actr_decay double precision DEFAULT 0.5, w_hebbian_scale double precision DEFAULT 4.0,
    filter_memory_type TEXT DEFAULT NULL, filter_scope TEXT DEFAULT NULL,
    filter_tags TEXT[] DEFAULT NULL, filter_session_id uuid DEFAULT NULL,
    filter_entities TEXT[] DEFAULT NULL, filter_intent TEXT DEFAULT NULL
) RETURNS TABLE (
    mneme_id uuid, score double precision, content_match double precision,
    activation double precision, hebbian_boost double precision,
    confidence double precision, concept TEXT, content TEXT
) STABLE LANGUAGE c AS 'pg_ghola', 'recall_inner_wrapper';

CREATE OR REPLACE FUNCTION ghola.recall(
    workspace_id uuid, query_text text, query_embedding vector,
    limit_n int DEFAULT 10, min_confidence float8 DEFAULT 0.0,
    weights ghola.score_weights DEFAULT NULL, memory_type text DEFAULT NULL,
    scope text DEFAULT NULL, tags text[] DEFAULT NULL, session_id uuid DEFAULT NULL,
    filter_entities text[] DEFAULT NULL, filter_intent text DEFAULT NULL
) RETURNS SETOF ghola.recall_result LANGUAGE SQL STABLE
AS \\\$\\\$
    SELECT (mneme_id, score, content_match, activation, hebbian_boost,
            confidence, concept, content)::ghola.recall_result
    FROM ghola.recall_inner(
        workspace_id, query_text, query_embedding::text, limit_n, min_confidence,
        COALESCE((weights).semantic, 0.6), COALESCE((weights).fts, 0.4),
        COALESCE((weights).actr_decay, 0.5), COALESCE((weights).hebbian_scale, 4.0),
        memory_type, scope, tags, session_id, filter_entities, filter_intent
    );
\\\$\\\$;"
```

## Verify

```bash
# All 3 workers running
kubectl exec -n ch-system memory-db-1 -- psql -U postgres -d memories -c "
SELECT pid, backend_type FROM pg_stat_activity WHERE backend_type LIKE '%ghola%';"

# Recall works
kubectl exec -n ch-system memory-db-1 -- psql -U postgres -d memories -c "
SELECT count(*) FROM ghola.recall(
    '00000000-0000-0000-0000-000000000001'::uuid, 'test',
    (SELECT embedding FROM ghola.mnemes LIMIT 1), 5, 0.0, NULL);"

# No crash-looping
kubectl logs -n ch-system memory-db-1 --since=2m 2>&1 | grep -i "exit code 1"
```
