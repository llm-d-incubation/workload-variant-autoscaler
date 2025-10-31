#!/bin/bash
#
# Workload-Variant-Autoscaler OpenShift Environment-Specific Configuration
# This script provides OpenShift-specific functions and variable overrides
# It is sourced by the main install.sh script
# Note: it is NOT meant to be executed directly
#

set -e  # Exit on error
set -o pipefail  # Exit on pipe failure

#
# OpenShift-specific Prometheus Configuration
# Note: overriding defaults from common script
#
PROMETHEUS_SVC_NAME="thanos-querier"
PROMETHEUS_BASE_URL="https://$PROMETHEUS_SVC_NAME.openshift-monitoring.svc.cluster.local"
PROMETHEUS_PORT="9091"
PROMETHEUS_URL=${PROMETHEUS_URL:-"$PROMETHEUS_BASE_URL:$PROMETHEUS_PORT"}
MONITORING_NAMESPACE="openshift-user-workload-monitoring"
PROMETHEUS_SECRET_NAME="thanos-querier-tls"
PROMETHEUS_SECRET_NS="openshift-monitoring"
DEPLOY_PROMETHEUS=false  # OpenShift uses built-in monitoring stack

# TLS verification enabled by default on OpenShift
SKIP_TLS_VERIFY=false
VALUES_FILE="${WVA_PROJECT}/charts/workload-variant-autoscaler/values.yaml"

#### REQUIRED FUNCTION used by deploy/install.sh ####
check_specific_prerequisites() {
    log_info "Checking OpenShift-specific prerequisites..."
    
    local missing_tools=()
    
    # Check for required tools (including OpenShift-specific ones)
    for tool in oc kubectl helm yq; do
        if ! command -v $tool &> /dev/null; then
            missing_tools+=($tool)
        fi
    done
    
    if [ ${#missing_tools[@]} -ne 0 ]; then
        log_error "Missing required tools: ${missing_tools[*]}"
        log_info "Please install the missing tools and try again"
        exit 1
    fi
    
    # Check OpenShift connection
    if ! oc whoami &> /dev/null; then
        log_error "Not logged into OpenShift cluster. Please run 'oc login' first"
        exit 1
    fi
    
    log_success "All OpenShift prerequisites met"
    log_info "Connected to OpenShift as: $(oc whoami)"
    log_info "Current project: $(oc project -q)"
}

#### REQUIRED FUNCTION used by deploy/install.sh ####
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

find_thanos_url() {
    log_info "Finding Thanos querier URL..."
    
    local thanos_svc=$(kubectl get svc -n $PROMETHEUS_SECRET_NS thanos-querier -o jsonpath='{.metadata.name}' 2>/dev/null || echo "")
    
    # Set PROMETHEUS_URL if Thanos service is found
    if [ -n "$thanos_svc" ]; then
        PROMETHEUS_URL="${PROMETHEUS_BASE_URL}:${PROMETHEUS_PORT}"
        log_success "Found Thanos querier: $PROMETHEUS_URL"
    else
        log_error "Thanos querier service not found in openshift-monitoring namespace"
    fi
    
    export PROMETHEUS_URL
}

#### REQUIRED FUNCTION used by deploy/install.sh ####
# OpenShift uses existing monitoring stack (Thanos/Prometheus)
deploy_prometheus_stack() {
    log_info "Using OpenShift built-in monitoring (Thanos)..."
    find_thanos_url
    log_success "OpenShift monitoring stack is available"
}

#### REQUIRED FUNCTION used by deploy/install.sh ####
deploy_wva_prerequisites() {
    log_info "Deploying Workload-Variant-Autoscaler..."

    # Extract OpenShift Service CA certificate (not the server cert - we need the CA that signed it)
    log_info "Extracting OpenShift Service CA certificate for Thanos verification"
    kubectl get configmap openshift-service-ca.crt -n $PROMETHEUS_SECRET_NS -o jsonpath='{.data.service-ca\.crt}' > $PROM_CA_CERT_PATH
    if [ ! -s "$PROM_CA_CERT_PATH" ]; then
        log_warning "Failed to extract service CA from openshift-service-ca.crt, trying openshift-config..."
        kubectl get configmap openshift-service-ca -n openshift-config -o jsonpath='{.data.service-ca\.crt}' > $PROM_CA_CERT_PATH
    fi
    if [ ! -s "$PROM_CA_CERT_PATH" ]; then
        log_error "Failed to extract OpenShift Service CA certificate"
        exit 1
    fi


    # Update Prometheus URL in configmap
    log_info "Updating Prometheus URL in config/manager/configmap.yaml"
    sed -i.bak "s|PROMETHEUS_BASE_URL:.*|PROMETHEUS_BASE_URL: \"$PROMETHEUS_URL\"|" config/manager/configmap.yaml
    
    log_success "WVA prerequisites deployed"
}

#### REQUIRED FUNCTION used by deploy/install.sh ####
# OpenShift uses built-in monitoring, nothing to undeploy
undeploy_prometheus_stack() {
    log_info "OpenShift uses built-in monitoring stack (no cleanup needed)"
}

#### REQUIRED FUNCTION used by deploy/install.sh ####
# Namespaces are not deleted on OpenShift to avoid removing user projects
delete_namespaces() {
    log_info "Not deleting namespaces on OpenShift to avoid removing user projects"
}

# Environment-specific functions are now sourced by the main install.sh script
# Do not call functions directly when sourced

