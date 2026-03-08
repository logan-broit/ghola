# Homelab Conversion Plan

## Context

Chapterhouse is a memory and context management system for AI coding agents, deployed as an MCP server over streamable HTTP. It was originally built for an air-gapped corporate environment (MITM proxy, private Nexus/Zot registries, Rancher-managed K8s, Istio ingress, ceph-rbd storage).

This plan converts the `final-refactor` branch — which has the latest application code — to run in a local Homelab environment. This branch will become the new `main` and canonical branch. The old corporate codebase is being retired.

### Source Branch

`final-refactor` — contains all latest Go code including:
- 9 MCP tools (remember, recall, forget, list_memories, share_memory, export_memories, list_sessions, session_summary, session_context)
- Near-duplicate detection (0.92 cosine similarity threshold)
- Recall count tracking
- Client-provided session_id support
- 8 database migrations (through session_id)
- Filtered search, RRF hybrid search

### Target Environment

| Property | Value |
|----------|-------|
| Cluster | K3s on AMD64 (single node) |
| Kubeconfig | `~/.kube/sandbox` |
| Namespace | `ch-system` |
| Ingress | HAProxy ingress controller |
| Storage | `local-path` (K3s default) |
| GitOps | Flux (manages core cluster services) |
| Registry | `ghcr.io/thinkwright/chapterhouse` |
| Container runtime | Podman (macOS dev) / containerd (K3s) |
| PostgreSQL | CNPG operator (already installed) |
| Vector DB | Qdrant v1.16.0 |
| Embeddings | Together.ai (`BAAI/bge-base-en-v1.5`, 768 dims) |

### Prior State

A previous homelab deployment exists in the K3s cluster from the `homelab` branch. It will be **completely torn down** before redeploying. This includes:
- ch-server and ch-web Helm releases
- CNPG PostgreSQL cluster
- Qdrant deployment and PVC
- Any secrets in ch-system namespace

---

## What Gets Removed

These files exist only for the corporate air-gapped environment and have no purpose in the homelab:

| File | Reason |
|------|--------|
| `ca-bundle.pem` (root + ch-server/ + ch-web/) | MITM proxy CA certificates |
| `.gitlab-ci.yml` | GitLab CI/CD pipeline (no GitLab in homelab) |
| `VERSION` | CI-driven versioning (replaced by git tags) |
| `SECURITY_STATEMENT.md` | Corporate compliance document |
| `ch-server/charts/ch-server/templates/servicemonitor.yaml` | Prometheus ServiceMonitor (no Prometheus stack) |
| `ch-server/charts/ch-server/templates/NOTES.txt` | Helm post-install notes (corporate-specific) |
| `ch-web/charts/ch-web/templates/NOTES.txt` | Helm post-install notes (corporate-specific) |

---

## What Gets Modified

### Dockerfiles (both)

Strip all ca-bundle.pem injection. The build stage just needs `apk add --no-cache git ca-certificates` without the MITM workaround. ch-web Dockerfile stays on distroless runtime. ch-server stays on alpine runtime (needs tzdata).

### Makefile

- Registry: `ghcr.io/thinkwright/chapterhouse`
- Auto-detect `podman` or `docker`
- Kubeconfig: `~/.kube/sandbox`
- Drop VERSION file integration, release target, buildx complexity
- Simple `build-server`, `build-web`, `push`, `deploy` targets

### .gitignore

Add: `ca-bundle.pem`, `dist/`, `.mcp.json`, `.env*`

### Helm Chart Defaults — ch-server values.yaml

- `image.registry`: `registry.example.com` (generic default)
- `database.host`: `memory-db-rw.ch-system.svc`
- `qdrant.host`: `qdrant.ch-system.svc`
- `cors.origins`: `"*"` (permissive default for single-user homelab)
- Drop `DATABASE_SSL_MODE` from configmap template (not needed without TLS to DB)
- Keep VirtualService template (gated, disabled by default) for portability
- Remove ServiceMonitor template

### Helm Chart Defaults — ch-web values.yaml

- `image.registry`: `registry.example.com` (generic default)
- `image.pullPolicy`: `IfNotPresent`
- Drop pod security context fields not applicable to distroless

### Chart.yaml (both)

Reset to `version: 0.5.2` / `appVersion: "0.5.2"` matching the current release.

### deploy/examples/

- `postgres-cnpg.yaml`: Change `storageClass: ceph-rbd` to `local-path`
- `qdrant.yaml`: Change `storageClassName: ceph-rbd` to `local-path`

---

## What Gets Added

### deploy/homelab/ directory

Environment-specific deployment artifacts for the K3s homelab:

| File | Purpose |
|------|---------|
| `deploy/homelab/deploy.sh` | Build, push, deploy script (podman + helm) |
| `deploy/homelab/ch-server-values.yaml` | Helm value overrides (ghcr.io, Together.ai, reduced resources) |
| `deploy/homelab/ch-web-values.yaml` | Helm value overrides (ghcr.io, reduced resources) |
| `deploy/homelab/infra/postgres-cluster.yaml` | CNPG cluster (local-path, homelab-sized) |
| `deploy/homelab/infra/qdrant.yaml` | Qdrant deployment + PVC + Service (local-path) |
| `deploy/homelab/infra/ingress.yaml` | HAProxy Ingress routing ch-server + ch-web |

---

## Execution Order

### Phase 1: Clean up corporate artifacts
1. Delete `ca-bundle.pem` (root, ch-server/, ch-web/)
2. Delete `.gitlab-ci.yml`, `VERSION`, `SECURITY_STATEMENT.md`
3. Delete `servicemonitor.yaml`, `NOTES.txt` templates
4. Update `.gitignore`

### Phase 2: Update build system
5. Rewrite ch-server/Dockerfile (strip CA injection)
6. Rewrite ch-web/Dockerfile (strip CA injection)
7. Rewrite Makefile (ghcr.io, podman/docker detect)

### Phase 3: Update Helm charts
8. Update ch-server values.yaml (registry, service names, CORS)
9. Update ch-web values.yaml (registry, pullPolicy)
10. Update both Chart.yaml files
11. Remove DATABASE_SSL_MODE from configmap template

### Phase 4: Add homelab deployment config
12. Create deploy/homelab/ directory structure
13. Write deploy.sh script
14. Write ch-server-values.yaml and ch-web-values.yaml overrides
15. Write infra/ manifests (postgres, qdrant, ingress)
16. Update deploy/examples/ storage classes

### Phase 5: Clean up old cluster and deploy
17. Tear down existing ch-system resources in K3s
18. Deploy infrastructure (CNPG, Qdrant)
19. Create secrets (admin bootstrap, embedding API key, ghcr pull secret, DB credentials)
20. Run database migrations
21. Build and push images
22. Helm install ch-server and ch-web
23. Verify endpoints

---

## Verification Checklist

- [ ] `podman build` succeeds for both images (no ca-bundle references)
- [ ] `helm lint` passes for both charts
- [ ] `helm template` renders correct image refs, configmap, secrets
- [ ] Images push to ghcr.io successfully
- [ ] CNPG cluster comes up healthy on local-path storage
- [ ] Qdrant pod running with local-path PVC
- [ ] ch-server /health and /ready return OK
- [ ] MCP stateless endpoint responds (401 without key, 200 with key)
- [ ] ch-web admin console loads through HAProxy ingress
- [ ] Remember/recall cycle works end-to-end through MCP

---

## Notes

- **No Go code changes.** This is purely build, config, and deployment work. The application code on `final-refactor` is the canonical version.
- **Flux manages core services** in this cluster but Chapterhouse will be deployed manually via Helm for now. It can be added to Flux later.
- **The homelab branch** (origin/homelab) was a prior conversion of an older codebase. Its infrastructure patterns were referenced but the Go code from that branch is not used.
