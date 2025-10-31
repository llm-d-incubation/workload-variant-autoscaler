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
ARCH=$(uname -m)

# Namespaces
LLMD_NS=${LLMD_NS:-"llm-d-$NAMESPACE_SUFFIX"}
MONITORING_NAMESPACE=${MONITORING_NAMESPACE:-"workload-variant-autoscaler-monitoring"}
WVA_NS=${WVA_NS:-"workload-variant-autoscaler-system"}
PROMETHEUS_SECRET_NS=${PROMETHEUS_SECRET_NS:-$MONITORING_NAMESPACE}

# WVA Configuration
WVA_IMAGE_REPO=${WVA_IMAGE_REPO:-"ghcr.io/llm-d/workload-variant-autoscaler"}
WVA_IMAGE_TAG=${WVA_IMAGE_TAG:-"latest"}
WVA_IMAGE_PULL_POLICY=${WVA_IMAGE_PULL_POLICY:-"Always"}
VLLM_SVC_ENABLED=${VLLM_SVC_ENABLED:-true}
VLLM_SVC_NODEPORT=${VLLM_SVC_NODEPORT:-30000}
GAIE_IMAGE_ARM64=${GAIE_IMAGE_ARM64:-"quay.io/infernoautoscaler/llm-d-inference-scheduler:v0.2.1-arm64"}
SKIP_TLS_VERIFY=${SKIP_TLS_VERIFY:-false}
WVA_LOG_LEVEL=${WVA_LOG_LEVEL:-"info"}
VALUES_FILE=${VALUES_FILE:-"$WVA_PROJECT/charts/workload-variant-autoscaler/values.yaml"}

# llm-d Configuration
LLM_D_OWNER=${LLM_D_OWNER:-"llm-d"}
LLM_D_PROJECT=${LLM_D_PROJECT:-"llm-d"}
LLM_D_RELEASE=${LLM_D_RELEASE:-"v0.3.0"}
LLM_D_MODELSERVICE_NAME=${LLM_D_MODELSERVICE_NAME:-"ms-$WELL_LIT_PATH_NAME-llm-d-modelservice"}
VLLM_EMULATOR_NAME=${VLLM_EMULATOR_NAME:-"vllm-emulator"}
CLIENT_PREREQ_DIR=${CLIENT_PREREQ_DIR:-"$WVA_PROJECT/$LLM_D_PROJECT/guides/prereq/client-setup"}
GATEWAY_PREREQ_DIR=${GATEWAY_PREREQ_DIR:-"$WVA_PROJECT/$LLM_D_PROJECT/guides/prereq/gateway-provider"}
EXAMPLE_DIR=${EXAMPLE_DIR:-"$WVA_PROJECT/$LLM_D_PROJECT/guides/$WELL_LIT_PATH_NAME"}
LLM_D_MODELSERVICE_VALUES=${LLM_D_MODELSERVICE_VALUES:-"ms-$WELL_LIT_PATH_NAME/values.yaml"}

# Gateway Configuration
GATEWAY_PROVIDER=${GATEWAY_PROVIDER:-"istio"} # Options: kgateway, istio
BENCHMARK_MODE=${BENCHMARK_MODE:-"true"} # if true, updates to Istio config for benchmark
INSTALL_GATEWAY_CTRLPLANE=${INSTALL_GATEWAY_CTRLPLANE:-"false"} # if true, installs gateway control plane providers - defaults to true for emulated clusters

# Model and SLO Configuration
DEFAULT_MODEL_ID=${DEFAULT_MODEL_ID:-"Qwen/Qwen3-0.6B"}
MODEL_ID=${MODEL_ID:-"unsloth/Meta-Llama-3.1-8B"}
ACCELERATOR_TYPE=${ACCELERATOR_TYPE:-"H100"}
SLO_TPOT=${SLO_TPOT:-10}  # Target time-per-output-token SLO (in ms)
SLO_TTFT=${SLO_TTFT:-1000}  # Target time-to-first-token SLO (in ms)

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
SKIP_CHECKS=${SKIP_CHECKS:-false}

# Script directories
SCRIPT_DIR=$(cd $(dirname "${BASH_SOURCE[0]}") && pwd)
ENVIRONMENT=${ENVIRONMENT:-"kubernetes"}
COMPATIBLE_ENV_LIST=("kubernetes" "openshift" "kind-emulator")
NON_EMULATED_ENV_LIST=("kubernetes" "openshift")

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

print_help() {
  cat <<EOF
Usage: $(basename "$0") [OPTIONS]

This script deploys the complete Workload-Variant-Autoscaler stack on a cluster with real GPUs.

Options:
  -i, --wva-image IMAGE        Container image to use for the WVA (default: $WVA_IMAGE_REPO:$WVA_IMAGE_TAG)
  -m, --model MODEL            Model ID to use (default: $MODEL_ID)
  -a, --accelerator TYPE       Accelerator type: A100, H100, L40S, etc. (default: $ACCELERATOR_TYPE)
  -u, --undeploy               Undeploy all components
  -d, --delete-namespaces      Delete namespaces after undeployment
  -e, --environment            Specify deployment environment: kubernetes, openshift, kind-emulated (default: kubernetes)
  -h, --help                   Show this help and exit

Environment Variables:
  IMG                          Container image to use for the WVA (alternative to -i flag)
  HF_TOKEN                     HuggingFace token for model access (required for llm-d deployment)
  DEPLOY_PROMETHEUS            Deploy Prometheus stack (default: true)
  DEPLOY_WVA                   Deploy WVA controller (default: true)
  DEPLOY_LLM_D                 Deploy llm-d infrastructure (default: true)
  DEPLOY_PROMETHEUS_ADAPTER    Deploy Prometheus Adapter (default: true)
  DEPLOY_VA                    Deploy VariantAutoscaling (default: true)
  DEPLOY_HPA                   Deploy HPA (default: true)
  UNDEPLOY                     Undeploy mode (default: false)
  DELETE_NAMESPACES            Delete namespaces after undeploy (default: false)

Examples:
  # Deploy with default values
  $(basename "$0")

  # Deploy with custom WVA image
  IMG=<your_registry>/workload-variant-autoscaler:tag $(basename "$0")
  
  # Deploy with custom model and accelerator
  $(basename "$0") -m unsloth/Meta-Llama-3.1-8B -a A100
  
  # Undeploy all components (keep namespaces)
  $(basename "$0") --undeploy
  
  # Undeploy all components and delete namespaces
  $(basename "$0") --undeploy --delete-namespaces
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
      -u|--undeploy)          UNDEPLOY=true; shift ;;
      -d|--delete-namespaces) DELETE_NAMESPACES=true; shift ;;
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
    for tool in kubectl helm; do
        if ! command -v $tool &> /dev/null; then
            missing_tools+=($tool)
        fi
    done
    
    if [ ${#missing_tools[@]} -ne 0 ]; then
        log_error "Missing required tools: ${missing_tools[*]}"
        log_info "Please install the missing tools and try again"
        exit 1
    fi

    log_success "All generic prerequisites tools met"
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

detect_cluster_environment() {
    log_info "Detecting cluster environment..."
    
    # Auto-detect cluster type if not specified
    if [ "$CLUSTER_TYPE" = "auto" ]; then
        # Check if this is a Kind cluster
        if kubectl config current-context | grep -q "kind"; then
            CLUSTER_TYPE="kind"
            log_info "Detected Kind cluster"
        elif kubectl get nodes -o json | jq -r '.items[].metadata.labels["kubernetes.io/hostname"]' | grep -q "kind"; then
            CLUSTER_TYPE="kind"
            log_info "Detected Kind cluster (by node hostname)"
        else
            CLUSTER_TYPE="production"
            log_info "Detected production cluster"
        fi
    fi
    
    # Auto-detect TLS verification setting if not specified
    if [ "$SKIP_TLS_VERIFY" = "auto" ]; then
        case "$CLUSTER_TYPE" in
            "kind")
                SKIP_TLS_VERIFY="true"
                log_info "Kind cluster detected - enabling TLS skip verification for self-signed certificates"
                ;;
            "production")
                SKIP_TLS_VERIFY="false"
                log_info "Production cluster detected - enabling strict TLS verification"
                ;;
            *)
                SKIP_TLS_VERIFY="false"
                log_warning "Unknown cluster type - defaulting to strict TLS verification"
                ;;
        esac
    fi
    
    # Set logging level based on environment
    if [ "$CLUSTER_TYPE" = "kind" ]; then
        WVA_LOG_LEVEL="debug"
        log_info "Development environment - using debug logging"
    else
        WVA_LOG_LEVEL="info"
        log_info "Production environment - using info logging"
    fi
    
    export CLUSTER_TYPE
    export SKIP_TLS_VERIFY
    export WVA_LOG_LEVEL
    
    log_success "Environment detection complete:"
    echo "    Cluster Type:        $CLUSTER_TYPE"
    echo "    Skip TLS Verify:     $SKIP_TLS_VERIFY"
    echo "    Log Level:           $WVA_LOG_LEVEL"
    echo ""
}

deploy_wva_controller() {
    log_info "Deploying Workload-Variant-Autoscaler..."
    log_info "Using image: $WVA_IMAGE_REPO:$WVA_IMAGE_TAG"

    # Deploy WVA using Helm chart
    log_info "Installing Workload-Variant-Autoscaler via Helm chart"
    
    helm upgrade -i workload-variant-autoscaler ${WVA_PROJECT}/charts/workload-variant-autoscaler \
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
        --set llmd.namespace=$LLMD_NS \
        --set wva.prometheus.baseURL=$PROMETHEUS_URL \
        --set wva.prometheus.monitoringNamespace=$MONITORING_NAMESPACE \
        --set vllmService.enabled=$VLLM_SVC_ENABLED \
        --set vllmService.nodePort=$VLLM_SVC_NODEPORT \
        --set wva.logging.level=$WVA_LOG_LEVEL \
        --set wva.prometheus.tls.insecureSkipVerify=$SKIP_TLS_VERIFY
    
    cd "$WVA_PROJECT"
    
    # Wait for WVA to be ready
    log_info "Waiting for WVA controller to be ready..."
    kubectl wait --for=condition=Ready pod -l app.kubernetes.io/name=workload-variant-autoscaler -n $WVA_NS --timeout=60s || \
        log_warning "WVA controller is not ready yet - check 'kubectl get pods -n $WVA_NS'"
    
    log_success "WVA deployment complete"
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
    
    # Check for HF_TOKEN (use dummy for emulator)
    export HF_TOKEN=${HF_TOKEN:-"dummy-token"}
    
    # Create HF token secret
    log_info "Creating HuggingFace token secret"
    kubectl create secret generic llm-d-hf-token \
        --from-literal="HF_TOKEN=${HF_TOKEN}" \
        --namespace "${LLMD_NS}" \
        --dry-run=client -o yaml | kubectl apply -f -
    
    # Install dependencies
    log_info "Installing llm-d dependencies"
    bash $CLIENT_PREREQ_DIR/install-deps.sh
    bash $GATEWAY_PREREQ_DIR/install-gateway-provider-dependencies.sh

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

    # Configure benchmark mode for Istio if enabled (not available for emulated deployments)
    if [ "$BENCHMARK_MODE" == "true" ] ; then
      log_info "Benchmark mode enabled - using benchmark configuration for Istio"
      GATEWAY_PROVIDER="istioBench"
    fi

    # Configure llm-d infrastructure
    log_info "Configuring llm-d infrastructure"
    
    cd $EXAMPLE_DIR

    # Deploy llm-d core components
    log_info "Deploying llm-d core components"
    helmfile apply -e $GATEWAY_PROVIDER -n ${LLMD_NS}
    kubectl apply -f httproute.yaml -n ${LLMD_NS}

    log_info "Configuring llm-d infrastructure"
    
    cd $EXAMPLE_DIR

    # Update model ID if different from default
    if [ "$MODEL_ID" != "$DEFAULT_MODEL_ID" ] ; then
        log_info "Updating deployment to use model: $MODEL_ID"
        yq eval "(.. | select(. == \"$DEFAULT_MODEL_ID\")) = \"$MODEL_ID\" | (.. | select(. == \"hf://$DEFAULT_MODEL_ID\")) = \"hf://$MODEL_ID\"" -i "$LLM_D_MODELSERVICE_VALUES"

        # Increase model-storage volume size
        log_info "Increasing model-storage volume size for model: $MODEL_ID"
        yq eval '.modelArtifacts.size = "30Gi"' -i "$LLM_D_MODELSERVICE_VALUES"
    fi

    # Configure llm-d-inference-simulator if needed
    if [ "$DEPLOY_LLM_D_INFERENCE_SIM" == "true" ]; then
      log_info "Deploying llm-d-inference-simulator..."
        yq eval ".decode.containers[0].image = \"$LLM_D_INFERENCE_SIM_IMG_REPO:$LLM_D_INFERENCE_SIM_IMG_TAG\" | .prefill.containers[0].image = \"$LLM_D_INFERENCE_SIM_IMG_REPO:$LLM_D_INFERENCE_SIM_IMG_TAG\"" -i "$LLM_D_MODELSERVICE_VALUES"
    else
      log_info "Skipping llm-d-inference-simulator deployment (DEPLOY_LLM_D_INFERENCE_SIM=false)"
    fi

    # Deploy llm-d core components
    log_info "Deploying llm-d core components"
    helmfile apply -e $GATEWAY_PROVIDER -n ${LLMD_NS}
    kubectl apply -f httproute.yaml -n ${LLMD_NS}

    if [ "$GATEWAY_PROVIDER" == "kgateway" ]; then
        log_info "Patching kgateway service to NodePort"
        export GATEWAY_NAME="infra-inference-scheduling-inference-gateway"
        kubectl patch gatewayparameters.gateway.kgateway.dev $GATEWAY_NAME \
        -n $LLMD_NS \
        --type='merge' \
        -p '{"spec":{"kube":{"service":{"type":"NodePort"}}}}'
    fi
    
    log_info "Waiting for llm-d components to initialize..."
    kubectl wait --for=condition=Available deployment --all -n $LLMD_NS --timeout=60s || \
        log_warning "llm-d components are not ready yet - check 'kubectl get pods -n $LLMD_NS'"
    
    cd "$WVA_PROJECT"
    log_success "llm-d infrastructure deployment complete"
}

deploy_prometheus_adapter() {
    log_info "Deploying Prometheus Adapter..."
    
    # Add Prometheus community helm repo
    log_info "Adding Prometheus community helm repo"
    helm repo add prometheus-community https://prometheus-community.github.io/helm-charts || true
    helm repo update

    # Create CA ConfigMap from TLS certificate
    log_info "Creating Prometheus CA ConfigMap in $MONITORING_NAMESPACE namespace for the Prometheus Adapter"
    kubectl create configmap prometheus-ca \
        --from-file=ca.crt=$PROM_CA_CERT_PATH \
        -n $MONITORING_NAMESPACE \
        --dry-run=client -o yaml | kubectl apply -f -
    
    # Create prometheus-adapter values for Kubernetes
    cat > /tmp/prometheus-adapter-values.yaml <<EOF
prometheus:
  url: $PROMETHEUS_BASE_URL
  port: $PROMETHEUS_PORT

rules:
  external:
  - seriesQuery: 'inferno_desired_replicas{variant_name!="",exported_namespace!=""}'
    resources:
      overrides:
        exported_namespace: {resource: "namespace"}
        variant_name: {resource: "deployment"}  
    name:
      matches: "^inferno_desired_replicas"
      as: "inferno_desired_replicas"
    metricsQuery: 'inferno_desired_replicas{<<.LabelMatchers>>}'

replicas: 2
logLevel: 4

tls:
  enable: false # Inbound TLS (Client → Adapter)

extraVolumes:
  - name: prometheus-ca
    configMap:
      name: prometheus-ca

extraVolumeMounts:
  - name: prometheus-ca
    mountPath: /etc/prometheus-ca
    readOnly: true

extraArguments:
  - --prometheus-ca-file=/etc/prometheus-ca/ca.crt
  - --prometheus-token-file=/var/run/secrets/kubernetes.io/serviceaccount/token

podSecurityContext:
  fsGroup: null

securityContext:
  allowPrivilegeEscalation: false
  capabilities:
    drop: ["ALL"]
  readOnlyRootFilesystem: true
  runAsNonRoot: true
  runAsUser: null
  seccompProfile:
    type: RuntimeDefault
EOF
    
    # Deploy Prometheus Adapter
    log_info "Installing Prometheus Adapter via Helm"
    helm upgrade -i prometheus-adapter prometheus-community/prometheus-adapter \
        -n $MONITORING_NAMESPACE \
        -f /tmp/prometheus-adapter-values.yaml \
        --timeout=3m \
        --wait || {
            log_warning "Prometheus Adapter deployment timed out or failed, but continuing..."
            log_warning "HPA may not work until adapter is healthy"
            log_info "Check adapter status: kubectl get pods -n $MONITORING_NAMESPACE | grep prometheus-adapter"
            log_info "Check adapter logs: kubectl logs -n $MONITORING_NAMESPACE deployment/prometheus-adapter"
        }
    
    log_success "Prometheus Adapter deployment initiated (may still be starting)"
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
    
    # Check Prometheus Adapter
    if [ "$DEPLOY_PROMETHEUS_ADAPTER" = "true" ]; then
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
    if [ "$DEPLOY_PROMETHEUS_ADAPTER" = "true" ]; then
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
    echo "   kubectl get --raw \"/apis/external.metrics.k8s.io/v1beta1/namespaces/$LLMD_NS/inferno_desired_replicas\" | jq"
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
    echo "  # Then visit https://localhost:${PROMETHEUS_PORT} and query: vllm:request_success_total"
    echo ""
    echo "• Check Prometheus Adapter logs:"
    echo "  kubectl logs -n $MONITORING_NAMESPACE deployment/prometheus-adapter"
    echo ""
    echo "=========================================="
}

# Undeployment functions
undeploy_prometheus_adapter() {
    log_info "Uninstalling Prometheus Adapter..."
    helm uninstall prometheus-adapter -n $MONITORING_NAMESPACE 2>/dev/null || \
        log_warning "Prometheus Adapter not found or already uninstalled"
    
    kubectl delete configmap prometheus-ca -n $MONITORING_NAMESPACE --ignore-not-found
    rm -f /tmp/prometheus-adapter-values-k8s.yaml
    
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
    log_info "Uninstalling Workload-Variant-Autoscaler..."
    
    helm uninstall workload-variant-autoscaler -n $WVA_NS 2>/dev/null || \
        log_warning "Workload-Variant-Autoscaler not found or already uninstalled"
    
    rm -f "$PROM_CA_CERT_PATH"
    
    log_success "WVA uninstalled"
}

cleanup() {
    log_info "Starting undeployment process..."
    log_info "======================================"
    echo ""

    # Undeploy environment-specific components (Prometheus, etc.)
    if [ "$DEPLOY_PROMETHEUS" = "true" ]; then
        undeploy_prometheus_stack
    fi
    
    # Undeploy in reverse order
    if [ "$DEPLOY_PROMETHEUS_ADAPTER" = "true" ]; then
        undeploy_prometheus_adapter
    fi
    
    if [ "$DEPLOY_LLM_D" = "true" ]; then
        undeploy_llm_d_infrastructure
    fi
    
    if [ "$DEPLOY_WVA" = "true" ]; then
        undeploy_wva_controller
    fi
    
    # Delete namespaces if requested
    if [ "$DELETE_NAMESPACES" = "true" ]; then
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
    echo " Undeployment Summary"
    echo "=========================================="
    echo ""
    echo "Removed components:"
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
    fi
    echo ""
    echo "=========================================="
}

# Main deployment flow
main() {
    # Parse command line arguments first
    parse_args "$@"

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

    # Detect cluster environment
    detect_cluster_environment

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
        log_error "Environment script not found: $SCRIPT_DIR/$ENVIRONMENT/$ENVIRONMENT.sh"
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
    echo "    Model:                $MODEL_ID"
    echo "    Accelerator:          $ACCELERATOR_TYPE"
    echo ""

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
    
    # Deploy Prometheus Adapter
    if [ "$DEPLOY_PROMETHEUS_ADAPTER" = "true" ]; then
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

