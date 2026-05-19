#!/usr/bin/env bash
#
# DEPRECATED: Transitional shim for llm-d infrastructure (repo clone, HF secret, gateway
# control plane, EPP, ModelService via helmfile, RBAC/EPP patches, WVA poolGroup upgrade).
#
# Prefer the llm-d project's own installation tooling (helmfile, llm-d CLI, or upstream
# guides) once your environment can consume it directly. Track removal in a follow-up issue.
#
# Ordering: run deploy/install.sh first (WVA + monitoring + scaler backend), then this script
# when you need llm-d. The poolGroup helm upgrade on WVA requires InferencePool objects created
# by this deploy.
#
# Usage:
#   ./deploy/install-llmd-infra.sh [-e kubernetes|openshift|kind-emulator] [--undeploy]
#
# Prerequisites: kubectl, helm, git, yq, jq (yq edits values.yaml; jq builds JSON merge patches for
# Service/ServiceMonitor label alignment; GPU discovery uses jq on kubernetes/openshift).

set -e
set -o pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

WVA_PROJECT=${WVA_PROJECT:-$PWD}
# Basename under llm-d guides/ (llm-d calls this GUIDE_NAME).
# v0.7.0+: guides/optimized-baseline replaces the former guides/inference-scheduling.
if [ -n "${GUIDE_NAME:-}" ]; then
    WELL_LIT_PATH_NAME="$GUIDE_NAME"
fi
: "${WELL_LIT_PATH_NAME:=optimized-baseline}"
GUIDE_NAME="$WELL_LIT_PATH_NAME"
NAMESPACE_SUFFIX=${NAMESPACE_SUFFIX:-"$GUIDE_NAME"}
LLMD_NS=${LLMD_NS:-"llm-d-$NAMESPACE_SUFFIX"}
WVA_NS=${WVA_NS:-"workload-variant-autoscaler-system"}
MONITORING_NAMESPACE=${MONITORING_NAMESPACE:-"workload-variant-autoscaler-monitoring"}

LLM_D_OWNER=${LLM_D_OWNER:-"llm-d"}
LLM_D_PROJECT=${LLM_D_PROJECT:-"llm-d"}
LLM_D_RELEASE=${LLM_D_RELEASE:-"v0.7.0"}
# v0.7.0+: model server deployed via Kustomize; Deployment name derives from kustomize namePrefix.
LLM_D_MODELSERVICE_NAME=${LLM_D_MODELSERVICE_NAME:-"$GUIDE_NAME-nvidia-gpu-vllm-decode"}
# v0.7.0+: EPP Helm release is $GUIDE_NAME (single release via GAIE standalone chart).
LLM_D_EPP_NAME=${LLM_D_EPP_NAME:-"$GUIDE_NAME-epp"}
# GAIE chart version; must match the CRD revision installed for the cluster (OpenShift override via GATEWAY_API_INFERENCE_EXTENSION_CRD_REVISION).
GAIE_VERSION=${GAIE_VERSION:-"v1.5.0"}
# Kustomize overlay under guides/$GUIDE_NAME/modelserver/ (base or gke).
INFRA_PROVIDER=${INFRA_PROVIDER:-"base"}
GATEWAY_PREREQ_DIR=${GATEWAY_PREREQ_DIR:-"$WVA_PROJECT/$LLM_D_PROJECT/guides/prereq/gateway-provider"}
EXAMPLE_DIR=${EXAMPLE_DIR:-"$WVA_PROJECT/$LLM_D_PROJECT/guides/$GUIDE_NAME"}
ITL_AVERAGE_LATENCY_MS=${ITL_AVERAGE_LATENCY_MS:-20}
TTFT_AVERAGE_LATENCY_MS=${TTFT_AVERAGE_LATENCY_MS:-200}
ENABLE_SCALE_TO_ZERO=${ENABLE_SCALE_TO_ZERO:-true}

GATEWAY_PROVIDER=${GATEWAY_PROVIDER:-"istio"}
# Install Gateway control plane (Istio/kgateway) via helmfile. Only needed for Gateway Mode deployments
# (inferencepool chart with an external Kubernetes Gateway controller). Standalone Mode (default) bundles
# its own Envoy proxy and does not require an external gateway controller.
INSTALL_GATEWAY_CTRLPLANE="${INSTALL_GATEWAY_CTRLPLANE:-false}"

# Model identity and vLLM / ModelService chart tuning (wired into llm-d values or overrides).
DEFAULT_MODEL_ID=${DEFAULT_MODEL_ID:-"Qwen/Qwen3-0.6B"}
MODEL_ID=${MODEL_ID:-"unsloth/Meta-Llama-3.1-8B"}
ACCELERATOR_TYPE=${ACCELERATOR_TYPE:-"H100"}

VLLM_SVC_ENABLED=${VLLM_SVC_ENABLED:-true}
VLLM_SVC_PORT=${VLLM_SVC_PORT:-8200}
VLLM_MAX_NUM_SEQS=${VLLM_MAX_NUM_SEQS:-""}
DECODE_REPLICAS=${DECODE_REPLICAS:-""}
LLMD_IMAGE_TAG=${LLMD_IMAGE_TAG:-""}
VLLM_GPU_MEM_UTIL=${VLLM_GPU_MEM_UTIL:-""}
VLLM_MAX_MODEL_LEN=${VLLM_MAX_MODEL_LEN:-""}
VLLM_BLOCK_SIZE=${VLLM_BLOCK_SIZE:-""}
VLLM_ENFORCE_EAGER=${VLLM_ENFORCE_EAGER:-""}

DEPLOY_WVA=${DEPLOY_WVA:-true}
WVA_RELEASE_NAME=${WVA_RELEASE_NAME:-"workload-variant-autoscaler"}
NAMESPACE_SCOPED=${NAMESPACE_SCOPED:-true}
SKIP_CHECKS=${SKIP_CHECKS:-false}
# When deploying on emulated clusters, delete chart decode Deployment after helmfile so tests can apply their own (default true).
LLMD_REMOVE_EMULATED_DECODE_DEPLOYMENTS=${LLMD_REMOVE_EMULATED_DECODE_DEPLOYMENTS:-true}

# CI / e2e-oriented flags (Makefile sets most of these; defaults are conservative for ad-hoc demos).
LLMD_SKIP_DEFAULT_MODELSERVICE=${LLMD_SKIP_DEFAULT_MODELSERVICE:-false}
LLMD_WAIT_FOR_ESSENTIAL_LLM_D_ONLY=${LLMD_WAIT_FOR_ESSENTIAL_LLM_D_ONLY:-false}
LLMD_PATCH_EPP_FLOW_CONTROL=${LLMD_PATCH_EPP_FLOW_CONTROL:-false}
LLMD_SKIP_INFERENCE_OBJECTIVE=${LLMD_SKIP_INFERENCE_OBJECTIVE:-false}

DEPLOY_LLM_D_INFERENCE_SIM=${DEPLOY_LLM_D_INFERENCE_SIM:-false}
LLM_D_INFERENCE_SIM_IMG_REPO=${LLM_D_INFERENCE_SIM_IMG_REPO:-"ghcr.io/llm-d/llm-d-inference-sim"}
LLM_D_INFERENCE_SIM_IMG_TAG=${LLM_D_INFERENCE_SIM_IMG_TAG:-"latest"}

ENVIRONMENT=${ENVIRONMENT:-"kubernetes"}
COMPATIBLE_ENV_LIST=("kubernetes" "openshift" "kind-emulator")
NON_EMULATED_ENV_LIST=("kubernetes" "openshift")
REQUIRED_TOOLS=("kubectl" "helm" "git" "yq" "jq")

UNDEPLOY=false

# Minimal bootstrap so parse_llmd_args can call log_error / containsElement
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEPLOY_LIB_DIR="$SCRIPT_DIR/lib"
# shellcheck source=lib/common.sh
source "$DEPLOY_LIB_DIR/common.sh"
# shellcheck source=lib/constants.sh
source "$DEPLOY_LIB_DIR/constants.sh"

print_llmd_help() {
    cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Deploy llm-d inference stack (gateway, EPP, ModelService) after WVA base infra.

Run deploy/install.sh first unless you only need to tear down llm-d (--undeploy).

Options:
  -e, --environment NAME   kubernetes | openshift | kind-emulator (default: $ENVIRONMENT)
  --undeploy               Remove llm-d helm releases and optional gateway control plane
  -h, --help               Show this help

Environment: see deploy/README.md (llm-d / gateway variables). Key flags:
  LLMD_SKIP_DEFAULT_MODELSERVICE, LLMD_WAIT_FOR_ESSENTIAL_LLM_D_ONLY,
  LLMD_PATCH_EPP_FLOW_CONTROL, LLMD_SKIP_INFERENCE_OBJECTIVE, INSTALL_GATEWAY_CTRLPLANE,
  LLMD_REMOVE_EMULATED_DECODE_DEPLOYMENTS, MODEL_ID, ACCELERATOR_TYPE, HF_TOKEN,
  GUIDE_NAME (or legacy WELL_LIT_PATH_NAME), RELEASE_NAME_POSTFIX
EOF
}

parse_llmd_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            -e|--environment)
                ENVIRONMENT="$2"
                shift 2
                ;;
            --undeploy)
                UNDEPLOY=true
                shift
                ;;
            -h|--help)
                print_llmd_help
                exit 0
                ;;
            *)
                echo "Unknown option: $1" >&2
                print_llmd_help
                exit 1
                ;;
        esac
    done
    if ! containsElement "$ENVIRONMENT" "${COMPATIBLE_ENV_LIST[@]}"; then
        log_error "Invalid environment: $ENVIRONMENT. Valid: ${COMPATIBLE_ENV_LIST[*]}"
    fi
}

# Entry flow: parse args → prerequisites → env plugin (kind/openshift/…) → GPU detect (non-emulated)
# → deploy_llm_d_infrastructure (clone llm-d, helmfile, WVA poolGroup upgrade, emulated cleanup).
main_llmd() {
    parse_llmd_args "$@"

    # shellcheck source=lib/verify.sh
    source "$DEPLOY_LIB_DIR/verify.sh"
    # shellcheck source=lib/wait_helpers.sh
    source "$DEPLOY_LIB_DIR/wait_helpers.sh"
    # shellcheck source=lib/prereqs.sh
    source "$DEPLOY_LIB_DIR/prereqs.sh"
    # shellcheck source=lib/discovery.sh
    source "$DEPLOY_LIB_DIR/discovery.sh"
    # shellcheck source=lib/cleanup.sh
    source "$DEPLOY_LIB_DIR/cleanup.sh"
    # shellcheck source=lib/infra_llmd.sh
    source "$DEPLOY_LIB_DIR/infra_llmd.sh"

    if [ "$UNDEPLOY" = "true" ]; then
        log_info "Undeploying llm-d infrastructure on $ENVIRONMENT"
        undeploy_llm_d_infrastructure
        log_success "llm-d undeploy finished (run deploy/install.sh --undeploy separately for WVA/monitoring if needed)"
        exit 0
    fi

    if [ "$SKIP_CHECKS" != "true" ]; then
        check_prerequisites
    fi

    if [[ "${CLUSTER_TYPE:-}" == "kind" ]]; then
        ENVIRONMENT="kind-emulator"
    fi

    if [ -f "$SCRIPT_DIR/$ENVIRONMENT/install.sh" ]; then
        # shellcheck source=/dev/null
        source "$SCRIPT_DIR/$ENVIRONMENT/install.sh"
    else
        log_error "Environment script not found: $SCRIPT_DIR/$ENVIRONMENT/install.sh"
    fi
    # Env plugins (e.g. kind-emulator) may override WELL_LIT_PATH_NAME / EXAMPLE_DIR / llm-d names.
    GUIDE_NAME="$WELL_LIT_PATH_NAME"

    if declare -f check_specific_prerequisites > /dev/null; then
        if [ "$SKIP_CHECKS" != "true" ]; then
            check_specific_prerequisites
        fi
    fi

    log_info "INSTALL_GATEWAY_CTRLPLANE=$INSTALL_GATEWAY_CTRLPLANE (set false to skip gateway control plane install)"

    kubectl create namespace "$LLMD_NS" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

    if containsElement "$ENVIRONMENT" "${NON_EMULATED_ENV_LIST[@]}"; then
        detect_gpu_type
    else
        log_info "Skipping GPU type detection for emulated environment (ENVIRONMENT=$ENVIRONMENT)"
    fi

    deploy_llm_d_infrastructure

    log_success "llm-d infrastructure deployment on $ENVIRONMENT complete!"
}

main_llmd "$@"
