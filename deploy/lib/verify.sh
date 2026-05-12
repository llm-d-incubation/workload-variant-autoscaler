#!/usr/bin/env bash
#
# Shared verification and deployment summary helpers for deploy/install.sh.
# Covers WVA / Prometheus / scaler backend only — llm-d is installed separately (install-llmd-infra.sh).
# Requires funcs: log_info/log_warning/log_success, containsElement().
# Uses constants: DEFAULT_VERIFY_STARTUP_SLEEP_SECONDS and shared selectors.
#

verify_deployment() {
    log_info "Verifying deployment..."

    local all_good=true

    # --- WVA
    log_info "Checking WVA controller pods..."
    sleep "$DEFAULT_VERIFY_STARTUP_SLEEP_SECONDS"
    if kubectl get pods -n "$WVA_NS" -l "$WVA_CONTROLLER_LABEL_SELECTOR" 2>/dev/null | grep -q Running; then
        log_success "WVA controller is running"
    else
        log_warning "WVA controller may still be starting"
        all_good=false
    fi

    # --- Monitoring
    if [ "$DEPLOY_PROMETHEUS" = "true" ]; then
        log_info "Checking Prometheus..."
        if kubectl get pods -n "$MONITORING_NAMESPACE" -l app.kubernetes.io/name=prometheus 2>/dev/null | grep -q Running; then
            log_success "Prometheus is running"
        else
            log_warning "Prometheus may still be starting"
        fi
    fi

    # --- Scaler backend
    if [ "$SCALER_BACKEND" = "keda" ]; then
        log_info "Checking KEDA..."
        if kubectl get pods -n "$KEDA_NAMESPACE" -l "$KEDA_OPERATOR_LABEL_SELECTOR" 2>/dev/null | grep -q Running; then
            log_success "KEDA is running"
        else
            log_warning "KEDA may still be starting"
        fi
    elif [ "$SCALER_BACKEND" = "none" ]; then
        log_info "Scaler backend skipped (SCALER_BACKEND=none) — assuming external metrics API is pre-installed"
    elif [ "$DEPLOY_PROMETHEUS_ADAPTER" = "true" ]; then
        log_info "Checking Prometheus Adapter..."
        if kubectl get pods -n "$MONITORING_NAMESPACE" -l "$PROMETHEUS_ADAPTER_LABEL_SELECTOR" 2>/dev/null | grep -q Running; then
            log_success "Prometheus Adapter is running"
        else
            log_warning "Prometheus Adapter may still be starting"
        fi
    fi

    if [ "$all_good" = true ]; then
        log_success "All components verified successfully!"
    else
        log_warning "Some components may still be starting. Check the logs above."
    fi
}

print_summary() {
    echo ""
    echo "=========================================="
    echo " Deployment Summary"
    echo "=========================================="
    echo ""
    echo "Deployment Environment: $ENVIRONMENT"
    echo "WVA Namespace:           $WVA_NS"
    echo "LLMD Namespace:          $LLMD_NS"
    echo "Monitoring Namespace:    $MONITORING_NAMESPACE"
    echo "WVA Image:              $WVA_IMAGE_REPO:$WVA_IMAGE_TAG"
    echo ""
    echo "Deployed Components:"
    echo "===================="
    if [ "$DEPLOY_PROMETHEUS" = "true" ]; then
        echo "✓ kube-prometheus-stack (Prometheus + Grafana)"
    fi
    if [ "$DEPLOY_WVA" = "true" ]; then
        echo "✓ WVA Controller (via Helm chart)"
    fi
    if [ "$SCALER_BACKEND" = "keda" ]; then
        echo "✓ KEDA (scaler backend, external metrics API)"
    elif [ "$SCALER_BACKEND" = "none" ]; then
        echo "- Scaler backend: skipped (SCALER_BACKEND=none, pre-installed on cluster)"
    elif [ "$DEPLOY_PROMETHEUS_ADAPTER" = "true" ]; then
        echo "✓ Prometheus Adapter (external metrics API)"
    fi
    echo ""
    echo "Next Steps:"
    echo "==========="
    echo ""
    echo "1. Deploy llm-d (EPP, gateway, ModelService) when needed:"
    echo "     ./deploy/install-llmd-infra.sh -e $ENVIRONMENT"
    echo ""
    echo "2. Create VariantAutoscaling / HPA via tests, operators, or helm with chart toggles."
    echo ""
    echo "3. View WVA logs:"
    echo "   kubectl logs -n $WVA_NS -l app.kubernetes.io/name=workload-variant-autoscaler -f"
    echo ""
    echo "4. Check external metrics API (when Prometheus Adapter is used):"
    echo "   kubectl get --raw \"/apis/external.metrics.k8s.io/v1beta1/namespaces/$LLMD_NS/wva_desired_replicas\" | jq"
    echo ""
    echo "5. Port-forward Prometheus to view metrics:"
    echo "   kubectl port-forward -n $MONITORING_NAMESPACE svc/${PROMETHEUS_SVC_NAME} ${PROMETHEUS_PORT}:${PROMETHEUS_PORT}"
    echo ""
    echo "=========================================="
}
