#!/usr/bin/env bash
#
# CLI help and argument parsing for deploy/install.sh.
# Requires vars: WVA_IMAGE_REPO, WVA_IMAGE_TAG, MODEL_ID, ACCELERATOR_TYPE,
# WVA_RELEASE_NAME, COMPATIBLE_ENV_LIST.
# Requires funcs: log_info/log_warning/log_error, containsElement().
#

print_help() {
  cat <<EOF
Usage: $(basename "$0") [OPTIONS]

This script deploys the complete Workload-Variant-Autoscaler stack on a cluster with real GPUs.

Options:
  -i, --wva-image IMAGE        Container image to use for the WVA (default: $WVA_IMAGE_REPO:$WVA_IMAGE_TAG)
  -m, --model MODEL            Model ID to use (default: $MODEL_ID)
  -a, --accelerator TYPE       Accelerator type: A100, H100, L40S, etc. (default: $ACCELERATOR_TYPE)
  -r, --release-name NAME      Helm release name for WVA (default: $WVA_RELEASE_NAME)
  --infra-only                 Deploy only llm-d infrastructure and WVA controller (skip VA/HPA, for e2e testing)
  -u, --undeploy               Undeploy all components
  -e, --environment            Specify deployment environment: kubernetes, openshift, kind-emulated (default: kubernetes)
  -h, --help                   Show this help and exit

Environment Variables:
  IMG                          Container image to use for the WVA (alternative to -i flag)
  HF_TOKEN                     HuggingFace token for model access (required for llm-d deployment)
  WVA_RELEASE_NAME             Helm release name for WVA (alternative to -r flag)
  INSTALL_GATEWAY_CTRLPLANE    Install Gateway control plane (default: prompt user, can be set to "true"/"false")
  DEPLOY_PROMETHEUS            Deploy Prometheus stack (default: true)
  DEPLOY_WVA                   Deploy WVA controller (default: true)
  DEPLOY_LLM_D                 Deploy llm-d infrastructure (default: true)
  DEPLOY_PROMETHEUS_ADAPTER    Deploy Prometheus Adapter (default: true)
  DEPLOY_VA                    Deploy VariantAutoscaling via chart (default: false)
  DEPLOY_HPA                   Deploy HPA via chart (default: false)
  HPA_STABILIZATION_SECONDS    HPA stabilization window in seconds (default: 240)
  HPA_MIN_REPLICAS             HPA minReplicas (default: 1, set to 0 for scale-to-zero)
  INFRA_ONLY                   Deploy only infrastructure (default: false, same as --infra-only flag)
  SCALER_BACKEND               Scaler backend: "prometheus-adapter" (default), "keda", or "none".
                               prometheus-adapter: installs Prometheus Adapter and patches the external metrics APIService.
                               keda: skips Prometheus Adapter; on kubernetes assumes cluster-managed KEDA (KEDA_HELM_INSTALL=true for Helm);
                                     kind-emulator installs KEDA via Helm when needed; OpenShift is platform-managed only.
                               none: skips all scaler backend deployment. Use this on clusters that already have
                                     KEDA or another external metrics API installed (e.g. llmd benchmark clusters).
  KEDA_HELM_INSTALL            When true with ENVIRONMENT=kubernetes, install/upgrade KEDA via Helm (default: false)
  KEDA_NAMESPACE               Namespace for KEDA (default: keda-system)
  UNDEPLOY                     Undeploy mode (default: false)
  DELETE_NAMESPACES            Delete namespaces after undeploy (default: false)
  CONTROLLER_INSTANCE          Controller instance label for multi-controller isolation (optional)

Examples:
  # Deploy with default values
  $(basename "$0")

  # Deploy with custom WVA image
  IMG=<your_registry>/llm-d-workload-variant-autoscaler:tag $(basename "$0")

  # Deploy with custom model and accelerator
  $(basename "$0") -m unsloth/Meta-Llama-3.1-8B -a A100

  # Deploy with custom release name (for multi-install support)
  $(basename "$0") -r my-wva-release

  # Deploy infra-only mode (for e2e testing)
  $(basename "$0") --infra-only
  # Or with environment variable
  INFRA_ONLY=true $(basename "$0")
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
      -r|--release-name)      WVA_RELEASE_NAME="$2"; shift 2 ;;
      --infra-only)           INFRA_ONLY=true; shift ;;
      -u|--undeploy)          UNDEPLOY=true; shift ;;
      -e|--environment)
        ENVIRONMENT="$2" ; shift 2
        if ! containsElement "$ENVIRONMENT" "${COMPATIBLE_ENV_LIST[@]}"; then
          log_error "Invalid environment: $ENVIRONMENT. Valid options are: ${COMPATIBLE_ENV_LIST[*]}"
        fi
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
