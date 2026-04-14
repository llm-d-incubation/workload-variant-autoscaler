#!/usr/bin/env bash
#
# Workload-Variant-Autoscaler KIND Emulator Deployment Script
# Automated deployment of WVA with llm-d infrastructure on Kind cluster with llm-d-inference-sim simulator
#
# Prerequisites:
# - kubectl installed and configured
# - helm installed
# - kind installed (for cluster creation)
# - Docker installed and running
#

set -e  # Exit on error
set -o pipefail  # Exit on pipe failure

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Configuration
WVA_PROJECT=${WVA_PROJECT:-$PWD}
WELL_LIT_PATH_NAME="simulated-accelerators"
NAMESPACE_SUFFIX="sim"
EXAMPLE_DIR="$WVA_PROJECT/$LLM_D_PROJECT/guides/$WELL_LIT_PATH_NAME"
DEPLOY_LLM_D_INFERENCE_SIM=true

# Namespaces
LLMD_NS="llm-d-$NAMESPACE_SUFFIX"
MONITORING_NAMESPACE="workload-variant-autoscaler-monitoring"
WVA_NS=${WVA_NS:-"workload-variant-autoscaler-system"}

# WVA Configuration
WVA_RECONCILE_INTERVAL=${WVA_RECONCILE_INTERVAL:-"60s"} # WVA controller reconcile interval - tests set 30s interval
SKIP_TLS_VERIFY=true  # Skip TLS verification in emulated environments
WVA_LOG_LEVEL="debug" # WVA log level set to debug for emulated environments
# Initial WVA pool group; install.sh auto-detects the actual InferencePool API group after llm-d deploy and upgrades WVA (scale-from-zero).
POOL_GROUP=${POOL_GROUP:-"inference.networking.k8s.io"}

# llm-d Configuration
LLM_D_INFERENCE_SIM_IMG_REPO=${LLM_D_INFERENCE_SIM_IMG_REPO:-"ghcr.io/llm-d/llm-d-inference-sim"}
LLM_D_INFERENCE_SIM_IMG_TAG=${LLM_D_INFERENCE_SIM_IMG_TAG:-"latest"}

LLM_D_MODELSERVICE_NAME="ms-$NAMESPACE_SUFFIX-llm-d-modelservice"
LLM_D_MODELSERVICE_VALUES="ms-$NAMESPACE_SUFFIX/values.yaml"
LLM_D_EPP_NAME="gaie-$NAMESPACE_SUFFIX-epp"

# Model and SLO Configuration
MODEL_ID=${MODEL_ID:-"unsloth/Meta-Llama-3.1-8B"}
DEFAULT_MODEL_ID="random"
ACCELERATOR_TYPE="A100"
SLO_TPOT=24     # Target time-per-output-token SLO (in ms)
SLO_TTFT=500  # Target time-to-first-token SLO (in ms)

# Gateway Configuration
INSTALL_GATEWAY_CTRLPLANE="true" # if true, installs gateway control plane providers - defaults to true for emulated clusters

# Prometheus Configuration
PROMETHEUS_SVC_NAME="kube-prometheus-stack-prometheus"
PROMETHEUS_BASE_URL="https://$PROMETHEUS_SVC_NAME.$MONITORING_NAMESPACE.svc.cluster.local"
PROMETHEUS_PORT="9090"
PROMETHEUS_URL="$PROMETHEUS_BASE_URL:$PROMETHEUS_PORT"
PROMETHEUS_SECRET_NAME="prometheus-web-tls"

# KIND cluster configuration
CLUSTER_NAME=${CLUSTER_NAME:-"kind-wva-gpu-cluster"}
CLUSTER_NODES=${CLUSTER_NODES:-"3"}
CLUSTER_GPUS=${CLUSTER_GPUS:-"4"}
CLUSTER_GPU_TYPE=${CLUSTER_GPU_TYPE:-"mix"}

# Flags for deployment steps
CREATE_CLUSTER=${CREATE_CLUSTER:-false}
DEPLOY_LLM_D_INFERENCE_SIM=${DEPLOY_LLM_D_INFERENCE_SIM:-true}
E2E_TESTS_ENABLED=${E2E_TESTS_ENABLED:-false}

# Undeployment flags
DELETE_CLUSTER=${DELETE_CLUSTER:-false}

# Kind-specific prerequisites
REQUIRED_TOOLS=("kind")

# Function to check Kind emulator-specific prerequisites
# - checks for kind, kubectl and helm
# - creates Kind cluster if CREATE_CLUSTER=true, otherwise tries to use an existing cluster
# - loads WVA image into Kind cluster
check_specific_prerequisites() {
    log_info "Checking Kubernetes-specific prerequisites..."
    
    local missing_tools=()
    
    # Check for required tools (including Kubernetes-specific ones)
    for tool in "${REQUIRED_TOOLS[@]}"; do
        if ! command -v "$tool" &> /dev/null; then
            missing_tools+=($tool)
        fi
    done
    
    if [ ${#missing_tools[@]} -ne 0 ]; then
        log_error "Missing required tools: ${missing_tools[*]}"
    fi
    
    # Create or use existing KIND cluster
    if [ "$CREATE_CLUSTER" = "true" ]; then
        # Check if the specific cluster exists - if so, delete and recreate
        if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
            log_info "KIND cluster '${CLUSTER_NAME}' already exists, tearing it down and recreating..."
            kind delete cluster --name "${CLUSTER_NAME}"
        else 
            log_info "KIND cluster '${CLUSTER_NAME}' not found, creating it..."
        fi
        create_kind_cluster

    else
        log_info "Cluster creation skipped (CREATE_CLUSTER=false)"
        # Verify the Kind cluster exists
        if ! kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
            log_error "KIND cluster '${CLUSTER_NAME}' not found and CREATE_CLUSTER=false"
        fi
        # Set kubectl context to the Kind cluster
        kubectl config use-context "kind-${CLUSTER_NAME}" &> /dev/null
    fi
    # Verify kubectl can connect to the cluster
    if ! kubectl cluster-info &> /dev/null; then
        log_error "Failed to connect to KIND cluster '${CLUSTER_NAME}'"
    fi
    log_success "Using KIND cluster '${CLUSTER_NAME}'"

    # Load WVA image into KIND cluster
    load_image

    log_success "All Kind emulated deployment prerequisites met"
}

# Creates Kind cluster using `setup.sh` script for GPU emulation
create_kind_cluster() {
    log_info "Creating KIND cluster with GPU emulation..."
    
    # Check if cluster already exists
    if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
        log_warning "KIND cluster '${CLUSTER_NAME}' already exists"
        log_info "Deleting existing cluster to create a fresh one..."
        kind delete cluster --name "${CLUSTER_NAME}"
    fi
    
    # Run setup.sh to create the cluster
    local SETUP_SCRIPT="${WVA_PROJECT}/deploy/kind-emulator/setup.sh"
    
    if [ ! -f "$SETUP_SCRIPT" ]; then
        log_error "Setup script not found at: $SETUP_SCRIPT"
        exit 1
    fi
    
    log_info "Running setup script with: cluster=$CLUSTER_NAME, nodes=$CLUSTER_NODES, gpus=$CLUSTER_GPUS, type=$CLUSTER_GPU_TYPE"
    bash "$SETUP_SCRIPT" -c "${CLUSTER_NAME}" -n "$CLUSTER_NODES" -g "$CLUSTER_GPUS" -t "$CLUSTER_GPU_TYPE"
    
    # Ensure kubectl context is set to the new cluster
    kubectl config use-context "kind-${CLUSTER_NAME}" &> /dev/null
    
    log_success "KIND cluster '${CLUSTER_NAME}' created successfully"
}

# Loads WVA image into the Kind cluster.
# When pulling from a registry, we pull a single platform (KIND_IMAGE_PLATFORM) to avoid
# "content digest ... not found" errors from kind load (multi-platform manifests reference
# blobs not included in the export stream; see kubernetes-sigs/kind#3795, #3845).
load_image() {
    log_info "Loading WVA image '$WVA_IMAGE_REPO:$WVA_IMAGE_TAG' into KIND cluster..."
    
    # If WVA_IMAGE_PULL_POLICY is IfNotPresent, skip pulling and use local image only
    if [ "$WVA_IMAGE_PULL_POLICY" = "IfNotPresent" ]; then
        log_info "Using local image only (WVA_IMAGE_PULL_POLICY=IfNotPresent)"
        
        # Check if the image exists locally
        if ! docker image inspect "$WVA_IMAGE_REPO:$WVA_IMAGE_TAG" >/dev/null 2>&1; then
            log_error "Image '$WVA_IMAGE_REPO:$WVA_IMAGE_TAG' not found locally - Please build the image first (e.g., 'make docker-build IMG=$WVA_IMAGE_REPO:$WVA_IMAGE_TAG')"
        else
            log_success "Found local image '$WVA_IMAGE_REPO:$WVA_IMAGE_TAG'"
        fi
    else
        # Pull a single-platform image so kind load does not hit "content digest not found"
        # (multi-platform manifests can reference blobs that are not in the docker save stream).
        local platform="${KIND_IMAGE_PLATFORM:-}"
        if [ -z "$platform" ]; then
            case "$(uname -m)" in
                aarch64|arm64) platform="linux/arm64" ;;
                *) platform="linux/amd64" ;;
            esac
        fi
        log_info "Pulling single-platform image for KIND (platform=$platform) to avoid load errors..."
        if ! docker pull --platform "$platform" "$WVA_IMAGE_REPO:$WVA_IMAGE_TAG"; then
            log_warning "Failed to pull image '$WVA_IMAGE_REPO:$WVA_IMAGE_TAG' (platform=$platform)"
            log_info "Attempting to use existing local image..."
            if ! docker image inspect "$WVA_IMAGE_REPO:$WVA_IMAGE_TAG" >/dev/null 2>&1; then
                log_error "Image '$WVA_IMAGE_REPO:$WVA_IMAGE_TAG' not found locally - Please build or pull the image"
                exit 1
            fi
        else
            log_success "Pulled image '$WVA_IMAGE_REPO:$WVA_IMAGE_TAG' (platform=$platform)"
        fi
    fi
    
    # Load the image into the KIND cluster.
    # Try `kind load docker-image` first. If it fails (common with Docker Desktop's
    # containerd image store where `docker save` chokes on multi-platform manifests),
    # fall back to pulling the image directly into each KIND node's containerd.
    local full_image="$WVA_IMAGE_REPO:$WVA_IMAGE_TAG"
    local load_stderr
    if load_stderr="$(kind load docker-image "$full_image" --name "$CLUSTER_NAME" 2>&1)"; then
        log_success "Image '$full_image' loaded into KIND cluster '$CLUSTER_NAME'"
        return
    fi

    # Only fall back to the crictl/ctr path for the known containerd image store
    # issue (docker save fails on multi-platform manifests, kubernetes-sigs/kind#3795).
    # For any other error, report it and abort.
    if ! echo "$load_stderr" | grep -qiE "docker save|multi-?platform|manifest|content digest|no such image|not found"; then
        log_error "'kind load docker-image' failed:"
        log_error "$load_stderr"
        exit 1
    fi

    log_warning "'kind load docker-image' failed (containerd image store issue) — falling back to pulling directly into KIND nodes"
    log_info "kind load stderr: $load_stderr"

    # Pull the image directly into each KIND node's containerd, bypassing
    # Docker Desktop entirely. This avoids the `docker save` multi-platform
    # manifest issue (kubernetes-sigs/kind#3795).
    local nodes
    nodes="$(kind get nodes --name "$CLUSTER_NAME")" || {
        log_error "No nodes found in KIND cluster '$CLUSTER_NAME'"
        exit 1
    }
    if [ -z "$nodes" ]; then
        log_error "No nodes found in KIND cluster '$CLUSTER_NAME'"
        exit 1
    fi

    # Detect if an image reference is qualified with an explicit registry hostname.
    # Heuristic used by Docker/containerd/podman:
    # If the first '/'-separated segment contains a '.', a ':', or equals 'localhost',
    # it is treated as a registry hostname (e.g., quay.io/foo, registry.k8s.io/pause,
    # localhost:5000/myimg).
    local first_segment
    first_segment="${full_image%%/*}"
    local has_explicit_registry=false
    case "$first_segment" in
        *.*|*:*|localhost) has_explicit_registry=true ;;
    esac

    local successful_nodes=()
    for node in $nodes; do
        log_info "Pulling image on node '$node'..."
        local pull_stderr
        if pull_stderr="$(docker exec "$node" crictl pull "$full_image" 2>&1)"; then
            successful_nodes+=("$node")
            continue
        fi
        log_warning "crictl pull failed on node '$node': $pull_stderr"

        # crictl may not resolve short names; try with docker.io prefix, but
        # only for unqualified image names (no registry hostname prefix like quay.io/).
        if [ "$has_explicit_registry" = true ]; then
            log_error "Failed to pull image on node '$node' (image has explicit registry, skipping docker.io fallback): $pull_stderr"
            # Best-effort rollback to avoid partial cluster state.
            for ok_node in "${successful_nodes[@]}"; do
                docker exec "$ok_node" ctr --namespace=k8s.io images rm "$full_image" >/dev/null 2>&1 || true
            done
            exit 1
        fi

        if pull_stderr="$(docker exec "$node" ctr --namespace=k8s.io images pull "docker.io/$full_image" 2>&1)"; then
            # Tag so kubelet can find it by the original name, but only if it doesn't already exist.
            if ! docker exec "$node" ctr --namespace=k8s.io images ls -q | grep -Fxq "$full_image"; then
                if ! docker exec "$node" ctr --namespace=k8s.io images tag "docker.io/$full_image" "$full_image" >/dev/null 2>&1; then
                    log_error "Failed to tag image on node '$node' (docker.io/$full_image -> $full_image)"
                    for ok_node in "${successful_nodes[@]}"; do
                        docker exec "$ok_node" ctr --namespace=k8s.io images rm "$full_image" >/dev/null 2>&1 || true
                    done
                    exit 1
                fi
            fi
            successful_nodes+=("$node")
        else
            log_error "Failed to pull image on node '$node': $pull_stderr"
            for ok_node in "${successful_nodes[@]}"; do
                docker exec "$ok_node" ctr --namespace=k8s.io images rm "$full_image" >/dev/null 2>&1 || true
            done
            exit 1
        fi
    done

    log_success "Image '$full_image' pulled directly into KIND cluster '$CLUSTER_NAME' nodes"
}

KUBE_LIKE_VALUES_DEV_IF_PRESENT=true

_wva_deploy_lib="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/../lib"
# shellcheck source=deploy_prometheus_kube_stack.sh
source "${_wva_deploy_lib}/deploy_prometheus_kube_stack.sh"
# shellcheck source=kube_like_adapter.sh
source "${_wva_deploy_lib}/kube_like_adapter.sh"

# REQUIRED FUNCTION - only for emulated environments ####
# Apply llm-d infrastructure fixes for Kind emulated clusters - e.g., remove prefill deployments, remove decode deployments if tests are enabled
apply_llm_d_infrastructure_fixes() {
    log_info "Applying llm-d infrastructure fixes for KIND emulator..."
    # Skip cleanup when modelservice release is not installed (e.g., e2e infra-only
    # path now excludes it via helmfile selector).
    if ! helm list -n "$LLMD_NS" --short 2>/dev/null | grep -q '^ms-'; then
        log_info "No llm-d modelservice release detected in $LLMD_NS; skipping prefill/decode cleanup"
        return
    fi

    # Delete prefill deployment
    # TODO: remove once WVA supports both prefill and decode
    log_info "Deleting prefill deployments..."
    kubectl delete deployments.apps \
        $LLM_D_MODELSERVICE_NAME-prefill \
        --ignore-not-found -n "$LLMD_NS"
        
    if [ "$E2E_TESTS_ENABLED" = "true" ]; then
        log_info "Deleting decode deployments for tests..."
        kubectl delete deployments.apps \
            $LLM_D_MODELSERVICE_NAME-decode \
            --ignore-not-found -n "$LLMD_NS"
    fi
}

#### REQUIRED FUNCTION used by deploy/install.sh ####
delete_namespaces() {
    delete_namespaces_kube_like
    if [ "$DELETE_CLUSTER" = true ]; then
        delete_kind_cluster
    fi
}

# Deletes the Kind cluster
# Used when DELETE_CLUSTER=true by delete_namespaces()
delete_kind_cluster() {
    log_info "Deleting KIND cluster '${CLUSTER_NAME}'..."
    
    if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
        kind delete cluster --name "${CLUSTER_NAME}"
        log_success "KIND cluster '${CLUSTER_NAME}' deleted"
    else
        log_warning "KIND cluster '${CLUSTER_NAME}' not found"
    fi
}
