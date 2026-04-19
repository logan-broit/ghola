# Build and Release Guide

How to build, package, and deploy Chapterhouse.

## Table of Contents

1. [Repository Structure](#repository-structure)
2. [Prerequisites](#prerequisites)
3. [Local Development](#local-development)
4. [Building Container Images](#building-container-images)
5. [Helm Charts](#helm-charts)
6. [Deploying](#deploying)
7. [Makefile Reference](#makefile-reference)

---

## Repository Structure

```
chapterhouse/
├── Makefile                    # Build, push, deploy targets
├── ch-server/                  # Go API + MCP server
│   ├── cmd/api/                # Server entrypoint
│   ├── cmd/init/               # Init container (pg_recall extension verification)
│   ├── internal/               # Auth, handlers, MCP, config, embedding, mneme
│   ├── db/migrations/          # SQL schema migrations (8 files)
│   ├── charts/ch-server/       # Helm chart
│   └── Dockerfile
├── ch-web/                     # Admin console (vanilla JS + Go server)
│   ├── cmd/server/
│   │   ├── main.go             # Go HTTP server (embed.FS + API proxy)
│   │   └── static/             # HTML, CSS, JS (zero build tools)
│   ├── charts/ch-web/          # Helm chart
│   └── Dockerfile
├── deploy/
│   ├── examples/               # CNPG manifests
│   └── homelab/                # Homelab deployment config
│       ├── deploy.sh           # Build, push, deploy script
│       ├── ch-server-values.yaml
│       ├── ch-web-values.yaml
│       └── infra/              # PostgreSQL, TEI, ingress manifests
├── README.md
├── RUNBOOK.md                  # Deployment operations guide
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
| podman or docker | latest | Container image building |
| Go | 1.24+ | Building from source |

### Registry Access

Images are stored at `ghcr.io/thinkwright/chapterhouse`. Log in before pushing:

```bash
echo $GITHUB_TOKEN | podman login ghcr.io -u USERNAME --password-stdin
```

---

## Local Development

### ch-server

```bash
cd ch-server
go build -o bin/ch-server ./cmd/api

# Run locally (requires PostgreSQL with pg_recall and an embedding provider)
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

For local builds, use `podman` (or `docker`) with `--platform linux/amd64`:

### Using Make Targets

```bash
make server   # Build + push ch-server
make web      # Build + push ch-web
make images   # Build + push both
```

### Using the Deploy Script

```bash
./deploy/homelab/deploy.sh --tag latest
```

This builds both images, pushes to GHCR, and deploys via Helm.

### Build Architecture

Both Dockerfiles use multi-stage builds:

**ch-server:** `golang:1.24-alpine` (builder) → `alpine:3.21` (runtime, non-root uid 1000)
**ch-web:** `golang:1.24-alpine` (builder) → `gcr.io/distroless/static-debian12:nonroot-amd64` (runtime)

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

Both charts default to `ghcr.io/thinkwright/chapterhouse`:

```yaml
image:
  registry: ghcr.io
  repository: thinkwright/chapterhouse/ch-server  # or ch-web
  tag: ""  # defaults to Chart appVersion
```

---

## Deploying

### Homelab (K3s)

Use the homelab values overrides:

```bash
helm upgrade --install ch-server ch-server/charts/ch-server \
  --namespace ch-system \
  -f deploy/homelab/ch-server-values.yaml

helm upgrade --install ch-web ch-web/charts/ch-web \
  --namespace ch-system \
  -f deploy/homelab/ch-web-values.yaml
```

Or use the deploy script which handles build + push + deploy:

```bash
./deploy/homelab/deploy.sh --tag latest
```

See [RUNBOOK.md](RUNBOOK.md) for full deployment operations guide.

---

## Makefile Reference

| Target | Description |
|--------|-------------|
| `make help` | Show all targets |
| `make test` | Run Go tests (`ch-server`) |
| `make lint` | Run Go vet (`ch-server`) |
| `make server` | Build + push ch-server image |
| `make web` | Build + push ch-web image |
| `make images` | Build + push both images |
| `make charts` | Package both Helm charts to `dist/` |
| `make deploy-server` | Deploy ch-server via Helm |
| `make deploy-web` | Deploy ch-web via Helm |
| `make clean` | Remove local built images |

Environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `REGISTRY` | `ghcr.io/thinkwright/chapterhouse` | Container registry prefix |
| `NAMESPACE` | `ch-system` | Kubernetes namespace |
