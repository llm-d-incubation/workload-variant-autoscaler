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
INSTALL_GATEWAY_CTRLPLANE=false  # OpenShift uses its own Gateway control plane stack

# OpenShift-specific prerequisites
REQUIRED_TOOLS=("oc")

# TLS verification enabled by default on OpenShift
SKIP_TLS_VERIFY=false
VALUES_FILE="${WVA_PROJECT}/charts/workload-variant-autoscaler/values.yaml"

#### REQUIRED FUNCTION used by deploy/install.sh ####
check_specific_prerequisites() {
    log_info "Checking OpenShift-specific prerequisites..."
    
    local missing_tools=()
    
    # Check for required tools (including OpenShift-specific ones)
    for tool in "${REQUIRED_TOOLS[@]}"; do
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
        local ns_exists=false
        local ns_terminating=false

        # Check namespace state
        if kubectl get namespace $ns &> /dev/null; then
            ns_exists=true
            local ns_status=$(kubectl get namespace $ns -o jsonpath='{.status.phase}' 2>/dev/null)
            if [ "$ns_status" = "Terminating" ]; then
                ns_terminating=true
            fi
        fi

        # Handle each case explicitly
        if [ "$ns_exists" = true ] && [ "$ns_terminating" = false ]; then
            # Namespace exists and is active - skip
            log_info "Namespace $ns already exists"
            continue
        elif [ "$ns_terminating" = true ]; then
            # Namespace is terminating - force delete and recreate
            log_info "Namespace $ns is terminating, forcing deletion..."
            kubectl get namespace $ns -o json | \
                jq '.spec.finalizers = []' | \
                kubectl replace --raw "/api/v1/namespaces/$ns/finalize" -f - 2>/dev/null || true
            kubectl wait --for=delete namespace/$ns --timeout=120s 2>/dev/null || true
        fi
        # At this point: namespace doesn't exist OR was terminating and is now deleted
        # Create namespace with OpenShift-specific labels for monitoring
        if [ "$ns" = "$WVA_NS" ]; then
            kubectl create namespace $ns --dry-run=client -o yaml | \
                kubectl label --local -f - openshift.io/user-monitoring=true -o yaml | \
                kubectl apply -f -
        else
            kubectl create namespace $ns
        fi
        log_success "Namespace $ns created"
    done
}

find_thanos_url() {
    log_info "Finding Thanos querier URL..."

    local thanos_svc=$(kubectl get svc -n $PROMETHEUS_SECRET_NS $PROMETHEUS_SVC_NAME -o jsonpath='{.metadata.name}' 2>/dev/null || echo "")

    # Set PROMETHEUS_URL if Thanos service is found
    if [ -n "$thanos_svc" ]; then
        # Extract the actual service name and port from the service
        local svc_name=$(kubectl get svc -n $PROMETHEUS_SECRET_NS $PROMETHEUS_SVC_NAME -o jsonpath='{.metadata.name}' 2>/dev/null)
        local svc_port=$(kubectl get svc -n $PROMETHEUS_SECRET_NS $PROMETHEUS_SVC_NAME -o jsonpath='{.spec.ports[?(@.name=="web")].port}' 2>/dev/null)
        
        # Fallback to default port if not found or try first port
        if [ -z "$svc_port" ]; then
            svc_port=$(kubectl get svc -n $PROMETHEUS_SECRET_NS $PROMETHEUS_SVC_NAME -o jsonpath='{.spec.ports[0].port}' 2>/dev/null)
        fi

        if [ -z "$svc_port" ]; then
            svc_port="9091"
            log_warning "Could not extract port from service, using default: $svc_port"
        fi
        
        # Construct the full URL
        PROMETHEUS_URL="https://${svc_name}.${PROMETHEUS_SECRET_NS}.svc.cluster.local:${svc_port}"
        log_success "Found Thanos querier: $PROMETHEUS_URL (port: $svc_port)"
    else
        log_error "Thanos querier service not found in openshift-monitoring namespace - using default URL: $PROMETHEUS_URL"
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

    # Extract OpenShift Service CA certificate for Thanos verification
    # Note: For OpenShift service certificates, we need the Service CA that signed the server cert,
    # not the server certificate itself. The server cert is in thanos-querier-tls, but we need the CA.
    log_info "Extracting OpenShift Service CA certificate for Thanos verification"
    
    # Method 1: Extract Service CA from openshift-service-ca.crt ConfigMap (preferred)
    # This is the actual CA certificate that signs OpenShift service certificates
    if kubectl get configmap openshift-service-ca.crt -n $PROMETHEUS_SECRET_NS &> /dev/null; then
        log_info "Extracting Service CA from openshift-service-ca.crt ConfigMap"
        kubectl get configmap openshift-service-ca.crt -n $PROMETHEUS_SECRET_NS -o jsonpath='{.data.service-ca\.crt}' > $PROM_CA_CERT_PATH 2>/dev/null || true
        if [ -s "$PROM_CA_CERT_PATH" ]; then
            log_success "Extracted Service CA from openshift-service-ca.crt ConfigMap"
        fi
    fi
    
    # Method 2: Extract Service CA from openshift-config namespace
    if [ ! -s "$PROM_CA_CERT_PATH" ]; then
        log_info "Trying to extract Service CA from openshift-config namespace"
        kubectl get configmap openshift-service-ca -n openshift-config -o jsonpath='{.data.service-ca\.crt}' > $PROM_CA_CERT_PATH 2>/dev/null || true
        if [ -s "$PROM_CA_CERT_PATH" ]; then
            log_success "Extracted Service CA from openshift-config namespace"
        fi
    fi
    
    # Method 3: Fallback to thanos-querier-tls secret (as per Helm README)
    # Note: This extracts the server certificate, which may work if the cert chain includes the CA
    # but it's not ideal - we should use the Service CA instead
    if [ ! -s "$PROM_CA_CERT_PATH" ]; then
        log_warning "Service CA not found, falling back to server certificate from thanos-querier-tls"
        log_warning "This may cause TLS verification issues - Service CA is preferred"
        if kubectl get secret $PROMETHEUS_SECRET_NAME -n $PROMETHEUS_SECRET_NS &> /dev/null; then
            log_info "Extracting certificate from thanos-querier-tls secret (as per Helm README)"
            kubectl get secret $PROMETHEUS_SECRET_NAME -n $PROMETHEUS_SECRET_NS -o jsonpath='{.data.tls\.crt}' | base64 -d > $PROM_CA_CERT_PATH
            if [ -s "$PROM_CA_CERT_PATH" ]; then
                log_success "Extracted certificate from thanos-querier-tls secret"
            fi
        fi
    fi
    
    # Verify we have a valid certificate
    if [ ! -s "$PROM_CA_CERT_PATH" ]; then
        log_error "Failed to extract OpenShift Service CA certificate"
        log_error "Tried: openshift-service-ca.crt ConfigMap, openshift-config ConfigMap, and thanos-querier-tls secret"
        exit 1
    fi
    
    # Verify the certificate is valid PEM format
    if ! openssl x509 -in "$PROM_CA_CERT_PATH" -text -noout &> /dev/null; then
        log_warning "Certificate file may not be in valid PEM format, but continuing..."
        log_warning "If TLS errors occur, verify the certificate format is correct"
    else
        # Log certificate details for debugging
        local cert_subject=$(openssl x509 -in "$PROM_CA_CERT_PATH" -noout -subject 2>/dev/null | sed 's/subject=//' || echo "unknown")
        log_info "Certificate subject: $cert_subject"
    fi

    CHART_VERSION=0.8.0
    log_info "Installing LeaderWorkerSet version $CHART_VERSION into lws-system namespace"
    helm upgrade -i lws oci://registry.k8s.io/lws/charts/lws \
        --version=$CHART_VERSION \
        --namespace lws-system \
        --create-namespace \
        --wait --timeout 300s
    
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

