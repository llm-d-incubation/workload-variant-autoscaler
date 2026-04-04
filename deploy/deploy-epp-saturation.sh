#!/usr/bin/env bash
#
# Deploy WVA with EPP saturation analyzer on GKE/Kubernetes.
#
# Prerequisites:
#   - kubectl configured for the target cluster
#   - EPP with saturation detector plugin deployed and emitting
#     inference_extension_latency_detector_pool_saturation to Prometheus
#   - Prometheus scraping the EPP pods
#   - Helm 3 installed
#
# Usage:
#   # Build and deploy with defaults
#   ./deploy/deploy-epp-saturation.sh
#
#   # Custom image and Prometheus URL
#   IMG=us-docker.pkg.dev/my-project/my-repo/wva:latest \
#   PROMETHEUS_URL=http://prometheus.monitoring.svc.cluster.local:9090 \
#     ./deploy/deploy-epp-saturation.sh
#
#   # Dry-run (print helm command without executing)
#   DRY_RUN=true ./deploy/deploy-epp-saturation.sh
#

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# --- Configurable variables (override via environment) ---

# Container image
IMG="${IMG:-us-docker.pkg.dev/kaushikmitra-gke-dev/kaushikmitra-docker-repo/wva:epp-saturation}"
BUILD_IMAGE="${BUILD_IMAGE:-true}"

# Kubernetes
WVA_NAMESPACE="${WVA_NAMESPACE:-workload-variant-autoscaler-system}"
MODEL_NAMESPACE="${MODEL_NAMESPACE:-}"  # Namespace where model pods live (for VA creation)

# Prometheus
PROMETHEUS_URL="${PROMETHEUS_URL:-http://kube-prometheus-stack-prometheus.monitoring.svc.cluster.local:9090}"
SKIP_TLS_VERIFY="${SKIP_TLS_VERIFY:-true}"

# EPP saturation analyzer thresholds
SCALE_UP_THRESHOLD="${SCALE_UP_THRESHOLD:-0.85}"
SCALE_DOWN_BOUNDARY="${SCALE_DOWN_BOUNDARY:-0.50}"

# HPA
HPA_MIN_REPLICAS="${HPA_MIN_REPLICAS:-1}"
HPA_MAX_REPLICAS="${HPA_MAX_REPLICAS:-20}"

# Model (for VariantAutoscaling CR)
MODEL_ID="${MODEL_ID:-}"
SCALE_TARGET_NAME="${SCALE_TARGET_NAME:-}"
SCALE_TARGET_KIND="${SCALE_TARGET_KIND:-Deployment}"
VARIANT_COST="${VARIANT_COST:-10.0}"

# Helm
HELM_RELEASE="${HELM_RELEASE:-workload-variant-autoscaler}"
NAMESPACE_SCOPED="${NAMESPACE_SCOPED:-false}"

# Control
DRY_RUN="${DRY_RUN:-false}"
SKIP_VA="${SKIP_VA:-false}"  # Skip VariantAutoscaling CR creation

# ---

log() { echo ">>> $*"; }

# Step 1: Build and push image
if [[ "${BUILD_IMAGE}" == "true" ]]; then
    log "Building and pushing image: ${IMG}"
    if [[ "${DRY_RUN}" == "true" ]]; then
        echo "  [dry-run] make docker-build docker-push IMG=${IMG}"
    else
        make -C "${REPO_ROOT}" docker-build docker-push IMG="${IMG}"
    fi
else
    log "Skipping image build (BUILD_IMAGE=false)"
fi

# Step 2: Deploy via Helm
log "Deploying WVA with EPP saturation analyzer"
log "  Namespace: ${WVA_NAMESPACE}"
log "  Image: ${IMG}"
log "  Prometheus: ${PROMETHEUS_URL}"
log "  Scale-up threshold: ${SCALE_UP_THRESHOLD}"
log "  Scale-down boundary: ${SCALE_DOWN_BOUNDARY}"
log "  HPA range: ${HPA_MIN_REPLICAS}-${HPA_MAX_REPLICAS}"

HELM_CMD=(
    helm upgrade -i "${HELM_RELEASE}"
    "${REPO_ROOT}/charts/workload-variant-autoscaler"
    -n "${WVA_NAMESPACE}" --create-namespace
    --set "wva.image.repository=$(echo "${IMG}" | rev | cut -d: -f2- | rev)"
    --set "wva.image.tag=$(echo "${IMG}" | rev | cut -d: -f1 | rev)"
    --set "wva.prometheus.baseURL=${PROMETHEUS_URL}"
    --set "wva.prometheus.tls.insecureSkipVerify=${SKIP_TLS_VERIFY}"
    --set "wva.namespaceScoped=${NAMESPACE_SCOPED}"
    --set "wva.capacityScaling.default.analyzerName=epp-saturation"
    --set "wva.capacityScaling.default.scaleUpThreshold=${SCALE_UP_THRESHOLD}"
    --set "wva.capacityScaling.default.scaleDownBoundary=${SCALE_DOWN_BOUNDARY}"
    --set "hpa.minReplicas=${HPA_MIN_REPLICAS}"
    --set "hpa.maxReplicas=${HPA_MAX_REPLICAS}"
    --set "controller.enabled=true"
)

if [[ "${DRY_RUN}" == "true" ]]; then
    echo "  [dry-run] ${HELM_CMD[*]}"
else
    "${HELM_CMD[@]}"
fi

# Step 3: Create VariantAutoscaling CR (optional)
if [[ "${SKIP_VA}" == "true" ]]; then
    log "Skipping VariantAutoscaling CR creation (SKIP_VA=true)"
elif [[ -z "${MODEL_ID}" || -z "${SCALE_TARGET_NAME}" || -z "${MODEL_NAMESPACE}" ]]; then
    log "Skipping VariantAutoscaling CR (set MODEL_ID, SCALE_TARGET_NAME, MODEL_NAMESPACE to create one)"
else
    log "Creating VariantAutoscaling CR for ${MODEL_ID} in ${MODEL_NAMESPACE}"
    VA_YAML=$(cat <<EOF
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: ${SCALE_TARGET_NAME}-va
  namespace: ${MODEL_NAMESPACE}
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: ${SCALE_TARGET_KIND}
    name: ${SCALE_TARGET_NAME}
  modelID: "${MODEL_ID}"
  minReplicas: ${HPA_MIN_REPLICAS}
  maxReplicas: ${HPA_MAX_REPLICAS}
  variantCost: "${VARIANT_COST}"
EOF
)
    if [[ "${DRY_RUN}" == "true" ]]; then
        echo "  [dry-run] kubectl apply:"
        echo "${VA_YAML}"
    else
        echo "${VA_YAML}" | kubectl apply -f -
    fi
fi

# Step 4: Verification hints
log "Deployment complete. Verify with:"
echo "  # Check WVA pods"
echo "  kubectl get pods -n ${WVA_NAMESPACE}"
echo ""
echo "  # Check WVA logs for EPP saturation readings"
echo "  kubectl logs -n ${WVA_NAMESPACE} -l control-plane=controller-manager -f | grep 'EPP'"
echo ""
echo "  # Check the saturation metric is available in Prometheus"
echo "  kubectl exec -n monitoring \$(kubectl get pods -n monitoring -l app.kubernetes.io/name=prometheus -o name | head -1) -- \\"
echo "    wget -qO- 'http://localhost:9090/api/v1/query?query=inference_extension_latency_detector_pool_saturation'"
echo ""
echo "  # Check VA status"
echo "  kubectl get variantautoscaling -A"
