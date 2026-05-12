#!/usr/bin/env bash
#
# CLI help and argument parsing for deploy/install.sh.
# Requires vars: WVA_IMAGE_REPO, WVA_IMAGE_TAG, WVA_RELEASE_NAME, COMPATIBLE_ENV_LIST.
# Requires funcs: log_info/log_warning/log_error, containsElement().
#

print_help() {
  cat <<EOF
Usage: $(basename "$0") [OPTIONS]

Bootstrap WVA (optional), monitoring, and scaler backend on a Kubernetes or OpenShift cluster.
For llm-d (gateway, EPP, ModelService), run deploy/install-llmd-infra.sh after this script.

Options:
  -i, --wva-image IMAGE        Container image for WVA (default: $WVA_IMAGE_REPO:$WVA_IMAGE_TAG)
  -r, --release-name NAME      Helm release name for WVA (default: $WVA_RELEASE_NAME)
  -u, --undeploy               Undeploy WVA, monitoring, and scaler backend (not llm-d; use install-llmd-infra.sh --undeploy)
  -e, --environment            kubernetes | openshift | kind-emulator (default: kubernetes)
  -h, --help                   Show this help and exit

Deprecated (ignored by install.sh for chart deploy; passed through for CI/scripts calling install-llmd-infra next):
  --model MODEL_ID             Same as MODEL_ID env (llm-d / chart overrides use install-llmd-infra.sh)
  --accelerator TYPE           Same as ACCELERATOR_TYPE env

Environment Variables:
  IMG                          WVA image as repo:tag (alternative to -i)
  WVA_RELEASE_NAME             Helm release name (alternative to -r)
  SKIP_CHECKS                  Skip kubectl/helm/git prerequisite check (default: false). Install scripts are non-interactive and fail fast on errors.
  DEPLOY_PROMETHEUS            Deploy Prometheus stack (default: true)
  DEPLOY_WVA                   Deploy WVA controller (default: true)
  DEPLOY_PROMETHEUS_ADAPTER    Deploy Prometheus Adapter when SCALER_BACKEND=prometheus-adapter (default: true)
  SCALER_BACKEND               prometheus-adapter (default), keda, or none
  KEDA_HELM_INSTALL            Install KEDA via Helm on kubernetes when true (default: false)
  KEDA_NAMESPACE               Namespace for KEDA (default: keda-system)
  UNDEPLOY                     Undeploy mode (default: false)
  DELETE_NAMESPACES            Delete namespaces after undeploy (default: false)
  LLMD_NS                      Namespace WVA watches for workloads (default: llm-d-inference-scheduler)
                               Optional overrides (not set by install.sh): LLM_D_MODELSERVICE_NAME, MODEL_ID
                               for chart llmd.modelName / llmd.modelID when you export them before running.

Examples:
  $(basename "$0")

  IMG=registry.example.com/wva:dev $(basename "$0") -e kind-emulator

  $(basename "$0") -r my-wva-release -e openshift
EOF
}

parse_args() {
  if [[ -n "$IMG" ]]; then
    log_info "Detected IMG environment variable: $IMG"
    if [[ "$IMG" == *":"* ]]; then
      IFS=':' read -r WVA_IMAGE_REPO WVA_IMAGE_TAG <<< "$IMG"
    else
      log_warning "IMG has wrong format, using default image"
    fi
  fi

  while [[ $# -gt 0 ]]; do
    case "$1" in
      -i|--wva-image)
        if [[ "$2" == *":"* ]]; then
          IFS=':' read -r WVA_IMAGE_REPO WVA_IMAGE_TAG <<< "$2"
        else
          WVA_IMAGE_REPO="$2"
        fi
        shift 2
        ;;
      -r|--release-name)      WVA_RELEASE_NAME="$2"; shift 2 ;;
      -u|--undeploy)          UNDEPLOY=true; shift ;;
      -e|--environment)
        ENVIRONMENT="$2" ; shift 2
        if ! containsElement "$ENVIRONMENT" "${COMPATIBLE_ENV_LIST[@]}"; then
          log_error "Invalid environment: $ENVIRONMENT. Valid options are: ${COMPATIBLE_ENV_LIST[*]}"
        fi
        ;;
      --model)
        # Legacy CI/scripts — install.sh does not consume MODEL_ID; install-llmd-infra.sh does.
        export MODEL_ID="$2"
        shift 2
        ;;
      --accelerator)
        export ACCELERATOR_TYPE="$2"
        shift 2
        ;;
      -h|--help)              print_help; exit 0 ;;
      *)
        echo "Error: Unknown option: $1" >&2
        print_help
        exit 1
        ;;
    esac
  done
}
