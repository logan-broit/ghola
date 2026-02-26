# Build and Release Guide

How to build, package, and release Chapterhouse in the corporate air-gapped environment.

## Table of Contents

1. [Repository Structure](#repository-structure)
2. [Prerequisites](#prerequisites)
3. [Local Development](#local-development)
4. [Building Container Images](#building-container-images)
5. [Helm Charts](#helm-charts)
6. [Deploying to ovas-ai-prod](#deploying-to-ovas-ai-prod)
7. [Release Workflow](#release-workflow)
8. [GitLab CI Pipeline](#gitlab-ci-pipeline)
9. [Makefile Reference](#makefile-reference)

---

## Repository Structure

```
chapterhouse/
├── VERSION                     # Single source of truth for version (semver)
├── ca-bundle.pem               # Corporate CA certificates (ATL-Palo, LAS-Palo)
├── Makefile                    # Build, push, release targets
├── .gitlab-ci.yml              # CI/CD pipeline
├── conversion-recipe.md        # Air-gap conversion plan and status
├── ch-server/                  # Go API + MCP server
│   ├── cmd/api/                # Server entrypoint
│   ├── cmd/reindex/            # Reindex tool
│   ├── cmd/init/               # Init container
│   ├── internal/               # Auth, handlers, MCP, config, embedding, vector
│   ├── db/migrations/          # SQL schema migrations (6 files)
│   ├── charts/ch-server/       # Helm chart
│   ├── ca-bundle.pem           # CA bundle (build context copy)
│   ├── scripts/                # Dev and test scripts
│   └── Dockerfile
├── ch-web/                     # Admin console (vanilla JS + Go server)
│   ├── cmd/server/
│   │   ├── main.go             # Go HTTP server (embed.FS + API proxy)
│   │   └── static/             # HTML, CSS, JS (zero build tools)
│   ├── charts/ch-web/          # Helm chart
│   ├── ca-bundle.pem           # CA bundle (build context copy)
│   └── Dockerfile
├── deploy/
│   └── examples/               # CNPG and Qdrant manifests
├── README.md
├── RUNBOOK.md                  # Deployment operations guide
├── SECURITY_STATEMENT.md       # Security posture and design decisions
└── BUILD_AND_RELEASE.md        # This file
```

Each component (ch-server, ch-web) is independently buildable with its own `go.mod`, `Dockerfile`, and Helm chart.

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

### Registry Access

Images and Helm charts are stored in the Zot OCI registry at `registry.switchcraft.pd.internal/chapterhouse`. Zot allows anonymous read; push requires credentials. Log in before building:

```bash
docker login registry.switchcraft.pd.internal
```

### Cluster Access

The production cluster kubeconfig is at `~/.kube/ovas-ai-prod.yaml`:

```bash
export KUBECONFIG=~/.kube/ovas-ai-prod.yaml
```

### CA Bundle

The `ca-bundle.pem` at the repo root contains the corporate MITM proxy certificates. Copies must exist in each component's build context:

- `ch-server/ca-bundle.pem`
- `ch-web/ca-bundle.pem`

These are tracked in git. If certificates rotate, replace the root copy and update the component copies.

---

## Local Development

### ch-server

```bash
cd ch-server
go build -o bin/ch-server ./cmd/api

# Run locally (requires PostgreSQL, Qdrant, and an embedding provider)
DATABASE_PASSWORD=secret ./bin/ch-server

# Or use the dev script (connects to K8s services via NodePort)
./scripts/dev.sh
```

### ch-web

```bash
cd ch-web
go build -o bin/ch-web ./cmd/server

# Run locally (proxies API requests to ch-server)
API_URL=http://localhost:8080 ./bin/ch-web
```

### Running Tests

```bash
cd ch-server
go test ./internal/... -cover
```

### Regenerating SQL Code

After modifying queries in `ch-server/db/queries/`:

```bash
cd ch-server
sqlc generate
```

---

## Building Container Images

**Production builds are driven through GitLab CI** — push to `main` or tag a release and the pipeline handles build, push, and deploy automatically. See [GitLab CI Pipeline](#gitlab-ci-pipeline).

For local builds, use `docker buildx` with `--platform linux/amd64`:

### Using Make Targets

```bash
make server   # Build + push ch-server
make web      # Build + push ch-web
make images   # Build + push both
```

The Makefile uses `docker buildx` with `--push` to build and push in one step.

### Build Architecture

Both Dockerfiles use multi-stage builds with the corporate CA bundle injected before any network operations:

**ch-server:** `golang:1.24-alpine` (builder) → `alpine:3.21` (runtime, non-root uid 1000)
**ch-web:** `golang:1.24-alpine` (builder) → `gcr.io/distroless/static-debian12:nonroot-amd64` (runtime)

**Critical:** The CA bundle must be appended to `/etc/ssl/certs/ca-certificates.crt` before any `apk add` command. The MITM proxy intercepts Alpine package mirror TLS, so even `apk add ca-certificates` fails without the bundle pre-loaded. See `conversion-recipe.md` work item 2 for the full pattern.

---

## Helm Charts

### Linting

```bash
helm lint ch-server/charts/ch-server
helm lint ch-web/charts/ch-web
```

### Packaging

```bash
make charts   # Packages both to dist/
```

### Chart Defaults

Both charts default to the Zot registry with no pull secret (anonymous read):

```yaml
image:
  registry: registry.switchcraft.pd.internal
  repository: chapterhouse/ch-server  # or ch-web
  tag: ""  # defaults to Chart appVersion
imagePullSecrets: []  # Zot allows anonymous read
```

### Version Alignment

The `VERSION` file is the single source of truth. Running `make release` stamps it into both `Chart.yaml` files (`version` and `appVersion`).

---

## Deploying to ovas-ai-prod

### Cluster Details

| Property | Value |
|----------|-------|
| Kubeconfig | `~/.kube/ovas-ai-prod.yaml` |
| Namespace | `ch-system` |
| Hostname | `chapterhouse.switchcraft.pd.internal` |
| Gateway | `istio-ingress/switch-wildcard-ingress` |
| StorageClass | `ceph-rbd` (default) |
| Registry | `registry.switchcraft.pd.internal/chapterhouse` |

### Deploy ch-server

```bash
export KUBECONFIG=~/.kube/ovas-ai-prod.yaml

helm upgrade --install ch-server ch-server/charts/ch-server \
  --namespace ch-system \
  --set image.tag=0.3.0 \
  --set virtualService.enabled=true \
  --set virtualService.gateway=istio-ingress/switch-wildcard-ingress \
  --set virtualService.host=chapterhouse.switchcraft.pd.internal
```

The VirtualService routes:
- `/api/*`, `/mcp*`, `/health`, `/ready`, `/metrics` → ch-server
- Everything else → ch-web

### Deploy ch-web

```bash
helm upgrade --install ch-web ch-web/charts/ch-web \
  --namespace ch-system \
  --set image.tag=0.3.0
```

### Homelab Overrides

For local K3s development, use the homelab values files:

```bash
helm upgrade --install ch-server ch-server/charts/ch-server \
  --namespace ch-system \
  -f ch-server/charts/ch-server/values-homelab.yaml

helm upgrade --install ch-web ch-web/charts/ch-web \
  --namespace ch-system \
  -f ch-web/charts/ch-web/values-homelab.yaml
```

---

## Release Workflow

Releases are driven through **GitLab CI**. The workflow:

1. **Cut the release** locally with `make release` — this stamps version metadata, commits, tags, and pushes
2. **GitLab CI picks up the tag** and runs the full pipeline: test → build → publish → deploy

### Cut a Release

```bash
# Preview what would change
make release-dry-run VERSION=0.4.0

# Execute the release
make release VERSION=0.4.0
```

This updates:
- `VERSION` — single source of truth
- `ch-server/charts/ch-server/Chart.yaml` — `version` and `appVersion`
- `ch-web/charts/ch-web/Chart.yaml` — `version` and `appVersion`

Then commits as `release: vX.Y.Z`, tags as `vX.Y.Z`, and pushes both the commit and tag to origin. GitLab CI handles the rest.

### Safety Checks

`make release` enforces:
- Working tree must be clean (no uncommitted changes)
- Must be on `main` branch
- Local `main` must be in sync with `origin/main`
- Version must be valid semver (X.Y.Z)
- Tag must not already exist

### Manual Deploy (Emergency)

If CI is unavailable, deploy manually:

```bash
docker login registry.switchcraft.pd.internal
make images   # or: make server && make web

export KUBECONFIG=~/.kube/ovas-ai-prod.yaml
helm upgrade --install ch-server ch-server/charts/ch-server \
  --namespace ch-system \
  --set image.tag=0.3.0 \
  --set virtualService.enabled=true \
  --set virtualService.gateway=istio-ingress/switch-wildcard-ingress \
  --set virtualService.host=chapterhouse.switchcraft.pd.internal

helm upgrade --install ch-web ch-web/charts/ch-web \
  --namespace ch-system \
  --set image.tag=0.3.0
```

---

## GitLab CI Pipeline

The `.gitlab-ci.yml` pipeline automates the build-test-publish-deploy cycle.

### Stages

| Stage | Purpose |
|-------|---------|
| `test` | `go vet`, `go test` |
| `build` | Build + push Docker images to Zot via buildx |
| `publish` | Package + push Helm charts to Zot OCI registry |
| `deploy` | `helm upgrade --install` to target cluster (`allow_failure: true`) |

### Required CI Variables

Configure under **Settings > CI/CD > Variables** (masked, protected):

| Variable | Description |
|----------|-------------|
| `ZOT_USER` | Zot registry username |
| `ZOT_PASSWORD` | Zot registry password |
| `KUBE_CONFIG_OVAS` | Base64-encoded kubeconfig for ovas-ai-prod |

Generate the kubeconfig variable:

```bash
base64 -i ~/.kube/ovas-ai-prod.yaml | tr -d '\n'
```

### Runner Requirements

Build jobs use the `docker` and `amd64` runner tags. The runner must have Docker-in-Docker capability and network access to `registry.switchcraft.pd.internal`.

### Version Strategy

- **Tagged commits** (`v*`): Uses the tag as the image version
- **Non-tagged commits**: Uses the short commit SHA
- **Deploy stage**: Automatic with `allow_failure: true` (non-blocking)

---

## Makefile Reference

| Target | Description |
|--------|-------------|
| `make help` | Show all targets and current version |
| `make test` | Run Go tests (`ch-server`) |
| `make lint` | Run Go vet (`ch-server`) |
| `make server` | Build + push ch-server image |
| `make web` | Build + push ch-web image |
| `make images` | Build + push both images |
| `make charts` | Package both Helm charts to `dist/` |
| `make deploy-server` | Deploy ch-server via Helm |
| `make deploy-web` | Deploy ch-web via Helm |
| `make reindex` | Run reindex via kubectl exec |
| `make clean` | Remove local built images |
| `make release VERSION=X.Y.Z` | Stamp, commit, tag, push |
| `make release-dry-run VERSION=X.Y.Z` | Preview release changes |

Environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `REGISTRY` | `registry.switchcraft.pd.internal/chapterhouse` | Container registry prefix |
| `NAMESPACE` | `ch-system` | Kubernetes namespace |

---

*Generated with [Claude Code](https://claude.com/claude-code) — 2026-02-24*
