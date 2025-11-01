#!/bin/bash
#
# Workload-Variant-Autoscaler OpenShift Deployment Script
# This script automates the deployment of WVA and llm-d infrastructure on OpenShift
#
# Prerequisites:
# - OpenShift CLI (oc) installed and configured
# - kubectl installed
# - helm installed
# - yq installed
# - Access to an OpenShift cluster with admin privileges
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

if [[ -z "${WELL_LIT_PATH_NAME}" ]]; then
    WELL_LIT_PATH_NAME="inference-scheduling"
fi

if [[ -z "${WVA_PROJECT}" ]]; then
    WVA_PROJECT="$PWD"
fi

if [[ -z "${LLM_D_PROJECT}" ]]; then
    LLM_D_PROJECT="llm-d"
fi

# Configuration
LLMD_NS=${LLMD_NS:-"llm-d-$WELL_LIT_PATH_NAME"}
MONITORING_NAMESPACE=${MONITORING_NAMESPACE:-"openshift-user-workload-monitoring"}
WVA_NS=${WVA_NS:-"workload-variant-autoscaler-system"}
WVA_IMAGE_REPO=${WVA_IMAGE_REPO:-"ghcr.io/llm-d/workload-variant-autoscaler"}
WVA_IMAGE_TAG=${WVA_IMAGE_TAG:-"v0.0.2"}
LLM_D_OWNER=${LLM_D_OWNER:-"llm-d"}
LLM_D_RELEASE=${LLM_D_RELEASE:-"v0.3.0"}
LLM_D_MODELSERVICE_NAME=${LLM_D_MODELSERVICE_NAME:-"ms-$WELL_LIT_PATH_NAME-llm-d-modelservice-decode"}
PREREQ_DIR=${PREREQ_DIR:-"$WVA_PROJECT/$LLM_D_PROJECT/guides/prereq"}
EXAMPLE_DIR=${EXAMPLE_DIR:-"$WVA_PROJECT/$LLM_D_PROJECT/guides/$WELL_LIT_PATH_NAME"}
PROM_CA_CERT_PATH=${PROM_CA_CERT_PATH:-"/tmp/prometheus-ca.crt"}
VLLM_SVC_ENABLED=${VLLM_SVC_ENABLED:-"true"}
VLLM_SVC_NODEPORT=${VLLM_SVC_NODEPORT:-30000}
DEFAULT_MODEL_ID=${DEFAULT_MODEL_ID:-"Qwen/Qwen3-0.6B"}
MODEL_ID=${MODEL_ID:-"unsloth/Meta-Llama-3.1-8B"}
ACCELERATOR_TYPE=${ACCELERATOR_TYPE:-"H100"}
SLO_TPOT=${SLO_TPOT:-9}  # Target time-per-output-token SLO (in ms)
SLO_TTFT=${SLO_TTFT:-1000}  # Target time-to-first-token SLO (in ms)
THANOS_SVC_URL=${THANOS_SVC_URL:-"https://thanos-querier.openshift-monitoring.svc.cluster.local"}
THANOS_PORT=${THANOS_PORT:-"9091"}
THANOS_URL=${THANOS_URL:-"$THANOS_SVC_URL:$THANOS_PORT"}
GATEWAY_PROVIDER=${GATEWAY_PROVIDER:-"istio"}
BENCHMARK_MODE=${BENCHMARK_MODE:-"true"} # if true, updates to Istio config for benchmark
INSTALL_GATEWAY_CTRLPLANE=${INSTALL_GATEWAY_CTRLPLANE:-"false"}

# Flags for deployment steps
DEPLOY_WVA=${DEPLOY_WVA:-true}
DEPLOY_LLM_D=${DEPLOY_LLM_D:-true}
DEPLOY_PROMETHEUS_ADAPTER=${DEPLOY_PROMETHEUS_ADAPTER:-true}
SKIP_CHECKS=${SKIP_CHECKS:-false}

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
}

check_prerequisites() {
    log_info "Checking prerequisites..."
    
    local missing_tools=()
    
    # Check for required tools
    for tool in oc kubectl helm yq; do
        if ! command -v $tool &> /dev/null; then
            missing_tools+=($tool)
        fi
    done
    
    if [ ${#missing_tools[@]} -ne 0 ]; then
        log_error "Missing required tools: ${missing_tools[*]}"
        log_info "Please install the missing tools and try again"
        exit 1
    fi
    
    # Check OpenShift connection
    if ! oc whoami &> /dev/null; then
        log_error "Not logged into OpenShift cluster. Please run 'oc login' first"
        exit 1
    fi
    
    log_success "All prerequisites met"
    log_info "Connected to OpenShift as: $(oc whoami)"
    log_info "Current project: $(oc project -q)"
}

detect_gpu_type() {
    log_info "Detecting GPU type in cluster..."
    
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
    else
        log_warning "No GPUs detected in cluster, using default: $ACCELERATOR_TYPE"
    fi
    
    export ACCELERATOR_TYPE
    log_info "Using accelerator type: $ACCELERATOR_TYPE"
}

find_thanos_url() {
    log_info "Finding Thanos querier URL..."
    
    local thanos_svc=$(kubectl get svc -n openshift-monitoring thanos-querier -o jsonpath='{.metadata.name}' 2>/dev/null || echo "")
    
    if [ -n "$thanos_svc" ]; then
        THANOS_URL="https://thanos-querier.openshift-monitoring.svc.cluster.local:9091"
        log_success "Found Thanos querier: $THANOS_URL"
    else
        log_error "Thanos querier service not found in openshift-monitoring namespace"
        exit 1
    fi
    
    export THANOS_URL
}

create_namespace() {
    log_info "Creating llm-d namespace: $LLMD_NS"
    
    if kubectl get namespace $LLMD_NS &> /dev/null; then
        log_warning "Namespace $LLMD_NS already exists"
    else
        kubectl create namespace $LLMD_NS
        log_success "Namespace $LLMD_NS created"
    fi

    log_info "Creating WVA namespace: $WVA_NS"

    if kubectl get namespace $WVA_NS &> /dev/null; then
        log_warning "Namespace $WVA_NS already exists"
    else
        kubectl create namespace $WVA_NS
        log_success "Namespace $WVA_NS created"
    fi
}

deploy_wva_controller() {
    log_info "Deploying Workload-Variant-Autoscaler..."

    if [[ -z "${IMG}" ]]; then
        WVA_IMAGE_REPO="ghcr.io/llm-d/workload-variant-autoscaler"
        WVA_IMAGE_TAG="v0.0.1"
    else
        IFS=':' read -r WVA_IMAGE_REPO WVA_IMAGE_TAG <<< "$IMG"
    fi

    # Extract Thanos TLS certificate
    log_info "Extracting Thanos TLS certificate"
    kubectl get secret thanos-querier-tls -n openshift-monitoring -o jsonpath='{.data.tls\.crt}' | base64 -d > $PROM_CA_CERT_PATH

    # Update Prometheus URL in configmap
    log_info "Updating Prometheus URL in config/manager/configmap.yaml"
    sed -i.bak "s|PROMETHEUS_BASE_URL:.*|PROMETHEUS_BASE_URL: \"$THANOS_URL\"|" config/manager/configmap.yaml
    
    # Deploy WVA
    log_info "Installing Workload-Variant-Autoscaler via Helm chart"

    cd $WVA_PROJECT/charts

    # TODO: update to use Helm repo
    helm upgrade -i workload-variant-autoscaler ./workload-variant-autoscaler \
    -n $WVA_NS \
    --set-file wva.prometheus.caCert=$PROM_CA_CERT_PATH \
    --set wva.image.repository=$WVA_IMAGE_REPO \
    --set wva.image.tag=$WVA_IMAGE_TAG \
    --set va.accelerator=$ACCELERATOR_TYPE \
    --set llmd.modelID=$MODEL_ID \
    --set va.sloTpot=$SLO_TPOT \
    --set va.sloTtft=$SLO_TTFT \
    --set vllmService.enabled=$VLLM_SVC_ENABLED \
    --set vllmService.nodePort=$VLLM_SVC_NODEPORT

    cd $WVA_PROJECT
    
    log_success "Workload-Variant-Autoscaler deployed"
}

deploy_llm_d_infrastructure() {
    log_info "Deploying llm-d infrastructure..."

    if [ "$BENCHMARK_MODE" == "true" ]; then
        log_info "Benchmark mode enabled - using benchmark configuration for Istio"
        GATEWAY_PROVIDER="istioBench"
    fi
    
    # Check for HF_TOKEN
    if [ -z "$HF_TOKEN" ]; then
        log_error "HF_TOKEN environment variable is not set"
        log_info "Please set your HuggingFace token: export HF_TOKEN='your-token-here'"
        exit 1
    fi
    
    # Create HF token secret
    log_info "Creating HuggingFace token secret"
    kubectl create secret generic llm-d-hf-token \
        --from-literal="HF_TOKEN=${HF_TOKEN}" \
        --namespace "${LLMD_NS}" \
        --dry-run=client -o yaml | kubectl apply -f -
    
    # Clone llm-d-infra if not exists
    if [ ! -d "$LLM_D_PROJECT" ]; then
        log_info "Cloning $LLM_D_PROJECT repository (release: $LLM_D_RELEASE)"
        git clone -b $LLM_D_RELEASE -- https://github.com/$LLM_D_OWNER/$LLM_D_PROJECT.git $LLM_D_PROJECT
    else
        log_warning "$LLM_D_PROJECT directory already exists, skipping clone"
    fi
    
    # Install dependencies
    log_info "Installing llm-d dependencies"
    bash $PREREQ_DIR/client-setup/install-deps.sh
    bash $PREREQ_DIR/gateway-provider/install-gateway-provider-dependencies.sh

    if [[ "$GATEWAY_PROVIDER" != "kgateway" && "$GATEWAY_PROVIDER" != "istio" && "$GATEWAY_PROVIDER" != "istioBench" ]]; then
        log_error "Unsupported gateway provider: $GATEWAY_PROVIDER"
        exit 1
    fi

    # Install Gateway provider (if kgateway, using v2.0.3)
    if [ "$GATEWAY_PROVIDER" == "kgateway" ]; then
        log_info "Installing $GATEWAY_PROVIDER v2.0.3"
        yq eval '.releases[].version = "v2.0.3"' -i "gateway-control-plane-providers/$GATEWAY_PROVIDER.helmfile.yaml"
    fi

    if [ "$INSTALL_GATEWAY_CTRLPLANE" == "true" ]; then
        log_info "Installing Gateway control plane ($GATEWAY_PROVIDER)"
        helmfile apply -f "$PREREQ_DIR/gateway-provider/$GATEWAY_PROVIDER.helmfile.yaml"
    else
        log_info "Skipping Gateway control plane installation (INSTALL_GATEWAY_CTRLPLANE=false)"
    fi

    # Configure llm-d for NodePort and correct model
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
    
    cd $WVA_PROJECT
    log_success "llm-d infrastructure deployment complete"
}

deploy_prometheus_adapter() {
    log_info "Deploying Prometheus Adapter..."
    
    # Create CA ConfigMap on Thanos TLS certificate
    kubectl create configmap prometheus-ca \
        --from-file=ca.crt=$PROM_CA_CERT_PATH \
        -n $MONITORING_NAMESPACE \
        --dry-run=client -o yaml | kubectl apply -f -
    
    # Add Prometheus community helm repo
    log_info "Adding Prometheus community helm repo"
    helm repo add prometheus-community https://prometheus-community.github.io/helm-charts || true
    helm repo update
    
    # Create prometheus-adapter-values-ocp.yaml if it doesn't exist
    if [ ! -f config/samples/prometheus-adapter-values-ocp.yaml ]; then
        log_info "Creating prometheus-adapter-values-ocp.yaml"
        cat > config/samples/prometheus-adapter-values-ocp.yaml <<EOF
prometheus:
  url: $THANOS_SVC_URL
  port: $THANOS_PORT

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
    fi
    
    # Deploy Prometheus Adapter
    log_info "Installing Prometheus Adapter via Helm"
    helm upgrade -i prometheus-adapter prometheus-community/prometheus-adapter \
        -n $MONITORING_NAMESPACE \
        -f config/samples/prometheus-adapter-values-ocp.yaml
    
    log_success "Prometheus Adapter deployment complete"
}

apply_vllm_probes() {
    log_info "Applying probe configuration to vLLM deployment..."
    
    # Create probes-patch.yaml if it doesn't exist
    if [ ! -f config/samples/probes-patch.yaml ]; then
        cat > config/samples/probes-patch.yaml <<'YAML'
spec:
  template:
    spec:
      containers:
      - name: vllm
        readinessProbe:
            httpGet:
              path: /health
              port: 8200
              scheme: HTTP
            periodSeconds: 1
            successThreshold: 1
            failureThreshold: 1
            timeoutSeconds: 1
        startupProbe:
            failureThreshold: 600
            initialDelaySeconds: 30
            periodSeconds: 1
            httpGet:
              path: /health
              port: 8200
              scheme: HTTP
YAML
    fi
    
    kubectl patch deployment $LLM_D_MODELSERVICE_NAME \
        -n $LLMD_NS \
        --patch-file config/samples/probes-patch.yaml
    
    log_success "Probe configuration applied"
}

verify_deployment() {
    log_info "Verifying deployment..."
    
    local all_good=true
    
    # Check WVA pods
    log_info "Checking WVA controller pods..."
    if kubectl get pods -n $WVA_NS -l app.kubernetes.io/name=workload-variant-autoscaler | grep -q Running; then
        log_success "WVA controller is running"
    else
        log_error "WVA controller is not running"
        all_good=false
    fi
    
    # Check llm-d pods
    log_info "Checking llm-d infrastructure..."
    if kubectl get deployment $LLM_D_MODELSERVICE_NAME -n $LLMD_NS &> /dev/null; then
        log_success "vLLM deployment exists"
    else
        log_error "vLLM deployment not found"
        all_good=false
    fi
    
    # Check Prometheus Adapter
    log_info "Checking Prometheus Adapter..."
    if kubectl get pods -n $MONITORING_NAMESPACE -l app.kubernetes.io/name=prometheus-adapter | grep -q Running; then
        log_success "Prometheus Adapter is running"
    else
        log_error "Prometheus Adapter is not running"
        all_good=false
    fi
    
    # Check VariantAutoscaling
    log_info "Checking VariantAutoscaling resource..."
    if kubectl get variantautoscaling $LLM_D_MODELSERVICE_NAME -n $LLMD_NS &> /dev/null; then
        log_success "VariantAutoscaling resource exists"
    else
        log_error "VariantAutoscaling resource not found"
        all_good=false
    fi
    
    # Check HPA
    log_info "Checking HPA..."
    if kubectl get hpa vllm-deployment-hpa -n $LLMD_NS &> /dev/null; then
        log_success "HPA exists"
    else
        log_error "HPA not found"
        all_good=false
    fi
    
    # Check external metrics API
    log_info "Checking external metrics API..."
    sleep 30  # Wait for metrics to be available
    if kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/$LLMD_NS/inferno_desired_replicas" &> /dev/null; then
        log_success "External metrics API is accessible"
    else
        log_warning "External metrics API not yet available (may need more time)"
    fi
    
    if [ "$all_good" = true ]; then
        log_success "All components verified successfully!"
    else
        log_warning "Some components failed verification. Check the logs above."
    fi
}

print_summary() {
    echo ""
    echo "=========================================="
    echo " Deployment Summary"
    echo "=========================================="
    echo ""
    echo "Namespace:              $LLMD_NS"
    echo "Monitoring Namespace:   $MONITORING_NAMESPACE"
    echo "Model:                  $MODEL_ID"
    echo "Accelerator:            $ACCELERATOR_TYPE"
    echo "SLO (TPOT):             $SLO_TPOT ms"
    echo "SLO (TTFT):             $SLO_TTFT ms"
    if [ "$BENCHMARK_MODE" == "true" ]; then
        echo "Gateway Provider:       $GATEWAY_PROVIDER (benchmark mode)"
    else
        echo "Gateway Provider:       $GATEWAY_PROVIDER"
    fi
    if [ "$DEPLOY_WVA" == "true" ]; then
        echo "WVA Image:              $WVA_IMAGE_REPO":"$WVA_IMAGE_TAG"

    fi
    echo ""
    echo "Next Steps:"
    echo "==========="
    echo ""
    echo "1. Check deployment status:"
    echo "   kubectl get pods -n $LLMD_NS"
    echo "   kubectl get variantautoscaling -n $LLMD_NS"
    echo "   kubectl get hpa -n $LLMD_NS"
    echo ""
    echo "2. View WVA logs:"
    echo "   kubectl logs -n $WVA_NS deployment/workload-variant-autoscaler-controller-manager -f"
    echo ""
    echo "3. Check external metrics:"
    echo "   kubectl get --raw \"/apis/external.metrics.k8s.io/v1beta1/namespaces/$LLMD_NS/inferno_desired_replicas\" | jq"
    echo ""
    echo "4. Run e2e tests:"
    echo "   make test-e2e-openshift"
    echo ""
    echo "5. For troubleshooting on scaling decisions, check the logs of the WVA controller leader:"
    echo "   kubectl logs -n $WVA_NS deployment/workload-variant-autoscaler-controller-manager -f"
    echo "   Look for logs indicating scaling decisions and potential allocation errors."
    echo ""
    echo "=========================================="
}

# Main deployment flow
main() {
    log_info "Starting Workload-Variant-Autoscaler OpenShift Deployment"
    log_info "=========================================================="
    echo ""
    
    # Check prerequisites
    if [ "$SKIP_CHECKS" != "true" ]; then
        check_prerequisites
    fi
    
    # Detect GPU type
    detect_gpu_type
    
    # Find Thanos URL
    find_thanos_url
    
    # Create namespace
    create_namespace
    
    # Deploy WVA
    if [ "$DEPLOY_WVA" = "true" ]; then
        deploy_wva_controller
    else
        log_info "Skipping WVA deployment (DEPLOY_WVA=false)"
    fi
    
    # Deploy llm-d
    if [ "$DEPLOY_LLM_D" = "true" ]; then
        deploy_llm_d_infrastructure
        
        # Apply vLLM probes
        apply_vllm_probes
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

