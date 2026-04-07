#!/usr/bin/env bash
#
# Shared monitoring orchestration helpers.
# Keeps install.sh main flow concise while delegating to environment/plugin functions.
# Requires funcs: deploy_prometheus_stack(), log_info/log_warning/log_success,
# wait_deployment_available_nonfatal().
# Requires vars: DEPLOY_PROMETHEUS, INSTALL_GRAFANA, MONITORING_NAMESPACE, WVA_PROJECT.
#

deploy_monitoring_stack() {
    # Deploy Prometheus Stack (environment-specific implementation)
    if [ "$DEPLOY_PROMETHEUS" = "true" ]; then
        deploy_prometheus_stack
    else
        log_info "Skipping Prometheus deployment (DEPLOY_PROMETHEUS=false)"
    fi
}

deploy_optional_benchmark_grafana() {
    # Deploy Grafana for benchmarking (optional, controlled by INSTALL_GRAFANA env var)
    if [ "${INSTALL_GRAFANA:-false}" = "true" ]; then
        deploy_benchmark_grafana
    fi
}

deploy_benchmark_grafana() {
    log_info "Deploying benchmark Grafana (INSTALL_GRAFANA=true)..."

    local GRAFANA_YAML="$WVA_PROJECT/deploy/grafana/benchmark-grafana.yaml"
    if [ ! -f "$GRAFANA_YAML" ]; then
        log_error "Grafana manifest not found: $GRAFANA_YAML"
        exit 1
    fi

    # Pre-load Grafana images into Kind cluster if applicable
    if [[ "$CLUSTER_TYPE" == "kind" ]] || [[ "$ENVIRONMENT" == "kind-emulator" ]]; then
        log_info "Pre-loading Grafana images into Kind cluster..."
        docker pull docker.io/grafana/grafana:11.4.0 || true
        docker pull docker.io/grafana/grafana-image-renderer:3.11.6 || true
        kind load docker-image docker.io/grafana/grafana:11.4.0 --name "${CLUSTER_NAME:-kind-wva-gpu-cluster}" || true
        kind load docker-image docker.io/grafana/grafana-image-renderer:3.11.6 --name "${CLUSTER_NAME:-kind-wva-gpu-cluster}" || true
    fi

    # Create the benchmark-dashboard ConfigMap from the JSON file
    local DASHBOARD_JSON="$WVA_PROJECT/deploy/grafana/benchmark-dashboard.json"
    if [ -f "$DASHBOARD_JSON" ]; then
        kubectl create configmap benchmark-dashboard \
            --from-file=benchmark-dashboard.json="$DASHBOARD_JSON" \
            -n "$MONITORING_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
    else
        log_warning "Dashboard JSON not found: $DASHBOARD_JSON — Grafana will start without dashboard"
        kubectl create configmap benchmark-dashboard \
            -n "$MONITORING_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
    fi

    kubectl apply -n "$MONITORING_NAMESPACE" -f "$GRAFANA_YAML"
    log_info "Waiting for benchmark Grafana to be ready..."
    wait_deployment_available_nonfatal \
        "$MONITORING_NAMESPACE" \
        "benchmark-grafana" \
        "120s" \
        "Grafana deployment not ready within timeout (non-fatal)"

    log_success "Benchmark Grafana deployed in $MONITORING_NAMESPACE"
}
