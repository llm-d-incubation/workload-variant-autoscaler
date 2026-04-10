#!/usr/bin/env bash
#
# Shared WVA-specific deployment helpers.
# Requires vars: WVA_NS, LLMD_NS, MONITORING_NAMESPACE, WVA_PROJECT,
# chart/image values, env mode lists.
# Requires funcs: log_info/log_warning/log_success/log_error, containsElement().
#

set_tls_verification() {
    log_info "Setting TLS verification..."

    # Auto-detect TLS verification setting if not specified
    if ! containsElement "$ENVIRONMENT" "${NON_EMULATED_ENV_LIST[@]}"; then
            SKIP_TLS_VERIFY="true"
            log_info "Emulated environment detected - enabling TLS skip verification for self-signed certificates"
    else
        case "$ENVIRONMENT" in
            "kubernetes")
                # TODO: change to false when Kubernetes support for TLS verification is enabled
                SKIP_TLS_VERIFY="true"
                log_info "Kubernetes cluster - enabling TLS skip verification for self-signed certificates"
                ;;
            "openshift")
                # For OpenShift, we can use proper TLS verification since we have the Service CA
                # However, defaulting to true for now to match current behavior
                # TODO: Set to false once Service CA certificate extraction is fully validated
                SKIP_TLS_VERIFY="true"
                log_info "OpenShift cluster - TLS verification setting: $SKIP_TLS_VERIFY"
                ;;
            *)
                SKIP_TLS_VERIFY="true"
                log_warning "Unknown environment - enabling TLS skip verification for self-signed certificates"
                ;;
        esac
    fi

    export SKIP_TLS_VERIFY

    log_success "Successfully set TLS verification to: $SKIP_TLS_VERIFY"
}

set_wva_logging_level() {
    log_info "Setting WVA logging level..."

    # Set logging level based on environment
    if ! containsElement "$ENVIRONMENT" "${NON_EMULATED_ENV_LIST[@]}"; then
        WVA_LOG_LEVEL="debug"
        log_info "Development environment - using debug logging"
    else
        WVA_LOG_LEVEL="info"
        log_info "Production environment - using info logging"
    fi

    export WVA_LOG_LEVEL
    log_success "WVA logging level set to: $WVA_LOG_LEVEL"
    echo ""
}

deploy_wva_controller() {
    log_info "Deploying Workload-Variant-Autoscaler..."
    log_info "Using image: $WVA_IMAGE_REPO:$WVA_IMAGE_TAG"
    log_info "Using release name: $WVA_RELEASE_NAME"

    # Deploy WVA using Helm chart
    log_info "Installing Workload-Variant-Autoscaler via Helm chart"

    # Default namespaceScoped to true if not set (matches chart default)
    # But allow override via env var (e.g. for E2E tests)
    NAMESPACE_SCOPED=${NAMESPACE_SCOPED:-true}

    helm upgrade -i "$WVA_RELEASE_NAME" ${WVA_PROJECT}/charts/workload-variant-autoscaler \
        -n "$WVA_NS" \
        --values $VALUES_FILE \
        --set-file wva.prometheus.caCert="$PROM_CA_CERT_PATH" \
        --set wva.image.repository="$WVA_IMAGE_REPO" \
        --set wva.image.tag="$WVA_IMAGE_TAG" \
        --set wva.imagePullPolicy="$WVA_IMAGE_PULL_POLICY" \
        --set wva.baseName="$WELL_LIT_PATH_NAME" \
        --set llmd.modelName="$LLM_D_MODELSERVICE_NAME" \
        --set va.enabled="$DEPLOY_VA" \
        --set va.accelerator="$ACCELERATOR_TYPE" \
        --set llmd.modelID="$MODEL_ID" \
        --set va.sloTpot="$SLO_TPOT" \
        --set va.sloTtft="$SLO_TTFT" \
        --set hpa.enabled="$DEPLOY_HPA" \
        --set hpa.minReplicas="$HPA_MIN_REPLICAS" \
        --set hpa.behavior.scaleUp.stabilizationWindowSeconds="$HPA_STABILIZATION_SECONDS" \
        --set hpa.behavior.scaleDown.stabilizationWindowSeconds="$HPA_STABILIZATION_SECONDS" \
        --set llmd.namespace="$LLMD_NS" \
        --set wva.prometheus.baseURL="$PROMETHEUS_URL" \
        --set wva.prometheus.monitoringNamespace="$MONITORING_NAMESPACE" \
        --set vllmService.enabled="$VLLM_SVC_ENABLED" \
        --set vllmService.port="$VLLM_SVC_PORT" \
        --set vllmService.targetPort="$VLLM_SVC_PORT" \
        --set vllmService.nodePort="$VLLM_SVC_NODEPORT" \
        --set wva.logging.level="$WVA_LOG_LEVEL" \
        --set wva.prometheus.tls.insecureSkipVerify="$SKIP_TLS_VERIFY" \
        --set wva.namespaceScoped="$NAMESPACE_SCOPED" \
        --set wva.metrics.secure="$WVA_METRICS_SECURE" \
        --set wva.scaleToZero="$ENABLE_SCALE_TO_ZERO" \
        ${CONTROLLER_INSTANCE:+--set wva.controllerInstance=$CONTROLLER_INSTANCE} \
        ${POOL_GROUP:+--set wva.poolGroup=$POOL_GROUP} \
        ${KV_CACHE_THRESHOLD:+--set wva.capacityScaling.default.kvCacheThreshold=$KV_CACHE_THRESHOLD} \
        ${QUEUE_LENGTH_THRESHOLD:+--set wva.capacityScaling.default.queueLengthThreshold=$QUEUE_LENGTH_THRESHOLD} \
        ${KV_SPARE_TRIGGER:+--set wva.capacityScaling.default.kvSpareTrigger=$KV_SPARE_TRIGGER} \
        ${QUEUE_SPARE_TRIGGER:+--set wva.capacityScaling.default.queueSpareTrigger=$QUEUE_SPARE_TRIGGER}

    # Wait for WVA to be ready
    log_info "Waiting for WVA controller to be ready..."
    if kubectl wait --for=condition=Ready pod -l "$WVA_CONTROLLER_LABEL_SELECTOR" -n "$WVA_NS" --timeout=60s; then
        :
    else
        log_warning "WVA controller is not ready yet - check 'kubectl get pods -n $WVA_NS'"
    fi

    log_success "WVA deployment complete"
}

# Shared namespace creation loop for deploy/*/install.sh environment plugins.
# Platform adapter provides materialize_namespace(ns), then calls this helper.
create_namespaces_shared_loop() {
    log_info "Creating namespaces..."

    for ns in $WVA_NS $MONITORING_NAMESPACE $LLMD_NS; do
        local ns_exists=false
        local ns_terminating=false

        if kubectl get namespace $ns &> /dev/null; then
            ns_exists=true
            local ns_status
            ns_status=$(kubectl get namespace $ns -o jsonpath='{.status.phase}' 2>/dev/null)
            if [ "$ns_status" = "Terminating" ]; then
                ns_terminating=true
            fi
        fi

        if [ "$ns_exists" = true ] && [ "$ns_terminating" = false ]; then
            log_info "Namespace $ns already exists"
            continue
        elif [ "$ns_terminating" = true ]; then
            log_info "Namespace $ns is terminating, forcing deletion..."
            kubectl get namespace $ns -o json | \
                jq '.spec.finalizers = []' | \
                kubectl replace --raw "/api/v1/namespaces/$ns/finalize" -f - 2>/dev/null || true
            kubectl wait --for=delete namespace/$ns --timeout=120s 2>/dev/null || true
        fi

        materialize_namespace "$ns"
        log_success "Namespace $ns created"
    done
}

delete_namespaces_kube_like() {
    log_info "Deleting namespaces..."

    for ns in $LLMD_NS $WVA_NS $MONITORING_NAMESPACE; do
        if kubectl get namespace $ns &> /dev/null; then
            if [[ "$ns" == "$LLMD_NS" && "$DEPLOY_LLM_D" == "false" ]] || [[ "$ns" == "$WVA_NS" && "$DEPLOY_WVA" == "false" ]] || [[ "$ns" == "$MONITORING_NAMESPACE" && "$DEPLOY_PROMETHEUS" == "false" ]]; then
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

# Shared WVA prerequisites for Kubernetes-like environments.
# Optional:
#   - KUBE_LIKE_VALUES_DEV_IF_PRESENT=true|false (defaults false)
deploy_wva_prerequisites_kube_like() {
    log_info "Deploying Workload-Variant-Autoscaler prerequisites for Kubernetes..."

    # Extract Prometheus CA certificate
    log_info "Extracting Prometheus TLS certificate"
    kubectl get secret "$PROMETHEUS_SECRET_NAME" -n "$MONITORING_NAMESPACE" -o jsonpath='{.data.tls\.crt}' | base64 -d > "$PROM_CA_CERT_PATH"

    local use_values_dev=false
    if [ "$SKIP_TLS_VERIFY" = "true" ]; then
        use_values_dev=true
    elif [ "${KUBE_LIKE_VALUES_DEV_IF_PRESENT:-false}" = "true" ] && [ -f "$WVA_PROJECT/charts/workload-variant-autoscaler/values-dev.yaml" ]; then
        use_values_dev=true
    fi

    if [ "$use_values_dev" = "true" ]; then
        log_warning "TLS verification NOT enabled: using values-dev.yaml for dev deployments - NOT FOR PRODUCTION USE"
        VALUES_FILE="${WVA_PROJECT}/charts/workload-variant-autoscaler/values-dev.yaml"
    else
        log_info "TLS verification enabled: using values.yaml for production deployments"
        VALUES_FILE="${WVA_PROJECT}/charts/workload-variant-autoscaler/values.yaml"
    fi

    # LeaderWorkerSet (WVA dependency; see upstream chart / #910).
    CHART_VERSION=0.8.0
    log_info "Installing LeaderWorkerSet version $CHART_VERSION into lws-system namespace"
    helm upgrade -i lws oci://registry.k8s.io/lws/charts/lws \
        --version="$CHART_VERSION" \
        --namespace lws-system \
        --create-namespace \
        --wait --timeout 300s

    log_success "WVA prerequisites complete"
}

# OpenShift-specific CA extraction used by deploy/openshift/install.sh.
extract_openshift_prometheus_ca() {
    # Extract OpenShift Service CA certificate for Thanos verification
    # Note: For OpenShift service certificates, we need the Service CA that signed the server cert,
    # not the server certificate itself. The server cert is in thanos-querier-tls, but we need the CA.
    log_info "Extracting OpenShift Service CA certificate for Thanos verification"

    # Method 1: Extract Service CA from openshift-service-ca.crt ConfigMap (preferred)
    # This is the actual CA certificate that signs OpenShift service certificates
    if kubectl get configmap openshift-service-ca.crt -n "$PROMETHEUS_SECRET_NS" &> /dev/null; then
        log_info "Extracting Service CA from openshift-service-ca.crt ConfigMap"
        kubectl get configmap openshift-service-ca.crt -n "$PROMETHEUS_SECRET_NS" -o jsonpath='{.data.service-ca\.crt}' > "$PROM_CA_CERT_PATH" 2>/dev/null || true
        if [ -s "$PROM_CA_CERT_PATH" ]; then
            log_success "Extracted Service CA from openshift-service-ca.crt ConfigMap"
        fi
    fi

    # Method 2: Extract Service CA from openshift-config namespace
    if [ ! -s "$PROM_CA_CERT_PATH" ]; then
        log_info "Trying to extract Service CA from openshift-config namespace"
        kubectl get configmap openshift-service-ca -n openshift-config -o jsonpath='{.data.service-ca\.crt}' > "$PROM_CA_CERT_PATH" 2>/dev/null || true
        if [ -s "$PROM_CA_CERT_PATH" ]; then
            log_success "Extracted Service CA from openshift-config namespace"
        fi
    fi

    # Method 3: Fallback to thanos-querier-tls secret (as per Helm README)
    # Note: This extracts the server certificate, which may work if the cert chain includes the CA
    # but it's not ideal - we should use the Service CA instead.
    if [ ! -s "$PROM_CA_CERT_PATH" ]; then
        log_warning "Service CA not found, falling back to server certificate from thanos-querier-tls"
        log_warning "This may cause TLS verification issues - Service CA is preferred"
        if kubectl get secret "$PROMETHEUS_SECRET_NAME" -n "$PROMETHEUS_SECRET_NS" &> /dev/null; then
            log_info "Extracting certificate from thanos-querier-tls secret (as per Helm README)"
            kubectl get secret "$PROMETHEUS_SECRET_NAME" -n "$PROMETHEUS_SECRET_NS" -o jsonpath='{.data.tls\.crt}' | base64 -d > "$PROM_CA_CERT_PATH"
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
        local cert_subject
        cert_subject=$(openssl x509 -in "$PROM_CA_CERT_PATH" -noout -subject 2>/dev/null | sed 's/subject=//' || echo "unknown")
        log_info "Certificate subject: $cert_subject"
    fi
}
