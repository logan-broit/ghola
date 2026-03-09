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
12. [Corporate Air-Gapped Environment](#corporate-air-gapped-environment)

**Related docs**: [BUILD_AND_RELEASE.md](BUILD_AND_RELEASE.md) | [SECURITY_STATEMENT.md](SECURITY_STATEMENT.md)

---

## Prerequisites

### Required Tools

| Tool | Version | Purpose |
|------|---------|---------|
| kubectl | 1.28+ | Kubernetes CLI |
| helm | 3.12+ | Helm chart deployment |
| docker | 27+ | Container image building (buildx required) |
| Go | 1.24+ | Building from source |
| glab | latest | GitLab CLI |

### Cluster Requirements

- Kubernetes cluster with [CloudNativePG](https://cloudnative-pg.io/) operator installed
- Istio service mesh (for VirtualService routing)
- Prometheus Operator (for ServiceMonitor)
- StorageClass: `ceph-rbd` (ovas-ai-prod) or `local-path` (homelab K3s)

### Container Registry

Images and Helm charts are stored in the Zot OCI registry at `registry.switchcraft.pd.internal/chapterhouse`. Zot allows anonymous read, so no imagePullSecret is needed. See `BUILD_AND_RELEASE.md` for build instructions.

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

**Embedding API key** (if using a hosted provider like Together.ai):

```bash
kubectl create secret generic together-api-key \
  -n ch-system \
  --from-literal=TOGETHER_API_KEY=your-api-key
```

### 3. Deploy PostgreSQL (CNPG)

See `deploy/examples/postgres-cnpg.yaml` for the CNPG Cluster manifest.

Key points:
- StorageClass: `ceph-rbd` (ovas-ai-prod) or `local-path` (homelab K3s)
- Bootstrap references the `chapterhouse-db-credentials` secret
- Database name: `memories`, owner: `memory_api`
- CNPG auto-creates a `chapterhouse-db-app` secret with credentials

```bash
kubectl apply -f deploy/examples/postgres-cnpg.yaml
kubectl wait --for=condition=Ready \
  clusters.postgresql.cnpg.io/chapterhouse-db \
  -n ch-system --timeout=300s
```

---

## Database Migrations

Migrations must be applied manually after the CNPG cluster is ready. Run them as the `postgres` superuser, then grant privileges to the application user.

### Apply All Migrations

```bash
# Run each migration in order
for f in 001_initial_schema.sql 002_admin_auth.sql 003_add_memory_type.sql \
         004_add_scope_and_org.sql 005_is_current_and_search.sql 006_add_tags_column.sql; do
  echo "--- Applying $f ---"
  kubectl exec -i chapterhouse-db-1 -n ch-system -- \
    psql -U postgres -d memories < ch-server/db/migrations/$f
done
```

### Grant Privileges

```bash
kubectl exec -i chapterhouse-db-1 -n ch-system -- psql -U postgres -d memories -c "
  GRANT ALL PRIVILEGES ON ALL TABLES IN SCHEMA public TO memory_api;
  GRANT ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public TO memory_api;
  GRANT USAGE ON SCHEMA public TO memory_api;
  ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO memory_api;
  ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO memory_api;
"
```

### Verify

```bash
kubectl exec chapterhouse-db-1 -n ch-system -- psql -U memory_api -d memories -c '\dt'
```

Expected tables: `users`, `audit_log`, `api_keys`, `admin_sessions` (memory data is stored in `pg_recall.mnemes`)

---

## Building Container Images

Production builds are driven through **GitLab CI** — see `BUILD_AND_RELEASE.md` for the full workflow. For local builds:

```bash
docker login registry.switchcraft.pd.internal
make server   # Build + push ch-server
make web      # Build + push ch-web
make images   # Build + push both
```

---

## Deploying with Helm

Each component has a Helm chart under its `charts/` directory. Both charts default to the Zot registry at `registry.switchcraft.pd.internal`.

### ch-server (ovas-ai-prod)

```bash
export KUBECONFIG=~/.kube/ovas-ai-prod.yaml

helm upgrade --install ch-server ch-server/charts/ch-server \
  --namespace ch-system \
  --set image.tag=0.3.0 \
  --set virtualService.enabled=true \
  --set virtualService.gateway=istio-ingress/switch-wildcard-ingress \
  --set virtualService.host=chapterhouse.switchcraft.pd.internal
```

### ch-web (ovas-ai-prod)

```bash
helm upgrade --install ch-web ch-web/charts/ch-web \
  --namespace ch-system \
  --set image.tag=0.3.0
```

### Homelab Overrides

For local K3s development, use the homelab values files which disable Istio, imagePullSecrets, and use local registry:

```bash
helm upgrade --install ch-server ch-server/charts/ch-server \
  --namespace ch-system \
  -f ch-server/charts/ch-server/values-homelab.yaml

helm upgrade --install ch-web ch-web/charts/ch-web \
  --namespace ch-system \
  -f ch-web/charts/ch-web/values-homelab.yaml
```

---

## Ingress Configuration

### Istio VirtualService (ovas-ai-prod)

The ch-server Helm chart includes a VirtualService template. When enabled, it routes traffic by path:

| Path | Destination |
|------|-------------|
| `/api/*` | ch-server:8080 |
| `/mcp`, `/mcp/*` | ch-server:8080 |
| `/health`, `/ready`, `/metrics` | ch-server:8080 |
| `/*` (everything else) | ch-web:80 |

Production settings for ovas-ai-prod:

```yaml
virtualService:
  enabled: true
  gateway: istio-ingress/switch-wildcard-ingress
  host: chapterhouse.switchcraft.pd.internal
```

### Kubernetes Ingress (homelab)

For clusters without Istio, use a standard Ingress resource. Set `ingress.enabled: true` in ch-server values and configure `className`, `hosts`, and `tls` as needed.

### TLS Considerations

**With trusted certificates** (Let's Encrypt, corporate CA): The default configuration works — secure cookies and the Clipboard API function correctly.

**With self-signed certificates**: Set `ENVIRONMENT` to anything other than `production` in your values. This disables the `Secure` flag on session cookies. The admin console includes a clipboard fallback for non-secure contexts.

**No TLS**: Same as self-signed — use a non-production environment value.

---

## Admin User Bootstrap

After migrations, seed the admin user. Get the admin password from the bootstrap secret:

```bash
ADMIN_PASS=$(kubectl get secret ch-admin-bootstrap -n ch-system \
  -o jsonpath='{.data.ADMIN_PASSWORD}' | base64 -d)
```

Insert the admin user with a bcrypt-hashed password:

```bash
kubectl exec -i chapterhouse-db-1 -n ch-system -- psql -U postgres -d memories -c "
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
  chapterhouse https://chapterhouse.switchcraft.pd.internal/mcp/stateless \
  --header "Authorization: Bearer ch_k1_YOUR_KEY"
```

Use the `/mcp/stateless` endpoint — it authenticates per-request and survives server restarts. The session-based `/mcp` endpoint loses state on restart.

Flags before the name, URL as the second positional argument, `--header` after.

### 3. Verify

Start a new Claude Code session. The Chapterhouse tools should appear: `remember`, `recall`, `forget`, `list_memories`, `share_memory`, `export_memories`.

---

## Verification

### Health and Readiness

```bash
curl -sk https://chapterhouse.switchcraft.pd.internal/health
# {"status":"ok","timestamp":"..."}

curl -sk https://chapterhouse.switchcraft.pd.internal/ready
# {"status":"ok","checks":{"database":"healthy"}}
```

### Admin Login

```bash
curl -sk -X POST https://chapterhouse.switchcraft.pd.internal/api/v1/admin/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"YOUR_PASSWORD"}' \
  -c /tmp/ch-cookies.txt
```

### MCP Tools

```bash
curl -sk -X POST https://chapterhouse.switchcraft.pd.internal/mcp/stateless \
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
kubectl exec chapterhouse-db-1 -n ch-system -- \
  pg_dump -U postgres memories > backup-$(date +%Y%m%d-%H%M%S).sql
```

---

## Troubleshooting

### Session Cookie Not Persisting After Login

**Symptom**: Login returns 200 but subsequent requests return 401. The page flashes and resets.

**Cause**: The `Secure` flag on session cookies requires a trusted TLS context. Self-signed certificates don't qualify — browsers silently discard the cookie.

**Fix**: Set `ENVIRONMENT` to anything other than `production` (e.g., `homelab`, `staging`, `development`). Only `production` enforces secure cookies.

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

**Fix**: Set `database.existingSecret` in your values file to match the bootstrap secret name (e.g., `chapterhouse-db-credentials`), not `chapterhouse-db-app`.

### Partial Index Migration Error

**Symptom**: `ERROR: functions in index predicate must be marked IMMUTABLE` when running migration 002.

**Cause**: This was a bug in an earlier version of migration 002 that used `NOW()` in partial index predicates.

**Fix**: Ensure you're using the current migration files where indexes use `WHERE revoked_at IS NULL` without time predicates.

### Helm Release Stuck in pending-upgrade

**Symptom**: `helm upgrade` fails with `another operation (install/upgrade/rollback) is in progress`.

**Cause**: A previous deploy was interrupted (e.g., CI timeout, network issue), leaving a Helm release in `pending-upgrade` or `pending-install` state.

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
kubectl logs -l cnpg.io/cluster=chapterhouse-db -n ch-system --tail=50
```

---

## Corporate Air-Gapped Environment

This section covers deployment to the corporate air-gapped Kubernetes cluster (Rancher on Oxide compute).

### CA Bundle Requirements

The corporate network uses a MITM TLS proxy. All outbound HTTPS traffic (including `go mod download`, Docker pulls, and Helm operations) requires the corporate CA bundle.

The `ca-bundle.pem` file at the repository root contains the required certificates (ATL-Palo, LAS-Palo). This file is:
- Copied into Docker build stages before any network operations
- Injected in every GitLab CI stage via `before_script`
- Required for `docker build` to succeed in the air-gapped environment

**Updating the CA bundle**: If certificates rotate, replace `ca-bundle.pem` at the repo root. The Dockerfiles and CI pipeline reference it by path.

### Zot OCI Registry

Images and Helm charts are stored in the Zot OCI registry:

| Artifact | Full Path |
|----------|-----------|
| ch-server image | `registry.switchcraft.pd.internal/chapterhouse/ch-server` |
| ch-web image | `registry.switchcraft.pd.internal/chapterhouse/ch-web` |
| ch-server chart | `oci://registry.switchcraft.pd.internal/charts/ch-server` |
| ch-web chart | `oci://registry.switchcraft.pd.internal/charts/ch-web` |

Zot allows anonymous read — no imagePullSecret is needed for pulling images. Push access requires credentials (`ZOT_USER` / `ZOT_PASSWORD`).

#### Manual Push

```bash
docker login registry.switchcraft.pd.internal
make server   # builds + pushes ch-server
make web      # builds + pushes ch-web
```

### Istio VirtualService Routing

When Istio is enabled (`virtualService.enabled: true` in ch-server values), traffic is routed by path:

| Path | Destination |
|------|-------------|
| `/api/*` | ch-server:8080 |
| `/mcp`, `/mcp/*` | ch-server:8080 |
| `/health`, `/ready`, `/metrics` | ch-server:8080 |
| `/*` (everything else) | ch-web:80 |

Configure the hostname and gateway in `values.yaml` or via `--set`:

```yaml
virtualService:
  enabled: true
  gateway: istio-ingress/switch-wildcard-ingress
  host: chapterhouse.switchcraft.pd.internal
```

### GitLab CI Pipeline

The `.gitlab-ci.yml` pipeline has four stages: `test` → `build` → `publish` → `deploy`.

#### Required CI Variables

| Variable | Description |
|----------|-------------|
| `ZOT_USER` | Zot registry username |
| `ZOT_PASSWORD` | Zot registry password |
| `KUBE_CONFIG_OVAS` | Base64-encoded kubeconfig for ovas-ai-prod |

Generate the kubeconfig variable:

```bash
base64 -i ~/.kube/ovas-ai-prod.yaml | tr -d '\n'
```

Add all three as CI/CD variables under **Settings > CI/CD > Variables** (masked, protected).

#### Runner Tags

Build jobs use the `docker` and `amd64` runner tags. The runner must have Docker-in-Docker capability and network access to `registry.switchcraft.pd.internal`.

### Storage Class

The corporate cluster uses `ceph-rbd` for persistent storage. This is set in the deploy examples:
- `deploy/examples/postgres-cnpg.yaml` — CNPG cluster storage
For homelab/K3s deployments, override with `local-path` or your cluster's default StorageClass.

### Version Management

The `VERSION` file at the repo root is the single source of truth. It is consumed by:
- `Makefile` — image tags and chart packaging
- GitLab CI — image tags (falls back to commit SHA for non-tag builds)
- `Chart.yaml` files — `version` and `appVersion` (stamped by `make release`)

Release workflow:

```bash
make release-dry-run VERSION=0.3.0   # preview changes
make release VERSION=0.3.0           # stamp, commit, tag, push
```

The CI pipeline builds and deploys automatically from tags.
