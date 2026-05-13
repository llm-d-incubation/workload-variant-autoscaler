#!/usr/bin/env bash
#
# Multi-Model Deployment Orchestrator
#
# Orchestrates deploy/install.sh (WVA + monitoring + scaler) and deploy/install-llmd-infra.sh
# per model into the same cluster, then creates a shared HTTPRoute with URLRewrite rules.
#
# Usage:
#   MODELS="Qwen/Qwen3-0.6B,unsloth/Meta-Llama-3.1-8B" ./deploy/install-multi-model.sh
#
# Architecture:
#   install.sh + install-llmd-infra.sh (call 1) → WVA, monitoring, scaler, Model 1 + gateway
#   install-llmd-infra.sh (call N) → additional Model N (no WVA / no monitoring)
#   This script → multi-model HTTPRoute and verification
#

set -e
set -o pipefail

# ── Logging ──────────────────────────────────────────────────────────
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info()    { echo -e "${BLUE}[INFO]${NC} $1"; }
log_success() { echo -e "${GREEN}[SUCCESS]${NC} $1"; }
log_warning() { echo -e "${YELLOW}[WARNING]${NC} $1"; }
log_error()   { echo -e "${RED}[ERROR]${NC} $1"; exit 1; }

# ── Utilities ────────────────────────────────────────────────────────
# Convert a HuggingFace model ID into a k8s-safe resource slug.
#   Qwen/Qwen3-0.6B        → qwen-qwen3-0-6b
#   unsloth/Meta-Llama-3.1-8B → unsloth-meta-llama-3-1-8b
model_to_slug() {
    echo "$1" | tr '[:upper:]' '[:lower:]' | sed 's|/|-|g; s|\.|-|g'
}

# ── Configuration ────────────────────────────────────────────────────
WVA_PROJECT=${WVA_PROJECT:-$PWD}
DEPLOY_SCRIPT="$WVA_PROJECT/deploy/install.sh"
LLMD_DEPLOY_SCRIPT="$WVA_PROJECT/deploy/install-llmd-infra.sh"
LLMD_NS=${LLMD_NS:-"llm-d-inference-scheduler"}
ENVIRONMENT=${ENVIRONMENT:-"kind-emulator"}
DECODE_REPLICAS=${DECODE_REPLICAS:-1}
LLM_D_RELEASE=${LLM_D_RELEASE:-v0.6.0}
MODELS=${MODELS:-"Qwen/Qwen3-0.6B,unsloth/Meta-Llama-3.1-8B"}

chmod +x "$DEPLOY_SCRIPT" "$LLMD_DEPLOY_SCRIPT"

# ── Parse model list ─────────────────────────────────────────────────
IFS=',' read -ra MODEL_LIST <<< "$MODELS"
if [ ${#MODEL_LIST[@]} -lt 1 ]; then
    log_error "MODELS must contain at least 1 comma-separated model ID (got: $MODELS)"
fi

SLUG_LIST=()
for model in "${MODEL_LIST[@]}"; do
    SLUG_LIST+=("$(model_to_slug "$model")")
done

# ── Undeploy mode ────────────────────────────────────────────────────
UNDEPLOY=false
for arg in "$@"; do
    case "$arg" in
        -u|--undeploy) UNDEPLOY=true ;;
    esac
done

if [ "$UNDEPLOY" = true ]; then
    log_info "Starting Multi-Model Undeployment"
    log_info "Models: ${MODEL_LIST[*]}"
    log_info "═══════════════════════════════════════════════════════════"
    echo ""

    # 1. Delete multi-model HTTPRoute and shared Gateway
    log_info "Deleting multi-model HTTPRoute..."
    kubectl delete httproute multi-model-route -n "$LLMD_NS" --ignore-not-found
    log_success "HTTPRoute deleted"

    log_info "Deleting shared Gateway..."
    kubectl delete gateway multi-model-inference-gateway -n "$LLMD_NS" --ignore-not-found
    log_success "Shared Gateway deleted"

    # 2. Delete InferencePools
    log_info "Deleting InferencePool resources..."
    for slug in "${SLUG_LIST[@]}"; do
        kubectl delete inferencepool "gaie-${slug}" -n "$LLMD_NS" --ignore-not-found
        log_success "InferencePool gaie-${slug} deleted"
    done

    # 3. Uninstall model-specific Helm releases directly.
    #    install.sh --undeploy uses NAMESPACE_SUFFIX (not RELEASE_NAME_POSTFIX)
    #    for its helm uninstall commands, so it won't find our slug-based releases.
    #    We handle model cleanup here; install.sh handles WVA/monitoring/scaler.
    log_info "Removing model-specific Helm releases..."
    for slug in "${SLUG_LIST[@]}"; do
        log_info "  Uninstalling releases for ${slug}..."
        helm uninstall "ms-${slug}" -n "$LLMD_NS" 2>/dev/null || \
            log_warning "  ms-${slug} not found"
        helm uninstall "gaie-${slug}" -n "$LLMD_NS" 2>/dev/null || \
            log_warning "  gaie-${slug} not found"
        helm uninstall "infra-${slug}" -n "$LLMD_NS" 2>/dev/null || \
            log_warning "  infra-${slug} not found"
    done

    # 4. Call install.sh --undeploy for the full stack (WVA, monitoring, scaler backend, namespaces).
    #    Only need to call once for the first model since it deployed the control plane.
    log_info "Undeploying llm-d (gateway) then WVA/monitoring/scaler..."
    ENVIRONMENT="$ENVIRONMENT" \
    INSTALL_GATEWAY_CTRLPLANE=true \
    DELETE_NAMESPACES="${DELETE_NAMESPACES:-false}" \
    "$LLMD_DEPLOY_SCRIPT" --undeploy -e "$ENVIRONMENT" || true
    ENVIRONMENT="$ENVIRONMENT" \
    DELETE_NAMESPACES="${DELETE_NAMESPACES:-false}" \
    "$DEPLOY_SCRIPT" --undeploy

    log_success "Multi-model Undeployment Completed!"
    exit 0
fi

# ── Deploy mode ──────────────────────────────────────────────────────
TOTAL_STEPS=$(( ${#MODEL_LIST[@]} + 2 ))  # N models + InferencePools + HTTPRoute
log_info "Models to deploy (${#MODEL_LIST[@]}): ${MODEL_LIST[*]}"
log_info "Resource slugs: ${SLUG_LIST[*]}"
echo ""

# ── Deploy models via install.sh ─────────────────────────────────────
for i in "${!MODEL_LIST[@]}"; do
    model="${MODEL_LIST[$i]}"
    slug="${SLUG_LIST[$i]}"
    step=$((i + 1))

    log_info "═══════════════════════════════════════════════════════════"
    if [ "$i" -eq 0 ]; then
        log_info "Step ${step}/${TOTAL_STEPS}: Full stack + ${slug}"
        log_info "═══════════════════════════════════════════════════════════"

        # First model: WVA + monitoring + scaler, then llm-d for this model
        ENVIRONMENT="$ENVIRONMENT" \
        DEPLOY_WVA=true \
        DEPLOY_PROMETHEUS=true \
        "$DEPLOY_SCRIPT"
        ENVIRONMENT="$ENVIRONMENT" \
        MODEL_ID="$model" \
        RELEASE_NAME_POSTFIX="$slug" \
        INSTALL_GATEWAY_CTRLPLANE=true \
        LLM_D_RELEASE="$LLM_D_RELEASE" \
        DECODE_REPLICAS="$DECODE_REPLICAS" \
        "$LLMD_DEPLOY_SCRIPT" -e "$ENVIRONMENT"

        # Replace the model-specific Gateway with a shared, model-agnostic one.
        # install.sh created infra-<slug>-inference-gateway; we replace it with
        # a standalone Gateway named 'multi-model-inference-gateway'.
        log_info "Creating shared Gateway (replacing model-specific infra-${slug})..."
        cat <<GWEOF | kubectl apply -f -
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: multi-model-inference-gateway
  namespace: $LLMD_NS
  labels:
    istio.io/enable-inference-extproc: "true"
spec:
  gatewayClassName: istio
  listeners:
    - port: 80
      protocol: HTTP
      name: default
      allowedRoutes:
        namespaces:
          from: All
GWEOF
        log_success "Shared Gateway 'multi-model-inference-gateway' created"

        # Remove Model 1's model-specific Gateway resource. We keep the infra
        # Helm release (it provides CRDs and other infra); only the Gateway
        # resource is redundant since we created a shared one above.
        log_info "Removing model-specific Gateway resource infra-${slug}-inference-gateway..."
        kubectl delete gateway "infra-${slug}-inference-gateway" \
            -n "$LLMD_NS" --ignore-not-found 2>/dev/null || true
    else
        log_info "Step ${step}/${TOTAL_STEPS}: Model-only ${slug}"
        log_info "═══════════════════════════════════════════════════════════"

        # Subsequent models: reuse existing control plane; install-llmd-infra only.
        # Inherits WVA_NS, NAMESPACE_SCOPED, LLM_D_RELEASE from the environment (Makefile or caller).

        # The gaie-* (inference-scheduler) chart creates a shared Secret with a
        # fixed name that causes Helm ownership conflicts across releases.
        # Delete it before deploying — the helmfile will recreate it.
        log_info "Removing shared EPP Secret to avoid Helm ownership conflict..."
        kubectl delete secret inference-scheduling-gateway-sa-metrics-reader-secret \
            -n "$LLMD_NS" --ignore-not-found 2>/dev/null || true

        ENVIRONMENT="$ENVIRONMENT" \
        MODEL_ID="$model" \
        RELEASE_NAME_POSTFIX="$slug" \
        INSTALL_GATEWAY_CTRLPLANE=false \
        DEPLOY_WVA=false \
        LLM_D_RELEASE="$LLM_D_RELEASE" \
        DECODE_REPLICAS="$DECODE_REPLICAS" \
        "$LLMD_DEPLOY_SCRIPT" -e "$ENVIRONMENT"

        # The helmfile creates an infra-<slug> release per model which includes
        # a redundant Gateway resource. Delete only the Gateway resource (not the
        # Helm release) to preserve CRDs that InferencePools depend on.
        log_info "Removing redundant Gateway resource for ${slug}..."
        kubectl delete gateway "infra-${slug}-inference-gateway" \
            -n "$LLMD_NS" --ignore-not-found 2>/dev/null || true
    fi
done

# ── Verify InferencePools ─────────────────────────────────────────────
# The gaie-* helmfile releases already create InferencePool resources
# for each model with the correct API version and labels. We just verify
# they exist rather than trying to re-create them.
POOL_STEP=$(( ${#MODEL_LIST[@]} + 1 ))
log_info "═══════════════════════════════════════════════════════════"
log_info "Step ${POOL_STEP}/${TOTAL_STEPS}: Verifying InferencePool resources"
log_info "═══════════════════════════════════════════════════════════"

for slug in "${SLUG_LIST[@]}"; do
    if kubectl get inferencepool "gaie-${slug}" -n "$LLMD_NS" &>/dev/null; then
        log_success "InferencePool gaie-${slug} exists"
    else
        log_warning "InferencePool gaie-${slug} not found — it should have been created by the gaie-${slug} Helm release"
    fi
done

# ── Deploy HTTPRoute with URLRewrite ─────────────────────────────────
ROUTE_STEP=$(( ${#MODEL_LIST[@]} + 2 ))
# Shared Gateway created earlier, not tied to any specific model
GATEWAY_NAME="multi-model-inference-gateway"

log_info "═══════════════════════════════════════════════════════════"
log_info "Step ${ROUTE_STEP}/${TOTAL_STEPS}: Deploying HTTPRoute (gateway=${GATEWAY_NAME})"
log_info "═══════════════════════════════════════════════════════════"

# Build the rules array dynamically — one rule per model with URLRewrite
# Detect InferencePool API group (moved from x-k8s.io to k8s.io in v1.4.0)
# Both CRDs may exist on the cluster; prefer the GA version.
if kubectl get crd inferencepools.inference.networking.k8s.io &>/dev/null; then
    POOL_API_GROUP="inference.networking.k8s.io"
else
    POOL_API_GROUP="inference.networking.x-k8s.io"
fi
log_info "Detected InferencePool API group: ${POOL_API_GROUP}"

RULES_YAML=""
for slug in "${SLUG_LIST[@]}"; do
    RULES_YAML+="
    - matches:
      - path:
          type: PathPrefix
          value: /${slug}/v1
      filters:
      - type: URLRewrite
        urlRewrite:
          path:
            type: ReplacePrefixMatch
            replacePrefixMatch: /v1
      backendRefs:
      - group: ${POOL_API_GROUP}
        kind: InferencePool
        name: gaie-${slug}
        port: 8000
      timeouts:
        request: 300s"
done

cat <<EOF | kubectl apply -f -
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: multi-model-route
  namespace: $LLMD_NS
spec:
  parentRefs:
  - group: gateway.networking.k8s.io
    kind: Gateway
    name: $GATEWAY_NAME
  rules:${RULES_YAML}
EOF

log_success "HTTPRoute deployed with ${#SLUG_LIST[@]} URLRewrite rules"

# ── Verification (in-cluster Job) ────────────────────────────────────
log_info "Running in-cluster connectivity verification..."

GW_SVC="${GATEWAY_NAME}-istio.${LLMD_NS}.svc.cluster.local"
VERIFY_TIMEOUT=180
VERIFY_INTERVAL=10
JOB_NAME="multi-model-verify"

# Build curl commands for all models
CURL_CMDS=""
for slug in "${SLUG_LIST[@]}"; do
    CURL_CMDS+="CODE=\$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://${GW_SVC}/${slug}/v1/models 2>/dev/null || echo 000); "
    CURL_CMDS+="if [ \"\$CODE\" != \"200\" ]; then ALL_OK=false; fi; "
done

RESULT_CMDS=""
for slug in "${SLUG_LIST[@]}"; do
    RESULT_CMDS+="CODE=\$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://${GW_SVC}/${slug}/v1/models 2>/dev/null || echo 000); "
    RESULT_CMDS+="echo \"RESULT ${slug} \$CODE\"; "
done

# Delete any previous verification Job
kubectl delete job "$JOB_NAME" -n "$LLMD_NS" --ignore-not-found 2>/dev/null || true

cat <<JOBEOF | kubectl apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: ${JOB_NAME}
  namespace: ${LLMD_NS}
spec:
  backoffLimit: 0
  ttlSecondsAfterFinished: 120
  template:
    spec:
      restartPolicy: Never
      containers:
      - name: verify
        image: curlimages/curl:latest
        command: ["/bin/sh", "-c"]
        args:
        - |
          ELAPSED=0
          while [ \$ELAPSED -lt ${VERIFY_TIMEOUT} ]; do
            ALL_OK=true
            ${CURL_CMDS}
            if [ "\$ALL_OK" = true ]; then
              echo "ALL_REACHABLE"
              ${RESULT_CMDS}
              exit 0
            fi
            echo "WAITING \${ELAPSED}/${VERIFY_TIMEOUT}s"
            sleep ${VERIFY_INTERVAL}
            ELAPSED=\$((ELAPSED + ${VERIFY_INTERVAL}))
          done
          echo "TIMED_OUT"
          ${RESULT_CMDS}
          exit 1
JOBEOF

log_info "Verification Job '${JOB_NAME}' launched (timeout=${VERIFY_TIMEOUT}s)"
log_info "Testing: http://${GW_SVC}/{model-slug}/v1/models"

# Wait for Job completion
WAIT_TIMEOUT=$((VERIFY_TIMEOUT + 60))
if kubectl wait --for=condition=complete job/"$JOB_NAME" -n "$LLMD_NS" --timeout="${WAIT_TIMEOUT}s" 2>/dev/null; then
    echo ""
    JOB_LOGS=$(kubectl logs job/"$JOB_NAME" -n "$LLMD_NS" 2>/dev/null || echo "")
    for slug in "${SLUG_LIST[@]}"; do
        log_success "✓ ${slug} reachable via Gateway"
    done
    echo ""
    log_success "All ${#MODEL_LIST[@]} models reachable through the Gateway!"
else
    echo ""
    JOB_LOGS=$(kubectl logs job/"$JOB_NAME" -n "$LLMD_NS" 2>/dev/null || echo "")
    for slug in "${SLUG_LIST[@]}"; do
        CODE=$(echo "$JOB_LOGS" | grep "RESULT ${slug}" | awk '{print $3}')
        if [ "$CODE" = "200" ]; then
            log_success "✓ ${slug} reachable (HTTP 200)"
        else
            log_warning "✗ ${slug} not reachable (HTTP ${CODE:-unknown})"
        fi
    done
    echo ""
    log_warning "Some models not reachable after ${VERIFY_TIMEOUT}s — they may still be loading."
    log_info "Check manually: kubectl port-forward -n $LLMD_NS deployment/${GATEWAY_NAME}-istio 8080:80"
fi

# Cleanup
kubectl delete job "$JOB_NAME" -n "$LLMD_NS" --ignore-not-found 2>/dev/null || true

log_success "Multi-model Infrastructure Deployment Completed!"

