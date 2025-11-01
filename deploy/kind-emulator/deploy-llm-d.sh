#!/usr/bin/env bash
#
# Workload-Variant-Autoscaler KIND Emulator Deployment Script
# Automated deployment of WVA with llm-d infrastructure on KIND cluster with vLLM emulator
#
# Prerequisites:
# - kubectl installed and configured
# - helm installed
# - yq installed
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
ARCH=$(uname -m)
WELL_LIT_PATH_NAME=${WELL_LIT_PATH_NAME:-"sim"}

# Namespaces
LLMD_NS=${LLMD_NS:-"llm-d-$WELL_LIT_PATH_NAME"}
MONITORING_NAMESPACE=${MONITORING_NAMESPACE:-"workload-variant-autoscaler-monitoring"}
WVA_NS=${WVA_NS:-"workload-variant-autoscaler-system"}

# WVA Configuration
WVA_IMAGE_REPO=${WVA_IMAGE_REPO:-"ghcr.io/llm-d/workload-variant-autoscaler"}
WVA_IMAGE_TAG=${WVA_IMAGE_TAG:-"v0.0.2"}
WVA_IMAGE_PULL_POLICY=${WVA_IMAGE_PULL_POLICY:-"Always"}
# TODO: remove once llm-d-inference-scheduler multi-arch image is available - when moving to llm-d
GAIE_IMAGE_ARM64=${GAIE_IMAGE_ARM64:-"quay.io/infernoautoscaler/llm-d-inference-scheduler:v0.2.1-arm64"}

# llm-d Configuration
# TODO: update to use llm-d once we move to llm-d-inference-sim 
LLM_D_OWNER=${LLM_D_OWNER:-"llm-d-incubation"}
LLM_D_PROJECT=${LLM_D_PROJECT:-"llm-d-infra"}
LLM_D_RELEASE=${LLM_D_RELEASE:-"v1.3.1"}
LLM_D_MODELSERVICE_NAME=${LLM_D_MODELSERVICE_NAME:-"ms-$WELL_LIT_PATH_NAME-llm-d-modelservice"}
VLLM_EMULATOR_NAME=${VLLM_EMULATOR_NAME:-"vllm-emulator"}
CLIENT_PREREQ_DIR=${CLIENT_PREREQ_DIR:-"$WVA_PROJECT/$LLM_D_PROJECT/quickstart/dependencies"}
GATEWAY_PREREQ_DIR=${GATEWAY_PREREQ_DIR:-"$WVA_PROJECT/$LLM_D_PROJECT/quickstart/gateway-control-plane-providers"}
EXAMPLE_DIR=${EXAMPLE_DIR:-"$WVA_PROJECT/$LLM_D_PROJECT/quickstart/examples/$WELL_LIT_PATH_NAME"}

# Model and SLO Configuration
MODEL_ID=${MODEL_ID:-"default/default"}  # vLLM emulator model
ACCELERATOR_TYPE=${ACCELERATOR_TYPE:-"A100"}
SLO_TPOT=${SLO_TPOT:-24}  # Target time-per-output-token SLO (in ms)
SLO_TTFT=${SLO_TTFT:-500}  # Target time-to-first-token SLO (in ms)

# Gateway Configuration
GATEWAY_PROVIDER=${GATEWAY_PROVIDER:-"kgateway"} # Options: kgateway, istio
# BENCHMARK_MODE=${BENCHMARK_MODE:-"true"} # if true, updates to Istio config for benchmark
INSTALL_GATEWAY_CTRLPLANE=${INSTALL_GATEWAY_CTRLPLANE:-"true"} # if true, installs gateway control plane providers - defaults to true for emulated clusters

# Prometheus Configuration
PROM_CA_CERT_PATH=${PROM_CA_CERT_PATH:-"/tmp/prometheus-ca.crt"}
PROMETHEUS_BASE_URL=${PROMETHEUS_BASE_URL:-"https://kube-prometheus-stack-prometheus.$MONITORING_NAMESPACE.svc.cluster.local"}
PROMETHEUS_PORT=${PROMETHEUS_PORT:-"9090"}
PROMETHEUS_URL=${PROMETHEUS_URL:-"$PROMETHEUS_BASE_URL:$PROMETHEUS_PORT"}
PROMETHEUS_SECRET_NAME=${PROMETHEUS_SECRET_NAME:-"prometheus-web-tls"}

# KIND cluster configuration
CLUSTER_NAME=${CLUSTER_NAME:-"kind-wva-gpu-cluster"}
CLUSTER_NODES=${CLUSTER_NODES:-"3"}
CLUSTER_GPUS=${CLUSTER_GPUS:-"4"}
CLUSTER_TYPE=${CLUSTER_TYPE:-"mix"}

# Flags for deployment steps
CREATE_CLUSTER=${CREATE_CLUSTER:-true}
DEPLOY_PROMETHEUS=${DEPLOY_PROMETHEUS:-true}
DEPLOY_WVA=${DEPLOY_WVA:-true}
DEPLOY_LLM_D=${DEPLOY_LLM_D:-true}
VLLM_SVC_ENABLED=${VLLM_SVC_ENABLED:-false}
VLLM_SVC_NODEPORT=${VLLM_SVC_NODEPORT:-"30000"}
DEPLOY_VA=${DEPLOY_VA:-true}
DEPLOY_HPA=${DEPLOY_HPA:-true}
DEPLOY_PROMETHEUS_ADAPTER=${DEPLOY_PROMETHEUS_ADAPTER:-true}
DEPLOY_VLLM_EMULATOR=${DEPLOY_VLLM_EMULATOR:-true}
DEPLOY_INFERENCE_MODEL=${DEPLOY_INFERENCE_MODEL:-true}
APPLY_VLLM_EMULATOR_FIXES=${APPLY_VLLM_EMULATOR_FIXES:-true}
SKIP_CHECKS=${SKIP_CHECKS:-false}

# Undeployment flags
UNDEPLOY_ALL=${UNDEPLOY_ALL:-false}
DELETE_CLUSTER=${DELETE_CLUSTER:-false}

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

This script creates a KIND cluster with GPU emulation and deploys the complete
Workload-Variant-Autoscaler stack with llm-d infrastructure and vLLM emulator.

Options:
  -i, --wva-image IMAGE        Container image to use for the WVA (default: $WVA_IMAGE_REPO:$WVA_IMAGE_TAG)
  -n, --nodes NUM              Number of nodes for KIND cluster (default: 3)
  -g, --gpus NUM               Number of GPUs per node (default: 4)  
  -t, --type TYPE              GPU type: nvidia, amd, intel, or mix (default: mix)
  -u, --undeploy               Undeploy all components
  -d, --delete-cluster         Delete the KIND cluster after undeployment
  -h, --help                   Show this help and exit

Environment Variables:
  IMG                          Container image to use for the WVA (alternative to -i flag)
  CLUSTER_NAME                 KIND cluster name (default: kind-wva-gpu-cluster)
  CREATE_CLUSTER               Create KIND cluster if needed (default: true)
  DEPLOY_PROMETHEUS            Deploy Prometheus stack (default: true)
  DEPLOY_WVA                   Deploy WVA controller (default: true)
  DEPLOY_LLM_D                 Deploy llm-d infrastructure (default: true)
  DEPLOY_PROMETHEUS_ADAPTER    Deploy Prometheus Adapter (default: true)
  DEPLOY_VLLM_EMULATOR         Deploy vLLM-emulator (default: true)
  APPLY_VLLM_EMULATOR_FIXES    Apply fixes for vLLM-emulator compatibility (default: true)
  UNDEPLOY_ALL                 Undeploy mode (default: false)
  DELETE_CLUSTER               Delete cluster after undeploy (default: false)

Examples:
  # Deploy with default values (creates cluster, deploys everything)
  $(basename "$0")

  # Deploy using Make with custom image
  make deploy-llm-d-wva-emulated-on-kind IMG=quay.io/infernoautoscaler/inferno-controller:0.0.1-test
  
  # Deploy with custom WVA image via environment variable
  IMG=<your_registry>/workload-variant-autoscaler:tag $(basename "$0")
  
  # Deploy with custom WVA image and cluster configuration
  $(basename "$0") -i <your_registry>/workload-variant-autoscaler:tag -n 3 -g 6 -t mix
  
  # Use existing cluster (skip cluster creation)
  CREATE_CLUSTER=false $(basename "$0")
  
  # Undeploy all components (keep cluster)
  $(basename "$0") --undeploy
  
  # Undeploy all components and delete cluster
  $(basename "$0") --undeploy --delete-cluster
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
      WVA_IMAGE_REPO="$IMG"
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
      -n|--nodes)             CLUSTER_NODES="$2"; shift 2 ;;
      -g|--gpus)              CLUSTER_GPUS="$2"; shift 2 ;;
      -t|--type)              CLUSTER_TYPE="$2"; shift 2 ;;
      -u|--undeploy)          UNDEPLOY_ALL=true; shift ;;
      -d|--delete-cluster)    DELETE_CLUSTER=true; shift ;;
      -h|--help)              print_help; exit 0 ;;
      *)                      log_error "Unknown option: $1"; print_help; exit 1 ;;
    esac
  done
}

check_prerequisites() {
    log_info "Checking prerequisites..."
    
    local missing_tools=()
    
    # Check for required tools
    for tool in kubectl helm yq kind docker; do
        if ! command -v $tool &> /dev/null; then
            missing_tools+=($tool)
        fi
    done
    
    if [ ${#missing_tools[@]} -ne 0 ]; then
        log_error "Missing required tools: ${missing_tools[*]}"
        log_info "Please install the missing tools and try again"
        exit 1
    fi
    
    # Check if Docker is running
    if ! docker info &> /dev/null; then
        log_error "Docker is not running. Please start Docker and try again"
        exit 1
    fi
    
    # Check Kubernetes connection (if cluster exists)
    if kubectl cluster-info &> /dev/null; then
        log_success "Connected to Kubernetes cluster"
    else
        log_info "No Kubernetes cluster detected (will create KIND cluster)"
    fi
    
    log_success "All prerequisites met"
}

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
    
    log_info "Running setup script with: cluster=$CLUSTER_NAME, nodes=$CLUSTER_NODES, gpus=$CLUSTER_GPUS, type=$CLUSTER_TYPE"
    bash "$SETUP_SCRIPT" -c "${CLUSTER_NAME}" -n "$CLUSTER_NODES" -g "$CLUSTER_GPUS" -t "$CLUSTER_TYPE"
    
    # Ensure kubectl context is set to the new cluster
    kubectl config use-context "kind-${CLUSTER_NAME}" &> /dev/null
    
    log_success "KIND cluster '${CLUSTER_NAME}' created successfully"
}

load_image() {
    log_info "Loading WVA image '$WVA_IMAGE_REPO:$WVA_IMAGE_TAG' into KIND cluster..."
    
    # Try to pull the image, or use local image if pull fails
    if ! docker pull "$WVA_IMAGE_REPO:$WVA_IMAGE_TAG"; then
        log_warning "Failed to pull image '$WVA_IMAGE_REPO:$WVA_IMAGE_TAG' from registry"
        log_info "Attempting to use local image..."
        
        # Check if the image exists locally
        if ! docker image inspect "$WVA_IMAGE_REPO:$WVA_IMAGE_TAG" >/dev/null 2>&1; then
            log_error "Image '$WVA_IMAGE_REPO:$WVA_IMAGE_TAG' not found locally either"
            log_info "Please build the image or check the registry"
            exit 1
        else
            log_info "Using local image '$WVA_IMAGE_REPO:$WVA_IMAGE_TAG'"
        fi
    else
        log_success "Successfully pulled image '$WVA_IMAGE_REPO:$WVA_IMAGE_TAG' from registry"
    fi
    
    # Load the image into the KIND cluster
    kind load docker-image "$WVA_IMAGE_REPO:$WVA_IMAGE_TAG" --name "$CLUSTER_NAME"
    
    log_success "Image '$WVA_IMAGE_REPO:$WVA_IMAGE_TAG' loaded into KIND cluster '$CLUSTER_NAME'"
}

get_llm_d_latest() {
    if [ -d "$WVA_PROJECT/$LLM_D_PROJECT" ]; then
        log_warning "$LLM_D_PROJECT directory already exists, skipping clone"
        return
    fi

    log_info "Cloning $LLM_D_PROJECT repository (release: $LLM_D_RELEASE)"
    cd "$WVA_PROJECT"
    git clone -b $LLM_D_RELEASE -- https://github.com/$LLM_D_OWNER/$LLM_D_PROJECT.git $LLM_D_PROJECT
    
    log_success "$LLM_D_PROJECT repository cloned successfully"
}

create_namespaces() {
    log_info "Creating namespaces..."
    
    for ns in $WVA_NS $MONITORING_NAMESPACE $LLMD_NS; do
        if kubectl get namespace $ns &> /dev/null; then
            log_warning "Namespace $ns already exists"
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
        --set wva.modelName=$VLLM_EMULATOR_NAME \
        --set wva.va.enabled=$DEPLOY_VA \
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
    kubectl wait --for=condition=Ready pod -l app.kubernetes.io/name=workload-variant-autoscaler -n $WVA_NS --timeout=60s || log_warning "WVA controller is not ready yet - check 'kubectl get pods -n $WVA_NS'"
    
    log_success "WVA deployment complete"
}

deploy_llm_d_infrastructure() {
    log_info "Deploying llm-d infrastructure..."
    
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

    # Install Gateway provider (kgateway v2.0.3)
    if [ "$GATEWAY_PROVIDER" == "kgateway" ]; then
        log_info "Installing $GATEWAY_PROVIDER v2.0.3"
        yq eval '.releases[].version = "v2.0.3"' -i "$GATEWAY_PREREQ_DIR/$GATEWAY_PROVIDER.helmfile.yaml"
    fi

    if [[ "$INSTALL_GATEWAY_CTRLPLANE" == "true" ]]; then
        log_info "Installing Gateway control plane ($GATEWAY_PROVIDER)"
        helmfile apply -f "$GATEWAY_PREREQ_DIR/$GATEWAY_PROVIDER.helmfile.yaml"
    else
        log_info "Skipping Gateway control plane installation (INSTALL_GATEWAY_CTRLPLANE=false)"
    fi

    # Configure benchmark mode for Istio if enabled
    # TODO: Currently disabled - to be re-enable once we move to llm-d
    # if [[ "$BENCHMARK_MODE" == "true" ]]; then
    #   log_info "Benchmark mode enabled - using benchmark configuration for Istio"
    #   GATEWAY_PROVIDER="istioBench"
    # fi

    # Configure llm-d infrastructure
    log_info "Configuring llm-d infrastructure"
    
    cd $EXAMPLE_DIR

    # Deploy llm-d core components
    log_info "Deploying llm-d core components"
    helmfile apply -e $GATEWAY_PROVIDER -n ${LLMD_NS}

    # TODO: re-enable HTTPRoute once we move to llm-d-inference-simulator - and move to llm-d
    # kubectl apply -f httproute.yaml -n ${LLMD_NS}

    if [ "$GATEWAY_PROVIDER" == "kgateway" ]; then
      log_info "Patching kgateway service to NodePort"
      export GATEWAY_NAME="infra-$WELL_LIT_PATH_NAME-inference-gateway"
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

    # Delete default ModelService deployments
    # TODO: remove once we move to llm-d-inference-simulator
    log_info "Deleting default ModelService deployments..."
    kubectl delete deployments.apps \
        $LLM_D_MODELSERVICE_NAME-decode \
        $LLM_D_MODELSERVICE_NAME-prefill \
        --ignore-not-found -n "$LLMD_NS"
    
    log_info "Waiting for llm-d components to initialize..."
    kubectl wait --for=condition=Available deployment --all -n $LLMD_NS --timeout=60s || \
        log_warning "llm-d components are not ready yet - check 'kubectl get pods -n $LLMD_NS'"
    
    cd "$WVA_PROJECT"
    log_success "llm-d infrastructure deployment complete"
}

# TODO: remove once we move to llm-d-inference-simulator
apply_vllm_emulator_fixes() {
    log_info "Applying vLLM emulator integration fixes..."
    
    # Patch InferencePool to target vLLM emulator port
    kubectl get inferencepool "gaie-$WELL_LIT_PATH_NAME" -n "$LLMD_NS" &> /dev/null || \
        log_error "InferencePool 'gaie-$WELL_LIT_PATH_NAME' not found"

    log_info "Patching InferencePool to target vLLM emulator port 80..."
    kubectl patch inferencepool "gaie-$WELL_LIT_PATH_NAME" -n "$LLMD_NS" --type='merge' \
        -p '{"spec":{"targetPortNumber":80}}' || log_error "Failed to patch InferencePool"
    
    log_success "InferencePool patched successfully"
    
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

deploy_vllm_emulator() {
    log_info "Deploying vLLM Metrics Emulator (No GPU required)..."
    
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
    kubectl wait --for=condition=available deployment/ms-inference-scheduling-llm-d-modelservice-decode -n "$LLMD_NS" --timeout=120s || \
        log_warning "vLLM emulator deployment may still be starting"
    
    log_success "vLLM Emulator deployment complete"
}

deploy_prometheus_adapter() {
    log_info "Deploying Prometheus Adapter..."
    
    # Add Prometheus community helm repo
    log_info "Adding Prometheus community helm repo"
    helm repo add prometheus-community https://prometheus-community.github.io/helm-charts || true
    helm repo update
    
    # Extract Prometheus CA certificate and create ConfigMap
    log_info "Creating prometheus-ca ConfigMap for TLS verification"
    kubectl get secret $PROMETHEUS_SECRET_NAME -n $MONITORING_NAMESPACE -o jsonpath='{.data.tls\.crt}' | base64 -d > $PROM_CA_CERT_PATH
    
    # Create or update prometheus-ca ConfigMap
    kubectl create configmap prometheus-ca --from-file=ca.crt=$PROM_CA_CERT_PATH -n $MONITORING_NAMESPACE --dry-run=client -o yaml | kubectl apply -f -
    
    log_success "prometheus-ca ConfigMap created/updated"
    
    # Create prometheus-adapter values
    cat > /tmp/prometheus-adapter-values-kind.yaml <<YAML
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
  enable: false

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
YAML
    
    # Deploy Prometheus Adapter
    log_info "Installing Prometheus Adapter via Helm"
    helm upgrade -i prometheus-adapter prometheus-community/prometheus-adapter \
        -n $MONITORING_NAMESPACE \
        -f /tmp/prometheus-adapter-values-kind.yaml \
        --timeout=3m \
        --wait || {
            log_warning "Prometheus Adapter deployment failed"
        }
    
    log_success "Prometheus Adapter deployment initiated"
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
        
        # Check for vLLM emulator
        if kubectl get deployment $VLLM_EMULATOR_NAME-decode -n $LLMD_NS &> /dev/null; then
            log_success "vLLM emulator deployment exists"
            if kubectl get pods -n $LLMD_NS -l app=$VLLM_EMULATOR_NAME 2>/dev/null | grep -q Running; then
                log_success "vLLM emulator pods are running"
            else
                log_warning "vLLM emulator pods may still be starting"
                all_good=false
            fi
        else
            log_warning "vLLM emulator deployment not found"
            all_good=false
        fi
    fi
    
    # Check VariantAutoscaling
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
    echo "Cluster Name:           $CLUSTER_NAME"
    echo "WVA Namespace:          $WVA_NS"
    echo "LLMD Namespace:         $LLMD_NS"
    echo "Monitoring Namespace:   $MONITORING_NAMESPACE"
    echo "Model:                  $MODEL_ID"
    echo "Accelerator:            $ACCELERATOR_TYPE (emulated)"
    echo "WVA Image:              $WVA_IMAGE_REPO:$WVA_IMAGE_TAG"
    echo "Cluster Nodes:          $CLUSTER_NODES"
    echo "Cluster GPUs:           $CLUSTER_GPUS (emulated)"
    echo "Cluster Type:           $CLUSTER_TYPE"
    echo "Gateway Provider:       $GATEWAY_PROVIDER"
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
        echo "✓ vLLM Emulator (no GPU required)"
    fi
    if [ "$DEPLOY_PROMETHEUS_ADAPTER" = "true" ]; then
        echo "✓ Prometheus Adapter (external metrics API)"
    fi
    echo "✓ VariantAutoscaling CR (via Helm chart)"
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
    echo "6. Test the emulated environment by generating load:"
    echo "   # Port-forward the gateway first:"
    echo "   kubectl port-forward -n $LLMD_NS svc/infra-$WELL_LIT_PATH_NAME-inference-gateway 8000:80"
    echo "   # Then run the vLLM-emulator load generator script in another terminal using:"
    echo ""
    echo "   cd tools/vllm-emulator"
    echo "   pip install -r requirements.txt"
    echo "   python loadgen.py --model $MODEL_ID"
    echo ""
    echo "Important Notes:"
    echo "================"
    echo ""
    echo "• This is an EMULATED environment (no real GPUs or models)"
    echo "• vLLM emulator generates synthetic metrics for testing"
    echo "• Perfect for development and testing without GPU hardware"
    echo "• All metrics are simulated - not real inference workload"
    echo ""
    echo "Troubleshooting:"
    echo "================"
    echo ""
    echo "• Check WVA controller logs:"
    echo "  kubectl logs -n $WVA_NS -l app.kubernetes.io/name=workload-variant-autoscaler --tail=50"
    echo ""
    echo "• Check all pods in llm-d namespace:"
    echo "  kubectl get pods -n $LLMD_NS"
    echo ""
    echo "• Check if metrics are being scraped by Prometheus:"
    echo "  kubectl port-forward -n $MONITORING_NAMESPACE svc/kube-prometheus-stack-prometheus 9090:9090"
    echo "  # Then visit https://localhost:9090 and query: vllm:request_success_total"
    echo ""
    echo "• Delete and recreate cluster:"
    echo "  kind delete cluster"
    echo "  $0"
    echo ""
    echo "=========================================="
}

# Undeployment functions
undeploy_prometheus_adapter() {
    log_info "Uninstalling Prometheus Adapter..."
    helm uninstall prometheus-adapter -n $MONITORING_NAMESPACE 2>/dev/null || \
        log_warning "Prometheus Adapter not found or already uninstalled"
    
    kubectl delete configmap prometheus-ca -n $MONITORING_NAMESPACE --ignore-not-found
    rm -f /tmp/prometheus-adapter-values-kind.yaml
    
    log_success "Prometheus Adapter uninstalled"
}

undeploy_vllm_emulator() {
    log_info "Removing vLLM Emulator..."

    kubectl delete servicemonitor $VLLM_EMULATOR_NAME-servicemonitor -n $MONITORING_NAMESPACE --ignore-not-found
    kubectl delete service $VLLM_EMULATOR_NAME-service -n $LLMD_NS --ignore-not-found
    kubectl delete deployment $VLLM_EMULATOR_NAME -n $LLMD_NS --ignore-not-found

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
    
    # Remove Gateway provider
    if [[ "$INSTALL_GATEWAY_CTRLPLANE" == true ]]; then
        log_info "Removing Gateway provider..."
        helmfile destroy -f "$GATEWAY_PREREQ_DIR/$GATEWAY_PROVIDER.helmfile.yaml" 2>/dev/null || \
            log_warning "Gateway provider cleanup incomplete"
        kubectl delete namespace ${GATEWAY_PROVIDER}-system --ignore-not-found 2>/dev/null || true

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

delete_kind_cluster() {
    log_info "Deleting KIND cluster '${CLUSTER_NAME}'..."
    
    if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
        kind delete cluster --name "${CLUSTER_NAME}"
        log_success "KIND cluster '${CLUSTER_NAME}' deleted"
    else
        log_warning "KIND cluster '${CLUSTER_NAME}' not found"
    fi
}

undeploy_all() {
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
    
    # Delete namespaces
    delete_namespaces
    
    # Delete cluster if requested
    if [ "$DELETE_CLUSTER" = "true" ]; then
        delete_kind_cluster
    else
        log_info "Keeping KIND cluster '${CLUSTER_NAME}' (use --delete-cluster to remove)"
    fi
    
    # Remove llm-d repository
    if [ -d "$WVA_PROJECT/$LLM_D_PROJECT" ]; then
        log_info "llm-d repository at $WVA_PROJECT/$LLM_D_PROJECT preserved (manual cleanup if needed)"
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
    echo "✓ Namespaces"
    
    if [ "$DELETE_CLUSTER" = "true" ]; then
        echo "✓ KIND Cluster"
    else
        echo ""
        echo "KIND cluster '${CLUSTER_NAME}' is still running."
        echo "To delete it manually: kind delete cluster --name ${CLUSTER_NAME}"
    fi
    echo ""
    echo "=========================================="
}


# Main deployment flow
main() {
    # Parse command line arguments first
    parse_args "$@"

    # Undeploy mode
    if [ "$UNDEPLOY_ALL" = "true" ]; then
        log_info "Starting Workload-Variant-Autoscaler KIND Emulator Undeployment"
        log_info "=================================================="
        echo ""
        undeploy_all
        exit 0
    fi

    # Normal deployment flow
    log_info "Starting Workload-Variant-Autoscaler KIND Emulator Deployment"
    log_info "=============================================================="
    echo ""
    
    # Check prerequisites
    if [ "$SKIP_CHECKS" != "true" ]; then
        check_prerequisites
    fi

    # Create or use existing KIND cluster
    if [ "$CREATE_CLUSTER" = "true" ]; then
        # Check if the specific cluster exists
        if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
            log_info "KIND cluster '${CLUSTER_NAME}' already exists, tearing it down and recreating..."
            kind delete cluster --name "${CLUSTER_NAME}"
            create_kind_cluster
            # Set kubectl context to this cluster
            kubectl config use-context "kind-${CLUSTER_NAME}" &> /dev/null
        else
            log_info "KIND cluster '${CLUSTER_NAME}' not found, creating it..."
            create_kind_cluster
        fi
    else
        log_info "Cluster creation skipped (CREATE_CLUSTER=false)"
        # Verify the Kind cluster exists
        if ! kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
            log_error "KIND cluster '${CLUSTER_NAME}' not found and CREATE_CLUSTER=false"
            exit 1
        fi
        # Set kubectl context to the Kind cluster
        kubectl config use-context "kind-${CLUSTER_NAME}" &> /dev/null
    fi
    # Verify kubectl can connect to the cluster
    if ! kubectl cluster-info &> /dev/null; then
        log_error "Failed to connect to KIND cluster '${CLUSTER_NAME}'"
        exit 1
    fi
    log_success "Using KIND cluster '${CLUSTER_NAME}'"

    # Load WVA image into KIND cluster
    load_image

    # Display configuration
    log_info "Using configuration:"
    echo "    Cluster Name:     $CLUSTER_NAME"
    echo "    WVA Image:        $WVA_IMAGE_REPO:$WVA_IMAGE_TAG"
    echo "    Cluster Nodes:    $CLUSTER_NODES"
    echo "    Cluster GPUs:     $CLUSTER_GPUS (emulated)"
    echo "    Cluster Type:     $CLUSTER_TYPE"
    echo "    WVA Namespace:    $WVA_NS"
    echo "    LLMD Namespace:   $LLMD_NS"
    echo "    Model:            $MODEL_ID"
    echo "    Accelerator:      $ACCELERATOR_TYPE (emulated)"
    echo "    Gateway:          $GATEWAY_PROVIDER"
    echo ""
    
    # Get llm-d repository
    get_llm_d_latest
    
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
        
        # Deploy vLLM emulator pods
        if [ "$DEPLOY_VLLM_EMULATOR" = "true" ]; then
            log_info "vLLM Emulator deployment enabled (DEPLOY_VLLM_EMULATOR=true)"
            deploy_vllm_emulator
        else
            log_info "vLLM Emulator deployment disabled (DEPLOY_VLLM_EMULATOR=false), skipping"
        fi
        
        if [ "$APPLY_VLLM_EMULATOR_FIXES" = "true" ]; then
            log_info "Applying vLLM emulator integration fixes (APPLY_VLLM_EMULATOR_FIXES=true)"
            
            # Apply vLLM emulator-specific fixes
            apply_vllm_emulator_fixes
        else
            log_info "Skipping vLLM emulator integration fixes (APPLY_VLLM_EMULATOR_FIXES=false)"
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