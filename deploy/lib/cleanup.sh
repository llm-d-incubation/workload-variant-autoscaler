#!/usr/bin/env bash
#
# Undeploy and cleanup helpers for deploy/install.sh.
# Requires vars: KEDA_NAMESPACE, MONITORING_NAMESPACE, LLMD_NS, WVA_NS,
# EXAMPLE_DIR, WVA_PROJECT, LLM_D_PROJECT, GATEWAY_PROVIDER.
# Requires funcs: stop_apiservice_guard(), containsElement(),
# undeploy_prometheus_stack(), delete_namespaces(), log_*().
#

undeploy_keda() {
    if [ "$ENVIRONMENT" = "openshift" ]; then
        log_info "OpenShift: skipping KEDA uninstall (platform-managed)"
        return
    fi
    if [ "$ENVIRONMENT" = "kubernetes" ] && [ "${KEDA_HELM_INSTALL:-false}" != "true" ]; then
        log_info "Kubernetes: skipping KEDA uninstall (cluster-managed; set KEDA_HELM_INSTALL=true if this script installed KEDA)"
        return
    fi
    log_info "Uninstalling KEDA..."
    helm uninstall "$KEDA_RELEASE_NAME" -n "$KEDA_NAMESPACE" 2>/dev/null || \
        log_warning "KEDA not found or already uninstalled"
    kubectl delete namespace "$KEDA_NAMESPACE" --ignore-not-found --timeout=120s 2>/dev/null || true
    log_success "KEDA uninstalled"
}

undeploy_prometheus_adapter() {
    log_info "Uninstalling Prometheus Adapter..."

    # Stop the APIService guard if running
    stop_apiservice_guard

    helm uninstall "$PROMETHEUS_ADAPTER_RELEASE_NAME" -n "$MONITORING_NAMESPACE" 2>/dev/null || \
        log_warning "Prometheus Adapter not found or already uninstalled"

    kubectl delete configmap "$PROMETHEUS_CA_CONFIGMAP_NAME" -n "$MONITORING_NAMESPACE" --ignore-not-found
    # Cleanup is handled by the values files in config/samples

    log_success "Prometheus Adapter uninstalled"
}

undeploy_llm_d_infrastructure() {
    log_info "Undeploying the llm-d infrastructure..."

    # Determine release name based on environment
    local RELEASE=""
    if ! containsElement "$ENVIRONMENT" "${NON_EMULATED_ENV_LIST[@]}" ; then
        RELEASE="$NAMESPACE_SUFFIX"
    else
        RELEASE="$WELL_LIT_PATH_NAME"
    fi

    # Helm uninstall only needs the release name and namespace — it does not
    # require the local llm-d clone directory. Always attempt to remove the
    # releases so cluster resources are cleaned up even when the local repo
    # has already been deleted or was never cloned.
    log_info "Removing llm-d core components..."

    helm uninstall "infra-$RELEASE" -n "${LLMD_NS}" 2>/dev/null || \
        log_warning "llm-d infra components not found or already uninstalled"
    helm uninstall "gaie-$RELEASE" -n "${LLMD_NS}" 2>/dev/null || \
        log_warning "llm-d inference-scheduler components not found or already uninstalled"
    helm uninstall "ms-$RELEASE" -n "${LLMD_NS}" 2>/dev/null || \
        log_warning "llm-d ModelService components not found or already uninstalled"

    if [ ! -d "$EXAMPLE_DIR" ]; then
        log_warning "llm-d example directory not found, skipping local cleanup"
    fi

    # Remove HF token secret
    kubectl delete secret llm-d-hf-token -n "${LLMD_NS}" --ignore-not-found

    # Remove Gateway provider if installed by the script
    if [[ "$INSTALL_GATEWAY_CTRLPLANE" == true ]]; then
        log_info "Removing Gateway provider..."
        helmfile destroy -f "$GATEWAY_PREREQ_DIR/$GATEWAY_PROVIDER.helmfile.yaml" 2>/dev/null || \
            log_warning "Gateway provider cleanup incomplete"
        kubectl delete namespace "${GATEWAY_PROVIDER}-system" --ignore-not-found 2>/dev/null || true

    fi

    log_info "Deleting llm-d cloned repository..."
    if [ ! -d "$WVA_PROJECT/$LLM_D_PROJECT" ]; then
        log_warning "llm-d repository directory not found, skipping deletion"
    else
        rm -rf "$WVA_PROJECT/$LLM_D_PROJECT" 2>/dev/null || \
            log_warning "Failed to delete llm-d repository directory"
    fi

    log_success "llm-d infrastructure removed"
}

undeploy_wva_controller() {
    log_info "Uninstalling Workload-Variant-Autoscaler (release: $WVA_RELEASE_NAME)..."

    helm uninstall "$WVA_RELEASE_NAME" -n "$WVA_NS" 2>/dev/null || \
        log_warning "Workload-Variant-Autoscaler not found or already uninstalled"

    rm -f "$PROM_CA_CERT_PATH"

    log_success "WVA uninstalled"
}

cleanup() {
    log_info "Starting undeployment process..."
    log_info "======================================"
    echo ""

    # Stop the APIService guard if running (safety net)
    stop_apiservice_guard

    # Undeploy environment-specific components (Prometheus, etc.)
    if [ "$DEPLOY_PROMETHEUS" = "true" ]; then
        undeploy_prometheus_stack
    fi

    # Undeploy scaler backend (KEDA or Prometheus Adapter)
    if [ "$SCALER_BACKEND" = "keda" ]; then
        undeploy_keda
    elif [ "$DEPLOY_PROMETHEUS_ADAPTER" = "true" ]; then
        undeploy_prometheus_adapter
    fi

    if [ "$DEPLOY_LLM_D" = "true" ]; then
        undeploy_llm_d_infrastructure
    fi

    if [ "$DEPLOY_WVA" = "true" ]; then
        undeploy_wva_controller
    fi

    # Delete namespaces if requested
    if [ "$DELETE_NAMESPACES" = "true" ] || [ "$DELETE_CLUSTER" = "true" ]; then
        delete_namespaces
    else
        log_info "Keeping namespaces (use --delete-namespaces or set DELETE_NAMESPACES=true to remove)"
    fi

    # Remove llm-d repository
    if [ -d "$(dirname "$WVA_PROJECT")/$LLM_D_PROJECT" ]; then
        log_info "llm-d repository at $(dirname "$WVA_PROJECT")/$LLM_D_PROJECT preserved (manual cleanup if needed)"
    fi

    echo ""
    log_success "Undeployment complete!"
    echo ""
    echo "=========================================="
    echo " Undeployment Summary for $ENVIRONMENT"
    echo "=========================================="
    echo ""
    echo "Removed components:"
    [ "$SCALER_BACKEND" = "keda" ] && echo "✓ KEDA"
    [ "$DEPLOY_PROMETHEUS_ADAPTER" = "true" ] && echo "✓ Prometheus Adapter"
    [ "$DEPLOY_LLM_D" = "true" ] && echo "✓ llm-d Infrastructure"
    [ "$DEPLOY_WVA" = "true" ] && echo "✓ WVA Controller"
    [ "$DEPLOY_PROMETHEUS" = "true" ] && echo "✓ Prometheus Stack"

    if [ "$DELETE_NAMESPACES" = "true" ]; then
        echo "✓ Namespaces"
    else
        echo ""
        echo "Namespaces preserved:"
        echo "  - $LLMD_NS"
        echo "  - $WVA_NS"
        echo "  - $MONITORING_NAMESPACE"
        [ "$SCALER_BACKEND" = "keda" ] && echo "  - $KEDA_NAMESPACE"
    fi
    echo ""
    echo "=========================================="
}
