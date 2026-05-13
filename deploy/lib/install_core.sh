#!/usr/bin/env bash
#
# Core install orchestration for deploy/install.sh.
# Requires vars: ENVIRONMENT, SCRIPT_DIR, SKIP_CHECKS, deployment toggles.
# Requires funcs sourced by install.sh: parse_args(), check_prerequisites(),
# set_tls_verification(), set_wva_logging_level(), create_namespaces(), deploy_*(), verify_deployment(), print_summary().
# llm-d install is deploy/install-llmd-infra.sh (not sourced here).
#

main() {
    parse_args "$@"

    # When using KEDA as scaler backend: skip Prometheus Adapter and deploy KEDA instead
    if [ "$SCALER_BACKEND" = "keda" ]; then
        log_info "Scaler backend is KEDA: Skipping Prometheus Adapter, will deploy KEDA"
        DEPLOY_PROMETHEUS_ADAPTER=false
    fi

    # Undeploy mode
    if [ "$UNDEPLOY" = "true" ]; then
        log_info "Starting Workload-Variant-Autoscaler Undeployment on $ENVIRONMENT"
        log_info "============================================================="
        echo ""

        if [ -f "$SCRIPT_DIR/$ENVIRONMENT/install.sh" ]; then
            # shellcheck source=/dev/null
            source "$SCRIPT_DIR/$ENVIRONMENT/install.sh"
        else
            log_error "Environment-specific script not found: $SCRIPT_DIR/$ENVIRONMENT/install.sh"
        fi

        cleanup
        exit 0
    fi

    log_info "Starting Workload-Variant-Autoscaler Deployment on $ENVIRONMENT"
    log_info "==========================================================="
    echo ""

    if [ "$SKIP_CHECKS" != "true" ]; then
        check_prerequisites
    fi

    set_tls_verification
    set_wva_logging_level

    if [[ "${CLUSTER_TYPE:-}" == "kind" ]]; then
        log_info "Kind cluster detected - setting environment to kind-emulated"
        ENVIRONMENT="kind-emulator"
    fi

    log_info "Loading environment-specific functions for $ENVIRONMENT..."
    if [ -f "$SCRIPT_DIR/$ENVIRONMENT/install.sh" ]; then
        # shellcheck source=/dev/null
        source "$SCRIPT_DIR/$ENVIRONMENT/install.sh"

        if declare -f check_specific_prerequisites > /dev/null; then
            if [ "$SKIP_CHECKS" != "true" ]; then
                check_specific_prerequisites
            fi
        fi
    else
        log_error "Environment script not found: $SCRIPT_DIR/$ENVIRONMENT/install.sh"
    fi

    log_info "Using configuration:"
    echo "    Deployed on:          $ENVIRONMENT"
    echo "    WVA Image:            $WVA_IMAGE_REPO:$WVA_IMAGE_TAG"
    echo "    WVA Namespace:        $WVA_NS"
    echo "    llm-d Namespace:      $LLMD_NS"
    echo "    Monitoring Namespace: $MONITORING_NAMESPACE"
    echo "    Scaler Backend:       $SCALER_BACKEND"
    echo ""

    create_namespaces

    deploy_monitoring_stack
    deploy_optional_benchmark_grafana

    # llm-d (gateway, EPP, ModelService, clone/helmfile, emulated ModelService cleanup) is not part of
    # install.sh — run deploy/install-llmd-infra.sh after this script when you need that stack; see
    # deploy/install.sh header and deploy/README.md. GPU discovery for llm-d lives there too.

    # Deploy WVA prerequisites first (environment-specific).
    if [ "$DEPLOY_WVA" = "true" ]; then
        deploy_wva_prerequisites
    fi

    # Deploy WVA controller (autoscaler + chart); InferencePool CRDs and llm-d workloads follow install-llmd-infra.sh.
    if [ "$DEPLOY_WVA" = "true" ]; then
        deploy_wva_controller
    else
        log_info "Skipping WVA deployment (DEPLOY_WVA=false)"
    fi

    deploy_scaler_backend

    verify_deployment

    print_summary

    log_success "Deployment on $ENVIRONMENT complete!"
}
