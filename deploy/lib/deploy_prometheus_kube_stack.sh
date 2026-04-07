#!/usr/bin/env bash
#
# Shared kube-prometheus-stack install for Kubernetes-like environments
# (vanilla Kubernetes, Kind emulator, etc.). Sourced by deploy/*/install.sh.
# Requires vars: MONITORING_NAMESPACE, PROMETHEUS_SECRET_NAME,
# PROMETHEUS_PORT, PROMETHEUS_URL.
# Requires funcs: log_info/log_warning/log_success.
#

deploy_prometheus_kube_stack() {
    log_info "Deploying kube-prometheus-stack with TLS..."

    helm repo add prometheus-community https://prometheus-community.github.io/helm-charts || true
    if [ "${SKIP_HELM_REPO_UPDATE:-}" = "true" ]; then
        log_info "Skipping helm repo update (SKIP_HELM_REPO_UPDATE=true)"
    else
        helm repo update
    fi

    log_info "Creating self-signed TLS certificate for Prometheus"
    openssl req -x509 -newkey rsa:2048 -nodes \
        -keyout /tmp/prometheus-tls.key \
        -out /tmp/prometheus-tls.crt \
        -days 365 \
        -subj "/CN=prometheus" \
        -addext "subjectAltName=DNS:kube-prometheus-stack-prometheus.${MONITORING_NAMESPACE}.svc.cluster.local,DNS:kube-prometheus-stack-prometheus.${MONITORING_NAMESPACE}.svc,DNS:prometheus,DNS:localhost" \
        &> /dev/null

    log_info "Creating Kubernetes secret for Prometheus TLS"
    kubectl create secret tls "$PROMETHEUS_SECRET_NAME" \
        --cert=/tmp/prometheus-tls.crt \
        --key=/tmp/prometheus-tls.key \
        -n "$MONITORING_NAMESPACE" \
        --dry-run=client -o yaml | kubectl apply -f - &> /dev/null

    rm -f /tmp/prometheus-tls.key /tmp/prometheus-tls.crt

    log_info "Installing kube-prometheus-stack with TLS configuration"
    helm upgrade --install kube-prometheus-stack prometheus-community/kube-prometheus-stack \
        -n "$MONITORING_NAMESPACE" \
        --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false \
        --set prometheus.prometheusSpec.podMonitorSelectorNilUsesHelmValues=false \
        --set prometheus.service.type=ClusterIP \
        --set prometheus.service.port="$PROMETHEUS_PORT" \
        --set prometheus.prometheusSpec.web.tlsConfig.cert.secret.name="$PROMETHEUS_SECRET_NAME" \
        --set prometheus.prometheusSpec.web.tlsConfig.cert.secret.key=tls.crt \
        --set prometheus.prometheusSpec.web.tlsConfig.keySecret.name="$PROMETHEUS_SECRET_NAME" \
        --set prometheus.prometheusSpec.web.tlsConfig.keySecret.key=tls.key \
        --set grafana.enabled=false \
        --set alertmanager.enabled=false \
        --timeout=10m \
        --wait

    log_success "kube-prometheus-stack deployed with TLS"
    log_info "Prometheus URL: $PROMETHEUS_URL"
}

undeploy_prometheus_kube_stack() {
    log_info "Uninstalling kube-prometheus-stack..."

    helm uninstall kube-prometheus-stack -n "$MONITORING_NAMESPACE" 2>/dev/null || \
        log_warning "Prometheus stack not found or already uninstalled"

    kubectl delete secret "$PROMETHEUS_SECRET_NAME" -n "$MONITORING_NAMESPACE" --ignore-not-found

    log_success "Prometheus stack uninstalled"
}
