#!/usr/bin/env bash
#
# Workload-Variant-Autoscaler infrastructure bootstrap: optional WVA controller,
# Prometheus monitoring stack, and scaler backend (KEDA or Prometheus Adapter).
#
# For llm-d (gateway, EPP, ModelService, HF secret, WVA poolGroup alignment), run
# deploy/install-llmd-infra.sh after this script when you need that stack.
#
# Prerequisites:
# - kubectl and helm installed
# - Cluster credentials configured
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

# Namespaces
LLMD_NS=${LLMD_NS:-"llm-d-inference-scheduler"}
MONITORING_NAMESPACE=${MONITORING_NAMESPACE:-"workload-variant-autoscaler-monitoring"}
WVA_NS=${WVA_NS:-"workload-variant-autoscaler-system"}
PROMETHEUS_SECRET_NS=${PROMETHEUS_SECRET_NS:-$MONITORING_NAMESPACE}

# WVA Configuration (required when DEPLOY_WVA=true)
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
# Optional: multi-controller isolation (sets controller_instance on metrics / selectors when non-empty).
CONTROLLER_INSTANCE=${CONTROLLER_INSTANCE:-""}
WVA_BASE_NAME=${WVA_BASE_NAME:-"inference-scheduling"}
NAMESPACE_SCOPED=${NAMESPACE_SCOPED:-true}

ENABLE_SCALE_TO_ZERO=${ENABLE_SCALE_TO_ZERO:-true}

# Prometheus Configuration
PROM_CA_CERT_PATH=${PROM_CA_CERT_PATH:-"/tmp/prometheus-ca.crt"}
PROMETHEUS_SECRET_NAME=${PROMETHEUS_SECRET_NAME:-"prometheus-web-tls"}

# Flags for deployment steps
DEPLOY_PROMETHEUS=${DEPLOY_PROMETHEUS:-true}
DEPLOY_WVA=${DEPLOY_WVA:-true}
DEPLOY_PROMETHEUS_ADAPTER=${DEPLOY_PROMETHEUS_ADAPTER:-true}
SKIP_CHECKS=${SKIP_CHECKS:-false}
WVA_METRICS_SECURE=${WVA_METRICS_SECURE:-true}

# Scaler backend: prometheus-adapter | keda | none.
# - keda on kubernetes: expects cluster CRD unless KEDA_HELM_INSTALL=true (then this script installs Helm KEDA).
# - keda on openshift: platform-managed KEDA only (no Helm install from this script).
# - none: skip scaler install (cluster already provides external metrics).
SCALER_BACKEND=${SCALER_BACKEND:-prometheus-adapter}
KEDA_NAMESPACE=${KEDA_NAMESPACE:-keda-system}
# Pinned for reproducible Helm installs (used when deploy_keda actually runs helm upgrade).
KEDA_CHART_VERSION=${KEDA_CHART_VERSION:-2.19.0}
# On kubernetes: default false (cluster-managed KEDA); kind-emulator flows often set true or use cluster path.
KEDA_HELM_INSTALL=${KEDA_HELM_INSTALL:-false}

# LeaderWorkerSet (WVA dependency). Set false when LWS is pre-installed or not needed (e.g. some benchmarks).
DEPLOY_LWS=${DEPLOY_LWS:-true}
LWS_NAMESPACE=${LWS_NAMESPACE:-"lws-system"}
LWS_CHART_VERSION=${LWS_CHART_VERSION:-"0.8.0"}

# Environment-related variables
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENVIRONMENT=${ENVIRONMENT:-"kubernetes"}
COMPATIBLE_ENV_LIST=("kubernetes" "openshift" "kind-emulator")
NON_EMULATED_ENV_LIST=("kubernetes" "openshift")
REQUIRED_TOOLS=("kubectl" "helm" "git")
DEPLOY_LIB_DIR="$SCRIPT_DIR/lib"

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
# shellcheck source=lib/infra_scaler_backend.sh
source "$DEPLOY_LIB_DIR/infra_scaler_backend.sh"
# shellcheck source=lib/scaler_runtime.sh
source "$DEPLOY_LIB_DIR/scaler_runtime.sh"
# shellcheck source=lib/infra_wva.sh
source "$DEPLOY_LIB_DIR/infra_wva.sh"
# shellcheck source=lib/infra_monitoring.sh
source "$DEPLOY_LIB_DIR/infra_monitoring.sh"
# shellcheck source=lib/cleanup.sh
source "$DEPLOY_LIB_DIR/cleanup.sh"
# shellcheck source=lib/install_core.sh
source "$DEPLOY_LIB_DIR/install_core.sh"

UNDEPLOY=${UNDEPLOY:-false}
DELETE_NAMESPACES=${DELETE_NAMESPACES:-false}

# Orchestration lives in deploy/lib/install_core.sh (keeps this entrypoint to variable defaults + sourcing only).
main "$@"
