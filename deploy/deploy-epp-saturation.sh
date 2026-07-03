#!/usr/bin/env bash
#
# Deploy WVA with EPP saturation analyzer on GKE/Kubernetes.
#
# Prerequisites:
#   - kubectl configured for the target cluster
#   - EPP with the predicted-latency producer plugin deployed; WVA derives the
#     saturation signal itself from the EPP's predicted/actual TTFT and TPOT
#     histograms (llm_d_epp_request_predicted_ttft_seconds etc.) vs the
#     configured SLOs — requests must carry x-llm-d-slo-*-ms headers
#   - Prometheus scraping the EPP pods (bearer-auth; the EPP ServiceAccount
#     needs system:auth-delegator — see
#     config/samples/epp-saturation-benchmark/)
#   - Helm 3 installed
#
# IMPORTANT: WVA requires HTTPS for Prometheus (hard validation in main.go).
# If your Prometheus only serves HTTP, see deploy/deploy-prometheus-tls-proxy.sh
# for a TLS-terminating nginx proxy.
#
# Usage:
#   # Build and deploy with defaults
#   ./deploy/deploy-epp-saturation.sh
#
#   # Custom image and Prometheus URL
#   IMG=us-docker.pkg.dev/my-project/my-repo/wva:latest \
#   PROMETHEUS_URL=https://prometheus.monitoring.svc.cluster.local:9443 \
#     ./deploy/deploy-epp-saturation.sh
#
#   # Observe-only mode (WVA computes recommendations, HPA disabled)
#   OBSERVE_ONLY=true MODEL_ID=Qwen/Qwen3-32B SCALE_TARGET_NAME=my-model \
#   MODEL_NAMESPACE=default ./deploy/deploy-epp-saturation.sh
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

# Prometheus (MUST be HTTPS - WVA validates the scheme)
PROMETHEUS_URL="${PROMETHEUS_URL:-https://kube-prometheus-stack-prometheus.monitoring.svc.cluster.local:9090}"
SKIP_TLS_VERIFY="${SKIP_TLS_VERIFY:-true}"

# EPP saturation analyzer thresholds
# Threshold band: leave unset to use the analyzer's calibrated defaults
# (scaleUpThreshold 0.55 / scaleDownBoundary 0.40); set the env vars only to
# override them.
SCALE_UP_THRESHOLD="${SCALE_UP_THRESHOLD:-}"
SCALE_DOWN_BOUNDARY="${SCALE_DOWN_BOUNDARY:-}"

# HPA (ignored when OBSERVE_ONLY=true)
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
OBSERVE_ONLY="${OBSERVE_ONLY:-false}"  # Disable HPA - WVA emits recommendations only
SKIP_VA="${SKIP_VA:-false}"            # Skip VariantAutoscaling CR creation
SKIP_CRD="${SKIP_CRD:-false}"          # Skip CRD install/upgrade

# ---

log() { echo ">>> $*"; }

# Validate PROMETHEUS_URL is HTTPS
if [[ "${PROMETHEUS_URL}" != https://* ]]; then
    echo "ERROR: PROMETHEUS_URL must use https:// scheme (WVA validates this)." >&2
    echo "       Got: ${PROMETHEUS_URL}" >&2
    echo "       If your Prometheus only serves HTTP, deploy the TLS proxy:" >&2
    echo "         ./deploy/deploy-prometheus-tls-proxy.sh" >&2
    exit 1
fi

# Step 1: Apply/upgrade CRD
if [[ "${SKIP_CRD}" == "true" ]]; then
    log "Skipping CRD install (SKIP_CRD=true)"
elif [[ "${DRY_RUN}" == "true" ]]; then
    echo "  [dry-run] kubectl apply -f charts/workload-variant-autoscaler/crds/llmd.ai_variantautoscalings.yaml"
else
    log "Applying VariantAutoscaling CRD"
    kubectl apply -f "${REPO_ROOT}/charts/workload-variant-autoscaler/crds/llmd.ai_variantautoscalings.yaml"
fi

# Step 2: Build and push image
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

# Step 3: Deploy via Helm
log "Deploying WVA with EPP saturation analyzer"
log "  Namespace: ${WVA_NAMESPACE}"
log "  Image: ${IMG}"
log "  Prometheus: ${PROMETHEUS_URL}"
log "  Scale-up threshold: ${SCALE_UP_THRESHOLD:-(analyzer default 0.55)}"
log "  Scale-down boundary: ${SCALE_DOWN_BOUNDARY:-(analyzer default 0.40)}"
# Only pass thresholds when explicitly overridden, so the analyzer's
# calibrated code defaults (0.55/0.40) apply otherwise.
if [[ -n "${SCALE_UP_THRESHOLD}" ]]; then
    HELM_CMD+=(--set "wva.capacityScaling.default.scaleUpThreshold=${SCALE_UP_THRESHOLD}")
fi
if [[ -n "${SCALE_DOWN_BOUNDARY}" ]]; then
    HELM_CMD+=(--set "wva.capacityScaling.default.scaleDownBoundary=${SCALE_DOWN_BOUNDARY}")
fi

if [[ "${OBSERVE_ONLY}" == "true" ]]; then
    log "  Mode: OBSERVE-ONLY (HPA disabled)"
else
    log "  HPA range: ${HPA_MIN_REPLICAS}-${HPA_MAX_REPLICAS}"
fi

IMAGE_REPO="$(echo "${IMG}" | rev | cut -d: -f2- | rev)"
IMAGE_TAG="$(echo "${IMG}" | rev | cut -d: -f1 | rev)"

HELM_CMD=(
    helm upgrade -i "${HELM_RELEASE}"
    "${REPO_ROOT}/charts/workload-variant-autoscaler"
    -n "${WVA_NAMESPACE}" --create-namespace
    --set "wva.image.repository=${IMAGE_REPO}"
    --set "wva.image.tag=${IMAGE_TAG}"
    --set "wva.prometheus.baseURL=${PROMETHEUS_URL}"
    --set "wva.prometheus.tls.insecureSkipVerify=${SKIP_TLS_VERIFY}"
    --set "wva.namespaceScoped=${NAMESPACE_SCOPED}"
    --set "wva.capacityScaling.default.analyzerName=epp-saturation"
    --set "controller.enabled=true"
    # Disable chart's sample VA/vllmService (we manage VA separately)
    --set "va.enabled=false"
    --set "vllmService.enabled=false"
)

# Only pass thresholds when explicitly overridden, so the analyzer's
# calibrated code defaults (0.55/0.40) apply otherwise.
if [[ -n "${SCALE_UP_THRESHOLD}" ]]; then
    HELM_CMD+=(--set "wva.capacityScaling.default.scaleUpThreshold=${SCALE_UP_THRESHOLD}")
fi
if [[ -n "${SCALE_DOWN_BOUNDARY}" ]]; then
    HELM_CMD+=(--set "wva.capacityScaling.default.scaleDownBoundary=${SCALE_DOWN_BOUNDARY}")
fi

if [[ "${OBSERVE_ONLY}" == "true" ]]; then
    HELM_CMD+=(--set "hpa.enabled=false")
else
    HELM_CMD+=(
        --set "hpa.enabled=true"
        --set "hpa.minReplicas=${HPA_MIN_REPLICAS}"
        --set "hpa.maxReplicas=${HPA_MAX_REPLICAS}"
    )
fi

if [[ "${DRY_RUN}" == "true" ]]; then
    echo "  [dry-run] ${HELM_CMD[*]}"
else
    "${HELM_CMD[@]}"
fi

# Step 4: Create VariantAutoscaling CR (optional)
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

# Step 5: Verification hints
log "Deployment complete. Verify with:"
echo "  # Check WVA pods"
echo "  kubectl get pods -n ${WVA_NAMESPACE}"
echo ""
echo "  # Watch EPP saturation readings and recommendations"
echo "  kubectl logs -n ${WVA_NAMESPACE} -l control-plane=controller-manager -f | \\"
echo "    grep -E 'EPP pool saturation|analysis result|action.*target'"
echo ""
echo "  # Check VA status (.status.desiredOptimizedAlloc.numReplicas shows recommendation)"
echo "  kubectl get variantautoscaling -A -o yaml"
echo ""
echo "  # Query metrics in Prometheus (port-forward first)"
echo "  kubectl port-forward -n <prom-ns> svc/<prom-svc> 19090:<port>"
echo "  # Then:"
echo "  #   wva_desired_replicas       - WVA's recommendation (what HPA consumes)"
echo "  #   wva_current_replicas       - current actual replica count"
echo "  #   wva_epp_saturation_raw / wva_epp_saturation_smoothed  - derived EPP signal"
