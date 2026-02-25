# Build and Release Guide

How to build, package, and release Chapterhouse.

## Table of Contents

1. [Repository Structure](#repository-structure)
2. [Local Development](#local-development)
3. [Building Container Images](#building-container-images)
4. [Helm Charts](#helm-charts)
5. [Deploy Script](#deploy-script)
6. [Branch Strategy](#branch-strategy)
7. [Release Checklist](#release-checklist)

---

## Repository Structure

```
chapterhouse/
├── ch-server/                  # Go API + MCP server
│   ├── cmd/api/                # Server entrypoint
│   ├── internal/               # Auth, handlers, MCP, config, embedding, vector
│   ├── db/migrations/          # SQL schema migrations (4 files)
│   ├── charts/ch-server/       # Helm chart
│   ├── scripts/                # Dev and test scripts
│   └── Dockerfile
├── ch-web/                     # Admin console (vanilla JS + Go server)
│   ├── cmd/server/
│   │   ├── main.go             # Go HTTP server (embed.FS + API proxy)
│   │   └── static/             # HTML, CSS, JS (zero build tools)
│   ├── charts/ch-web/          # Helm chart
│   └── Dockerfile
├── deploy/                     # Environment-specific deployment configs
│   └── homelab/                # Homelab overlay (values + infra manifests)
├── README.md                   # Project overview
├── RUNBOOK.md                  # Deployment operations guide
├── BUILD_AND_RELEASE.md        # This file
└── LICENSE                     # Apache 2.0
```

Each component (ch-server, ch-web) is independently buildable with its own `go.mod`, `Dockerfile`, and Helm chart.

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

Use podman for container builds. When building on ARM Macs for x86 clusters, use `--platform linux/amd64`.

### ch-server

```bash
podman build --platform linux/amd64 \
  -t ghcr.io/thinkwright/chapterhouse/ch-server:latest \
  ch-server/
```

Multi-stage build: `golang:1.24-alpine` -> `alpine:3.21` (non-root, uid 1000).

### ch-web

```bash
podman build --platform linux/amd64 \
  -t ghcr.io/thinkwright/chapterhouse/ch-web:latest \
  ch-web/
```

Multi-stage build: `golang:1.22-alpine` -> `gcr.io/distroless/static-debian12:nonroot-amd64`.

### Pushing

```bash
podman login ghcr.io
podman push ghcr.io/thinkwright/chapterhouse/ch-server:latest
podman push ghcr.io/thinkwright/chapterhouse/ch-web:latest
```

### Tagging Releases

```bash
VERSION=v0.2.0
podman tag ghcr.io/thinkwright/chapterhouse/ch-server:latest \
  ghcr.io/thinkwright/chapterhouse/ch-server:${VERSION}
podman push ghcr.io/thinkwright/chapterhouse/ch-server:${VERSION}
```

---

## Helm Charts

### Linting

```bash
helm lint ch-server/charts/ch-server
helm lint ch-web/charts/ch-web
```

### Packaging

```bash
helm package ch-server/charts/ch-server
helm package ch-web/charts/ch-web
```

### Versioning

Chart versions are in each chart's `Chart.yaml`:

```yaml
# ch-server/charts/ch-server/Chart.yaml
version: 0.1.0      # Chart version — bump on chart changes
appVersion: "0.1.0"  # App version — bump on application changes
```

### Installing

```bash
helm upgrade --install ch-server ch-server/charts/ch-server \
  --namespace ch-system \
  -f deploy/your-env/ch-server-values.yaml

helm upgrade --install ch-web ch-web/charts/ch-web \
  --namespace ch-system \
  -f deploy/your-env/ch-web-values.yaml
```

---

## Deploy Script

The homelab includes a convenience script at `deploy/homelab/deploy.sh`:

```bash
# Full deploy: build, push, helm install
./deploy/homelab/deploy.sh

# Skip build, just redeploy
./deploy/homelab/deploy.sh --no-build --no-push

# Only deploy ch-server
./deploy/homelab/deploy.sh --only ch-server

# Use a specific tag
./deploy/homelab/deploy.sh --tag v0.2.0
```

Flags:

| Flag | Effect |
|------|--------|
| `--no-build` | Skip building images |
| `--no-push` | Skip pushing to registry |
| `--no-deploy` | Skip Helm deploy |
| `--only NAME` | Build/deploy only `ch-server` or `ch-web` |
| `--tag TAG` | Image tag (default: `latest`) |

---

## Branch Strategy

### `main` branch

The open-source, generic branch. All code uses `registry.example.com` and `example.com` as placeholders. No environment-specific values, no secrets, no internal hostnames.

Changes that affect application behavior (bug fixes, features, API changes) go to `main` first.

### Environment branches (e.g., `homelab`)

Extend `main` with environment-specific deployment configs under `deploy/<env>/`:

```
deploy/homelab/
├── deploy.sh               # Build + deploy script
├── ch-server-values.yaml   # Helm value overrides
├── ch-web-values.yaml      # Helm value overrides
└── infra/
    ├── postgres-cluster.yaml
    ├── qdrant.yaml
    └── ingress.yaml
```

Environment branches are periodically rebased onto `main`:

```bash
git checkout homelab
git rebase main
git push --force-with-lease origin homelab
```

### Workflow

1. Make code changes on `main`
2. Commit, push `main`
3. Switch to environment branch, rebase onto `main`
4. Force-push environment branch
5. Build, push, deploy from environment branch

---

## Release Checklist

- [ ] All changes committed to `main`
- [ ] Tests pass: `cd ch-server && go test ./internal/...`
- [ ] Both images build: `podman build` for ch-server and ch-web
- [ ] Helm charts lint: `helm lint` for both charts
- [ ] Update `version` and `appVersion` in both `Chart.yaml` files
- [ ] Tag the release: `git tag v0.X.0`
- [ ] Build and push images with version tag
- [ ] Push tag: `git push origin v0.X.0`
- [ ] Rebase environment branches and deploy
