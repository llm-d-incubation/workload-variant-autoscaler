#!/usr/bin/env bash
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
LLM_D_RELEASE=${LLM_D_RELEASE:-"v0.6.0"}
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
LLM_D_INFERENCE_SCHEDULER_IMG=${LLM_D_INFERENCE_SCHEDULER_IMG:-"ghcr.io/llm-d/llm-d-inference-scheduler:v0.7.0"}

# Gateway Configuration
GATEWAY_PROVIDER=${GATEWAY_PROVIDER:-"istio"} # Options: kgateway, istio
# Save original value to detect if explicitly set via environment variable
INSTALL_GATEWAY_CTRLPLANE_ORIGINAL="${INSTALL_GATEWAY_CTRLPLANE:-}"
INSTALL_GATEWAY_CTRLPLANE="${INSTALL_GATEWAY_CTRLPLANE:-false}"

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
# Infra-first: chart-managed VariantAutoscaling / HPA are opt-in (e2e and operators
# typically create their own CRs). Set DEPLOY_VA=true and DEPLOY_HPA=true for a demo stack.
DEPLOY_VA=${DEPLOY_VA:-false}
DEPLOY_HPA=${DEPLOY_HPA:-false}
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

# Scaler backend: "prometheus-adapter" (default), "keda", or "none"
# prometheus-adapter: deploy Prometheus Adapter + patch external metrics APIService
# keda:              on kubernetes assume cluster-managed KEDA (no Helm; set KEDA_HELM_INSTALL=true to install);
#                    on kind-emulator install via Helm when needed; OpenShift is always platform-managed (no Helm)
# none:              skip all scaler backend deployment; use when KEDA or another metrics API
#                    is already installed on the cluster (e.g. llmd benchmark clusters)
SCALER_BACKEND=${SCALER_BACKEND:-prometheus-adapter}
KEDA_NAMESPACE=${KEDA_NAMESPACE:-keda-system}
# Pin KEDA chart version for reproducible installs (only used when deploy_keda installs from helm)
KEDA_CHART_VERSION=${KEDA_CHART_VERSION:-2.19.0}
# kubernetes: default false (cluster-managed KEDA); set true to let this script install/upgrade KEDA via Helm
KEDA_HELM_INSTALL=${KEDA_HELM_INSTALL:-false}

# Environment-related variables
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVIRONMENT=${ENVIRONMENT:-"kubernetes"}
COMPATIBLE_ENV_LIST=("kubernetes" "openshift" "kind-emulator")
NON_EMULATED_ENV_LIST=("kubernetes" "openshift")
REQUIRED_TOOLS=("kubectl" "helm" "git")
DEPLOY_LIB_DIR="$SCRIPT_DIR/lib"

# TODO: add kubernetes to these defaults to enable TLS verification when deploying to production clusters
PRODUCTION_ENV_LIST=("openshift")

# Shared deploy helpers
# shellcheck source=lib/verify.sh
source "$DEPLOY_LIB_DIR/verify.sh"
# shellcheck source=lib/common.sh
source "$DEPLOY_LIB_DIR/common.sh"
# shellcheck source=lib/constants.sh
source "$DEPLOY_LIB_DIR/constants.sh"
# shellcheck source=lib/wait_helpers.sh
source "$DEPLOY_LIB_DIR/wait_helpers.sh"
# shellcheck source=lib/cli.sh
source "$DEPLOY_LIB_DIR/cli.sh"
# shellcheck source=lib/prereqs.sh
source "$DEPLOY_LIB_DIR/prereqs.sh"
# shellcheck source=lib/discovery.sh
source "$DEPLOY_LIB_DIR/discovery.sh"
# shellcheck source=lib/infra_scaler_backend.sh
source "$DEPLOY_LIB_DIR/infra_scaler_backend.sh"
# shellcheck source=lib/scaler_runtime.sh
source "$DEPLOY_LIB_DIR/scaler_runtime.sh"
# shellcheck source=lib/infra_llmd.sh
source "$DEPLOY_LIB_DIR/infra_llmd.sh"
# shellcheck source=lib/infra_wva.sh
source "$DEPLOY_LIB_DIR/infra_wva.sh"
# shellcheck source=lib/infra_monitoring.sh
source "$DEPLOY_LIB_DIR/infra_monitoring.sh"
# shellcheck source=lib/cleanup.sh
source "$DEPLOY_LIB_DIR/cleanup.sh"
# shellcheck source=lib/install_core.sh
source "$DEPLOY_LIB_DIR/install_core.sh"

# Undeployment flags
UNDEPLOY=${UNDEPLOY:-false}
DELETE_NAMESPACES=${DELETE_NAMESPACES:-false}

# Main deployment flow
# Core orchestration moved to deploy/lib/install_core.sh

# Run main function
main "$@"
