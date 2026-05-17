#!/bin/bash
set -e

# Chapterhouse Local Development Script
# Uses K3s with NodePort services for infrastructure access

NAMESPACE="ch-system"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

check_prereqs() {
    log_info "Checking prerequisites..."

    if ! command -v kubectl &> /dev/null; then
        log_error "kubectl not found"
        exit 1
    fi

    if ! kubectl get namespace "$NAMESPACE" &> /dev/null; then
        log_error "Namespace $NAMESPACE not found. Deploy infrastructure first."
        exit 1
    fi

    # Check if PostgreSQL is ready
    if ! kubectl get pod -n "$NAMESPACE" -l cnpg.io/cluster=chapterhouse-db -o jsonpath='{.items[0].status.phase}' 2>/dev/null | grep -q "Running"; then
        log_error "PostgreSQL is not running"
        exit 1
    fi

    # Verify NodePort services exist
    if ! kubectl get svc -n "$NAMESPACE" chapterhouse-db-nodeport &> /dev/null; then
        log_warn "NodePort services not found, creating..."
        kubectl apply -f "$PROJECT_DIR/deploy/k8s/local/services.yaml"
    fi

    log_info "All prerequisites met"
}

verify_services() {
    log_info "Verifying service connectivity..."

    # Test PostgreSQL
    if nc -z localhost 30432 2>/dev/null; then
        log_info "  PostgreSQL: localhost:30432"
    else
        log_error "  PostgreSQL: localhost:30432 unreachable"
        exit 1
    fi
}

get_db_password() {
    kubectl get secret -n "$NAMESPACE" chapterhouse-db-app -o jsonpath='{.data.password}' | base64 -d
}

run_api() {
    log_info "Starting Chapterhouse API..."

    cd "$PROJECT_DIR"

    # Get database password from secret
    DB_PASSWORD=$(get_db_password)

    # Export environment variables for local development
    export ENVIRONMENT="local"
    export SERVER_HOST="0.0.0.0"
    export SERVER_PORT="8080"
    export DATABASE_HOST="localhost"
    export DATABASE_PORT="30432"
    export DATABASE_NAME="memories"
    export DATABASE_USER="memory_api"
    export DATABASE_PASSWORD="$DB_PASSWORD"
    export DATABASE_SSL_MODE="disable"
    export AUTH_PROVIDER="default"
    export AUTH_DEFAULT_USER="00000000-0000-0000-0000-000000000000"
    export EMBEDDING_PROVIDER="openai"
    export EMBEDDING_URL="https://api.openai.com"
    export EMBEDDING_MODEL="text-embedding-3-small"
    export EMBEDDING_DIMENSIONS="768"
    export LOG_LEVEL="debug"

    log_info "Environment configured:"
    log_info "  API: http://localhost:8080"
    log_info "  Health: http://localhost:8080/health"

    # Run the API
    go run ./cmd/api
}

show_help() {
    echo "Chapterhouse Local Development Script"
    echo ""
    echo "Usage: $0 [command]"
    echo ""
    echo "Commands:"
    echo "  start     Verify services and run API (default)"
    echo "  check     Check prerequisites and service connectivity"
    echo "  env       Print environment variables for manual run"
    echo "  password  Print database password"
    echo "  deploy    Deploy infrastructure to Kubernetes"
    echo "  help      Show this help"
    echo ""
    echo "Service Ports (via NodePort):"
    echo "  PostgreSQL: localhost:30432"
}

deploy_infra() {
    log_info "Deploying Chapterhouse infrastructure to Kubernetes..."

    # Create namespace
    kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

    # Apply base manifests
    kubectl apply -f "$PROJECT_DIR/deploy/k8s/base/postgres-cluster.yaml"

    # Apply local development resources
    kubectl apply -f "$PROJECT_DIR/deploy/k8s/local/services.yaml"
    kubectl apply -f "$PROJECT_DIR/deploy/k8s/local/ingress.yaml"

    log_info "Waiting for PostgreSQL to be ready..."
    kubectl -n "$NAMESPACE" wait --for=condition=Ready pod -l cnpg.io/cluster=chapterhouse-db --timeout=120s || true

    log_info "Infrastructure deployed!"
}

# Main
case "${1:-start}" in
    start)
        check_prereqs
        verify_services
        run_api
        ;;
    check)
        check_prereqs
        verify_services
        log_info "All services are accessible"
        ;;
    env)
        DB_PASSWORD=$(get_db_password)
        echo "export DATABASE_HOST=localhost"
        echo "export DATABASE_PORT=30432"
        echo "export DATABASE_NAME=memories"
        echo "export DATABASE_USER=memory_api"
        echo "export DATABASE_PASSWORD='$DB_PASSWORD'"
        echo "export DATABASE_SSL_MODE=disable"
        echo "export EMBEDDING_URL=http://localhost:11434"
        ;;
    password)
        get_db_password
        echo ""
        ;;
    deploy)
        deploy_infra
        ;;
    help|--help|-h)
        show_help
        ;;
    *)
        log_error "Unknown command: $1"
        show_help
        exit 1
        ;;
esac
