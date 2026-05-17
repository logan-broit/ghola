# Chapterhouse Build System
# =========================
#
# Configuration via environment variables:
#   REGISTRY    - Container registry (default: ghcr.io/thinkwright/chapterhouse)
#   TAG         - Image tag (default: latest)
#   NAMESPACE   - Kubernetes namespace (default: ch-system)
#   KUBECONFIG  - Path to kubeconfig (default: ~/.kube/sandbox)

REGISTRY   ?= ghcr.io/thinkwright/chapterhouse
TAG        ?= latest
NAMESPACE  ?= ch-system
KUBECONFIG ?= $(HOME)/.kube/sandbox

# Detect container runtime (prefer podman)
CONTAINER_CMD := $(shell command -v podman 2>/dev/null || command -v docker 2>/dev/null)

.PHONY: build build-server build-web push push-server push-web deploy deploy-server deploy-web test lint reindex clean help

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | sort | awk 'BEGIN {FS = ":.*?## "}; {printf "\033[36m%-18s\033[0m %s\n", $$1, $$2}'

# =============================================================================
# Test & Lint
# =============================================================================

test: ## Run Go tests
	cd ch-server && go test ./...

lint: ## Run Go vet
	cd ch-server && go vet ./...

# =============================================================================
# Container Image Targets
# =============================================================================

build: build-server build-web ## Build all container images

build-server: ## Build ch-server image
	$(CONTAINER_CMD) build --platform linux/amd64 \
		-t $(REGISTRY)/ch-server:$(TAG) \
		-f ch-server/Dockerfile ch-server/

build-web: ## Build ch-web image
	$(CONTAINER_CMD) build --platform linux/amd64 \
		-t $(REGISTRY)/ch-web:$(TAG) \
		-f ch-web/Dockerfile ch-web/

push: push-server push-web ## Push all images to registry

push-server: ## Push ch-server image
	$(CONTAINER_CMD) push $(REGISTRY)/ch-server:$(TAG)

push-web: ## Push ch-web image
	$(CONTAINER_CMD) push $(REGISTRY)/ch-web:$(TAG)

clean: ## Remove built images
	$(CONTAINER_CMD) rmi $(REGISTRY)/ch-server:$(TAG) 2>/dev/null || true
	$(CONTAINER_CMD) rmi $(REGISTRY)/ch-web:$(TAG) 2>/dev/null || true

# =============================================================================
# Helm Targets
# =============================================================================

deploy: deploy-server deploy-web ## Deploy all via Helm

deploy-server: ## Deploy ch-server via Helm
	helm upgrade --install ch-server ch-server/charts/ch-server \
		--namespace $(NAMESPACE) --create-namespace \
		-f deploy/homelab/ch-server-values.yaml \
		--set image.tag=$(TAG)

deploy-web: ## Deploy ch-web via Helm
	helm upgrade --install ch-web ch-web/charts/ch-web \
		--namespace $(NAMESPACE) --create-namespace \
		-f deploy/homelab/ch-web-values.yaml \
		--set image.tag=$(TAG)

reindex: ## Run reindex via kubectl
	kubectl -n $(NAMESPACE) exec deploy/ch-server -- /app/ch-reindex

.DEFAULT_GOAL := help
