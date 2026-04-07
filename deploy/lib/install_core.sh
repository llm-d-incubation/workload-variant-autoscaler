#!/usr/bin/env bash
#
# Core install orchestration for deploy/install.sh.
# Requires vars: ENVIRONMENT, SCRIPT_DIR, SKIP_CHECKS, deployment toggles.
# Requires funcs sourced by install.sh: parse_args(), check_prerequisites(),
# set_tls_verification(), set_wva_logging_level(), create_namespaces(), deploy_*(), verify_deployment(), print_summary().
#

main() {
    # Parse command line arguments first
    parse_args "$@"

    # Handle infra-only mode: skip VA and HPA deployment
    if [ "$INFRA_ONLY" = "true" ]; then
        log_info "Infra-only mode enabled: Skipping VA and HPA deployment"
        DEPLOY_VA=false
        DEPLOY_HPA=false
    fi

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

        # Source environment-specific script to make functions available
        if [ -f "$SCRIPT_DIR/$ENVIRONMENT/install.sh" ]; then
            source "$SCRIPT_DIR/$ENVIRONMENT/install.sh"
        else
            log_error "Environment-specific script not found: $SCRIPT_DIR/$ENVIRONMENT/install.sh"
        fi

        cleanup
        exit 0
    fi

    # Normal deployment flow
    log_info "Starting Workload-Variant-Autoscaler Deployment on $ENVIRONMENT"
    log_info "==========================================================="
    echo ""

    # Check prerequisites
    if [ "$SKIP_CHECKS" != "true" ]; then
        check_prerequisites
    fi

    # Set TLS verification and logging level based on environment
    set_tls_verification
    set_wva_logging_level

    if [[ "${CLUSTER_TYPE:-}" == "kind" ]]; then
        log_info "Kind cluster detected - setting environment to kind-emulated"
        ENVIRONMENT="kind-emulator"
    fi

    # Source environment-specific script to make functions available
    log_info "Loading environment-specific functions for $ENVIRONMENT..."
    if [ -f "$SCRIPT_DIR/$ENVIRONMENT/install.sh" ]; then
        source "$SCRIPT_DIR/$ENVIRONMENT/install.sh"

        # Run environment-specific prerequisite checks if function exists
        if declare -f check_specific_prerequisites > /dev/null; then
            if [ "$SKIP_CHECKS" != "true" ]; then
                check_specific_prerequisites
            fi
        fi
    else
        log_error "Environment script not found: $SCRIPT_DIR/$ENVIRONMENT/install.sh"
    fi

    # Detect GPU type for non-emulated environments
    if containsElement "$ENVIRONMENT" "${NON_EMULATED_ENV_LIST[@]}"; then
        detect_gpu_type
    else
        log_info "Skipping GPU type detection for emulated environment (ENVIRONMENT=$ENVIRONMENT)"
    fi

    # Display configuration
    log_info "Using configuration:"
    echo "    Deployed on:          $ENVIRONMENT"
    echo "    WVA Image:            $WVA_IMAGE_REPO:$WVA_IMAGE_TAG"
    echo "    WVA Namespace:        $WVA_NS"
    echo "    llm-d Namespace:      $LLMD_NS"
    echo "    Monitoring Namespace: $MONITORING_NAMESPACE"
    echo "    Scaler Backend:       $SCALER_BACKEND"
    echo "    Model:                $MODEL_ID"
    echo "    Accelerator:          $ACCELERATOR_TYPE"
    echo ""

    # Prompt for Gateway control plane installation
    if [[ "$E2E_TESTS_ENABLED" == "false" ]]; then
        prompt_gateway_installation
    elif [[ -n "$INSTALL_GATEWAY_CTRLPLANE_ORIGINAL" ]]; then
        log_info "Using explicitly set INSTALL_GATEWAY_CTRLPLANE=$INSTALL_GATEWAY_CTRLPLANE"
    else
        log_info "Enabling Gateway control plane installation for tests"
        export INSTALL_GATEWAY_CTRLPLANE="true"
    fi

    # Create namespaces
    create_namespaces

    deploy_monitoring_stack
    deploy_optional_benchmark_grafana

    # Deploy WVA prerequisites (environment-specific)
    if [ "$DEPLOY_WVA" = "true" ]; then
        deploy_wva_prerequisites
    fi

    # Deploy WVA
    if [ "$DEPLOY_WVA" = "true" ]; then
        deploy_wva_controller
    else
        log_info "Skipping WVA deployment (DEPLOY_WVA=false)"
    fi

    # Deploy llm-d
    if [ "$DEPLOY_LLM_D" = "true" ]; then
        deploy_llm_d_infrastructure

        # For emulated environments, apply specific fixes
        if ! containsElement "$ENVIRONMENT" "${NON_EMULATED_ENV_LIST[@]}"; then
            apply_llm_d_infrastructure_fixes
        else
            log_info "Skipping llm-d related fixes for non-emulated environment (ENVIRONMENT=$ENVIRONMENT)"
        fi

    else
        log_info "Skipping llm-d deployment (DEPLOY_LLM_D=false)"
    fi

    deploy_scaler_backend

    # Verify deployment
    verify_deployment

    # Print summary
    print_summary

    log_success "Deployment on $ENVIRONMENT complete!"
}
