#!/usr/bin/env bash
set -euo pipefail

# Homelab deployment script for Chapterhouse
# Builds images, pushes to ghcr.io, deploys to K3s cluster

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
KUBECONFIG="${KUBECONFIG:-$HOME/.kube/sandbox}"
NAMESPACE="ch-system"
REGISTRY="ghcr.io/thinkwright/chapterhouse"
TAG="${TAG:-latest}"

export KUBECONFIG

echo "=== Chapterhouse Homelab Deploy ==="
echo "Registry: ${REGISTRY}"
echo "Tag:      ${TAG}"
echo "Cluster:  ${KUBECONFIG}"
echo ""

# Parse arguments
BUILD=true
PUSH=true
DEPLOY=true
COMPONENT=""

while [[ $# -gt 0 ]]; do
    case $1 in
        --no-build)  BUILD=false; shift ;;
        --no-push)   PUSH=false; shift ;;
        --no-deploy) DEPLOY=false; shift ;;
        --only)      COMPONENT="$2"; shift 2 ;;
        --tag)       TAG="$2"; shift 2 ;;
        -h|--help)
            echo "Usage: deploy.sh [OPTIONS]"
            echo ""
            echo "Options:"
            echo "  --no-build   Skip building images"
            echo "  --no-push    Skip pushing images"
            echo "  --no-deploy  Skip helm deploy"
            echo "  --only NAME  Only build/deploy one component (ch-server or ch-web)"
            echo "  --tag TAG    Image tag (default: latest)"
            exit 0
            ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

should_run() {
    [[ -z "$COMPONENT" || "$COMPONENT" == "$1" ]]
}

# Build
if $BUILD; then
    if should_run "ch-server"; then
        echo "--- Building ch-server ---"
        podman build --platform linux/amd64 -t "${REGISTRY}/ch-server:${TAG}" "${REPO_ROOT}/ch-server"
    fi
    if should_run "ch-web"; then
        echo "--- Building ch-web ---"
        podman build --platform linux/amd64 -t "${REGISTRY}/ch-web:${TAG}" "${REPO_ROOT}/ch-web"
    fi
fi

# Push
if $PUSH; then
    if should_run "ch-server"; then
        echo "--- Pushing ch-server ---"
        podman push "${REGISTRY}/ch-server:${TAG}"
    fi
    if should_run "ch-web"; then
        echo "--- Pushing ch-web ---"
        podman push "${REGISTRY}/ch-web:${TAG}"
    fi
fi

# Deploy
if $DEPLOY; then
    echo "--- Creating namespace ---"
    kubectl create namespace "${NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -

    if should_run "ch-server"; then
        echo "--- Deploying ch-server ---"
        helm upgrade --install ch-server "${REPO_ROOT}/ch-server/charts/ch-server" \
            --namespace "${NAMESPACE}" \
            -f "${REPO_ROOT}/deploy/homelab/ch-server-values.yaml" \
            --set "image.tag=${TAG}"
    fi

    if should_run "ch-web"; then
        echo "--- Deploying ch-web ---"
        helm upgrade --install ch-web "${REPO_ROOT}/ch-web/charts/ch-web" \
            --namespace "${NAMESPACE}" \
            -f "${REPO_ROOT}/deploy/homelab/ch-web-values.yaml" \
            --set "image.tag=${TAG}"
    fi

    echo ""
    echo "--- Status ---"
    kubectl get pods -n "${NAMESPACE}"
fi

echo ""
echo "Done."
