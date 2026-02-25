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

---

## Prerequisites

### Required Tools

| Tool | Version | Purpose |
|------|---------|---------|
| kubectl | 1.28+ | Kubernetes CLI |
| helm | 3.12+ | Helm chart deployment |
| podman | 4+ | Container image building |
| Go | 1.24+ | Building from source |

### Cluster Requirements

- Kubernetes cluster (K3s, K8s, etc.)
- [CloudNativePG](https://cloudnative-pg.io/) operator installed
- StorageClass available (e.g., `local-path` for K3s)
- Ingress controller (e.g., HAProxy, nginx, Traefik)

### Container Registry

Images must be published to a container registry accessible by the cluster. The default open-source path is `ghcr.io`, but any OCI-compliant registry works.

---

## Infrastructure Setup

### 1. Create Namespace

```bash
kubectl create namespace ch-system
```

### 2. Create Secrets

**Database bootstrap** (for CNPG):

```bash
kubectl create secret generic memory-db-credentials \
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

**Container registry pull secret** (if using a private registry):

```bash
kubectl create secret docker-registry ghcr-pull-secret \
  -n ch-system \
  --docker-server=ghcr.io \
  --docker-username=YOUR_USERNAME \
  --docker-password=YOUR_TOKEN
```

**Embedding API key** (if using a hosted provider like Together.ai):

```bash
kubectl create secret generic together-api-key \
  -n ch-system \
  --from-literal=TOGETHER_API_KEY=your-api-key
```

### 3. Deploy PostgreSQL (CNPG)

Create a CNPG Cluster manifest. See `deploy/homelab/infra/postgres-cluster.yaml` for a reference.

Key points:
- Set `storageClass` to match your cluster (e.g., `local-path` for K3s)
- Bootstrap references the `memory-db-credentials` secret
- Database name: `memories`, owner: `memory_api`

```bash
kubectl apply -f deploy/homelab/infra/postgres-cluster.yaml
kubectl wait --for=condition=Ready \
  clusters.postgresql.cnpg.io/memory-db \
  -n ch-system --timeout=300s
```

### 4. Deploy Qdrant

See `deploy/homelab/infra/qdrant.yaml` for a reference.

Key points:
- Use `strategy: Recreate` for single-replica deployments with RWO PVCs
- Set `storageClassName` to match your cluster

```bash
kubectl apply -f deploy/homelab/infra/qdrant.yaml
kubectl rollout status deployment/qdrant -n ch-system --timeout=120s
```

---

## Database Migrations

Migrations must be applied manually after the CNPG cluster is ready. Run them as the `postgres` superuser, then grant privileges to the application user.

### Apply All Migrations

```bash
# Run each migration in order
for f in 001_initial_schema.sql 002_admin_auth.sql 003_add_memory_type.sql 004_add_scope_and_org.sql; do
  echo "--- Applying $f ---"
  kubectl exec -i memory-db-1 -n ch-system -- \
    psql -U postgres -d memories < ch-server/db/migrations/$f
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
"
```

### Verify

```bash
kubectl exec memory-db-1 -n ch-system -- psql -U memory_api -d memories -c '\dt'
```

Expected tables: `users`, `memory_blocks`, `journal`, `git_commits`, `audit_log`, `api_keys`, `admin_sessions`

---

## Building Container Images

Both components are built with podman. Use `--platform linux/amd64` when building on ARM Macs for x86 clusters.

### ch-server

```bash
podman build --platform linux/amd64 \
  -t your-registry/ch-server:latest \
  ch-server/
```

### ch-web

```bash
podman build --platform linux/amd64 \
  -t your-registry/ch-web:latest \
  ch-web/
```

### Push

```bash
podman login your-registry
podman push your-registry/ch-server:latest
podman push your-registry/ch-web:latest
```

---

## Deploying with Helm

Each component has a Helm chart under its `charts/` directory. Use environment-specific values files for overrides.

### ch-server

```bash
helm upgrade --install ch-server ch-server/charts/ch-server \
  --namespace ch-system \
  -f deploy/your-env/ch-server-values.yaml
```

### ch-web

```bash
helm upgrade --install ch-web ch-web/charts/ch-web \
  --namespace ch-system \
  -f deploy/your-env/ch-web-values.yaml
```

### Values Override Pattern

The generic `values.yaml` in each chart uses `registry.example.com` placeholders. Environment-specific values files override these:

```yaml
# deploy/your-env/ch-server-values.yaml
image:
  registry: ghcr.io
  repository: your-org/chapterhouse/ch-server
  tag: latest
  pullPolicy: Always

imagePullSecrets:
  - name: ghcr-pull-secret

config:
  environment: your-env
  logLevel: info

database:
  host: memory-db-rw.ch-system.svc
  existingSecret: memory-db-credentials

embedding:
  provider: openai
  url: "https://api.together.xyz"
  model: BAAI/bge-base-en-v1.5
  dimensions: 768
  existingSecret: together-api-key
  secretKey: TOGETHER_API_KEY
```

---

## Ingress Configuration

Use `spec.ingressClassName` (not the deprecated `kubernetes.io/ingress.class` annotation).

See `deploy/homelab/infra/ingress.yaml` for a reference with path-based routing:
- `/api/*`, `/mcp/*`, `/health`, `/ready` -> ch-server:8080
- `/*` -> ch-web:80

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

```bash
claude mcp add -s user --transport http \
  chapterhouse https://your-host/mcp/stateless \
  --header "Authorization: Bearer ch_k1_YOUR_KEY"
```

Use the `/mcp/stateless` endpoint — it authenticates per-request and survives server restarts. The session-based `/mcp` endpoint loses state on restart.

Flags before the name, URL as the second positional argument, `--header` after.

Scope options: `-s user` (all projects), `-s local` (current project only), `-s project` (shared via `.mcp.json`).

### 3. Verify

Start a new Claude Code session. The Chapterhouse tools should appear: `remember`, `recall`, `forget`, `list_memories`, `share_memory`, `export_memories`.

---

## Verification

### Health and Readiness

```bash
curl -sk https://your-host/health
# {"status":"ok","timestamp":"..."}

curl -sk https://your-host/ready
# {"status":"ok","checks":{"database":"healthy","qdrant":"healthy"}}
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

**Fix**: Set `ENVIRONMENT` to anything other than `production` (e.g., `homelab`, `staging`, `development`). Only `production` enforces secure cookies.

### Clipboard Copy Fails

**Symptom**: "Failed to copy" toast when clicking copy buttons in the admin console.

**Cause**: `navigator.clipboard.writeText()` requires a secure context. Same root cause as the cookie issue.

**Fix**: The admin console includes a `document.execCommand('copy')` fallback, but ensure you're on a recent deployment that includes this fix.

### PVC Stuck Pending

**Symptom**: Qdrant or PostgreSQL pod stuck in Pending, PVC not binding.

**Cause**: StorageClass mismatch. Check what's available:

```bash
kubectl get storageclass
```

**Fix**: Update the `storageClassName` in your manifests to match an available StorageClass (e.g., `local-path` for K3s).

### Qdrant RWO PVC Deadlock on Update

**Symptom**: New Qdrant pod stuck Pending during rolling update, old pod still running.

**Cause**: `RollingUpdate` strategy tries to start the new pod before terminating the old one, but both need the same RWO PVC.

**Fix**: Use `strategy: Recreate` in the Qdrant Deployment.

### CNPG Secret Naming

**Symptom**: ch-server can't connect to database, looking for a secret that doesn't exist.

**Cause**: When you provide your own bootstrap secret to CNPG, it uses that secret directly instead of creating a separate `-app` secret.

**Fix**: Set `database.existingSecret` in your values file to match the bootstrap secret name (e.g., `memory-db-credentials`), not `memory-db-app`.

### Partial Index Migration Error

**Symptom**: `ERROR: functions in index predicate must be marked IMMUTABLE` when running migration 002.

**Cause**: This was a bug in an earlier version of migration 002 that used `NOW()` in partial index predicates.

**Fix**: Ensure you're using the current migration files where indexes use `WHERE revoked_at IS NULL` without time predicates.

### Pod Logs

```bash
kubectl logs -l app.kubernetes.io/name=ch-server -n ch-system --tail=50
kubectl logs -l app.kubernetes.io/name=ch-web -n ch-system --tail=50
kubectl logs -l cnpg.io/cluster=memory-db -n ch-system --tail=50
kubectl logs -l app.kubernetes.io/name=qdrant -n ch-system --tail=50
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

### Nexus Registry

Images are stored at:

| Image | Full Path |
|-------|-----------|
| ch-server | `ats-dev.nexus.switchnet.nv/chapterhouse/ch-server` |
| ch-web | `ats-dev.nexus.switchnet.nv/chapterhouse/ch-web` |

#### Image Pull Secret

Create the `nexus-registry` pull secret in the target namespace:

```bash
kubectl create secret docker-registry nexus-registry \
  -n ch-system \
  --docker-server=ats-dev.nexus.switchnet.nv \
  --docker-username=YOUR_NEXUS_USER \
  --docker-password=YOUR_NEXUS_PASSWORD
```

Both Helm charts default to `imagePullSecrets: [{name: nexus-registry}]`.

#### Manual Push

```bash
docker login ats-dev.nexus.switchnet.nv
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

Configure the hostname and gateway in `values.yaml`:

```yaml
virtualService:
  enabled: true
  gateway: istio-ingress/default-gateway
  host: chapterhouse.switchnet.nv
```

### GitLab CI Pipeline

The `.gitlab-ci.yml` pipeline has four stages: `test` → `build` → `publish` → `deploy`.

#### Required CI Variables

| Variable | Description |
|----------|-------------|
| `NEXUS_USER` | Nexus registry username |
| `NEXUS_PASSWORD` | Nexus registry password |
| `KUBE_CONFIG` | Base64-encoded kubeconfig for the target cluster |

Generate the kubeconfig variable:

```bash
base64 -i ~/.kube/your-cluster.yaml | tr -d '\n'
```

Add all three as CI/CD variables under **Settings > CI/CD > Variables** (masked, protected).

#### Runner Tag

All jobs use the `chapterhouse` runner tag. Ensure a GitLab runner with this tag is registered and has Docker-in-Docker capability.

### Storage Class

The corporate cluster uses `ceph-rbd` for persistent storage. This is set in the deploy examples:
- `deploy/examples/postgres-cnpg.yaml` — CNPG cluster storage
- `deploy/examples/qdrant.yaml` — Qdrant PVC

For homelab/K3s deployments, override with `local-path` or your cluster's default StorageClass.

### Version Management

The `VERSION` file at the repo root is the single source of truth. It is consumed by:
- `Makefile` — image tags and chart packaging
- GitLab CI — image tags (falls back to commit SHA for non-tag builds)
- `Chart.yaml` files — `version` and `appVersion` (stamped by `make release`)

Release workflow:

```bash
make release-dry-run VERSION=0.2.0   # preview changes
make release VERSION=0.2.0           # stamp, commit, tag, push
```

The CI pipeline builds and deploys automatically from tags.
