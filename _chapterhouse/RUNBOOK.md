# Chapterhouse Deployment Runbook

Operational guide for deploying Chapterhouse to a Kubernetes cluster.

## Table of Contents

1. [Prerequisites](#prerequisites)
2. [Infrastructure Setup](#infrastructure-setup)
3. [Database Migrations](#database-migrations)
4. [Building Container Images](#building-container-images)
5. [Deploying with Helm](#deploying-with-helm)
6. [Ingress Configuration](#ingress-configuration)
7. [Admin User Bootstrap](#admin-user-bootstrap)
8. [Claude Code MCP Setup](#claude-code-mcp-setup)
9. [Verification](#verification)
10. [Rollback Procedures](#rollback-procedures)
11. [Troubleshooting](#troubleshooting)

**Related docs**: [BUILD_AND_RELEASE.md](BUILD_AND_RELEASE.md)

---

## Prerequisites

### Required Tools

| Tool | Version | Purpose |
|------|---------|---------|
| kubectl | 1.28+ | Kubernetes CLI |
| helm | 3.12+ | Helm chart deployment |
| podman or docker | latest | Container image building |
| Go | 1.24+ | Building from source |

### Cluster Requirements

- Kubernetes cluster with [CloudNativePG](https://cloudnative-pg.io/) operator installed
- StorageClass available (e.g., `local-path` for K3s)
- PostgreSQL 18 with pgvector and pg_recall extensions

### Container Registry

Images are stored at `ghcr.io/thinkwright/chapterhouse`. See `BUILD_AND_RELEASE.md` for build instructions.

---

## Infrastructure Setup

### 1. Create Namespace

```bash
kubectl create namespace ch-system
```

### 2. Create Secrets

**Database bootstrap** (for CNPG):

```bash
kubectl create secret generic chapterhouse-db-credentials \
  -n ch-system \
  --from-literal=username=memory_api \
  --from-literal=password="$(openssl rand -base64 24)"
```

**Admin bootstrap**:

```bash
kubectl create secret generic ch-admin-bootstrap \
  -n ch-system \
  --from-literal=ADMIN_USERNAME=admin \
  --from-literal=ADMIN_PASSWORD="$(openssl rand -base64 16)"
```

**Embedding API key** (if using a hosted provider):

```bash
kubectl create secret generic together-api-key \
  -n ch-system \
  --from-literal=TOGETHER_API_KEY=your-api-key
```

### 3. Deploy PostgreSQL (CNPG)

See `deploy/examples/postgres-cnpg.yaml` for the CNPG Cluster manifest. For the homelab, use `deploy/homelab/infra/postgres-cluster.yaml` which includes the custom CNPG image with pgvector and pg_recall baked in.

Key points:
- Bootstrap references the `chapterhouse-db-credentials` secret
- Database name: `memories`, owner: `memory_api`
- Custom image includes `shared_preload_libraries: [pg_recall]`

```bash
kubectl apply -f deploy/homelab/infra/postgres-cluster.yaml
kubectl wait --for=condition=Ready \
  clusters.postgresql.cnpg.io/memory-db \
  -n ch-system --timeout=300s
```

### 4. Deploy Embeddings (TEI)

Self-hosted embeddings using HuggingFace Text Embeddings Inference with `Alibaba-NLP/gte-modernbert-base` (768 dims, 8K context):

```bash
kubectl apply -f deploy/homelab/infra/tei.yaml
```

---

## Database Migrations

Migrations must be applied manually after the CNPG cluster is ready. Run them as the `postgres` superuser, then grant privileges to the application user.

### Enable pg_recall Extension

```bash
kubectl exec -i memory-db-1 -n ch-system -- psql -U postgres -d memories -c "
  SET allow_system_table_mods = on;
  CREATE EXTENSION IF NOT EXISTS vector;
  CREATE EXTENSION IF NOT EXISTS pg_recall;
  SELECT pg_recall.configure_dimensions(768);
"
```

### Apply All Migrations

```bash
for f in ch-server/db/migrations/*.sql; do
  echo "--- Applying $(basename $f) ---"
  kubectl exec -i memory-db-1 -n ch-system -- \
    psql -U postgres -d memories < "$f"
done
```

### Grant Privileges

```bash
kubectl exec -i memory-db-1 -n ch-system -- psql -U postgres -d memories -c "
  GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA public TO memory_api;
  GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public TO memory_api;
  GRANT USAGE ON SCHEMA public TO memory_api;
  ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO memory_api;
  ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO memory_api;
  GRANT USAGE ON SCHEMA pg_recall TO memory_api;
  GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA pg_recall TO memory_api;
  GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA pg_recall TO memory_api;
  GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA pg_recall TO memory_api;
"
```

### Verify

```bash
kubectl exec memory-db-1 -n ch-system -- psql -U memory_api -d memories -c '\dt'
```

Expected tables: `users`, `audit_log`, `api_keys`, `admin_sessions` (memory data is stored in `pg_recall.mnemes`)

---

## Building Container Images

For local builds using podman:

```bash
podman build --platform linux/amd64 -t ghcr.io/thinkwright/chapterhouse/ch-server:latest ch-server/
podman build --platform linux/amd64 -t ghcr.io/thinkwright/chapterhouse/ch-web:latest ch-web/
podman push ghcr.io/thinkwright/chapterhouse/ch-server:latest
podman push ghcr.io/thinkwright/chapterhouse/ch-web:latest
```

Or use the deploy script:

```bash
./deploy/homelab/deploy.sh --tag latest
```

---

## Deploying with Helm

Each component has a Helm chart under its `charts/` directory.

### Homelab Deployment

Use the homelab values files which configure GHCR registry, TEI embeddings, and homelab-sized resources:

```bash
helm upgrade --install ch-server ch-server/charts/ch-server \
  --namespace ch-system \
  -f deploy/homelab/ch-server-values.yaml

helm upgrade --install ch-web ch-web/charts/ch-web \
  --namespace ch-system \
  -f deploy/homelab/ch-web-values.yaml
```

---

## Ingress Configuration

Use a Kubernetes Ingress resource. Set `ingress.enabled: true` in ch-server values and configure `className`, `hosts`, and `tls` as needed.

### TLS Considerations

**With trusted certificates** (Let's Encrypt, internal CA): The default configuration works — secure cookies and the Clipboard API function correctly.

**With self-signed certificates**: Set `ENVIRONMENT` to `local` or `development`. This disables the `Secure` flag on session cookies. The admin console includes a clipboard fallback for non-secure contexts.

---

## Admin User Bootstrap

After migrations, seed the admin user. Get the admin password from the bootstrap secret:

```bash
ADMIN_PASS=$(kubectl get secret ch-admin-bootstrap -n ch-system \
  -o jsonpath='{.data.ADMIN_PASSWORD}' | base64 -d)
```

Insert the admin user with a bcrypt-hashed password:

```bash
kubectl exec -i memory-db-1 -n ch-system -- psql -U postgres -d memories -c "
  INSERT INTO users (id, username, email, display_name, password_hash, is_admin)
  VALUES (
    '00000000-0000-0000-0000-000000000001',
    'admin',
    'admin@localhost',
    'Administrator',
    crypt('${ADMIN_PASS}', gen_salt('bf', 10)),
    true
  ) ON CONFLICT (username) DO UPDATE SET
    password_hash = crypt('${ADMIN_PASS}', gen_salt('bf', 10)),
    is_admin = true;
"
```

---

## Claude Code MCP Setup

### 1. Create a User and API Key

Log into the admin console, create a user, and generate an API key. The key is shown once at creation — copy it immediately.

API keys use the format `ch_k1_<64 hex characters>`.

### 2. Add to Claude Code

Always install globally with `-s user` so the MCP server is available across all projects:

```bash
claude mcp add -s user --transport http \
  chapterhouse https://your-host/mcp \
  --header "Authorization: Bearer ch_k1_YOUR_KEY"
```

Use the `/mcp` endpoint for full session lifecycle support (list_sessions, session_summary, session_context).

### 3. Verify

Start a new Claude Code session. The Chapterhouse tools should appear: `remember`, `recall`, `forget`, `list_memories`, `share_memory`, `export_memories`, `list_sessions`, `session_summary`, `session_context`.

---

## Verification

### Health and Readiness

```bash
curl -sk https://your-host/health
# {"status":"ok","timestamp":"..."}

curl -sk https://your-host/ready
# {"status":"ok","checks":{"database":"healthy"}}
```

### Admin Login

```bash
curl -sk -X POST https://your-host/api/v1/admin/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"YOUR_PASSWORD"}' \
  -c /tmp/ch-cookies.txt
```

### MCP Tools

```bash
curl -sk -X POST https://your-host/mcp/stateless \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer ch_k1_YOUR_KEY' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'
```

---

## Rollback Procedures

### Helm Rollback

```bash
helm history ch-server -n ch-system
helm rollback ch-server -n ch-system        # previous revision
helm rollback ch-server 2 -n ch-system      # specific revision
```

### Deployment Rollback

```bash
kubectl rollout undo deployment/ch-server -n ch-system
kubectl rollout undo deployment/ch-web -n ch-system
```

### Database Backup

Always backup before schema changes:

```bash
kubectl exec memory-db-1 -n ch-system -- \
  pg_dump -U postgres memories > backup-$(date +%Y%m%d-%H%M%S).sql
```

---

## Troubleshooting

### Session Cookie Not Persisting After Login

**Symptom**: Login returns 200 but subsequent requests return 401. The page flashes and resets.

**Cause**: The `Secure` flag on session cookies requires a trusted TLS context. Self-signed certificates don't qualify — browsers silently discard the cookie.

**Fix**: Set `ENVIRONMENT` to `local` or `development`. Only `production` enforces secure cookies.

### Clipboard Copy Fails

**Symptom**: "Failed to copy" toast when clicking copy buttons in the admin console.

**Cause**: `navigator.clipboard.writeText()` requires a secure context. Same root cause as the cookie issue.

**Fix**: The admin console includes a `document.execCommand('copy')` fallback, but ensure you're on a recent deployment that includes this fix.

### PVC Stuck Pending

**Symptom**: PostgreSQL pod stuck in Pending, PVC not binding.

**Cause**: StorageClass mismatch. Check what's available:

```bash
kubectl get storageclass
```

**Fix**: Update the `storageClassName` in your manifests to match an available StorageClass (e.g., `local-path` for K3s).

### CNPG Secret Naming

**Symptom**: ch-server can't connect to database, looking for a secret that doesn't exist.

**Cause**: When you provide your own bootstrap secret to CNPG, it uses that secret directly instead of creating a separate `-app` secret.

**Fix**: Set `database.existingSecret` in your values file to match the bootstrap secret name (e.g., `memory-db-credentials`), not `memory-db-app`.

### pg_recall Extension Creation Fails

**Symptom**: `ERROR: unacceptable schema name "pg_recall"` when running `CREATE EXTENSION pg_recall`.

**Cause**: PostgreSQL 18 blocks extensions using the `pg_` schema prefix by default.

**Fix**: Run `SET allow_system_table_mods = on;` before `CREATE EXTENSION pg_recall;`.

### Partial Index Migration Error

**Symptom**: `ERROR: functions in index predicate must be marked IMMUTABLE` when running migration 002.

**Cause**: This was a bug in an earlier version of migration 002 that used `NOW()` in partial index predicates.

**Fix**: Ensure you're using the current migration files where indexes use `WHERE revoked_at IS NULL` without time predicates.

### Helm Release Stuck in pending-upgrade

**Symptom**: `helm upgrade` fails with `another operation (install/upgrade/rollback) is in progress`.

**Cause**: A previous deploy was interrupted, leaving a Helm release in `pending-upgrade` or `pending-install` state.

**Fix**: Roll back to the last successful revision:

```bash
helm history ch-server -n ch-system   # find the last "deployed" revision
helm rollback ch-server <REVISION> -n ch-system
```

Then retry the deploy.

### Pod Logs

```bash
kubectl logs -l app.kubernetes.io/name=ch-server -n ch-system --tail=50
kubectl logs -l app.kubernetes.io/name=ch-web -n ch-system --tail=50
kubectl logs -l cnpg.io/cluster=memory-db -n ch-system --tail=50
```
