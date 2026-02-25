# Chapterhouse Air-Gap Conversion

## Context

Chapterhouse currently targets GitHub (ghcr.io) and public internet for builds and deployment. It needs to be converted for the corporate air-gapped environment: Rancher Kubernetes on Oxide compute, Nexus artifact registry at `ats-dev.nexus.switchnet.nv`, and corporate MITM SSL proxy. The runcell project serves as the reference implementation for all patterns. The switch-endura conversion recipe defines the work items.

This is purely build and deployment work — no application code changes.

## Files Modified / Created

### New Files
| File | Purpose |
|------|---------|
| `VERSION` | Single source of truth for version (`0.1.0`) |
| `ca-bundle.pem` | Corporate CA certificates (copied from runcell) |
| `.gitlab-ci.yml` | CI/CD pipeline (test → build → publish → deploy) |
| `ch-server/charts/ch-server/templates/servicemonitor.yaml` | Prometheus scraping for ch-server metrics |
| `ch-server/charts/ch-server/templates/NOTES.txt` | Post-install instructions |
| `ch-web/charts/ch-web/templates/NOTES.txt` | Post-install instructions |
| `ch-server/charts/ch-server/values-homelab.yaml` | Local K3s overrides |
| `ch-web/charts/ch-web/values-homelab.yaml` | Local K3s overrides |

### Modified Files
| File | Changes |
|------|---------|
| `Makefile` | Nexus registry, VERSION integration, release target, ch-web push |
| `ch-server/Dockerfile` | Mandatory CA bundle injection (replace optional `ARG CUSTOM_CA_BUNDLE`) |
| `ch-web/Dockerfile` | CA bundle injection + bump to golang:1.24-alpine |
| `ch-server/charts/ch-server/values.yaml` | Nexus registry defaults, imagePullSecrets |
| `ch-server/charts/ch-server/Chart.yaml` | appVersion from VERSION |
| `ch-server/charts/ch-server/templates/deployment.yaml` | Security context hardening |
| `ch-server/charts/ch-server/templates/virtualservice.yaml` | Route updates for ch-web paths |
| `ch-web/charts/ch-web/values.yaml` | Nexus registry defaults, imagePullSecrets, image helper |
| `ch-web/charts/ch-web/Chart.yaml` | appVersion from VERSION |
| `ch-web/charts/ch-web/templates/_helpers.tpl` | Add image helper (matching ch-server pattern) |
| `ch-web/charts/ch-web/templates/deployment.yaml` | Security context, use image helper |
| `deploy/examples/memory-db.yaml` | Add `storageClassName: ceph-rbd` |
| `deploy/examples/qdrant.yaml` | Add `storageClassName: ceph-rbd` |
| `RUNBOOK.md` | Corporate environment sections |

---

## Work Items

### 1. VERSION File + CA Bundle
Create `VERSION` containing `0.1.0`. Copy `ca-bundle.pem` from `../runcell/ca-bundle.pem` (ATL-Palo + LAS-Palo corporate CA certs).

### 2. Dockerfile — ch-server (`ch-server/Dockerfile`)
**Current state:** Has optional `ARG CUSTOM_CA_BUNDLE` pattern with conditional copy.
**Change:** Replace with mandatory `COPY ca-bundle.pem` before any network operations, matching runcell pattern. Remove `ARG CUSTOM_CA_BUNDLE` and conditional logic. Keep existing multi-stage build, `CGO_ENABLED=0`, alpine runtime, non-root user.

**Critical: CA bundle must be injected before `apk add` in EVERY stage.** The MITM proxy intercepts Alpine package mirror TLS, so even `apk add ca-certificates` will fail without the bundle already appended to the trust store. The pattern is:

```dockerfile
COPY ca-bundle.pem /usr/local/share/ca-certificates/switch-ca.crt
RUN cat /usr/local/share/ca-certificates/switch-ca.crt >> /etc/ssl/certs/ca-certificates.crt \
    && apk add --no-cache git ca-certificates \
    && update-ca-certificates
```

This applies to both the builder stage (golang-alpine) and the runtime stage (alpine). Distroless images (ch-web runtime) don't use apk and are unaffected.

**Build context:** `ca-bundle.pem` must be present in each component's build context directory (`ch-server/`, `ch-web/`), not just the repo root. The Makefile `server` and `web` targets pass the component directory as context.

### 3. Dockerfile — ch-web (`ch-web/Dockerfile`)
**Current state:** No CA bundle handling. Uses `golang:1.22-alpine`.
**Change:** Bump to `golang:1.24-alpine`. Add CA bundle injection before `apk add` in builder stage (same `cat >> /etc/ssl/certs` pattern). Keep distroless runtime (no apk needed there).

### 4. Makefile Overhaul (`Makefile`)
**Current state:** `REGISTRY = ghcr.io/thinkwright/chapterhouse`, no VERSION integration, missing ch-web push target.

**Changes:**
- `REGISTRY ?= ats-dev.nexus.switchnet.nv/chapterhouse`
- `VERSION = $(shell cat VERSION)`
- `make server` — build + push ch-server image
- `make web` — build + push ch-web image
- `make release VERSION=X.Y.Z` — stamp version in VERSION + all Chart.yaml files, commit, tag
- `make images` — build + push both
- `make charts` — package both Helm charts
- Explicit `--platform linux/amd64` for buildx
- Reference: `../runcell/Makefile`

### 5. Helm — ch-web `_helpers.tpl` Enhancement
**Current state:** Has name, fullname, labels, selectorLabels, serviceAccountName helpers. Missing image helper.
**Change:** Add `ch-web.image` helper matching the existing ch-server pattern:
```
{{- define "ch-web.image" -}}
{{- $tag := default .Chart.AppVersion .Values.image.tag -}}
{{- printf "%s/%s:%s" .Values.image.registry .Values.image.repository $tag }}
{{- end }}
```

### 6. Helm — Values Files (both charts)
**ch-server `values.yaml`:**
- Change `registry: registry.example.com` → `registry: ats-dev.nexus.switchnet.nv`
- Change `repository: chapterhouse/ch-server` (keep as-is, already correct pattern)
- Add `imagePullSecrets: [{name: nexus-registry}]`

**ch-web `values.yaml`:**
- Add structured `image:` block with `registry`, `repository`, `tag` (matching ch-server pattern)
- Add `imagePullSecrets: [{name: nexus-registry}]`

**New `values-homelab.yaml` for each chart:**
- Local registry overrides, reduced resources, no Istio, no imagePullSecrets

### 7. Helm — Security Context (ch-web deployment)
**Current state:** ch-server already has security context. ch-web deployment is missing it.
**Change:** Add to ch-web `deployment.yaml`:
```yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 65534
  allowPrivilegeEscalation: false
  capabilities:
    drop: [ALL]
```

### 8. Helm — VirtualService Update
**Current state:** ch-server has a `virtualservice.yaml` template (disabled by default).
**Change:** Update route configuration to include ch-web routing:
- `/api/*`, `/health`, `/metrics` → ch-server:8080
- `/*` → ch-web:8080

### 9. Helm — ServiceMonitor (`ch-server/charts/ch-server/templates/servicemonitor.yaml`)
New template. Prometheus ServiceMonitor targeting `/metrics` on port 8080. Gated by `serviceMonitor.enabled` in values.yaml (default: true).

### 10. Helm — NOTES.txt
Post-install connection instructions for each chart, using template helpers for service names and ports.

### 11. Helm — Storage Class
Add `storageClassName: ceph-rbd` to:
- `deploy/examples/memory-db.yaml` (CNPG cluster storage)
- `deploy/examples/qdrant.yaml` (PVC)

### 12. GitLab CI Pipeline (`.gitlab-ci.yml`)
**Stages:** `test` → `build` → `publish` → `deploy`

**Key patterns from runcell:**
- CA bundle injection in every stage's `before_script`
- Docker-in-Docker with `--insecure-registry=ats-dev.nexus.switchnet.nv`
- Version from tag (`$CI_COMMIT_TAG`) or commit SHA (`$CI_COMMIT_SHORT_SHA`)
- `test` stage: `go vet`, `go test`
- `build` stage: Build both images with buildx `--platform linux/amd64`
- `publish` stage: Push images to Nexus, package + push Helm charts
- `deploy` stage: `helm upgrade --install` with kubeconfig from CI variable
- Manual deploy gate for production

### 13. RUNBOOK Updates
Add corporate-specific sections:
- CA bundle requirements and maintenance
- Nexus registry coordinates (`ats-dev.nexus.switchnet.nv/chapterhouse`)
- Istio VirtualService routing configuration
- GitLab CI pipeline usage and variables
- `ceph-rbd` storage class requirements
- `nexus-registry` image pull secret setup

---

## Execution Order

| Step | Items | Dependencies |
|------|-------|-------------|
| 1 | VERSION file, ca-bundle.pem | — |
| 2 | Both Dockerfiles (CA injection) | ca-bundle.pem |
| 3 | Makefile overhaul | VERSION |
| 4 | Helm _helpers.tpl, values, security contexts, storage class | — |
| 5 | Helm VirtualService, ServiceMonitor, NOTES.txt, env values | — |
| 6 | GitLab CI pipeline | Makefile, VERSION, Dockerfiles |
| 7 | RUNBOOK updates | All above |

Steps 1-5 are largely parallelizable. Step 6 depends on 1-3. Step 7 is last.

## Verification

1. **Dockerfiles:** `docker build --platform linux/amd64 -f ch-server/Dockerfile .` and `docker build --platform linux/amd64 -f ch-web/Dockerfile .` — both must succeed with CA bundle in place
2. **Makefile:** `make server`, `make web`, `make charts` — verify correct registry/tags
3. **Helm lint:** `helm lint ch-server/charts/ch-server` and `helm lint ch-web/charts/ch-web`
4. **Helm template:** `helm template test ch-server/charts/ch-server` — verify security contexts, imagePullSecrets, image coordinates, ServiceMonitor, VirtualService
5. **GitLab CI:** Validate YAML syntax with `gitlab-ci-lint` or manual review
6. **Version flow:** `make release VERSION=0.1.0` — verify VERSION file, Chart.yaml appVersion, git tag all updated

---

## Deployment Status (ovas-ai-prod)

Deployed 2026-02-24 to the `ovas-ai-prod` cluster.

### Cluster Details

| Property | Value |
|----------|-------|
| Kubeconfig | `~/.kube/ovas-ai-prod.yaml` |
| Namespace | `ch-system` |
| Hostname | `chapterhouse.switchcraft.pd.internal` |
| Gateway | `istio-ingress/switch-wildcard-ingress` |
| StorageClass | `ceph-rbd` (default) |
| Image version | `0.1.0` |

### Components

| Component | Image | Status |
|-----------|-------|--------|
| ch-server | `ats-dev.nexus.switchnet.nv/chapterhouse/ch-server:0.1.0` | Running |
| ch-web | `ats-dev.nexus.switchnet.nv/chapterhouse/ch-web:0.1.0` | Running |
| PostgreSQL (CNPG) | `memory-db` cluster, 1 instance | Healthy |
| Qdrant | Single instance, `ceph-rbd` PVC | Running |

### Verified Endpoints

| Endpoint | Result |
|----------|--------|
| `/health` | `{"status":"ok"}` |
| `/ready` | `{"status":"ok","checks":{"database":"healthy","qdrant":"healthy"}}` |
| `/` (landing page) | Serving HTML |
| `/admin/login` | Serving HTML |
| `/mcp/stateless` | Auth enforced (401 without key) |
| Admin login | Session cookie working |

### Deployment Commands Used

```bash
export KUBECONFIG=~/.kube/ovas-ai-prod.yaml

# Namespace + pull secret
kubectl create namespace ch-system
# nexus-registry secret copied from runcell-system

# Infrastructure
kubectl apply -f deploy/examples/postgres-cnpg.yaml
kubectl apply -f deploy/examples/qdrant.yaml

# Secrets
kubectl create secret generic ch-admin-bootstrap -n ch-system \
  --from-literal=ADMIN_USERNAME=admin \
  --from-literal=ADMIN_PASSWORD="$(openssl rand -base64 16)"

# Migrations (all 6 files + privilege grants)

# Build + push images
docker login ats-dev.nexus.switchnet.nv
make server
make web

# Helm deploys
helm upgrade --install ch-server ch-server/charts/ch-server \
  --namespace ch-system \
  --set image.tag=0.1.0 \
  --set virtualService.enabled=true \
  --set virtualService.gateway=istio-ingress/switch-wildcard-ingress \
  --set virtualService.host=chapterhouse.switchcraft.pd.internal

helm upgrade --install ch-web ch-web/charts/ch-web \
  --namespace ch-system \
  --set image.tag=0.1.0
```

### Lessons Learned

1. **CA bundle before `apk add`**: The MITM proxy intercepts Alpine mirror TLS. `cat ca-bundle.pem >> /etc/ssl/certs/ca-certificates.crt` must happen before any `apk add`, including `apk add ca-certificates` itself. This affects every Dockerfile stage that uses Alpine.

2. **Build context copies**: `ca-bundle.pem` must exist in each component's build context directory (`ch-server/`, `ch-web/`), not just the repo root. The Makefile passes component directories as Docker build context.

3. **CNPG auto-creates `-app` secret**: CNPG creates `memory-db-app` secret automatically from the bootstrap credentials. The ch-server chart references this via `database.existingSecret: memory-db-app`.

4. **Istio gateway discovery**: The wildcard gateway at `istio-ingress/switch-wildcard-ingress` accepts `*.switchcraft.pd.internal` and `*.aidt.pd.internal`. This was discovered by inspecting the existing runcell deployment.

---

*Generated with [Claude Code](https://claude.com/claude-code) — 2026-02-24*
