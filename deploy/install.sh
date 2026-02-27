#!/bin/bash
#
# Workload-Variant-Autoscaler Deployment Script
# Automated deployment of WVA, llm-d infrastructure, Prometheus, and HPA
#
# Prerequisites:
# - Access to a Kubernetes/OpenShift cluster or Kind cluster with emulated GPUs
# - HuggingFace token (for llm-d deployment)
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
WELL_LIT_PATH_NAME=${WELL_LIT_PATH_NAME:-"inference-scheduling"}
NAMESPACE_SUFFIX=${NAMESPACE_SUFFIX:-"inference-scheduler"}

# Namespaces
LLMD_NS=${LLMD_NS:-"llm-d-$NAMESPACE_SUFFIX"}
MONITORING_NAMESPACE=${MONITORING_NAMESPACE:-"workload-variant-autoscaler-monitoring"}
WVA_NS=${WVA_NS:-"workload-variant-autoscaler-system"}
PROMETHEUS_SECRET_NS=${PROMETHEUS_SECRET_NS:-$MONITORING_NAMESPACE}

# WVA Configuration
WVA_IMAGE_REPO=${WVA_IMAGE_REPO:-"ghcr.io/llm-d/llm-d-workload-variant-autoscaler"}
WVA_IMAGE_TAG=${WVA_IMAGE_TAG:-"latest"}
WVA_IMAGE_PULL_POLICY=${WVA_IMAGE_PULL_POLICY:-"Always"}
WVA_RELEASE_NAME=${WVA_RELEASE_NAME:-"workload-variant-autoscaler"}
VLLM_SVC_ENABLED=${VLLM_SVC_ENABLED:-true}
VLLM_SVC_PORT=${VLLM_SVC_PORT:-8200}
VLLM_SVC_NODEPORT=${VLLM_SVC_NODEPORT:-30000}
SKIP_TLS_VERIFY=${SKIP_TLS_VERIFY:-"false"}
WVA_LOG_LEVEL=${WVA_LOG_LEVEL:-"info"}
VALUES_FILE=${VALUES_FILE:-"$WVA_PROJECT/charts/workload-variant-autoscaler/values.yaml"}
# Controller instance identifier for multi-controller isolation (optional)
# When set, adds controller_instance label to metrics and HPA selectors
CONTROLLER_INSTANCE=${CONTROLLER_INSTANCE:-""}

# llm-d Configuration
LLM_D_OWNER=${LLM_D_OWNER:-"llm-d"}
LLM_D_PROJECT=${LLM_D_PROJECT:-"llm-d"}
LLM_D_RELEASE=${LLM_D_RELEASE:-"v0.3.0"}
LLM_D_MODELSERVICE_NAME=${LLM_D_MODELSERVICE_NAME:-"ms-$WELL_LIT_PATH_NAME-llm-d-modelservice"}
LLM_D_EPP_NAME=${LLM_D_EPP_NAME:-"gaie-$WELL_LIT_PATH_NAME-epp"}
CLIENT_PREREQ_DIR=${CLIENT_PREREQ_DIR:-"$WVA_PROJECT/$LLM_D_PROJECT/guides/prereq/client-setup"}
GATEWAY_PREREQ_DIR=${GATEWAY_PREREQ_DIR:-"$WVA_PROJECT/$LLM_D_PROJECT/guides/prereq/gateway-provider"}
EXAMPLE_DIR=${EXAMPLE_DIR:-"$WVA_PROJECT/$LLM_D_PROJECT/guides/$WELL_LIT_PATH_NAME"}
LLM_D_MODELSERVICE_VALUES=${LLM_D_MODELSERVICE_VALUES:-"$EXAMPLE_DIR/ms-$WELL_LIT_PATH_NAME/values.yaml"}
ITL_AVERAGE_LATENCY_MS=${ITL_AVERAGE_LATENCY_MS:-20}
TTFT_AVERAGE_LATENCY_MS=${TTFT_AVERAGE_LATENCY_MS:-200}
ENABLE_SCALE_TO_ZERO=${ENABLE_SCALE_TO_ZERO:-true}
# llm-d-inference scheduler with image with flowcontrol support
# TODO: update once the llm-d-inference-scheduler v0.5.0 is released
LLM_D_INFERENCE_SCHEDULER_IMG=${LLM_D_INFERENCE_SCHEDULER_IMG:-"ghcr.io/llm-d/llm-d-inference-scheduler:v0.5.0-rc.1"}

# Gateway Configuration
GATEWAY_PROVIDER=${GATEWAY_PROVIDER:-"istio"} # Options: kgateway, istio
BENCHMARK_MODE=${BENCHMARK_MODE:-"false"} # if true, updates to Istio config for benchmark (istioBench env required in helmfile)
# Save original value to detect if explicitly set via environment variable
INSTALL_GATEWAY_CTRLPLANE_ORIGINAL="${INSTALL_GATEWAY_CTRLPLANE:-}"
INSTALL_GATEWAY_CTRLPLANE="${INSTALL_GATEWAY_CTRLPLANE:-false}"

# Model and SLO Configuration
DEFAULT_MODEL_ID=${DEFAULT_MODEL_ID:-"Qwen/Qwen3-0.6B"}
MODEL_ID=${MODEL_ID:-"unsloth/Meta-Llama-3.1-8B"}
ACCELERATOR_TYPE=${ACCELERATOR_TYPE:-"H100"}
SLO_TPOT=${SLO_TPOT:-10}  # Target time-per-output-token SLO (in ms)
SLO_TTFT=${SLO_TTFT:-1000}  # Target time-to-first-token SLO (in ms)

# Multi-model testing configuration (for limiter e2e tests)
# When enabled, deploys a second InferencePool with a different model
MULTI_MODEL_TESTING=${MULTI_MODEL_TESTING:-false}
MODEL_ID_2=${MODEL_ID_2:-"unsloth/Llama-3.2-1B"}

# Prometheus Configuration
PROM_CA_CERT_PATH=${PROM_CA_CERT_PATH:-"/tmp/prometheus-ca.crt"}
PROMETHEUS_SECRET_NAME=${PROMETHEUS_SECRET_NAME:-"prometheus-web-tls"}

# Flags for deployment steps
DEPLOY_PROMETHEUS=${DEPLOY_PROMETHEUS:-true}
DEPLOY_WVA=${DEPLOY_WVA:-true}
DEPLOY_LLM_D=${DEPLOY_LLM_D:-true}
DEPLOY_PROMETHEUS_ADAPTER=${DEPLOY_PROMETHEUS_ADAPTER:-true}
DEPLOY_VA=${DEPLOY_VA:-true}
DEPLOY_HPA=${DEPLOY_HPA:-true}
HPA_STABILIZATION_SECONDS=${HPA_STABILIZATION_SECONDS:-240}
# HPA minReplicas: 0 enables scale-to-zero (requires HPAScaleToZero feature gate)
# Default to 1 for safety; set to 0 for scale-to-zero testing
HPA_MIN_REPLICAS=${HPA_MIN_REPLICAS:-1}
SKIP_CHECKS=${SKIP_CHECKS:-false}
E2E_TESTS_ENABLED=${E2E_TESTS_ENABLED:-false}
# WVA metrics endpoint security (set false to disable bearer token auth on /metrics)
WVA_METRICS_SECURE=${WVA_METRICS_SECURE:-true}
# vLLM max-num-seqs (max concurrent sequences per replica, lower = easier to saturate for testing)
VLLM_MAX_NUM_SEQS=${VLLM_MAX_NUM_SEQS:-""}
# Decode replicas override (useful for e2e testing with limited GPUs)
DECODE_REPLICAS=${DECODE_REPLICAS:-""}

# Infra-only mode: Deploy only llm-d infrastructure and WVA controller (skip VA/HPA)
# Useful for e2e testing where tests create their own VA/HPA resources
INFRA_ONLY=${INFRA_ONLY:-false}

# Saturation threshold overrides (V1 analyzer)
# kvSpareTrigger: scale-up fires when (kvCacheThreshold - avgKvUsage) < this value
# queueSpareTrigger: scale-up fires when (queueLengthThreshold - avgQueueLength) < this value
# Leave empty to use chart defaults (kvSpareTrigger=0.1, queueSpareTrigger=3)
KV_SPARE_TRIGGER=${KV_SPARE_TRIGGER:-""}
QUEUE_SPARE_TRIGGER=${QUEUE_SPARE_TRIGGER:-""}

# Scaler backend for e2e: "prometheus-adapter" (default) or "keda"
# When keda: do not deploy Prometheus Adapter; deploy KEDA instead (ScaledObjects, external metrics API)
SCALER_BACKEND=${SCALER_BACKEND:-prometheus-adapter}
KEDA_NAMESPACE=${KEDA_NAMESPACE:-keda-system}

# Environment-related variables
SCRIPT_DIR=$(cd $(dirname "${BASH_SOURCE[0]}") && pwd)
ENVIRONMENT=${ENVIRONMENT:-"kubernetes"}
COMPATIBLE_ENV_LIST=("kubernetes" "openshift" "kind-emulator")
NON_EMULATED_ENV_LIST=("kubernetes" "openshift")
REQUIRED_TOOLS=("kubectl" "helm" "git")

# TODO: add kubernetes to these defaults to enable TLS verification when deploying to production clusters
PRODUCTION_ENV_LIST=("openshift")

# Undeployment flags
UNDEPLOY=${UNDEPLOY:-false}
DELETE_NAMESPACES=${DELETE_NAMESPACES:-false}

# Helper functions
log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
    exit 1
}

# APIService guard: background loop that continuously ensures the
# v1beta1.external.metrics.k8s.io APIService points to prometheus-adapter.
# On clusters with KEDA, the operator continuously reconciles the APIService
# back to keda-metrics-apiserver, breaking HPA scaling for WVA.
# This guard re-patches it every 10 seconds without modifying KEDA itself.
APISERVICE_GUARD_PID=""

start_apiservice_guard() {
    local monitoring_ns="$1"
    log_info "Starting APIService guard (background re-patch loop every 10s)"
    (
        while true; do
            sleep 10
            current_svc=$(kubectl get apiservice v1beta1.external.metrics.k8s.io \
                -o jsonpath='{.spec.service.name}' 2>/dev/null || echo "")
            current_ns=$(kubectl get apiservice v1beta1.external.metrics.k8s.io \
                -o jsonpath='{.spec.service.namespace}' 2>/dev/null || echo "")
            if [ "$current_svc" != "prometheus-adapter" ] || [ "$current_ns" != "$monitoring_ns" ]; then
                echo "[apiservice-guard] KEDA reclaimed APIService (now: $current_svc/$current_ns), re-patching to prometheus-adapter/$monitoring_ns"
                kubectl patch apiservice v1beta1.external.metrics.k8s.io --type=merge -p "{
                    \"spec\": {
                        \"insecureSkipTLSVerify\": true,
                        \"service\": {
                            \"name\": \"prometheus-adapter\",
                            \"namespace\": \"$monitoring_ns\"
                        }
                    }
                }" 2>/dev/null || true
            fi
        done
    ) &
    APISERVICE_GUARD_PID=$!
    echo "$APISERVICE_GUARD_PID" > /tmp/apiservice-guard.pid
    log_success "APIService guard started (PID: $APISERVICE_GUARD_PID)"
}

stop_apiservice_guard() {
    if [ -n "$APISERVICE_GUARD_PID" ] && kill -0 "$APISERVICE_GUARD_PID" 2>/dev/null; then
        log_info "Stopping APIService guard (PID: $APISERVICE_GUARD_PID)"
        kill "$APISERVICE_GUARD_PID" 2>/dev/null || true
        wait "$APISERVICE_GUARD_PID" 2>/dev/null || true
    elif [ -f /tmp/apiservice-guard.pid ]; then
        local pid
        pid=$(cat /tmp/apiservice-guard.pid)
        if kill -0 "$pid" 2>/dev/null; then
            log_info "Stopping APIService guard (PID: $pid from pidfile)"
            kill "$pid" 2>/dev/null || true
        fi
    fi
    rm -f /tmp/apiservice-guard.pid
    APISERVICE_GUARD_PID=""
}

print_help() {
  cat <<EOF
Usage: $(basename "$0") [OPTIONS]

This script deploys the complete Workload-Variant-Autoscaler stack on a cluster with real GPUs.

Options:
  -i, --wva-image IMAGE        Container image to use for the WVA (default: $WVA_IMAGE_REPO:$WVA_IMAGE_TAG)
  -m, --model MODEL            Model ID to use (default: $MODEL_ID)
  -a, --accelerator TYPE       Accelerator type: A100, H100, L40S, etc. (default: $ACCELERATOR_TYPE)
  -r, --release-name NAME      Helm release name for WVA (default: $WVA_RELEASE_NAME)
  --infra-only                 Deploy only llm-d infrastructure and WVA controller (skip VA/HPA, for e2e testing)
  -u, --undeploy               Undeploy all components
  -e, --environment            Specify deployment environment: kubernetes, openshift, kind-emulated (default: kubernetes)
  -h, --help                   Show this help and exit

Environment Variables:
  IMG                          Container image to use for the WVA (alternative to -i flag)
  HF_TOKEN                     HuggingFace token for model access (required for llm-d deployment)
  WVA_RELEASE_NAME             Helm release name for WVA (alternative to -r flag)
  INSTALL_GATEWAY_CTRLPLANE    Install Gateway control plane (default: prompt user, can be set to "true"/"false")
  DEPLOY_PROMETHEUS            Deploy Prometheus stack (default: true)
  DEPLOY_WVA                   Deploy WVA controller (default: true)
  DEPLOY_LLM_D                 Deploy llm-d infrastructure (default: true)
  DEPLOY_PROMETHEUS_ADAPTER    Deploy Prometheus Adapter (default: true)
  DEPLOY_VA                    Deploy VariantAutoscaling (default: true)
  DEPLOY_HPA                   Deploy HPA (default: true)
  HPA_STABILIZATION_SECONDS    HPA stabilization window in seconds (default: 240)
  HPA_MIN_REPLICAS             HPA minReplicas (default: 1, set to 0 for scale-to-zero)
  INFRA_ONLY                   Deploy only infrastructure (default: false, same as --infra-only flag)
  SCALER_BACKEND               Scaler backend: prometheus-adapter (default) or keda. When keda, deploys KEDA and skips Prometheus Adapter.
  KEDA_NAMESPACE               Namespace for KEDA (default: keda-system)
  UNDEPLOY                     Undeploy mode (default: false)
  DELETE_NAMESPACES            Delete namespaces after undeploy (default: false)
  CONTROLLER_INSTANCE          Controller instance label for multi-controller isolation (optional)

Examples:
  # Deploy with default values
  $(basename "$0")

  # Deploy with custom WVA image
  IMG=<your_registry>/workload-variant-autoscaler:tag $(basename "$0")

  # Deploy with custom model and accelerator
  $(basename "$0") -m unsloth/Meta-Llama-3.1-8B -a A100

  # Deploy with custom release name (for multi-install support)
  $(basename "$0") -r my-wva-release

  # Deploy infra-only mode (for e2e testing)
  $(basename "$0") --infra-only
  # Or with environment variable
  INFRA_ONLY=true $(basename "$0")
EOF
}

# Used to check if the environment variable is in a list
containsElement () {
  local e match="$1"
  shift
  for e; do [[ "$e" == "$match" ]] && return 0; done
  return 1
}

parse_args() {
  # Check for IMG environment variable (used by Make)
  if [[ -n "$IMG" ]]; then
    log_info "Detected IMG environment variable: $IMG"
    # Split image into repo and tag
    if [[ "$IMG" == *":"* ]]; then
      IFS=':' read -r WVA_IMAGE_REPO WVA_IMAGE_TAG <<< "$IMG"
    else
      log_warning "IMG has wrong format, using default image"
    fi
  fi

  # Parse command-line arguments
  while [[ $# -gt 0 ]]; do
    case "$1" in
      -i|--wva-image)
        # Split image into repo and tag - overrides IMG env var
        if [[ "$2" == *":"* ]]; then
          IFS=':' read -r WVA_IMAGE_REPO WVA_IMAGE_TAG <<< "$2"
        else
          WVA_IMAGE_REPO="$2"
        fi
        shift 2
        ;;
      -m|--model)             MODEL_ID="$2"; shift 2 ;;
      -a|--accelerator)       ACCELERATOR_TYPE="$2"; shift 2 ;;
      -r|--release-name)      WVA_RELEASE_NAME="$2"; shift 2 ;;
      --infra-only)           INFRA_ONLY=true; shift ;;
      -u|--undeploy)          UNDEPLOY=true; shift ;;
      -e|--environment)
        ENVIRONMENT="$2" ; shift 2
        if ! containsElement "$ENVIRONMENT" "${COMPATIBLE_ENV_LIST[@]}"; then
          log_error "Invalid environment: $ENVIRONMENT. Valid options are: ${COMPATIBLE_ENV_LIST[*]}"
        fi
        ;;
      -h|--help)              print_help; exit 0 ;;
      *)                      log_error "Unknown option: $1"; print_help; exit 1 ;;
    esac
  done
}

check_prerequisites() {
    log_info "Checking prerequisites..."

    local missing_tools=()

    # Check for required tools
    for tool in "${REQUIRED_TOOLS[@]}"; do
        if ! command -v $tool &> /dev/null; then
            missing_tools+=($tool)
        fi
    done

    if [ ${#missing_tools[@]} -ne 0 ]; then
        log_warning "Missing required tools: ${missing_tools[*]}"
        if [ "$E2E_TESTS_ENABLED" == "false" ]; then
            prompt_install_missing_tools
        else
            log_info "E2E tests enabled - will install missing tools by default"
        fi
    fi

    log_success "All generic prerequisites tools met"
}

prompt_install_missing_tools() {
    echo ""
    log_info "The client tools are required to install and manage the infrastructure."
    echo "  You can either:"
    echo "      1. Install them manually."
        echo "      2. Let the script attempt to install them for you."
        echo "The environment is currently set to: ${ENVIRONMENT}"

        while true; do
        read -p "Do you want to install the required client tools? (y/n): " -r answer
        case $answer in
            [Yy]* )
                INSTALL_CLIENT_TOOLS="true"
                log_success "Will install client tools when deploying llm-d"
                break
                ;;
            [Nn]* )
                INSTALL_CLIENT_TOOLS="false"
                log_warning "Will not install the required client tools. Please install them manually."
                break
                ;;
            * )
                echo "Please answer y (yes) or n (no)."
                ;;
        esac
    done
}

detect_gpu_type() {
    log_info "Detecting GPU type in cluster..."

    # Check if GPUs are visible
    local gpu_count=$(kubectl get nodes -o json | jq -r '.items[].status.allocatable["nvidia.com/gpu"]' | grep -v null | head -1)

    if [ -z "$gpu_count" ] || [ "$gpu_count" == "null" ]; then
        log_warning "No GPUs visible"
        log_warning "GPUs may exist on host but need NVIDIA Device Plugin or GPU Operator"

        # Check if GPUs exist on host
        if nvidia-smi &> /dev/null; then
            log_info "nvidia-smi detected GPUs on host:"
            nvidia-smi --query-gpu=name,memory.total --format=csv,noheader | head -5
            log_warning "Install NVIDIA GPU Operator"
        else
            log_warning "No GPUs detected on host either"
            log_info "Setting DEPLOY_LLM_D_INFERENCE_SIM=true for demo mode"
            DEPLOY_LLM_D_INFERENCE_SIM=true
        fi
    else
        log_success "GPUs visible: $gpu_count GPU(s) per node"

        # Detect GPU type from labels
        local gpu_product=$(kubectl get nodes -o json | jq -r '.items[] | select(.status.allocatable["nvidia.com/gpu"] != null) | .metadata.labels["nvidia.com/gpu.product"]' | head -1)

        if [ -n "$gpu_product" ]; then
            log_success "Detected GPU: $gpu_product"

            # Map GPU product to accelerator type
            case "$gpu_product" in
                *H100*)
                    ACCELERATOR_TYPE="H100"
                    ;;
                *A100*)
                    ACCELERATOR_TYPE="A100"
                    ;;
                *L40S*)
                    ACCELERATOR_TYPE="L40S"
                    ;;
                *)
                    log_warning "Unknown GPU type: $gpu_product, using default: $ACCELERATOR_TYPE"
                    ;;
            esac
        fi
    fi

    export ACCELERATOR_TYPE
    export DEPLOY_LLM_D_INFERENCE_SIM
    log_info "Using detected accelerator type: $ACCELERATOR_TYPE"
}

prompt_gateway_installation() {
    echo ""
    log_info "Gateway Control Plane Configuration"
    echo -e "${BLUE}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo ""
    echo "The Gateway control plane (${GATEWAY_PROVIDER}) is required to serve requests."
    echo "You can either:"
    echo "  1. Install the Gateway control plane (recommended for new clusters or emulated clusters)"
    echo "  2. Use an existing Gateway control plane in your cluster (recommended for production clusters)"
    echo "The environment is currently set to: ${ENVIRONMENT}"

    while true; do
        read -p "Do you want to install the Gateway control plane? (y/n): " -r answer
        case $answer in
            [Yy]* )
                INSTALL_GATEWAY_CTRLPLANE="true"
                log_success "Will install Gateway control plane ($GATEWAY_PROVIDER) when deploying llm-d"
                break
                ;;
            [Nn]* )
                INSTALL_GATEWAY_CTRLPLANE="false"
                log_info "Will attempt to use existing Gateway control plane when deploying llm-d"
                break
                ;;
            * )
                echo "Please answer y (yes) or n (no)."
                ;;
        esac
    done

    export INSTALL_GATEWAY_CTRLPLANE
    echo ""
}

set_tls_verification() {
    log_info "Setting TLS verification..."

    # Auto-detect TLS verification setting if not specified
    if ! containsElement "$ENVIRONMENT" "${NON_EMULATED_ENV_LIST[@]}"; then
            SKIP_TLS_VERIFY="true"
            log_info "Emulated environment detected - enabling TLS skip verification for self-signed certificates"
    else
        case "$ENVIRONMENT" in
            "kubernetes")
                # TODO: change to false when Kubernetes support for TLS verification is enabled
                SKIP_TLS_VERIFY="true"
                log_info "Kubernetes cluster - enabling TLS skip verification for self-signed certificates"
                ;;
            "openshift")
                # For OpenShift, we can use proper TLS verification since we have the Service CA
                # However, defaulting to true for now to match current behavior
                # TODO: Set to false once Service CA certificate extraction is fully validated
                SKIP_TLS_VERIFY="true"
                log_info "OpenShift cluster - TLS verification setting: $SKIP_TLS_VERIFY"
                ;;
            *)
                SKIP_TLS_VERIFY="true"
                log_warning "Unknown environment - enabling TLS skip verification for self-signed certificates"
                ;;
        esac
    fi

    export SKIP_TLS_VERIFY

    log_success "Successfully set TLS verification to: $SKIP_TLS_VERIFY"
}

set_wva_logging_level() {
    log_info "Setting WVA logging level..."

    # Set logging level based on environment
    if ! containsElement "$ENVIRONMENT" "${NON_EMULATED_ENV_LIST[@]}"; then
        WVA_LOG_LEVEL="debug"
        log_info "Development environment - using debug logging"
    else
        WVA_LOG_LEVEL="info"
        log_info "Production environment - using info logging"
    fi

    export WVA_LOG_LEVEL
    log_success "WVA logging level set to: $WVA_LOG_LEVEL"
    echo ""
}

deploy_wva_controller() {
    log_info "Deploying Workload-Variant-Autoscaler..."
    log_info "Using image: $WVA_IMAGE_REPO:$WVA_IMAGE_TAG"
    log_info "Using release name: $WVA_RELEASE_NAME"

    # Deploy WVA using Helm chart
    log_info "Installing Workload-Variant-Autoscaler via Helm chart"

    # Default namespaceScoped to true if not set (matches chart default)
    # But allow override via env var (e.g. for E2E tests)
    NAMESPACE_SCOPED=${NAMESPACE_SCOPED:-true}

    helm upgrade -i "$WVA_RELEASE_NAME" ${WVA_PROJECT}/charts/workload-variant-autoscaler \
        -n $WVA_NS \
        --values $VALUES_FILE \
        --set-file wva.prometheus.caCert=$PROM_CA_CERT_PATH \
        --set wva.image.repository=$WVA_IMAGE_REPO \
        --set wva.image.tag=$WVA_IMAGE_TAG \
        --set wva.imagePullPolicy=$WVA_IMAGE_PULL_POLICY \
        --set wva.baseName=$WELL_LIT_PATH_NAME \
        --set llmd.modelName=$LLM_D_MODELSERVICE_NAME \
        --set va.enabled=$DEPLOY_VA \
        --set va.accelerator=$ACCELERATOR_TYPE \
        --set llmd.modelID=$MODEL_ID \
        --set va.sloTpot=$SLO_TPOT \
        --set va.sloTtft=$SLO_TTFT \
        --set hpa.enabled=$DEPLOY_HPA \
        --set hpa.minReplicas=$HPA_MIN_REPLICAS \
        --set hpa.behavior.scaleUp.stabilizationWindowSeconds=$HPA_STABILIZATION_SECONDS \
        --set hpa.behavior.scaleDown.stabilizationWindowSeconds=$HPA_STABILIZATION_SECONDS \
        --set llmd.namespace=$LLMD_NS \
        --set wva.prometheus.baseURL=$PROMETHEUS_URL \
        --set wva.prometheus.monitoringNamespace=$MONITORING_NAMESPACE \
        --set vllmService.enabled=$VLLM_SVC_ENABLED \
        --set vllmService.port=$VLLM_SVC_PORT \
        --set vllmService.targetPort=$VLLM_SVC_PORT \
        --set vllmService.nodePort=$VLLM_SVC_NODEPORT \
        --set wva.logging.level=$WVA_LOG_LEVEL \
        --set wva.prometheus.tls.insecureSkipVerify=$SKIP_TLS_VERIFY \
        --set wva.namespaceScoped=$NAMESPACE_SCOPED \
        --set wva.metrics.secure=$WVA_METRICS_SECURE \
        ${CONTROLLER_INSTANCE:+--set wva.controllerInstance=$CONTROLLER_INSTANCE} \
        ${KV_SPARE_TRIGGER:+--set wva.capacityScaling.default.kvSpareTrigger=$KV_SPARE_TRIGGER} \
        ${QUEUE_SPARE_TRIGGER:+--set wva.capacityScaling.default.queueSpareTrigger=$QUEUE_SPARE_TRIGGER}

    # Wait for WVA to be ready
    log_info "Waiting for WVA controller to be ready..."
    kubectl wait --for=condition=Ready pod -l app.kubernetes.io/name=workload-variant-autoscaler -n $WVA_NS --timeout=60s || \
        log_warning "WVA controller is not ready yet - check 'kubectl get pods -n $WVA_NS'"

    log_success "WVA deployment complete"
}

# Deploy second model infrastructure for multi-model/limiter testing
# Creates a second InferencePool, modelservice deployment, and updates HTTPRoute
deploy_second_model_infrastructure() {
    log_info "Deploying second model infrastructure for multi-model testing..."
    log_info "Second model: $MODEL_ID_2"

    local POOL_NAME_2="gaie-sim-2"
    local MS_NAME_2="ms-sim-2"
    local MODEL_LABEL_2="model-2"
    # Sanitize model name for use in Kubernetes labels (replace / with -)
    local MODEL_ID_2_SANITIZED=$(echo "$MODEL_ID_2" | tr '/' '-')

    # Create second InferencePool with different selector
    log_info "Creating second InferencePool: $POOL_NAME_2"
    cat <<EOF | kubectl apply -n $LLMD_NS -f -
apiVersion: inference.networking.x-k8s.io/v1alpha2
kind: InferencePool
metadata:
  name: $POOL_NAME_2
spec:
  targetPortNumber: 8000
  selector:
    llm-d.ai/model-pool: "$MODEL_LABEL_2"
  extensionRef:
    name: ${POOL_NAME_2}-epp
EOF

    # Create EPP deployment for second pool
    log_info "Creating EPP deployment for second pool"
    cat <<EOF | kubectl apply -n $LLMD_NS -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${POOL_NAME_2}-epp
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ${POOL_NAME_2}-epp
  template:
    metadata:
      labels:
        app: ${POOL_NAME_2}-epp
    spec:
      serviceAccountName: gaie-sim-sa
      containers:
      - name: epp
        image: ghcr.io/llm-d/llm-d-inference-scheduler:v0.3.2
        imagePullPolicy: Always
        args:
        - --poolName=$POOL_NAME_2
        - --poolNamespace=$LLMD_NS
        - --extProcPort=9002
        - --grpcHealthPort=9003
        ports:
        - containerPort: 9002
          name: grpc
        - containerPort: 9003
          name: grpc-health
        - containerPort: 9090
          name: metrics
        readinessProbe:
          grpc:
            port: 9003
          initialDelaySeconds: 5
          periodSeconds: 10
        livenessProbe:
          grpc:
            port: 9003
          initialDelaySeconds: 15
          periodSeconds: 20
---
apiVersion: v1
kind: Service
metadata:
  name: ${POOL_NAME_2}-epp
spec:
  selector:
    app: ${POOL_NAME_2}-epp
  ports:
  - name: grpc
    port: 9002
    targetPort: 9002
  - name: grpc-health
    port: 9003
    targetPort: 9003
  - name: metrics
    port: 9090
    targetPort: 9090
EOF

    # Wait for second EPP to be ready
    log_info "Waiting for second EPP deployment to be ready..."
    kubectl wait --for=condition=Available deployment/${POOL_NAME_2}-epp -n $LLMD_NS --timeout=120s || \
        log_warning "Second EPP deployment not ready yet - check 'kubectl get pods -n $LLMD_NS -l app=${POOL_NAME_2}-epp'"

    # Create second modelservice deployment (using llm-d-inference-sim)
    log_info "Creating second modelservice deployment: $MS_NAME_2"
    cat <<EOF | kubectl apply -n $LLMD_NS -f -
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${MS_NAME_2}-decode
spec:
  replicas: 2
  selector:
    matchLabels:
      app: ${MS_NAME_2}-decode
      llm-d.ai/model-pool: "$MODEL_LABEL_2"
  template:
    metadata:
      labels:
        app: ${MS_NAME_2}-decode
        llm-d.ai/model-pool: "$MODEL_LABEL_2"
        llm-d.ai/model: "${MODEL_ID_2_SANITIZED}"
    spec:
      containers:
      - name: vllm
        image: ghcr.io/llm-d/llm-d-inference-sim:v0.5.1
        imagePullPolicy: Always
        args:
        - --model=$MODEL_ID_2
        - --time-to-first-token=$TTFT_AVERAGE_LATENCY_MS
        - --inter-token-latency=$ITL_AVERAGE_LATENCY_MS
        - --enable-kvcache
        - --kv-cache-size=1024
        - --block-size=16
        ports:
        - containerPort: 8000
          name: http
        - containerPort: 8200
          name: metrics
        env:
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
        readinessProbe:
          httpGet:
            path: /health
            port: 8000
          periodSeconds: 5
---
apiVersion: v1
kind: Service
metadata:
  name: ${MS_NAME_2}-decode
  labels:
    llm-d.ai/model-pool: "$MODEL_LABEL_2"
spec:
  selector:
    app: ${MS_NAME_2}-decode
  ports:
  - name: http
    port: 8000
    targetPort: 8000
  - name: metrics
    port: 8200
    targetPort: 8200
EOF

    # Create InferenceModel for second model (maps model name to pool)
    # Note: InferenceModel CRD may not be available in all environments
    if kubectl get crd inferencemodels.inference.networking.x-k8s.io &>/dev/null; then
        log_info "Creating InferenceModel for second model"
        cat <<EOF | kubectl apply -n $LLMD_NS -f -
apiVersion: inference.networking.x-k8s.io/v1alpha2
kind: InferenceModel
metadata:
  name: ${MS_NAME_2}-model
spec:
  modelName: $MODEL_ID_2
  criticality: Critical
  poolRef:
    name: $POOL_NAME_2
  targetModels:
  - name: $MODEL_ID_2
    weight: 100
EOF
    else
        log_warning "InferenceModel CRD not available - skipping InferenceModel creation for second model"
        log_warning "Model routing may need to be configured manually or via HTTPRoute"
    fi

    # Create PodMonitor for second model metrics
    log_info "Creating PodMonitor for second model"
    cat <<EOF | kubectl apply -n $LLMD_NS -f -
apiVersion: monitoring.coreos.com/v1
kind: PodMonitor
metadata:
  name: ${MS_NAME_2}-podmonitor
  labels:
    release: kube-prometheus-stack
spec:
  selector:
    matchLabels:
      app: ${MS_NAME_2}-decode
  podMetricsEndpoints:
  - port: metrics
    path: /metrics
    interval: 15s
EOF

    # Wait for second model deployment to be ready
    log_info "Waiting for second model deployment to be ready..."
    kubectl wait --for=condition=Available deployment/${MS_NAME_2}-decode -n $LLMD_NS --timeout=120s || \
        log_warning "Second model deployment not ready yet - check 'kubectl get pods -n $LLMD_NS'"

    log_success "Second model infrastructure deployed successfully"
}

deploy_llm_d_infrastructure() {
    log_info "Deploying llm-d infrastructure..."

     # Clone llm-d repo if not exists
    if [ ! -d "$LLM_D_PROJECT" ]; then
        log_info "Cloning $LLM_D_PROJECT repository (release: $LLM_D_RELEASE)"
        git clone -b $LLM_D_RELEASE -- https://github.com/$LLM_D_OWNER/$LLM_D_PROJECT.git $LLM_D_PROJECT &> /dev/null
    else
        log_warning "$LLM_D_PROJECT directory already exists, skipping clone"
    fi

    # Check for HF_TOKEN (use dummy for emulated deployments)
    if [ -z "$HF_TOKEN" ]; then
        if ! containsElement "$ENVIRONMENT" "${NON_EMULATED_ENV_LIST[@]}"; then
            log_warning "HF_TOKEN not set - using dummy token for emulated deployment"
            export HF_TOKEN="dummy-token"
        else
            log_error "HF_TOKEN is required for non-emulated deployments. Please set HF_TOKEN and try again."
        fi
    fi

    # Create HF token secret
    log_info "Creating HuggingFace token secret"
    kubectl create secret generic llm-d-hf-token \
        --from-literal="HF_TOKEN=${HF_TOKEN}" \
        --namespace "${LLMD_NS}" \
        --dry-run=client -o yaml | kubectl apply -f -

    # Install dependencies
    log_info "Installing llm-d dependencies"
    bash $CLIENT_PREREQ_DIR/install-deps.sh

    # On OpenShift, skip base Gateway API CRDs (managed by Ingress Operator via
    # ValidatingAdmissionPolicy "openshift-ingress-operator-gatewayapi-crd-admission").
    # Only install Gateway API Inference Extension (GAIE) CRDs directly.
    if [[ "$ENVIRONMENT" == "openshift" ]]; then
        log_info "Skipping Gateway API base CRDs on OpenShift (managed by Ingress Operator)"
        GAIE_CRD_REV=${GATEWAY_API_INFERENCE_EXTENSION_CRD_REVISION:-"v1.3.0"}
        log_info "Installing Gateway API Inference Extension CRDs (${GAIE_CRD_REV})"
        kubectl apply -k "https://github.com/kubernetes-sigs/gateway-api-inference-extension/config/crd/?ref=${GAIE_CRD_REV}" \
            && log_success "GAIE CRDs installed" \
            || log_warning "Failed to install GAIE CRDs (may already exist or network issue)"
    else
        bash $GATEWAY_PREREQ_DIR/install-gateway-provider-dependencies.sh
    fi

    # Install Gateway provider (if kgateway, use v2.0.3)
    if [ "$GATEWAY_PROVIDER" == "kgateway" ]; then
        log_info "Installing $GATEWAY_PROVIDER v2.0.3"
        yq eval '.releases[].version = "v2.0.3"' -i "$GATEWAY_PREREQ_DIR/$GATEWAY_PROVIDER.helmfile.yaml"
    fi

    # Install Gateway control plane if enabled
    if [[ "$INSTALL_GATEWAY_CTRLPLANE" == "true" ]]; then
        log_info "Installing Gateway control plane ($GATEWAY_PROVIDER)"
        helmfile apply -f "$GATEWAY_PREREQ_DIR/$GATEWAY_PROVIDER.helmfile.yaml"
    else
        log_info "Skipping Gateway control plane installation (INSTALL_GATEWAY_CTRLPLANE=false)"
    fi

    # Configuring llm-d before installation
    cd $EXAMPLE_DIR
    log_info "Configuring llm-d infrastructure"

    # Detect the actual default model from the values file (not the hardcoded DEFAULT_MODEL_ID)
    ACTUAL_DEFAULT_MODEL=$(yq eval '.modelArtifacts.name' "$LLM_D_MODELSERVICE_VALUES" 2>/dev/null || echo "$DEFAULT_MODEL_ID")
    if [ -z "$ACTUAL_DEFAULT_MODEL" ] || [ "$ACTUAL_DEFAULT_MODEL" == "null" ]; then
        ACTUAL_DEFAULT_MODEL="$DEFAULT_MODEL_ID"
    fi

    # Update model ID if different from the guide's actual default
    if [ "$MODEL_ID" != "$ACTUAL_DEFAULT_MODEL" ] ; then
        log_info "Updating deployment to use model: $MODEL_ID (replacing guide default: $ACTUAL_DEFAULT_MODEL)"
        yq eval "(.. | select(. == \"$ACTUAL_DEFAULT_MODEL\")) = \"$MODEL_ID\" | (.. | select(. == \"hf://$ACTUAL_DEFAULT_MODEL\")) = \"hf://$MODEL_ID\"" -i "$LLM_D_MODELSERVICE_VALUES"

        # Increase model-storage volume size
        log_info "Increasing model-storage volume size for model: $MODEL_ID"
        yq eval '.modelArtifacts.size = "100Gi"' -i "$LLM_D_MODELSERVICE_VALUES"
    else
        log_info "Model ID matches guide default ($ACTUAL_DEFAULT_MODEL), no replacement needed"
    fi

    # Configure llm-d-inference-simulator if needed
    if [ "$DEPLOY_LLM_D_INFERENCE_SIM" == "true" ]; then
      log_info "Deploying llm-d-inference-simulator..."
        yq eval ".decode.containers[0].image = \"$LLM_D_INFERENCE_SIM_IMG_REPO:$LLM_D_INFERENCE_SIM_IMG_TAG\" | \
                 .prefill.containers[0].image = \"$LLM_D_INFERENCE_SIM_IMG_REPO:$LLM_D_INFERENCE_SIM_IMG_TAG\" | \
                 .decode.containers[0].args = [\"--time-to-first-token=$TTFT_AVERAGE_LATENCY_MS\", \"--inter-token-latency=$ITL_AVERAGE_LATENCY_MS\"] | \
                 .prefill.containers[0].args = [\"--time-to-first-token=$TTFT_AVERAGE_LATENCY_MS\", \"--inter-token-latency=$ITL_AVERAGE_LATENCY_MS\"]" \
                 -i "$LLM_D_MODELSERVICE_VALUES"
    else
        log_info "Skipping llm-d-inference-simulator deployment (DEPLOY_LLM_D_INFERENCE_SIM=false)"
    fi

    # Configure vLLM max-num-seqs if set (useful for e2e testing to force saturation)
    if [ -n "$VLLM_MAX_NUM_SEQS" ]; then
      log_info "Setting vLLM max-num-seqs to $VLLM_MAX_NUM_SEQS for decode containers"
      yq eval ".decode.containers[0].args += [\"--max-num-seqs=$VLLM_MAX_NUM_SEQS\"]" -i "$LLM_D_MODELSERVICE_VALUES"
    fi

    # Configure decode replicas if set (useful for e2e testing with limited GPUs)
    if [ -n "$DECODE_REPLICAS" ]; then
      log_info "Setting decode replicas to $DECODE_REPLICAS"
      yq eval ".decode.replicas = $DECODE_REPLICAS" -i "$LLM_D_MODELSERVICE_VALUES"
    fi

    # Check if the guide's llm-d.ai/model label differs from what WVA's vllm-service expects.
    # If so, we'll patch pod labels post-deploy (not pre-deploy) to avoid violating the
    # llm-d-modelservice chart schema which disallows extra properties under modelArtifacts.
    CURRENT_MODEL_LABEL=$(yq eval '.modelArtifacts.labels."llm-d.ai/model"' "$LLM_D_MODELSERVICE_VALUES" 2>/dev/null || echo "")
    NEEDS_LABEL_ALIGNMENT=false
    if [ -n "$CURRENT_MODEL_LABEL" ] && [ "$CURRENT_MODEL_LABEL" != "null" ] && [ "$CURRENT_MODEL_LABEL" != "$LLM_D_MODELSERVICE_NAME" ]; then
      log_info "Will align llm-d.ai/model label post-deploy: '$CURRENT_MODEL_LABEL' -> '$LLM_D_MODELSERVICE_NAME'"
      NEEDS_LABEL_ALIGNMENT=true
    fi

    # Auto-detect vLLM port from guide configuration and update WVA vllm-service.
    # When routing proxy is disabled, vLLM serves directly on containerPort (typically 8000).
    # When proxy is enabled, vLLM serves on proxy.targetPort (typically 8200).
    PROXY_ENABLED=$(yq eval '.routing.proxy.enabled // true' "$LLM_D_MODELSERVICE_VALUES" 2>/dev/null || echo "true")
    if [ "$PROXY_ENABLED" == "false" ]; then
      DETECTED_PORT=$(yq eval '.decode.containers[0].ports[0].containerPort // 8000' "$LLM_D_MODELSERVICE_VALUES" 2>/dev/null || echo "8000")
      if [ "$VLLM_SVC_PORT" != "$DETECTED_PORT" ]; then
        log_info "Routing proxy disabled - updating vLLM service port: $VLLM_SVC_PORT -> $DETECTED_PORT"
        VLLM_SVC_PORT=$DETECTED_PORT
        # Update the WVA vllm-service port (WVA was deployed before llm-d infra)
        if [ "$DEPLOY_WVA" == "true" ] && [ "$VLLM_SVC_ENABLED" == "true" ]; then
          helm upgrade "$WVA_RELEASE_NAME" ${WVA_PROJECT}/charts/workload-variant-autoscaler \
            -n $WVA_NS --reuse-values \
            --set vllmService.port=$VLLM_SVC_PORT \
            --set vllmService.targetPort=$VLLM_SVC_PORT
        fi
      fi
    fi

    # Deploy llm-d core components
    log_info "Deploying llm-d core components"
    # When DEPLOY_WVA is true, skip WVA in helmfile — install.sh deploys it
    # separately using the local chart (supports dev/test of chart changes).
    # The helmfile's WVA release uses the published OCI chart which may not
    # have the latest fixes and uses KIND-specific defaults (e.g. monitoringNamespace).
    local helmfile_selector=""
    if [ "$DEPLOY_WVA" == "true" ]; then
      helmfile_selector="--selector kind!=autoscaling"
      log_info "Skipping WVA in helmfile (will be deployed separately from local chart)"
    fi
    helmfile apply -e $GATEWAY_PROVIDER -n ${LLMD_NS} $helmfile_selector

    # Post-deploy: align the WVA vllm-service selector and ServiceMonitor to match
    # the actual pod labels. The llm-d-modelservice chart sets pod labels from
    # modelArtifacts.labels (e.g. "Qwen3-32B"), but the WVA chart's Service selector
    # uses llmd.modelName (e.g. "ms-inference-scheduling-llm-d-modelservice").
    # We patch the Service/ServiceMonitor selectors (which ARE mutable) rather than
    # the deployment labels (which have immutable selectors).
    if [ "$NEEDS_LABEL_ALIGNMENT" == "true" ]; then
      # Compute the chart fullname (mirrors _helpers.tpl logic)
      local chart_name="workload-variant-autoscaler"
      local wva_fullname
      if echo "$WVA_RELEASE_NAME" | grep -q "$chart_name"; then
        wva_fullname="$WVA_RELEASE_NAME"
      else
        wva_fullname="${WVA_RELEASE_NAME}-${chart_name}"
      fi
      wva_fullname=$(echo "$wva_fullname" | cut -c1-63 | sed 's/-$//')
      local svc_name="${wva_fullname}-vllm"
      local svcmon_name="${wva_fullname}-vllm-mon"
      log_info "Aligning WVA Service/ServiceMonitor selectors: llm-d.ai/model=$CURRENT_MODEL_LABEL"
      # Patch Service selector
      kubectl patch service "$svc_name" -n "$LLMD_NS" --type=merge -p "{
        \"spec\": {\"selector\": {\"llm-d.ai/model\": \"$CURRENT_MODEL_LABEL\"}}
      }" && log_success "Patched Service $svc_name selector" \
         || log_warning "Failed to patch Service $svc_name selector"
      # Patch ServiceMonitor matchLabels
      kubectl patch servicemonitor "$svcmon_name" -n "$LLMD_NS" --type=merge -p "{
        \"spec\": {\"selector\": {\"matchLabels\": {\"llm-d.ai/model\": \"$CURRENT_MODEL_LABEL\"}}}
      }" && log_success "Patched ServiceMonitor $svcmon_name selector" \
         || log_warning "Failed to patch ServiceMonitor $svcmon_name selector"
      # Also patch the Service labels so the ServiceMonitor can find it
      kubectl label service "$svc_name" -n "$LLMD_NS" "llm-d.ai/model=$CURRENT_MODEL_LABEL" --overwrite \
        && log_success "Patched Service $svc_name label" \
        || log_warning "Failed to patch Service $svc_name label"
    fi

    # Apply HTTPRoute with correct resource name references.
    # The static httproute.yaml uses resource names matching the helmfile's default
    # RELEASE_NAME_POSTFIX (e.g. "workload-autoscaler"). When RELEASE_NAME_POSTFIX
    # is overridden (e.g. in CI), gateway and InferencePool names change, so we
    # must template the HTTPRoute references to match the actual deployed resources.
    # RELEASE_NAME_POSTFIX is set by the reusable nightly workflow
    # (llm-d-infra reusable-nightly-e2e-openshift.yaml) via the guide_name input.
    if [ -f httproute.yaml ]; then
        local rn="${RELEASE_NAME_POSTFIX:-}"
        if [ -n "$rn" ]; then
            local gw_name="infra-${rn}-inference-gateway"
            local pool_name="gaie-${rn}"
            log_info "Applying HTTPRoute (gateway=$gw_name, pool=$pool_name)"
            if ! yq eval "
                .spec.parentRefs[0].name = \"${gw_name}\" |
                .spec.rules[0].backendRefs[0].name = \"${pool_name}\"
            " httproute.yaml | kubectl apply -f - -n ${LLMD_NS}; then
                log_error "Failed to apply templated HTTPRoute for gateway=${gw_name}, pool=${pool_name}"
                exit 1
            fi
        else
            if ! kubectl apply -f httproute.yaml -n ${LLMD_NS}; then
                log_error "Failed to apply HTTPRoute from httproute.yaml"
                exit 1
            fi
        fi
    fi

    # Patch llm-d-inference-scheduler deployment if scale-to-zero is enabled
    if [ "$ENABLE_SCALE_TO_ZERO" == "true" ]; then
        # Patch llm-d-inference-scheduler to enable flowcontrol and use new image
        log_info "Patching llm-d-inference-scheduler deployment to enable flowcontrol and use a new image"
        if kubectl get deployment "$LLM_D_EPP_NAME" -n "$LLMD_NS" &> /dev/null; then
            kubectl patch deployment "$LLM_D_EPP_NAME" -n "$LLMD_NS" --type='json' -p='[
                {
                    "op": "replace",
                    "path": "/spec/template/spec/containers/0/image",
                    "value": "'$LLM_D_INFERENCE_SCHEDULER_IMG'"
                },
                {
                    "op": "add",
                    "path": "/spec/template/spec/containers/0/env/-",
                    "value": {
                    "name": "ENABLE_EXPERIMENTAL_FLOW_CONTROL_LAYER",
                    "value": "true"
                    }
                }
            ]'
        else
            log_warning "Skipping inference-scheduler patch for SCALE_TO_ZERO: Deployment $LLM_D_EPP_NAME not found in $LLMD_NS"
        fi
    fi

    log_info "Waiting for llm-d components to initialize..."
    kubectl wait --for=condition=Available deployment --all -n $LLMD_NS --timeout=60s || \
        log_warning "llm-d components are not ready yet - check 'kubectl get pods -n $LLMD_NS'"

    # Deploy second model infrastructure for multi-model testing (limiter e2e tests)
    if [ "$MULTI_MODEL_TESTING" == "true" ]; then
        deploy_second_model_infrastructure
    fi

    cd "$WVA_PROJECT"
    log_success "llm-d infrastructure deployment complete"
}

deploy_keda() {
    log_info "Deploying KEDA (scaler backend)..."

    kubectl create namespace "$KEDA_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

    helm repo add kedacore https://kedacore.github.io/charts 2>/dev/null || true
    helm repo update

    if ! helm upgrade -i keda kedacore/keda \
        -n "$KEDA_NAMESPACE" \
        --set prometheus.metricServer.enabled=true \
        --set prometheus.operator.enabled=true \
        --wait \
        --timeout=5m; then
        if [ "$E2E_TESTS_ENABLED" = "true" ]; then
            log_error "KEDA Helm installation failed - required for E2E tests with SCALER_BACKEND=keda"
        else
            log_warning "KEDA Helm installation failed, but continuing..."
        fi
    else
        log_success "KEDA deployed in $KEDA_NAMESPACE"
    fi
}

deploy_prometheus_adapter() {
    log_info "Deploying Prometheus Adapter..."

    # Add Prometheus community helm repo
    log_info "Adding Prometheus community helm repo"
    helm repo add prometheus-community https://prometheus-community.github.io/helm-charts || true
    helm repo update

    # Create prometheus-ca ConfigMap from the CA certificate
    log_info "Creating prometheus-ca ConfigMap for Prometheus Adapter"
    if [ ! -f "$PROM_CA_CERT_PATH" ] || [ ! -s "$PROM_CA_CERT_PATH" ]; then
        log_error "CA certificate file not found or empty: $PROM_CA_CERT_PATH"
        log_error "Please ensure deploy_wva_prerequisites() was called first"
        exit 1
    fi

    # Create or update the prometheus-ca ConfigMap
    kubectl create configmap prometheus-ca \
        --from-file=ca.crt=$PROM_CA_CERT_PATH \
        -n $MONITORING_NAMESPACE \
        --dry-run=client -o yaml | kubectl apply -f -

    log_success "prometheus-ca ConfigMap created/updated"

    # Use existing values files from config/samples
    local values_file=""
    if [ "$ENVIRONMENT" = "openshift" ]; then
        values_file="${WVA_PROJECT}/config/samples/prometheus-adapter-values-ocp.yaml"
        log_info "Using OpenShift-specific Prometheus Adapter configuration: $values_file"
    else
        values_file="${WVA_PROJECT}/config/samples/prometheus-adapter-values.yaml"
        log_info "Using Kubernetes Prometheus Adapter configuration: $values_file"
    fi

    if [ ! -f "$values_file" ]; then
        log_error "Prometheus Adapter values file not found: $values_file"
        exit 1
    fi

    # Deploy Prometheus Adapter using existing values file and override URL/port
    log_info "Installing Prometheus Adapter via Helm"
    
    # In CI/E2E mode, skip --wait to avoid hanging, then verify separately
    # For local dev, use --wait for immediate feedback
    local wait_flag=""
    if [ "${PROMETHEUS_ADAPTER_WAIT:-true}" = "true" ]; then
        wait_flag="--wait"
        log_info "Using --wait flag (will wait for Prometheus Adapter to be ready)"
    else
        log_info "Skipping --wait flag (will verify status separately)"
    fi
    
    if ! helm upgrade -i prometheus-adapter prometheus-community/prometheus-adapter \
        -n $MONITORING_NAMESPACE \
        -f "$values_file" \
        --set prometheus.url="$PROMETHEUS_BASE_URL" \
        --set prometheus.port="$PROMETHEUS_PORT" \
        --timeout=3m \
        $wait_flag; then
        if [ "$E2E_TESTS_ENABLED" = "true" ]; then
            log_error "Prometheus Adapter Helm installation failed - required for E2E tests"
        else
            log_warning "Prometheus Adapter Helm installation failed, but continuing..."
            log_warning "HPA may not work until adapter is healthy"
            log_info "Check adapter status: kubectl get pods -n $MONITORING_NAMESPACE | grep prometheus-adapter"
            log_info "Check adapter logs: kubectl logs -n $MONITORING_NAMESPACE deployment/prometheus-adapter"
        fi
    fi

    # If we skipped --wait (e.g., in CI), verify Prometheus Adapter is actually running
    if [ "${PROMETHEUS_ADAPTER_WAIT:-true}" != "true" ]; then
        log_info "Verifying Prometheus Adapter is running (skipped --wait, checking status)..."
        local max_attempts=12
        local attempt=1
        local adapter_ready=false
        
        while [ $attempt -le $max_attempts ]; do
            if kubectl get pods -n $MONITORING_NAMESPACE -l app.kubernetes.io/name=prometheus-adapter 2>/dev/null | grep -q Running; then
                adapter_ready=true
                break
            fi
            log_info "Waiting for Prometheus Adapter to be ready (attempt $attempt/$max_attempts)..."
            sleep 10
            attempt=$((attempt + 1))
        done
        
        if [ "$adapter_ready" = "true" ]; then
            log_success "Prometheus Adapter is running"
        else
            if [ "$E2E_TESTS_ENABLED" = "true" ]; then
                log_error "Prometheus Adapter failed to become ready after ${max_attempts} attempts - required for E2E tests"
            else
                log_warning "Prometheus Adapter may still be starting (not ready after ${max_attempts} attempts)"
                log_info "Check adapter status: kubectl get pods -n $MONITORING_NAMESPACE | grep prometheus-adapter"
            fi
        fi
    else
        log_success "Prometheus Adapter deployment completed"
    fi

    # On clusters with KEDA, the v1beta1.external.metrics.k8s.io APIService may
    # point to KEDA's metrics server instead of Prometheus Adapter. KEDA's server
    # only serves metrics for ScaledObjects, not arbitrary external metrics like
    # wva_desired_replicas. Detect and fix this conflict.
    # Only patch if the Prometheus Adapter service actually exists (i.e. helm install succeeded).
    if ! kubectl get service prometheus-adapter -n "$MONITORING_NAMESPACE" &>/dev/null; then
        log_warning "Prometheus Adapter service not found in $MONITORING_NAMESPACE — skipping APIService patch"
        log_warning "HPA may not work until Prometheus Adapter is deployed"
    elif kubectl get apiservice v1beta1.external.metrics.k8s.io &>/dev/null; then
        local current_svc current_ns
        current_svc=$(kubectl get apiservice v1beta1.external.metrics.k8s.io -o jsonpath='{.spec.service.name}' 2>/dev/null || echo "")
        current_ns=$(kubectl get apiservice v1beta1.external.metrics.k8s.io -o jsonpath='{.spec.service.namespace}' 2>/dev/null || echo "")

        if [ "$current_svc" = "prometheus-adapter" ] && [ "$current_ns" = "$MONITORING_NAMESPACE" ]; then
            log_info "external.metrics.k8s.io APIService already points to prometheus-adapter in $MONITORING_NAMESPACE"
        else
            log_warning "external.metrics.k8s.io APIService points to '$current_svc' in '$current_ns'"
            log_info "Patching APIService to point to Prometheus Adapter in $MONITORING_NAMESPACE"
            kubectl patch apiservice v1beta1.external.metrics.k8s.io --type=merge -p "{
                \"spec\": {
                    \"insecureSkipTLSVerify\": true,
                    \"service\": {
                        \"name\": \"prometheus-adapter\",
                        \"namespace\": \"$MONITORING_NAMESPACE\"
                    }
                }
            }" && log_success "APIService patched to use Prometheus Adapter" \
               || log_warning "Failed to patch external.metrics.k8s.io APIService — HPA may not work"
        fi

        # Start background guard to prevent KEDA from reclaiming the APIService.
        # KEDA's operator continuously reconciles the APIService back to its own
        # metrics server within ~2 minutes of any patch. The guard re-patches it
        # every 10 seconds without modifying KEDA itself.
        start_apiservice_guard "$MONITORING_NAMESPACE"
    else
        log_warning "external.metrics.k8s.io APIService not found — skipping patch"
    fi
}

verify_deployment() {
    log_info "Verifying deployment..."

    local all_good=true

    # Check WVA pods
    log_info "Checking WVA controller pods..."
    sleep 10
    if kubectl get pods -n $WVA_NS -l app.kubernetes.io/name=workload-variant-autoscaler 2>/dev/null | grep -q Running; then
        log_success "WVA controller is running"
    else
        log_warning "WVA controller may still be starting"
        all_good=false
    fi

    # Check Prometheus
    if [ "$DEPLOY_PROMETHEUS" = "true" ]; then
        log_info "Checking Prometheus..."
        if kubectl get pods -n $MONITORING_NAMESPACE -l app.kubernetes.io/name=prometheus 2>/dev/null | grep -q Running; then
            log_success "Prometheus is running"
        else
            log_warning "Prometheus may still be starting"
        fi
    fi

    # Check llm-d infrastructure
    if [ "$DEPLOY_LLM_D" = "true" ]; then
        log_info "Checking llm-d infrastructure..."
        if kubectl get deployment -n $LLMD_NS 2>/dev/null | grep -q gaie; then
            log_success "llm-d infrastructure deployed"
        else
            log_warning "llm-d infrastructure may still be deploying"
        fi
    fi

    # Check VariantAutoscaling deployed by WVA Helm chart
    if [ "$DEPLOY_VA" = "true" ]; then
        log_info "Checking VariantAutoscaling resource..."
        if kubectl get variantautoscaling -n $LLMD_NS &> /dev/null; then
            local va_count=$(kubectl get variantautoscaling -n $LLMD_NS --no-headers 2>/dev/null | wc -l)
            if [ "$va_count" -gt 0 ]; then
                log_success "VariantAutoscaling resource(s) found"
                kubectl get variantautoscaling -n $LLMD_NS -o wide
            fi
        else
            log_info "No VariantAutoscaling resources deployed yet (will be created by Helm chart)"
        fi
    fi

    # Check scaler backend (KEDA or Prometheus Adapter)
    if [ "$SCALER_BACKEND" = "keda" ]; then
        log_info "Checking KEDA..."
        if kubectl get pods -n "$KEDA_NAMESPACE" -l app.kubernetes.io/name=keda-operator 2>/dev/null | grep -q Running; then
            log_success "KEDA is running"
        else
            log_warning "KEDA may still be starting"
        fi
    elif [ "$DEPLOY_PROMETHEUS_ADAPTER" = "true" ]; then
        log_info "Checking Prometheus Adapter..."
        if kubectl get pods -n $MONITORING_NAMESPACE -l app.kubernetes.io/name=prometheus-adapter 2>/dev/null | grep -q Running; then
            log_success "Prometheus Adapter is running"
        else
            log_warning "Prometheus Adapter may still be starting"
        fi
    fi

    if [ "$all_good" = true ]; then
        log_success "All components verified successfully!"
    else
        log_warning "Some components may still be starting. Check the logs above."
    fi
}

print_summary() {
    echo ""
    echo "=========================================="
    echo " Deployment Summary"
    echo "=========================================="
    echo ""
    echo "Deployment Environment: $ENVIRONMENT"
    echo "WVA Namespace:          $WVA_NS"
    echo "LLMD Namespace:         $LLMD_NS"
    echo "Monitoring Namespace:   $MONITORING_NAMESPACE"
    echo "Model:                  $MODEL_ID"
    echo "Accelerator:            $ACCELERATOR_TYPE"
    echo "WVA Image:              $WVA_IMAGE_REPO:$WVA_IMAGE_TAG"
    echo "SLO (TPOT):             $SLO_TPOT ms"
    echo "SLO (TTFT):             $SLO_TTFT ms"
    echo ""
    echo "Deployed Components:"
    echo "===================="
    if [ "$DEPLOY_PROMETHEUS" = "true" ]; then
        echo "✓ kube-prometheus-stack (Prometheus + Grafana)"
    fi
    if [ "$DEPLOY_WVA" = "true" ]; then
        echo "✓ WVA Controller (via Helm chart)"
    fi
    if [ "$DEPLOY_LLM_D" = "true" ]; then
        echo "✓ llm-d Infrastructure (Gateway, GAIE, ModelService)"
    fi
    if [ "$SCALER_BACKEND" = "keda" ]; then
        echo "✓ KEDA (scaler backend, external metrics API)"
    elif [ "$DEPLOY_PROMETHEUS_ADAPTER" = "true" ]; then
        echo "✓ Prometheus Adapter (external metrics API)"
    fi
    if [ "$DEPLOY_VA" = "true" ]; then
        echo "✓ VariantAutoscaling CR (via Helm chart)"
    fi
    if [ "$DEPLOY_HPA" = "true" ]; then
        echo "✓ HPA (via Helm chart)"
    fi
    echo ""
    echo "Next Steps:"
    echo "==========="
    echo ""
    echo "1. Check VariantAutoscaling status:"
    echo "   kubectl get variantautoscaling -n $LLMD_NS"
    echo ""
    echo "2. View detailed status with conditions:"
    echo "   kubectl describe variantautoscaling $LLM_D_MODELSERVICE_NAME-decode -n $LLMD_NS"
    echo ""
    echo "3. View WVA logs:"
    echo "   kubectl logs -n $WVA_NS -l app.kubernetes.io/name=workload-variant-autoscaler -f"
    echo ""
    echo "4. Check external metrics API:"
    echo "   kubectl get --raw \"/apis/external.metrics.k8s.io/v1beta1/namespaces/$LLMD_NS/wva_desired_replicas\" | jq"
    echo ""
    echo "5. Port-forward Prometheus to view metrics:"
    echo "   kubectl port-forward -n $MONITORING_NAMESPACE svc/${PROMETHEUS_SVC_NAME} ${PROMETHEUS_PORT}:${PROMETHEUS_PORT}"
    echo "   # Then visit https://localhost:${PROMETHEUS_PORT}"
    echo ""
    echo "Important Notes:"
    echo "================"
    echo ""
    if  ! containsElement "$ENVIRONMENT" "${NON_EMULATED_ENV_LIST[@]}"; then
        echo "• This deployment uses the llm-d inference simulator without real GPUs"
        echo "• The llm-d inference simulator generates synthetic metrics for testing"
    else
        echo "• Model Loading:"
        echo "  - Using $MODEL_ID"
        echo "  - Model loading takes 2-3 minutes on $ACCELERATOR_TYPE GPUs"
        echo "  - Metrics will appear once model is fully loaded"
        echo "  - WVA will automatically detect metrics and start optimization"
    fi
    echo ""
    echo "Troubleshooting:"
    echo "================"
    echo ""
    echo "• Check WVA controller logs:"
    echo "  kubectl logs -n $WVA_NS -l app.kubernetes.io/name=workload-variant-autoscaler"
    echo ""
    echo "• Check all pods in llm-d namespace:"
    echo "  kubectl get pods -n $LLMD_NS"
    echo ""
    echo "• Check if metrics are being scraped by Prometheus:"
    echo "  kubectl port-forward -n $MONITORING_NAMESPACE svc/${PROMETHEUS_SVC_NAME} ${PROMETHEUS_PORT}:${PROMETHEUS_PORT}"
    echo "  # Then visit https://localhost:${PROMETHEUS_PORT} and query: vllm:num_requests_running"
    echo ""
    echo "• Check Prometheus Adapter logs:"
    echo "  kubectl logs -n $MONITORING_NAMESPACE deployment/prometheus-adapter"
    echo ""
    echo "=========================================="
}

# Undeployment functions
undeploy_keda() {
    log_info "Uninstalling KEDA..."
    helm uninstall keda -n "$KEDA_NAMESPACE" 2>/dev/null || \
        log_warning "KEDA not found or already uninstalled"
    kubectl delete namespace "$KEDA_NAMESPACE" --ignore-not-found --timeout=120s 2>/dev/null || true
    log_success "KEDA uninstalled"
}

undeploy_prometheus_adapter() {
    log_info "Uninstalling Prometheus Adapter..."

    # Stop the APIService guard if running
    stop_apiservice_guard

    helm uninstall prometheus-adapter -n $MONITORING_NAMESPACE 2>/dev/null || \
        log_warning "Prometheus Adapter not found or already uninstalled"

    kubectl delete configmap prometheus-ca -n $MONITORING_NAMESPACE --ignore-not-found
    # Cleanup is handled by the values files in config/samples

    log_success "Prometheus Adapter uninstalled"
}

undeploy_llm_d_infrastructure() {
    log_info "Undeploying the llm-d infrastructure..."

    # Determine release name based on environment
    local RELEASE=""
    if ! containsElement "$ENVIRONMENT" "${NON_EMULATED_ENV_LIST[@]}" ; then
        RELEASE="$NAMESPACE_SUFFIX"
    else
        RELEASE="$WELL_LIT_PATH_NAME"
    fi

    if [ ! -d "$EXAMPLE_DIR" ]; then
        log_warning "llm-d example directory not found, skipping cleanup"
    else
        cd "$EXAMPLE_DIR"

        log_info "Removing llm-d core components..."

        helm uninstall infra-$RELEASE -n ${LLMD_NS} 2>/dev/null || \
            log_warning "llm-d infra components not found or already uninstalled"
        helm uninstall gaie-$RELEASE -n ${LLMD_NS} 2>/dev/null || \
            log_warning "llm-d inference-scheduler components not found or already uninstalled"
        helm uninstall ms-$RELEASE -n ${LLMD_NS} 2>/dev/null || \
            log_warning "llm-d ModelService components not found or already uninstalled"

    fi

    # Remove HF token secret
    kubectl delete secret llm-d-hf-token -n "${LLMD_NS}" --ignore-not-found

    # Remove Gateway provider if installed by the script
    if [[ "$INSTALL_GATEWAY_CTRLPLANE" == true ]]; then
        log_info "Removing Gateway provider..."
        helmfile destroy -f "$GATEWAY_PREREQ_DIR/$GATEWAY_PROVIDER.helmfile.yaml" 2>/dev/null || \
            log_warning "Gateway provider cleanup incomplete"
        kubectl delete namespace ${GATEWAY_PROVIDER}-system --ignore-not-found 2>/dev/null || true

    fi

    log_info "Deleting llm-d cloned repository..."
    if [ ! -d "$WVA_PROJECT/$LLM_D_PROJECT" ]; then
        log_warning "llm-d repository directory not found, skipping deletion"
    else
        rm -rf "$WVA_PROJECT/$LLM_D_PROJECT" 2>/dev/null || \
            log_warning "Failed to delete llm-d repository directory"
    fi

    log_success "llm-d infrastructure removed"
}

undeploy_wva_controller() {
    log_info "Uninstalling Workload-Variant-Autoscaler (release: $WVA_RELEASE_NAME)..."

    helm uninstall "$WVA_RELEASE_NAME" -n $WVA_NS 2>/dev/null || \
        log_warning "Workload-Variant-Autoscaler not found or already uninstalled"

    rm -f "$PROM_CA_CERT_PATH"

    log_success "WVA uninstalled"
}

cleanup() {
    log_info "Starting undeployment process..."
    log_info "======================================"
    echo ""

    # Stop the APIService guard if running (safety net)
    stop_apiservice_guard

    # Undeploy environment-specific components (Prometheus, etc.)
    if [ "$DEPLOY_PROMETHEUS" = "true" ]; then
        undeploy_prometheus_stack
    fi

    # Undeploy scaler backend (KEDA or Prometheus Adapter)
    if [ "$SCALER_BACKEND" = "keda" ]; then
        undeploy_keda
    elif [ "$DEPLOY_PROMETHEUS_ADAPTER" = "true" ]; then
        undeploy_prometheus_adapter
    fi

    if [ "$DEPLOY_LLM_D" = "true" ]; then
        undeploy_llm_d_infrastructure
    fi

    if [ "$DEPLOY_WVA" = "true" ]; then
        undeploy_wva_controller
    fi

    # Delete namespaces if requested
    if [ "$DELETE_NAMESPACES" = "true" ] || [ "$DELETE_CLUSTER" = "true" ]; then
        delete_namespaces
    else
        log_info "Keeping namespaces (use --delete-namespaces or set DELETE_NAMESPACES=true to remove)"
    fi

    # Remove llm-d repository
    if [ -d "$(dirname $WVA_PROJECT)/$LLM_D_PROJECT" ]; then
        log_info "llm-d repository at $(dirname $WVA_PROJECT)/$LLM_D_PROJECT preserved (manual cleanup if needed)"
    fi

    echo ""
    log_success "Undeployment complete!"
    echo ""
    echo "=========================================="
    echo " Undeployment Summary for $ENVIRONMENT"
    echo "=========================================="
    echo ""
    echo "Removed components:"
    [ "$SCALER_BACKEND" = "keda" ] && echo "✓ KEDA"
    [ "$DEPLOY_PROMETHEUS_ADAPTER" = "true" ] && echo "✓ Prometheus Adapter"
    [ "$DEPLOY_LLM_D" = "true" ] && echo "✓ llm-d Infrastructure"
    [ "$DEPLOY_WVA" = "true" ] && echo "✓ WVA Controller"
    [ "$DEPLOY_PROMETHEUS" = "true" ] && echo "✓ Prometheus Stack"

    if [ "$DELETE_NAMESPACES" = "true" ]; then
        echo "✓ Namespaces"
    else
        echo ""
        echo "Namespaces preserved:"
        echo "  - $LLMD_NS"
        echo "  - $WVA_NS"
        echo "  - $MONITORING_NAMESPACE"
        [ "$SCALER_BACKEND" = "keda" ] && echo "  - $KEDA_NAMESPACE"
    fi
    echo ""
    echo "=========================================="
}

# Main deployment flow
main() {
    # Parse command line arguments first
    parse_args "$@"

    # Handle infra-only mode: skip VA and HPA deployment
    if [ "$INFRA_ONLY" = "true" ]; then
        log_info "Infra-only mode enabled: Skipping VA and HPA deployment"
        DEPLOY_VA=false
        DEPLOY_HPA=false
    fi

    # When using KEDA as scaler backend: skip Prometheus Adapter and deploy KEDA instead
    if [ "$SCALER_BACKEND" = "keda" ]; then
        log_info "Scaler backend is KEDA: Skipping Prometheus Adapter, will deploy KEDA"
        DEPLOY_PROMETHEUS_ADAPTER=false
    fi

    # Undeploy mode
    if [ "$UNDEPLOY" = "true" ]; then
        log_info "Starting Workload-Variant-Autoscaler Undeployment on $ENVIRONMENT"
        log_info "============================================================="
        echo ""

        # Source environment-specific script to make functions available
        if [ -f "$SCRIPT_DIR/$ENVIRONMENT/install.sh" ]; then
            source "$SCRIPT_DIR/$ENVIRONMENT/install.sh"
        else
            log_error "Environment-specific script not found: $SCRIPT_DIR/$ENVIRONMENT/install.sh"
        fi

        cleanup
        exit 0
    fi

    # Normal deployment flow
    log_info "Starting Workload-Variant-Autoscaler Deployment on $ENVIRONMENT"
    log_info "==========================================================="
    echo ""

    # Check prerequisites
    if [ "$SKIP_CHECKS" != "true" ]; then
        check_prerequisites
    fi

    # Set TLS verification and logging level based on environment
    set_tls_verification
    set_wva_logging_level

    if [[ "$CLUSTER_TYPE" == "kind" ]]; then
        log_info "Kind cluster detected - setting environment to kind-emulated"
        ENVIRONMENT="kind-emulator"
    fi

    # Source environment-specific script to make functions available
    log_info "Loading environment-specific functions for $ENVIRONMENT..."
    if [ -f "$SCRIPT_DIR/$ENVIRONMENT/install.sh" ]; then
        source "$SCRIPT_DIR/$ENVIRONMENT/install.sh"

        # Run environment-specific prerequisite checks if function exists
        if declare -f check_prerequisites > /dev/null; then
            if [ "$SKIP_CHECKS" != "true" ]; then
                check_prerequisites
                check_specific_prerequisites
            fi
        fi
    else
        log_error "Environment script not found: $SCRIPT_DIR/$ENVIRONMENT/install.sh"
    fi

    # Detect GPU type for non-emulated environments
    if containsElement "$ENVIRONMENT" "${NON_EMULATED_ENV_LIST[@]}"; then
        detect_gpu_type
    else
        log_info "Skipping GPU type detection for emulated environment (ENVIRONMENT=$ENVIRONMENT)"
    fi

    # Display configuration
    log_info "Using configuration:"
    echo "    Deployed on:          $ENVIRONMENT"
    echo "    WVA Image:            $WVA_IMAGE_REPO:$WVA_IMAGE_TAG"
    echo "    WVA Namespace:        $WVA_NS"
    echo "    llm-d Namespace:      $LLMD_NS"
    echo "    Monitoring Namespace: $MONITORING_NAMESPACE"
    echo "    Scaler Backend:       $SCALER_BACKEND"
    echo "    Model:                $MODEL_ID"
    echo "    Accelerator:          $ACCELERATOR_TYPE"
    echo ""

    # Prompt for Gateway control plane installation
    if [[ "$E2E_TESTS_ENABLED" == "false" ]]; then
        prompt_gateway_installation
    elif [[ -n "$INSTALL_GATEWAY_CTRLPLANE_ORIGINAL" ]]; then
        log_info "Using explicitly set INSTALL_GATEWAY_CTRLPLANE=$INSTALL_GATEWAY_CTRLPLANE"
    else
        log_info "Enabling Gateway control plane installation for tests"
        export INSTALL_GATEWAY_CTRLPLANE="true"
    fi

    # Create namespaces
    create_namespaces

    # Deploy Prometheus Stack (environment-specific)
    if [ "$DEPLOY_PROMETHEUS" = "true" ]; then
        deploy_prometheus_stack
    else
        log_info "Skipping Prometheus deployment (DEPLOY_PROMETHEUS=false)"
    fi

    # Deploy WVA prerequisites (environment-specific)
    if [ "$DEPLOY_WVA" = "true" ]; then
        deploy_wva_prerequisites
    fi

    # Deploy WVA
    if [ "$DEPLOY_WVA" = "true" ]; then
        deploy_wva_controller
    else
        log_info "Skipping WVA deployment (DEPLOY_WVA=false)"
    fi

    # Deploy llm-d
    if [ "$DEPLOY_LLM_D" = "true" ]; then
        deploy_llm_d_infrastructure

        # For emulated environments, apply specific fixes
        if ! containsElement "$ENVIRONMENT" "${NON_EMULATED_ENV_LIST[@]}"; then
            apply_llm_d_infrastructure_fixes
        else
            log_info "Skipping llm-d related fixes for non-emulated environment (ENVIRONMENT=$ENVIRONMENT)"
        fi

    else
        log_info "Skipping llm-d deployment (DEPLOY_LLM_D=false)"
    fi

    # Deploy scaler backend: KEDA or Prometheus Adapter
    # KEDA in this script is for kind-emulator e2e only; on OpenShift use the platform CMA / Prometheus Adapter.
    if [ "$SCALER_BACKEND" = "keda" ]; then
        if [ "$ENVIRONMENT" != "kind-emulator" ]; then
            log_error "KEDA scaler backend is only supported for kind-emulator environment (ENVIRONMENT=kind-emulator). Current: ENVIRONMENT=$ENVIRONMENT. Use SCALER_BACKEND=prometheus-adapter or run with ENVIRONMENT=kind-emulator."
            exit 1
        fi
        deploy_keda
    elif [ "$DEPLOY_PROMETHEUS_ADAPTER" = "true" ]; then
        deploy_prometheus_adapter
    else
        log_info "Skipping Prometheus Adapter deployment (DEPLOY_PROMETHEUS_ADAPTER=false)"
    fi

    # Verify deployment
    verify_deployment

    # Print summary
    print_summary

    log_success "Deployment on $ENVIRONMENT complete!"
}

# Run main function
main "$@"
