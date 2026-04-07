#!/usr/bin/env bash
#
# Scaler backend deployment/runtime helpers for deploy/install.sh.
# Requires vars: MONITORING_NAMESPACE, KEDA_NAMESPACE, KEDA_CHART_VERSION,
# PROM_CA_CERT_PATH, PROMETHEUS_BASE_URL, PROMETHEUS_PORT, E2E_TESTS_ENABLED.
# Requires funcs: log_info/log_warning/log_success/log_error,
# should_skip_helm_repo_update(), retry_until_success().
#

# APIService guard: background loop that continuously ensures the
# v1beta1.external.metrics.k8s.io APIService points to prometheus-adapter.
# On clusters with KEDA, the operator continuously reconciles the APIService
# back to keda-metrics-apiserver, breaking HPA scaling for WVA.
# This guard re-patches it every WAIT_INTERVAL_10S seconds without modifying KEDA itself.
APISERVICE_GUARD_PID=""

start_apiservice_guard() {
    local monitoring_ns="$1"
    log_info "Starting APIService guard (background re-patch loop every ${WAIT_INTERVAL_10S}s)"
    (
        while true; do
            sleep "$WAIT_INTERVAL_10S"
            # Exit if cluster is gone (e.g. kind cluster deleted) to avoid spamming the terminal
            if ! kubectl cluster-info &>/dev/null; then
                echo "[apiservice-guard] Cluster unreachable, stopping guard"
                exit 0
            fi
            current_svc=$(kubectl get apiservice "$EXTERNAL_METRICS_APISERVICE_NAME" \
                -o jsonpath='{.spec.service.name}' 2>/dev/null || echo "")
            current_ns=$(kubectl get apiservice "$EXTERNAL_METRICS_APISERVICE_NAME" \
                -o jsonpath='{.spec.service.namespace}' 2>/dev/null || echo "")
            if [ "$current_svc" != "$PROMETHEUS_ADAPTER_SERVICE_NAME" ] || [ "$current_ns" != "$monitoring_ns" ]; then
                echo "[apiservice-guard] KEDA reclaimed APIService (now: $current_svc/$current_ns), re-patching to ${PROMETHEUS_ADAPTER_SERVICE_NAME}/$monitoring_ns"
                kubectl patch apiservice "$EXTERNAL_METRICS_APISERVICE_NAME" --type=merge -p "{
                    \"spec\": {
                        \"insecureSkipTLSVerify\": true,
                        \"service\": {
                            \"name\": \"$PROMETHEUS_ADAPTER_SERVICE_NAME\",
                            \"namespace\": \"$monitoring_ns\"
                        }
                    }
                }" 2>/dev/null || true
            fi
        done
    ) &
    APISERVICE_GUARD_PID=$!
    echo "$APISERVICE_GUARD_PID" > /tmp/apiservice-guard.pid
    log_success "APIService guard started (PID: $APISERVICE_GUARD_PID)"
}

stop_apiservice_guard() {
    if [ -n "$APISERVICE_GUARD_PID" ] && kill -0 "$APISERVICE_GUARD_PID" 2>/dev/null; then
        log_info "Stopping APIService guard (PID: $APISERVICE_GUARD_PID)"
        kill "$APISERVICE_GUARD_PID" 2>/dev/null || true
        wait "$APISERVICE_GUARD_PID" 2>/dev/null || true
    elif [ -f /tmp/apiservice-guard.pid ]; then
        local pid
        pid=$(cat /tmp/apiservice-guard.pid)
        if kill -0 "$pid" 2>/dev/null; then
            log_info "Stopping APIService guard (PID: $pid from pidfile)"
            kill "$pid" 2>/dev/null || true
        fi
    fi
    rm -f /tmp/apiservice-guard.pid
    APISERVICE_GUARD_PID=""
}

deploy_keda() {
    log_info "Deploying KEDA (scaler backend)..."

    # OpenShift: KEDA is cluster-managed (OLM/operator); never Helm-install — avoids
    # ClusterRole/release conflicts with an existing platform KEDA.
    if [ "$ENVIRONMENT" = "openshift" ]; then
        log_info "OpenShift: assuming platform-managed KEDA — skipping Helm install"
        if kubectl get crd scaledobjects.keda.sh >/dev/null 2>&1; then
            log_success "KEDA ScaledObject CRD is available on the cluster"
        else
            if [ "$E2E_TESTS_ENABLED" = "true" ]; then
                log_error "OpenShift: scaledobjects.keda.sh CRD not found — install cluster KEDA before E2E (SCALER_BACKEND=keda)"
                exit 1
            fi
            log_warning "KEDA ScaledObject CRD not found — ScaledObject-based scaling will not work"
        fi
        return
    fi

    # Kubernetes (e.g. CKS, shared clusters): assume cluster-managed KEDA; never Helm unless opted in.
    if [ "$ENVIRONMENT" = "kubernetes" ] && [ "${KEDA_HELM_INSTALL:-false}" != "true" ]; then
        log_info "Kubernetes: assuming cluster-managed KEDA — skipping Helm (set KEDA_HELM_INSTALL=true to install via Helm)"
        if kubectl get crd scaledobjects.keda.sh >/dev/null 2>&1; then
            log_success "KEDA ScaledObject CRD is available on the cluster"
        else
            if [ "$E2E_TESTS_ENABLED" = "true" ]; then
                log_error "Kubernetes: scaledobjects.keda.sh CRD not found — install KEDA on the cluster or set KEDA_HELM_INSTALL=true"
                exit 1
            fi
            log_warning "KEDA ScaledObject CRD not found — ScaledObject-based scaling will not work"
        fi
        return
    fi

    # Skip install if KEDA is already fully operational on the cluster.
    # Check CRD + operator pods + external metrics APIService to avoid false positives
    # from stale CRDs left behind after a prior uninstall.
    if kubectl get crd scaledobjects.keda.sh >/dev/null 2>&1; then
        if kubectl get pods -A -l "$KEDA_OPERATOR_LABEL_SELECTOR" 2>/dev/null | grep -q Running; then
            if kubectl get apiservice "$EXTERNAL_METRICS_APISERVICE_NAME" >/dev/null 2>&1; then
                log_success "KEDA CRD, operator, and metrics APIService detected — skipping helm install"
                return
            fi
        fi
        # Shared clusters (e.g. CKS) often pre-install KEDA without the exact pod label / APIService
        # shape our probe expects, but ClusterRole keda-operator already exists without Helm metadata.
        # Helm install then fails with ownership errors — skip Helm when that pattern is present.
        if kubectl get clusterrole keda-operator >/dev/null 2>&1; then
            keda_cr_managed_by=$(kubectl get clusterrole keda-operator -o jsonpath='{.metadata.labels.app\.kubernetes\.io/managed-by}' 2>/dev/null || true)
            if [ "$keda_cr_managed_by" != "Helm" ]; then
                log_info "KEDA CRD present and ClusterRole keda-operator is not Helm-managed — skipping Helm install (pre-installed KEDA)"
                return
            fi
        fi
        log_warning "KEDA ScaledObject CRD found but operator or metrics APIService not detected; proceeding with helm install"
    fi

    kubectl create namespace "$KEDA_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

    helm repo add kedacore https://kedacore.github.io/charts 2>/dev/null || true
    if [ "$(should_skip_helm_repo_update)" = "true" ]; then
        log_info "Skipping helm repo update for KEDA (SKIP_HELM_REPO_UPDATE=true)"
    else
        helm repo update
    fi

    if ! helm upgrade -i "$KEDA_RELEASE_NAME" kedacore/keda \
        --version "$KEDA_CHART_VERSION" \
        -n "$KEDA_NAMESPACE" \
        --set prometheus.metricServer.enabled=true \
        --set prometheus.operator.enabled=true \
        --wait \
        --timeout=5m; then
        if [ "$E2E_TESTS_ENABLED" = "true" ]; then
            log_error "KEDA Helm installation failed - required for E2E tests with SCALER_BACKEND=keda"
        else
            log_warning "KEDA Helm installation failed, but continuing..."
        fi
    else
        log_success "KEDA deployed in $KEDA_NAMESPACE"
    fi
}

deploy_prometheus_adapter() {
    log_info "Deploying Prometheus Adapter..."

    # Add Prometheus community helm repo
    log_info "Adding Prometheus community helm repo"
    helm repo add prometheus-community https://prometheus-community.github.io/helm-charts || true
    if [ "$(should_skip_helm_repo_update)" = "true" ]; then
        log_info "Skipping helm repo update for Prometheus Adapter (SKIP_HELM_REPO_UPDATE=true)"
    else
        helm repo update
    fi

    # Create prometheus-ca ConfigMap from the CA certificate
    log_info "Creating prometheus-ca ConfigMap for Prometheus Adapter"
    if [ ! -f "$PROM_CA_CERT_PATH" ] || [ ! -s "$PROM_CA_CERT_PATH" ]; then
        log_error "CA certificate file not found or empty: $PROM_CA_CERT_PATH"
        log_error "Please ensure deploy_wva_prerequisites() was called first"
        exit 1
    fi

    # Create or update the prometheus-ca ConfigMap
    kubectl create configmap "$PROMETHEUS_CA_CONFIGMAP_NAME" \
        --from-file="ca.crt=$PROM_CA_CERT_PATH" \
        -n "$MONITORING_NAMESPACE" \
        --dry-run=client -o yaml | kubectl apply -f -

    log_success "prometheus-ca ConfigMap created/updated"

    # Use existing values files from config/samples
    local values_file=""
    if [ "$ENVIRONMENT" = "openshift" ]; then
        values_file="${WVA_PROJECT}/config/samples/prometheus-adapter-values-ocp.yaml"
        log_info "Using OpenShift-specific Prometheus Adapter configuration: $values_file"
    else
        values_file="${WVA_PROJECT}/config/samples/prometheus-adapter-values.yaml"
        log_info "Using Kubernetes Prometheus Adapter configuration: $values_file"
    fi

    if [ ! -f "$values_file" ]; then
        log_error "Prometheus Adapter values file not found: $values_file"
        exit 1
    fi

    # Deploy Prometheus Adapter using existing values file and override URL/port
    log_info "Installing Prometheus Adapter via Helm"

    # In CI/E2E mode, skip --wait to avoid hanging, then verify separately
    # For local dev, use --wait for immediate feedback
    local wait_flag=""
    if [ "${PROMETHEUS_ADAPTER_WAIT:-true}" = "true" ]; then
        wait_flag="--wait"
        log_info "Using --wait flag (will wait for Prometheus Adapter to be ready)"
    else
        log_info "Skipping --wait flag (will verify status separately)"
    fi

    if ! helm upgrade -i "$PROMETHEUS_ADAPTER_RELEASE_NAME" prometheus-community/prometheus-adapter \
        -n "$MONITORING_NAMESPACE" \
        -f "$values_file" \
        --set prometheus.url="$PROMETHEUS_BASE_URL" \
        --set prometheus.port="$PROMETHEUS_PORT" \
        --timeout=3m \
        $wait_flag; then
        if [ "$E2E_TESTS_ENABLED" = "true" ]; then
            log_error "Prometheus Adapter Helm installation failed - required for E2E tests"
        else
            log_warning "Prometheus Adapter Helm installation failed, but continuing..."
            log_warning "HPA may not work until adapter is healthy"
            log_info "Check adapter status: kubectl get pods -n \"$MONITORING_NAMESPACE\" | grep prometheus-adapter"
            log_info "Check adapter logs: kubectl logs -n \"$MONITORING_NAMESPACE\" deployment/prometheus-adapter"
        fi
    fi

    # If we skipped --wait (e.g., in CI), verify Prometheus Adapter is actually running
    if [ "${PROMETHEUS_ADAPTER_WAIT:-true}" != "true" ]; then
        log_info "Verifying Prometheus Adapter is running (skipped --wait, checking status)..."
        local max_attempts=12
        local adapter_ready=false
        if retry_until_success "$max_attempts" "$WAIT_INTERVAL_10S" "Prometheus Adapter" \
            bash -c "kubectl get pods -n \"$MONITORING_NAMESPACE\" -l \"$PROMETHEUS_ADAPTER_LABEL_SELECTOR\" 2>/dev/null | grep -q Running"; then
            adapter_ready=true
        fi

        if [ "$adapter_ready" = "true" ]; then
            log_success "Prometheus Adapter is running"
        else
            if [ "$E2E_TESTS_ENABLED" = "true" ]; then
                log_error "Prometheus Adapter failed to become ready after ${max_attempts} attempts - required for E2E tests"
            else
                log_warning "Prometheus Adapter may still be starting (not ready after ${max_attempts} attempts)"
                log_info "Check adapter status: kubectl get pods -n \"$MONITORING_NAMESPACE\" | grep prometheus-adapter"
            fi
        fi
    else
        log_success "Prometheus Adapter deployment completed"
    fi

    # On clusters with KEDA, the v1beta1.external.metrics.k8s.io APIService may
    # point to KEDA's metrics server instead of Prometheus Adapter. KEDA's server
    # only serves metrics for ScaledObjects, not arbitrary external metrics like
    # wva_desired_replicas. Detect and fix this conflict.
    # Only patch if the Prometheus Adapter service actually exists (i.e. helm install succeeded).
    if ! kubectl get service "$PROMETHEUS_ADAPTER_SERVICE_NAME" -n "$MONITORING_NAMESPACE" &>/dev/null; then
        log_warning "Prometheus Adapter service not found in $MONITORING_NAMESPACE — skipping APIService patch"
        log_warning "HPA may not work until Prometheus Adapter is deployed"
    elif kubectl get apiservice "$EXTERNAL_METRICS_APISERVICE_NAME" &>/dev/null; then
        local current_svc current_ns
        current_svc=$(kubectl get apiservice "$EXTERNAL_METRICS_APISERVICE_NAME" -o jsonpath='{.spec.service.name}' 2>/dev/null || echo "")
        current_ns=$(kubectl get apiservice "$EXTERNAL_METRICS_APISERVICE_NAME" -o jsonpath='{.spec.service.namespace}' 2>/dev/null || echo "")

        if [ "$current_svc" = "$PROMETHEUS_ADAPTER_SERVICE_NAME" ] && [ "$current_ns" = "$MONITORING_NAMESPACE" ]; then
            log_info "external.metrics.k8s.io APIService already points to $PROMETHEUS_ADAPTER_SERVICE_NAME in $MONITORING_NAMESPACE"
        else
            log_warning "external.metrics.k8s.io APIService points to '$current_svc' in '$current_ns'"
            log_info "Patching APIService to point to Prometheus Adapter in $MONITORING_NAMESPACE"
            kubectl patch apiservice "$EXTERNAL_METRICS_APISERVICE_NAME" --type=merge -p "{
                \"spec\": {
                    \"insecureSkipTLSVerify\": true,
                    \"service\": {
                        \"name\": \"$PROMETHEUS_ADAPTER_SERVICE_NAME\",
                        \"namespace\": \"$MONITORING_NAMESPACE\"
                    }
                }
            }" && log_success "APIService patched to use Prometheus Adapter" \
               || log_warning "Failed to patch external.metrics.k8s.io APIService — HPA may not work"
        fi

        # Start background guard to prevent KEDA from reclaiming the APIService.
        # KEDA's operator continuously reconciles the APIService back to its own
        # metrics server within ~2 minutes of any patch. The guard re-patches it
        # every 10 seconds without modifying KEDA itself.
        start_apiservice_guard "$MONITORING_NAMESPACE"
    else
        log_warning "external.metrics.k8s.io APIService not found — skipping patch"
    fi
}
