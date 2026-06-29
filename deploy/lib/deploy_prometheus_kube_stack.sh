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
    # Create the wva-operation-dashboard ConfigMap from the JSON file with Grafana sidecar label
    local WVA_DASHBOARD_JSON="$WVA_PROJECT/deploy/grafana/operational-dashboard.json"
    if [ "$DEPLOY_OPERATIONAL_DASHBOARD" = "true" ]; then
        kubectl create configmap wva-operation-dashboard \
            --from-file=operational-dashboard.json="$WVA_DASHBOARD_JSON" \
            -n "$MONITORING_NAMESPACE" \
            --dry-run=client -o yaml | \
        kubectl label --local -f - grafana_dashboard=1 -o yaml | \
        kubectl apply -f -
    fi

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
        --set grafana.enabled="$DEPLOY_OPERATIONAL_DASHBOARD" \
        --set grafana.sidecar.dashboards.enabled=true \
        --set grafana.sidecar.dashboards.label=grafana_dashboard \
        --set 'grafana.datasources.datasources\.yaml.apiVersion=1' \
        --set-string 'grafana.datasources.datasources\.yaml.datasources[0].name=Prometheus' \
        --set-string 'grafana.datasources.datasources\.yaml.datasources[0].type=prometheus' \
        --set-string 'grafana.datasources.datasources\.yaml.datasources[0].url=https://kube-prometheus-stack-prometheus.'"$MONITORING_NAMESPACE"'.svc.cluster.local:9090' \
        --set-string 'grafana.datasources.datasources\.yaml.datasources[0].access=proxy' \
        --set-string 'grafana.datasources.datasources\.yaml.datasources[0].jsonData.httpMethod=POST' \
        --set-string 'grafana.datasources.datasources\.yaml.datasources[0].jsonData.timeInterval=30s' \
        --set 'grafana.datasources.datasources\.yaml.datasources[0].jsonData.tlsSkipVerify=true' \
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
