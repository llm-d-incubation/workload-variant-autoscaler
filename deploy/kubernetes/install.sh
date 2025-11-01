#!/bin/bash
#
# Workload-Variant-Autoscaler Kubernetes Deployment Script
# Automated deployment of WVA, llm-d infrastructure, Prometheus, and HPA
#
# Prerequisites:
# - kubectl installed and configured
# - helm installed
# - yq installed (optional)
# - Access to a Kubernetes cluster
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

# Namespaces
LLMD_NS=${LLMD_NS:-"llm-d-$WELL_LIT_PATH_NAME"}
MONITORING_NAMESPACE=${MONITORING_NAMESPACE:-"workload-variant-autoscaler-monitoring"}
WVA_NS=${WVA_NS:-"workload-variant-autoscaler-system"}

# WVA Configuration
WVA_IMAGE_REPO=${WVA_IMAGE_REPO:-"ghcr.io/llm-d/workload-variant-autoscaler"}
WVA_IMAGE_TAG=${WVA_IMAGE_TAG:-"v0.0.2"}
WVA_IMAGE_PULL_POLICY=${WVA_IMAGE_PULL_POLICY:-"Always"}
VLLM_SVC_ENABLED=${VLLM_SVC_ENABLED:-true}
VLLM_SVC_NODEPORT=${VLLM_SVC_NODEPORT:-30000}

# llm-d Configuration
LLM_D_OWNER=${LLM_D_OWNER:-"llm-d"}
LLM_D_PROJECT=${LLM_D_PROJECT:-"llm-d"}
LLM_D_RELEASE=${LLM_D_RELEASE:-"v0.3.0"}
LLM_D_MODELSERVICE_NAME=${LLM_D_MODELSERVICE_NAME:-"ms-$WELL_LIT_PATH_NAME-llm-d-modelservice"}
VLLM_EMULATOR_NAME=${VLLM_EMULATOR_NAME:-"vllm-emulator"}
CLIENT_PREREQ_DIR=${CLIENT_PREREQ_DIR:-"$WVA_PROJECT/$LLM_D_PROJECT/guides/prereq/client-setup"}
GATEWAY_PREREQ_DIR=${GATEWAY_PREREQ_DIR:-"$WVA_PROJECT/$LLM_D_PROJECT/guides/prereq/gateway-provider"}
EXAMPLE_DIR=${EXAMPLE_DIR:-"$WVA_PROJECT/$LLM_D_PROJECT/guides/$WELL_LIT_PATH_NAME"}

# Gateway Configuration
GATEWAY_PROVIDER=${GATEWAY_PROVIDER:-"kgateway"} # Options: kgateway, istio
BENCHMARK_MODE=${BENCHMARK_MODE:-"false"} # if true, updates to Istio config for benchmark
INSTALL_GATEWAY_CTRLPLANE=${INSTALL_GATEWAY_CTRLPLANE:-"true"} # if true, installs gateway control plane providers - defaults to true for emulated clusters

# Model and SLO Configuration
DEFAULT_MODEL_ID=${DEFAULT_MODEL_ID:-"Qwen/Qwen3-0.6B"}
MODEL_ID=${MODEL_ID:-"unsloth/Meta-Llama-3.1-8B"}
ACCELERATOR_TYPE=${ACCELERATOR_TYPE:-"A100"}
SLO_TPOT=${SLO_TPOT:-10}  # Target time-per-output-token SLO (in ms)
SLO_TTFT=${SLO_TTFT:-1000}  # Target time-to-first-token SLO (in ms)

# Prometheus Configuration
PROM_CA_CERT_PATH=${PROM_CA_CERT_PATH:-"/tmp/prometheus-ca.crt"}
PROMETHEUS_BASE_URL=${PROMETHEUS_BASE_URL:-"https://kube-prometheus-stack-prometheus.$MONITORING_NAMESPACE.svc.cluster.local"}
PROMETHEUS_PORT=${PROMETHEUS_PORT:-"9090"}
PROMETHEUS_URL=${PROMETHEUS_URL:-"$PROMETHEUS_BASE_URL:$PROMETHEUS_PORT"}
PROMETHEUS_SECRET_NAME=${PROMETHEUS_SECRET_NAME:-"prometheus-web-tls"}

# Flags for deployment steps
DEPLOY_PROMETHEUS=${DEPLOY_PROMETHEUS:-true}
DEPLOY_WVA=${DEPLOY_WVA:-true}
DEPLOY_LLM_D=${DEPLOY_LLM_D:-true}
DEPLOY_PROMETHEUS_ADAPTER=${DEPLOY_PROMETHEUS_ADAPTER:-true}
DEPLOY_VA=${DEPLOY_VA:-true}
DEPLOY_HPA=${DEPLOY_HPA:-true}
DEPLOY_VLLM_EMULATOR=${DEPLOY_VLLM_EMULATOR:-false}  # Set to true if no GPUs available
APPLY_VLLM_EMULATOR_FIXES=${APPLY_VLLM_EMULATOR_FIXES:-false}
SKIP_CHECKS=${SKIP_CHECKS:-false}

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

This script deploys the complete Workload-Variant-Autoscaler stack on a
Kubernetes cluster with real GPUs.

Options:
  -i, --wva-image IMAGE        Container image to use for the WVA (default: $WVA_IMAGE_REPO:$WVA_IMAGE_TAG)
  -m, --model MODEL            Model ID to use (default: $MODEL_ID)
  -a, --accelerator TYPE       Accelerator type: A100, H100, L40S, etc. (default: $ACCELERATOR_TYPE)
  -u, --undeploy               Undeploy all components
  -d, --delete-namespaces      Delete namespaces after undeployment
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
  DEPLOY_VLLM_EMULATOR            Use vLLM emulator instead of real vLLM (default: false)
  UNDEPLOY                     Undeploy mode (default: false)
  DELETE_NAMESPACES            Delete namespaces after undeploy (default: false)

Examples:
  # Deploy with default values
  $(basename "$0")

  # Deploy with custom WVA image
  IMG=<your_registry>/workload-variant-autoscaler:tag $(basename "$0")
  
  # Deploy with custom model and accelerator
  $(basename "$0") -m unsloth/Meta-Llama-3.1-8B -a A100
  
  # Deploy with vLLM emulator (no GPU required)
  DEPLOY_VLLM_EMULATOR=true $(basename "$0")
  
  # Undeploy all components (keep namespaces)
  $(basename "$0") --undeploy
  
  # Undeploy all components and delete namespaces
  $(basename "$0") --undeploy --delete-namespaces
EOF
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
    
    # Check Kubernetes connection
    if ! kubectl cluster-info &> /dev/null; then
        log_error "Cannot connect to Kubernetes cluster. Please check your kubeconfig"
        exit 1
    fi
    
    log_success "All prerequisites met"
    log_info "Connected to Kubernetes cluster"
}

detect_gpu_type() {
    log_info "Detecting GPU type in cluster..."
    
    # Check if GPUs are visible to Kubernetes
    local gpu_count=$(kubectl get nodes -o json | jq -r '.items[].status.allocatable["nvidia.com/gpu"]' | grep -v null | head -1)
    
    if [ -z "$gpu_count" ] || [ "$gpu_count" == "null" ]; then
        log_warning "No GPUs visible to Kubernetes"
        log_warning "GPUs may exist on host but need NVIDIA Device Plugin or GPU Operator"
        
        # Check if GPUs exist on host
        if nvidia-smi &> /dev/null; then
            log_info "nvidia-smi detected GPUs on host:"
            nvidia-smi --query-gpu=name,memory.total --format=csv,noheader | head -5
            log_warning "Install NVIDIA GPU Operator or set DEPLOY_VLLM_EMULATOR=true for demo"
        else
            log_warning "No GPUs detected on host either"
            log_info "Setting DEPLOY_VLLM_EMULATOR=true for demo mode"
            DEPLOY_VLLM_EMULATOR=true
        fi
    else
        log_success "GPUs visible to Kubernetes: $gpu_count GPU(s) per node"
        
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
    export DEPLOY_VLLM_EMULATOR
    log_info "Using detected accelerator type: $ACCELERATOR_TYPE"
    log_info "Emulator mode: $DEPLOY_VLLM_EMULATOR"
}

create_namespaces() {
    log_info "Creating namespaces..."
    
    for ns in $WVA_NS $MONITORING_NAMESPACE $LLMD_NS; do
        if kubectl get namespace $ns &> /dev/null; then
            log_info "Namespace $ns already exists"
        else
            kubectl create namespace $ns
            log_success "Namespace $ns created"
        fi
    done
}

deploy_prometheus_stack() {
    log_info "Deploying kube-prometheus-stack with TLS..."
    
    # Add helm repo
    helm repo add prometheus-community https://prometheus-community.github.io/helm-charts || true
    helm repo update
    
    # Create self-signed TLS certificate for Prometheus
    log_info "Creating self-signed TLS certificate for Prometheus"
    openssl req -x509 -newkey rsa:2048 -nodes \
        -keyout /tmp/prometheus-tls.key \
        -out /tmp/prometheus-tls.crt \
        -days 365 \
        -subj "/CN=prometheus" \
        -addext "subjectAltName=DNS:kube-prometheus-stack-prometheus.${MONITORING_NAMESPACE}.svc.cluster.local,DNS:kube-prometheus-stack-prometheus.${MONITORING_NAMESPACE}.svc,DNS:prometheus,DNS:localhost" \
        &> /dev/null
    
    # Create Kubernetes secret with TLS certificate
    log_info "Creating Kubernetes secret for Prometheus TLS"
    kubectl create secret tls $PROMETHEUS_SECRET_NAME \
        --cert=/tmp/prometheus-tls.crt \
        --key=/tmp/prometheus-tls.key \
        -n $MONITORING_NAMESPACE \
        --dry-run=client -o yaml | kubectl apply -f - &> /dev/null
    
    # Clean up temp files
    rm -f /tmp/prometheus-tls.{key,crt}
    
    # Install kube-prometheus-stack with TLS enabled
    log_info "Installing kube-prometheus-stack with TLS configuration"
    helm upgrade --install kube-prometheus-stack prometheus-community/kube-prometheus-stack \
        -n $MONITORING_NAMESPACE \
        --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false \
        --set prometheus.prometheusSpec.podMonitorSelectorNilUsesHelmValues=false \
        --set prometheus.service.type=ClusterIP \
        --set prometheus.service.port=$PROMETHEUS_PORT \
        --set prometheus.prometheusSpec.web.tlsConfig.cert.secret.name=$PROMETHEUS_SECRET_NAME \
        --set prometheus.prometheusSpec.web.tlsConfig.cert.secret.key=tls.crt \
        --set prometheus.prometheusSpec.web.tlsConfig.keySecret.name=$PROMETHEUS_SECRET_NAME \
        --set prometheus.prometheusSpec.web.tlsConfig.keySecret.key=tls.key \
        --timeout=5m \
        --wait
    
    log_success "kube-prometheus-stack deployed with TLS"
    log_info "Prometheus URL: $PROMETHEUS_URL"
}

deploy_wva_controller() {
    log_info "Deploying Workload-Variant-Autoscaler..."
    log_info "Using image: $WVA_IMAGE_REPO:$WVA_IMAGE_TAG"
    
    # Extract Prometheus CA certificate
    log_info "Extracting Prometheus TLS certificate"
    kubectl get secret $PROMETHEUS_SECRET_NAME -n $MONITORING_NAMESPACE -o jsonpath='{.data.tls\.crt}' | base64 -d > $PROM_CA_CERT_PATH

    # Deploy WVA using Helm chart
    log_info "Installing Workload-Variant-Autoscaler via Helm chart"
    cd "$WVA_PROJECT/charts"
    
    helm upgrade -i workload-variant-autoscaler ./workload-variant-autoscaler \
        -n $WVA_NS \
        --set-file wva.prometheus.caCert=$PROM_CA_CERT_PATH \
        --set wva.image.repository=$WVA_IMAGE_REPO \
        --set wva.image.tag=$WVA_IMAGE_TAG \
        --set wva.imagePullPolicy=$WVA_IMAGE_PULL_POLICY \
        --set wva.baseName=$WELL_LIT_PATH_NAME \
        --set wva.modelName=$LLM_D_MODELSERVICE_NAME \
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
        --set vllmService.nodePort=$VLLM_SVC_NODEPORT
    
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
        git clone -b $LLM_D_RELEASE -- https://github.com/$LLM_D_OWNER/$LLM_D_PROJECT.git $LLM_D_PROJECT
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

    # Configure benchmark mode for Istio if enabled
    if [[ "$BENCHMARK_MODE" == "true" ]]; then
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
    sed -i.bak "s/llm-d-inference-scheduler/$LLMD_NS/g" helmfile.yaml.gotmpl

    if [ "$MODEL_ID" != "$DEFAULT_MODEL_ID" ]; then
        log_info "Updating deployment to use model: $MODEL_ID"
        yq eval "(.. | select(. == \"$DEFAULT_MODEL_ID\")) = \"$MODEL_ID\" | (.. | select(. == \"hf://$DEFAULT_MODEL_ID\")) = \"hf://$MODEL_ID\"" -i ms-$WELL_LIT_PATH_NAME/values.yaml

        # Increase model-storage volume size
        log_info "Increasing model-storage volume size for model: $MODEL_ID"
        yq eval '.modelArtifacts.size = "30Gi"' -i ms-$WELL_LIT_PATH_NAME/values.yaml
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

    # Edit GAIE image for ARM64 architecture if needed
    # TODO: remove once multi-arch image is available in llm-d
    if [ "$ARCH" == "aarch64" ] || [ "$ARCH" == "arm64" ]; then
        log_info "Patching EPP image for ARM64 architecture"
        kubectl patch deployment gaie-$WELL_LIT_PATH_NAME-epp \
            -n $LLMD_NS \
            --type='json' \
            -p="[{'op':'replace','path':'/spec/template/spec/containers/0/image','value':'$GAIE_IMAGE_ARM64'}]"
    fi
    
    log_info "Waiting for llm-d components to initialize..."
    kubectl wait --for=condition=Available deployment --all -n $LLMD_NS --timeout=60s || \
        log_warning "llm-d components are not ready yet - check 'kubectl get pods -n $LLMD_NS'"
    
    cd "$WVA_PROJECT"
    log_success "llm-d infrastructure deployment complete"
}

deploy_vllm_emulator() {
    log_info "Deploying vLLM Metrics Emulator (No GPU required)..."

    if [ "$VLLM_SVC_ENABLED" == "true" ]; then
        log_info "Deleting default vLLM Service and ServiceMonitor to avoid conflicts..."
        kubectl delete vllm-service -n "$LLMD_NS" || \
            log_warning "Failed to delete default vLLM Service"
        kubectl delete servicemonitor -n "$MONITORING_NAMESPACE" || \
            log_warning "Failed to delete default vLLM ServiceMonitor"
    fi
    
    # Create vLLM emulator deployment
    log_info "Creating vLLM emulator deployment..."
    kubectl apply -f - <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: $VLLM_EMULATOR_NAME-decode
  namespace: $LLMD_NS
spec:
  replicas: 1
  selector:
    matchLabels:
      app: $VLLM_EMULATOR_NAME
      llm-d.ai/inferenceServing: "true"
      llm-d.ai/model: $LLM_D_MODELSERVICE_NAME
  template:
    metadata:
      labels:
        app: $VLLM_EMULATOR_NAME
        llm-d.ai/inferenceServing: "true"
        llm-d.ai/model: $LLM_D_MODELSERVICE_NAME
    spec:
      containers:
      - name: $VLLM_EMULATOR_NAME
        image: quay.io/infernoautoscaler/vllme:0.2.3-multi-arch
        imagePullPolicy: Always
        env: 
        - name: MODEL_NAME
          value: "$MODEL_ID"
        - name: DECODE_TIME
          value: "20"        # In milliseconds, e.g., 20ms per token decode
        - name: PREFILL_TIME
          value: "20"        # In milliseconds, e.g., 20ms for prefill
        - name: MODEL_SIZE
          value: "25000"     # In MB, e.g., 25GB model size
        - name: KVC_PER_TOKEN
          value: "2"         # In MB, e.g., 2MB per token for KV cache
        - name: MAX_SEQ_LEN
          value: "2048"      # Max sequence length
        - name: MEM_SIZE
          value: "80000"     # Total device memory in MB, e.g., 80GB
        - name: AVG_TOKENS
          value: "128"       # Average generated tokens
        - name: TOKENS_DISTRIBUTION
          value: "deterministic"   # "uniform", "normal", "deterministic"
        - name: MAX_BATCH_SIZE
          value: "8"         # Max concurrent requests in a batch 
        - name: REALTIME
          value: "True"      # Boolean: "True" or "False"
        - name: MUTE_PRINT
          value: "False"     # Boolean: "True" or "False"
        ports:
        - containerPort: 80
        resources:
          limits:
            cpu: 500m
            memory: 1Gi
            nvidia.com/gpu: "1"  # Limit to 1 GPU (emulated)
          requests:
            cpu: 100m
            memory: 500Mi
            nvidia.com/gpu: "1"  # Request 1 GPU (emulated)
---
apiVersion: v1
kind: Service
metadata:
  name: $VLLM_EMULATOR_NAME-service
  namespace: $LLMD_NS
  labels:
    app: $VLLM_EMULATOR_NAME
    llm-d.ai/inferenceServing: "true"
spec:
  selector:
    app: $VLLM_EMULATOR_NAME
    llm-d.ai/inferenceServing: "true"
  ports:
    - name: $VLLM_EMULATOR_NAME
      port: 80
      protocol: TCP
      targetPort: 80
  type: ClusterIP
---
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: $VLLM_EMULATOR_NAME-servicemonitor
  namespace: $MONITORING_NAMESPACE
  labels:
    app: $VLLM_EMULATOR_NAME
    release: kube-prometheus-stack
spec:
  selector:
    matchLabels:
      app: $VLLM_EMULATOR_NAME
  endpoints:
  - port: $VLLM_EMULATOR_NAME
    path: /metrics
    interval: 15s
  namespaceSelector:
    matchNames:
    - $LLMD_NS
EOF
    
    log_info "Waiting for vLLM emulator to be ready..."
    kubectl wait --for=condition=available deployment/$VLLM_EMULATOR_NAME-decode -n "$LLMD_NS" --timeout=120s || \
        log_warning "vLLM emulator deployment may still be starting"
    
    log_success "vLLM Emulator deployment complete"
}

# TODO: remove once we move to llm-d-inference-simulator
apply_vllm_emulator_fixes() {
    log_info "Applying vLLM emulator integration fixes..."
        
    # Patch InferencePool to target vLLM emulator port
    kubectl get inferencepool "gaie-$WELL_LIT_PATH_NAME" -n "$LLMD_NS" &> /dev/null || \
        log_warning "InferencePool 'gaie-$WELL_LIT_PATH_NAME' not found"

    log_info "Patching InferencePool to target vLLM emulator port 80..."
    kubectl patch inferencepool "gaie-$WELL_LIT_PATH_NAME" -n "$LLMD_NS" --type='merge' \
        -p '{"spec":{"targetPortNumber":80}}' || log_warning "Failed to patch InferencePool"
    
    log_success "InferencePool patched successfully"
    
    # Delete default ModelService deployments - using vLLM emulator
    log_info "Deleting default ModelService deployments..."
    kubectl delete deployments.apps \
        $LLM_D_MODELSERVICE_NAME-decode \
        $LLM_D_MODELSERVICE_NAME-prefill \
        --ignore-not-found -n "$LLMD_NS"
    
    # Restart EPP deployment to pick up ConfigMap changes
    log_info "Restarting EPP deployment..."
    kubectl rollout restart deployment gaie-$WELL_LIT_PATH_NAME-epp -n $LLMD_NS 2>/dev/null || \
        log_warning "Could not restart EPP deployment"

    if [ "$DEPLOY_INFERENCE_MODEL" == "true" ]; then
      log_info "Creating InferenceModel for vLLM emulator..."
      kubectl apply -f - <<EOF
apiVersion: inference.networking.x-k8s.io/v1alpha2
kind: InferenceModel
metadata:
  name: $VLLM_EMULATOR_NAME
  namespace: $LLMD_NS
spec:
  modelName: $MODEL_ID
  criticality: Critical  
  poolRef:
    name: gaie-$WELL_LIT_PATH_NAME
  targetModels:
    - name: $MODEL_ID
      weight: 100
EOF
    log_success "InferenceModel created successfully"

    else
        log_info "Skipping InferenceModel creation (DEPLOY_INFERENCE_MODEL=false)"
    fi
    
    log_success "vLLM emulator integration fixes applied"
}

deploy_prometheus_adapter() {
    log_info "Deploying Prometheus Adapter..."
    
    # Add Prometheus community helm repo
    log_info "Adding Prometheus community helm repo"
    helm repo add prometheus-community https://prometheus-community.github.io/helm-charts || true
    helm repo update
    
    # Extract Prometheus CA certificate and create ConfigMap
    log_info "Creating prometheus-ca ConfigMap for TLS verification"
    kubectl get secret $PROMETHEUS_SECRET_NAME -n $MONITORING_NAMESPACE -o jsonpath='{.data.tls\.crt}' | base64 -d > /tmp/prometheus-ca.crt
    
    # Create or update prometheus-ca ConfigMap
    kubectl create configmap prometheus-ca --from-file=ca.crt=/tmp/prometheus-ca.crt -n $MONITORING_NAMESPACE --dry-run=client -o yaml | kubectl apply -f -
    
    log_success "prometheus-ca ConfigMap created/updated"
    
    # Create prometheus-adapter values for Kubernetes
    cat > /tmp/prometheus-adapter-values-k8s.yaml <<EOF
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
        -f /tmp/prometheus-adapter-values-k8s.yaml \
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
        
        # Check for vLLM emulator if enabled
        if [ "$DEPLOY_VLLM_EMULATOR" = "true" ]; then
            if kubectl get deployment $LLM_D_MODELSERVICE_NAME-decode -n $LLMD_NS &> /dev/null; then
                log_success "vLLM emulator deployment exists"
            else
                log_warning "vLLM emulator deployment not found"
                all_good=false
            fi
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
    echo "WVA Namespace:          $WVA_NS"
    echo "LLMD Namespace:         $LLMD_NS"
    echo "Monitoring Namespace:   $MONITORING_NAMESPACE"
    echo "Model:                  $MODEL_ID"
    echo "Accelerator:            $ACCELERATOR_TYPE"
    echo "WVA Image:              $WVA_IMAGE_REPO:$WVA_IMAGE_TAG"
    echo "Emulator Mode:          $DEPLOY_VLLM_EMULATOR"
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
        if [ "$DEPLOY_VLLM_EMULATOR" = "true" ]; then
            echo "✓ vLLM Emulator (no GPU required)"
        fi
        
        if [ "$APPLY_VLLM_EMULATOR_FIXES" = "true" ]; then
            log_info " Applied vLLM emulator integration fixes"
        fi
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
    echo "   kubectl port-forward -n $MONITORING_NAMESPACE svc/kube-prometheus-stack-prometheus 9090:9090"
    echo "   # Then visit https://localhost:9090 (accept self-signed cert)"
    echo ""
    echo "Important Notes:"
    echo "================"
    echo ""
    if [ "$DEPLOY_VLLM_EMULATOR" = "true" ]; then
        echo "• This deployment uses vLLM EMULATOR (no real GPUs or models)"
        echo "• vLLM emulator generates synthetic metrics for testing"
        echo "• Perfect for development and testing without GPU hardware"
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
    echo "  kubectl port-forward -n $MONITORING_NAMESPACE svc/kube-prometheus-stack-prometheus 9090:9090"
    echo "  # Then visit https://localhost:9090 and query: vllm:request_success_total"
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

undeploy_vllm_emulator() {
    log_info "Removing vLLM Emulator..."
    
    kubectl delete deployment $VLLM_EMULATOR_NAME-decode -n $LLMD_NS --ignore-not-found
    kubectl delete service $VLLM_EMULATOR_NAME-service -n $LLMD_NS --ignore-not-found
    kubectl delete servicemonitor $VLLM_EMULATOR_NAME-servicemonitor -n $MONITORING_NAMESPACE --ignore-not-found
    
    log_success "vLLM Emulator removed"
}

undeploy_llm_d_infrastructure() {
    log_info "Undeploying the llm-d infrastructure..."
    
    if [ ! -d "$EXAMPLE_DIR" ]; then
        log_warning "llm-d example directory not found, skipping cleanup"
    else
        cd "$EXAMPLE_DIR"
        
        log_info "Removing llm-d core components..."

        helm uninstall infra-$WELL_LIT_PATH_NAME -n ${LLMD_NS} 2>/dev/null || \
            log_warning "llm-d infra components not found or already uninstalled"
        helm uninstall gaie-$WELL_LIT_PATH_NAME -n ${LLMD_NS} 2>/dev/null || \
            log_warning "llm-d inference-scheduler components not found or already uninstalled"
        helm uninstall ms-$WELL_LIT_PATH_NAME -n ${LLMD_NS} 2>/dev/null || \
            log_warning "llm-d ModelService components not found or already uninstalled"

        cd "$WVA_PROJECT"
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
    if [ ! -d "$(dirname $WVA_PROJECT)/$LLM_D_PROJECT" ]; then
        log_warning "llm-d repository directory not found, skipping deletion"
    else
        rm -rf "$(dirname $WVA_PROJECT)/$LLM_D_PROJECT" 2>/dev/null || \
            log_warning "Failed to delete llm-d repository directory"
    fi

    log_success "llm-d infrastructure removed"
}

undeploy_wva_controller() {
    log_info "Uninstalling Workload-Variant-Autoscaler..."
    
    cd "$WVA_PROJECT/charts"
    helm uninstall workload-variant-autoscaler -n $WVA_NS 2>/dev/null || \
        log_warning "Workload-Variant-Autoscaler not found or already uninstalled"
    cd "$WVA_PROJECT"
    
    rm -f "$PROM_CA_CERT_PATH"
    
    log_success "WVA uninstalled"
}

undeploy_prometheus_stack() {
    log_info "Uninstalling kube-prometheus-stack..."
    
    helm uninstall kube-prometheus-stack -n $MONITORING_NAMESPACE 2>/dev/null || \
        log_warning "Prometheus stack not found or already uninstalled"

    kubectl delete secret $PROMETHEUS_SECRET_NAME -n $MONITORING_NAMESPACE --ignore-not-found

    log_success "Prometheus stack uninstalled"
}

delete_namespaces() {
    log_info "Deleting namespaces..."
    
    for ns in $LLMD_NS $WVA_NS $MONITORING_NAMESPACE; do
        if kubectl get namespace $ns &> /dev/null; then
            log_info "Deleting namespace $ns..."
            kubectl delete namespace $ns 2>/dev/null || \
                log_warning "Failed to delete namespace $ns"
        fi
    done
    
    log_success "Namespaces deleted"
}

cleanup() {
    log_info "Starting undeployment process..."
    log_info "======================================"
    echo ""
    
    # Undeploy in reverse order
    if [ "$DEPLOY_PROMETHEUS_ADAPTER" = "true" ]; then
        undeploy_prometheus_adapter
    fi
    
    if [ "$DEPLOY_VLLM_EMULATOR" = "true" ]; then
        undeploy_vllm_emulator
    fi
    
    if [ "$DEPLOY_LLM_D" = "true" ]; then
        undeploy_llm_d_infrastructure
    fi
    
    if [ "$DEPLOY_WVA" = "true" ]; then
        undeploy_wva_controller
    fi
    
    if [ "$DEPLOY_PROMETHEUS" = "true" ]; then
        undeploy_prometheus_stack
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
    [ "$DEPLOY_VLLM_EMULATOR" = "true" ] && echo "✓ vLLM Emulator"
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
        log_info "Starting Workload-Variant-Autoscaler Kubernetes Undeployment"
        log_info "============================================================="
        echo ""
        cleanup
        exit 0
    fi

    # Normal deployment flow
    log_info "Starting Workload-Variant-Autoscaler Kubernetes Deployment"
    log_info "==========================================================="
    echo ""
    
    # Check prerequisites
    if [ "$SKIP_CHECKS" != "true" ]; then
        check_prerequisites
    fi
    
    # Detect GPU type
    detect_gpu_type
    
    # Display configuration
    log_info "Using configuration:"
    echo "    WVA Image:            $WVA_IMAGE_REPO:$WVA_IMAGE_TAG"
    echo "    WVA Namespace:        $WVA_NS"
    echo "    LLMD Namespace:       $LLMD_NS"
    echo "    Monitoring Namespace: $MONITORING_NAMESPACE"
    echo "    Model:                $MODEL_ID"
    echo "    Accelerator:          $ACCELERATOR_TYPE"
    echo "    Emulator Mode:        $DEPLOY_VLLM_EMULATOR"
    echo ""
    
    # Create namespaces
    create_namespaces
    
    # Deploy Prometheus Stack
    if [ "$DEPLOY_PROMETHEUS" = "true" ]; then
        deploy_prometheus_stack
    else
        log_info "Skipping Prometheus deployment (DEPLOY_PROMETHEUS=false)"
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
        
        # Deploy vLLM Emulator if needed
        if [ "$DEPLOY_VLLM_EMULATOR" = "true" ]; then
            log_info "vLLM Emulator deployment enabled (DEPLOY_VLLM_EMULATOR=true)"
            deploy_vllm_emulator

            if [ "$APPLY_VLLM_EMULATOR_FIXES" = "true" ]; then
                log_info "Applying vLLM emulator integration fixes (APPLY_VLLM_EMULATOR_FIXES=true)"
            
                # Apply vLLM emulator-specific fixes
                apply_vllm_emulator_fixes
            fi
        else
            log_info "vLLM Emulator deployment disabled (DEPLOY_VLLM_EMULATOR=false), skipping"
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
    
    log_success "Deployment complete!"
}

# Run main function
main "$@"

