#!/bin/bash
#
# Workload-Variant-Autoscaler Kubernetes Environment-Specific Configuration
# This script provides Kubernetes-specific functions and variable overrides
# It is sourced by the main install.sh script
# Note: it is NOT meant to be executed directly
#

set -e  # Exit on error
set -o pipefail  # Exit on pipe failure

#
# Kubernetes-specific Prometheus Configuration
# Note: overriding defaults from common script
#
PROMETHEUS_SVC_NAME="kube-prometheus-stack-prometheus"
PROMETHEUS_BASE_URL="https://$PROMETHEUS_SVC_NAME.$MONITORING_NAMESPACE.svc.cluster.local"
PROMETHEUS_PORT="9090"
PROMETHEUS_URL=${PROMETHEUS_URL:-"$PROMETHEUS_BASE_URL:$PROMETHEUS_PORT"}
DEPLOY_PROMETHEUS=${DEPLOY_PROMETHEUS:-true}
SKIP_TLS_VERIFY=${SKIP_TLS_VERIFY:-false}

check_specific_prerequisites() {
    log_info "Checking Kubernetes-specific prerequisites..."
    
    local missing_tools=()
    
    # Check for required tools (including Kubernetes-specific ones)
    for tool in kubectl helm; do
        if ! command -v $tool &> /dev/null; then
            missing_tools+=($tool)
        fi
    done
    
    if [ ${#missing_tools[@]} -ne 0 ]; then
        log_error "Missing required tools: ${missing_tools[*]}"
    fi
    
    log_success "All Kubernetes prerequisites met"
}

# Deploy WVA prerequisites for Kubernetes
deploy_wva_prerequisites() {
    log_info "Deploying Workload-Variant-Autoscaler prerequisites for Kubernetes..."

    # Extract Prometheus CA certificate
    log_info "Extracting Prometheus TLS certificate"
    kubectl get secret $PROMETHEUS_SECRET_NAME -n $MONITORING_NAMESPACE -o jsonpath='{.data.tls\.crt}' | base64 -d > $PROM_CA_CERT_PATH

    if [ "$SKIP_TLS_VERIFY" = true ]; then
        log_warning "TLS verification NOT enabled: using values-dev.yaml for dev deployments - NOT FOR PRODUCTION USE"
        VALUES_FILE="${WVA_PROJECT}/charts/workload-variant-autoscaler/values-dev.yaml"
    else
        log_info "TLS verification enabled: using values.yaml for production deployments"
        VALUES_FILE="${WVA_PROJECT}/charts/workload-variant-autoscaler/values.yaml"
    fi

    log_success "WVA prerequisites complete"
}

create_namespaces() {
    log_info "Creating namespaces..."
    
    for ns in $WVA_NS $MONITORING_NAMESPACE $LLMD_NS; do
        if kubectl get namespace $ns &> /dev/null; then
            log_info "Namespace $ns already exists"
        else
            kubectl create namespace $ns
            log_success "Namespace $ns created"
        fi
    done
}

# Deploy Prometheus on Kubernetes
deploy_prometheus_stack() {
    log_info "Deploying kube-prometheus-stack with TLS..."
    
    # Add helm repo
    helm repo add prometheus-community https://prometheus-community.github.io/helm-charts || true
    helm repo update
    
    # Create self-signed TLS certificate for Prometheus
    log_info "Creating self-signed TLS certificate for Prometheus"
    openssl req -x509 -newkey rsa:2048 -nodes \
        -keyout /tmp/prometheus-tls.key \
        -out /tmp/prometheus-tls.crt \
        -days 365 \
        -subj "/CN=prometheus" \
        -addext "subjectAltName=DNS:kube-prometheus-stack-prometheus.${MONITORING_NAMESPACE}.svc.cluster.local,DNS:kube-prometheus-stack-prometheus.${MONITORING_NAMESPACE}.svc,DNS:prometheus,DNS:localhost" \
        &> /dev/null
    
    # Create Kubernetes secret with TLS certificate
    log_info "Creating Kubernetes secret for Prometheus TLS"
    kubectl create secret tls $PROMETHEUS_SECRET_NAME \
        --cert=/tmp/prometheus-tls.crt \
        --key=/tmp/prometheus-tls.key \
        -n $MONITORING_NAMESPACE \
        --dry-run=client -o yaml | kubectl apply -f - &> /dev/null
    
    # Clean up temp files
    rm -f /tmp/prometheus-tls.{key,crt}
    
    # Install kube-prometheus-stack with TLS enabled
    log_info "Installing kube-prometheus-stack with TLS configuration"
    helm upgrade --install kube-prometheus-stack prometheus-community/kube-prometheus-stack \
        -n $MONITORING_NAMESPACE \
        --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false \
        --set prometheus.prometheusSpec.podMonitorSelectorNilUsesHelmValues=false \
        --set prometheus.service.type=ClusterIP \
        --set prometheus.service.port=$PROMETHEUS_PORT \
        --set prometheus.prometheusSpec.web.tlsConfig.cert.secret.name=$PROMETHEUS_SECRET_NAME \
        --set prometheus.prometheusSpec.web.tlsConfig.cert.secret.key=tls.crt \
        --set prometheus.prometheusSpec.web.tlsConfig.keySecret.name=$PROMETHEUS_SECRET_NAME \
        --set prometheus.prometheusSpec.web.tlsConfig.keySecret.key=tls.key \
        --timeout=5m \
        --wait
    
    log_success "kube-prometheus-stack deployed with TLS"
    log_info "Prometheus URL: $PROMETHEUS_URL"
}

# Kubernetes-specific Undeployment functions
undeploy_prometheus_stack() {
    log_info "Uninstalling kube-prometheus-stack..."
    
    helm uninstall kube-prometheus-stack -n $MONITORING_NAMESPACE 2>/dev/null || \
        log_warning "Prometheus stack not found or already uninstalled"

    kubectl delete secret $PROMETHEUS_SECRET_NAME -n $MONITORING_NAMESPACE --ignore-not-found

    log_success "Prometheus stack uninstalled"
}

delete_namespaces() {
    log_info "Deleting namespaces..."
    
    for ns in $LLMD_NS $WVA_NS $MONITORING_NAMESPACE; do
        if kubectl get namespace $ns &> /dev/null; then
            if [[ "$ns" == "$LLMD_NS" && "$DEPLOY_LLM_D" == "false" ]] || [[ "$ns" == "$WVA_NS" && "$DEPLOY_WVA" == "false" ]] || [[ "$ns" == "$MONITORING_NAMESPACE" && "$DEPLOY_PROMETHEUS" == "false" ]] ; then
                log_info "Skipping deletion of namespace $ns as it was not deployed"
            else 
                log_info "Deleting namespace $ns..."
                kubectl delete namespace $ns 2>/dev/null || \
                    log_warning "Failed to delete namespace $ns"
            fi
        fi
    done
    
    log_success "Namespaces deleted"
}

# Environment-specific functions are now sourced by the main install.sh script
# Do not call functions directly when sourced

