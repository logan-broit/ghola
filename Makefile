# Chapterhouse Build System
# =========================

.PHONY: build test lint clean help server web images charts deploy-server deploy-web reindex release release-dry-run release-check

REGISTRY   ?= registry.switchcraft.pd.internal/chapterhouse
VERSION     = $(shell cat VERSION 2>/dev/null || echo "0.1.0")
NAMESPACE  ?= ch-system

# Help target (default)
help:
	@echo "Chapterhouse Makefile"
	@echo ""
	@echo "Build targets:"
	@echo "  make test            Run Go tests"
	@echo "  make lint            Run Go vet"
	@echo "  make clean           Remove built images"
	@echo ""
	@echo "Container targets:"
	@echo "  make server          Build + push ch-server image"
	@echo "  make web             Build + push ch-web image"
	@echo "  make images          Build + push all images"
	@echo ""
	@echo "Helm targets:"
	@echo "  make charts          Package Helm charts"
	@echo "  make deploy-server   Deploy ch-server via Helm"
	@echo "  make deploy-web      Deploy ch-web via Helm"
	@echo ""
	@echo "Release targets:"
	@echo "  make release VERSION=X.Y.Z        Stamp versions, commit, tag, push"
	@echo "  make release-dry-run VERSION=X.Y.Z Preview what release would do"
	@echo ""
	@echo "Current version: $(VERSION)"

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

server: ## Build + push ch-server image (amd64)
	@echo "Building ch-server:$(VERSION)..."
	@docker buildx build --platform linux/amd64 \
		-f ch-server/Dockerfile \
		-t $(REGISTRY)/ch-server:$(VERSION) \
		-t $(REGISTRY)/ch-server:latest \
		--push \
		ch-server/
	@echo "ch-server:$(VERSION) pushed to $(REGISTRY)"

web: ## Build + push ch-web image (amd64)
	@echo "Building ch-web:$(VERSION)..."
	@docker buildx build --platform linux/amd64 \
		-f ch-web/Dockerfile \
		-t $(REGISTRY)/ch-web:$(VERSION) \
		-t $(REGISTRY)/ch-web:latest \
		--push \
		ch-web/
	@echo "ch-web:$(VERSION) pushed to $(REGISTRY)"

images: server web ## Build + push all images

clean: ## Remove built images
	docker rmi $(REGISTRY)/ch-server:$(VERSION) 2>/dev/null || true
	docker rmi $(REGISTRY)/ch-web:$(VERSION) 2>/dev/null || true

# =============================================================================
# Helm Chart Targets
# =============================================================================

charts: ## Package Helm charts
	@mkdir -p dist
	@helm package ch-server/charts/ch-server --version $(VERSION) --app-version $(VERSION) -d dist/
	@helm package ch-web/charts/ch-web --version $(VERSION) --app-version $(VERSION) -d dist/
	@echo "Charts packaged in dist/"

deploy-server: ## Deploy ch-server via Helm
	helm upgrade --install ch-server ch-server/charts/ch-server \
		--namespace $(NAMESPACE) --create-namespace \
		--set image.tag=$(VERSION)

deploy-web: ## Deploy ch-web via Helm
	helm upgrade --install ch-web ch-web/charts/ch-web \
		--namespace $(NAMESPACE) --create-namespace \
		--set image.tag=$(VERSION)

reindex: ## Run reindex via kubectl
	kubectl exec -n $(NAMESPACE) deploy/ch-server -- /app/reindex

# =============================================================================
# Release Target
# =============================================================================
# Stamps version across all metadata files, commits, tags, and pushes.
# CI pipeline handles the actual build/publish/deploy from the tag.
#
# Files updated:
#   VERSION                               - single source of truth
#   ch-server/charts/ch-server/Chart.yaml - version + appVersion
#   ch-web/charts/ch-web/Chart.yaml       - version + appVersion
# =============================================================================

SEMVER_RE := ^[0-9]+\.[0-9]+\.[0-9]+$$

release-check:
	@if [ -n "$$(git status --porcelain)" ]; then \
		echo "ERROR: Working tree is dirty. Commit or stash changes first."; \
		git status --short; \
		exit 1; \
	fi
	@BRANCH=$$(git rev-parse --abbrev-ref HEAD); \
	if [ "$$BRANCH" != "main" ]; then \
		echo "ERROR: Releases must be cut from main (currently on $$BRANCH)"; \
		exit 1; \
	fi
	@git fetch origin main --quiet; \
	LOCAL=$$(git rev-parse HEAD); \
	REMOTE=$$(git rev-parse origin/main); \
	if [ "$$LOCAL" != "$$REMOTE" ]; then \
		echo "ERROR: Local main is not up to date with origin/main"; \
		echo "  local:  $$LOCAL"; \
		echo "  remote: $$REMOTE"; \
		echo "Run: git pull --rebase origin main"; \
		exit 1; \
	fi

define stamp-version
	@echo "  VERSION                               → $(1)"
	@echo "$(1)" > VERSION
	@sed -i '' 's/^version: .*/version: $(1)/' ch-server/charts/ch-server/Chart.yaml
	@sed -i '' 's/^appVersion: .*/appVersion: "$(1)"/' ch-server/charts/ch-server/Chart.yaml
	@echo "  ch-server/charts/ch-server/Chart.yaml → $(1)"
	@sed -i '' 's/^version: .*/version: $(1)/' ch-web/charts/ch-web/Chart.yaml
	@sed -i '' 's/^appVersion: .*/appVersion: "$(1)"/' ch-web/charts/ch-web/Chart.yaml
	@echo "  ch-web/charts/ch-web/Chart.yaml       → $(1)"
endef

release-dry-run:
ifndef VERSION
	$(error VERSION is required. Usage: make release-dry-run VERSION=0.2.0)
endif
	@if ! echo "$(VERSION)" | grep -qE '$(SEMVER_RE)'; then \
		echo "ERROR: VERSION must be semver (X.Y.Z), got: $(VERSION)"; \
		exit 1; \
	fi
	@echo "=== DRY RUN: Release v$(VERSION) ==="
	@echo ""
	@echo "Current version: $$(cat VERSION)"
	@echo ""
	@echo "Files that would be updated:"
	@printf "  %-42s %s → %s\n" "VERSION" "$$(cat VERSION)" "$(VERSION)"
	@printf "  %-42s %s → %s\n" "ch-server Chart.yaml" "$$(grep '^version:' ch-server/charts/ch-server/Chart.yaml | awk '{print $$2}')" "$(VERSION)"
	@printf "  %-42s %s → %s\n" "ch-web Chart.yaml" "$$(grep '^version:' ch-web/charts/ch-web/Chart.yaml | awk '{print $$2}')" "$(VERSION)"
	@echo ""
	@echo "Would commit, tag v$(VERSION), and push to origin."

release: release-check
ifndef VERSION
	$(error VERSION is required. Usage: make release VERSION=0.2.0)
endif
	@if ! echo "$(VERSION)" | grep -qE '$(SEMVER_RE)'; then \
		echo "ERROR: VERSION must be semver (X.Y.Z), got: $(VERSION)"; \
		exit 1; \
	fi
	@if git rev-parse "v$(VERSION)" >/dev/null 2>&1; then \
		echo "ERROR: Tag v$(VERSION) already exists"; \
		exit 1; \
	fi
	@echo "=== Releasing v$(VERSION) ==="
	@echo ""
	$(call stamp-version,$(VERSION))
	@echo ""
	@git add VERSION ch-server/charts/ch-server/Chart.yaml ch-web/charts/ch-web/Chart.yaml
	@git diff --cached --quiet && echo "Version files already at $(VERSION), tagging only" \
		|| git commit -m "release: v$(VERSION)"
	@git tag -a v$(VERSION) -m "Release v$(VERSION)"
	@git push origin main && git push origin v$(VERSION)
	@echo ""
	@echo "Release v$(VERSION) complete!"
	@echo "CI will build and publish:"
	@echo "  - Docker images (ch-server:$(VERSION), ch-web:$(VERSION))"
	@echo "  - Helm charts (ch-server-$(VERSION), ch-web-$(VERSION))"

.DEFAULT_GOAL := help
