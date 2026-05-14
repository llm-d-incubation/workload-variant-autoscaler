#!/usr/bin/env bash
#
# Build and apply WVA controller manifests via Kustomize (replaces Helm for the controller).
# Requires deploy script variables: WVA_PROJECT, WVA_NS, ENVIRONMENT, WVA_IMAGE_REPO, WVA_IMAGE_TAG,
# WVA_IMAGE_PULL_POLICY, NAMESPACE_SCOPED, PROMETHEUS_URL, MONITORING_NAMESPACE, SKIP_TLS_VERIFY,
# ENABLE_SCALE_TO_ZERO, WVA_LOG_LEVEL, CONTROLLER_INSTANCE, POOL_GROUP, WVA_WATCH_TARGET_NS (optional),
# PROM_CA_CERT_PATH, VLLM_SVC_ENABLED, LLMD_NS, LLM_D_MODELSERVICE_NAME, VLLM_SVC_PORT, WVA_RELEASE_NAME.
# jq must be on PATH (JSON strategic-merge patch for the manager Deployment + JSON6902 for EPP metrics token Secret).
# Optional WVA_SATURATION_OVERLAY: name under config/deploy/overlays/<name>/{kubernetes,openshift}/ that
# composes the base controller bundle plus checked-in saturation patches (e.g. e2e-simulator, benchmark).
# Unset or "default" uses config/deploy/wva-controller[-openshift] only.
# Optional: WVA_KUSTOMIZE_TMP (caller-provided empty dir); otherwise mktemp is used.
#

wva_resource_prefix() {
    local chart_name="workload-variant-autoscaler"
    local r="${WVA_RELEASE_NAME:-workload-variant-autoscaler}"
    if echo "$r" | grep -q "$chart_name"; then
        echo "$r"
    else
        echo "${r}-${chart_name}"
    fi | cut -c1-63 | sed 's/-$//'
}

# Prints "openshift" or "kubernetes" for config/deploy paths (ENVIRONMENT anything other than openshift → kubernetes).
wva_kustomize_platform_slug() {
    case "${ENVIRONMENT:-kubernetes}" in
    openshift) printf '%s\n' openshift ;;
    *) printf '%s\n' kubernetes ;;
    esac
}

wva_kustomize_platform_dir() {
    printf '%s\n' "${WVA_PROJECT}/config/deploy/$(wva_kustomize_platform_slug)"
}

# Controller RBAC + workload only (no CRDs). Used for apply/delete overlays so uninstall never
# removes cluster CRDs, and so temp overlays can add nameSuffix without touching API definitions.
wva_kustomize_controller_bundle_dir() {
    local overlay="${WVA_SATURATION_OVERLAY:-}"
    local platform
    platform=$(wva_kustomize_platform_slug)

    if [ -n "$overlay" ] && [ "$overlay" != "default" ]; then
        local d="${WVA_PROJECT}/config/deploy/overlays/${overlay}/${platform}"
        if [ -f "${d}/kustomization.yaml" ]; then
            printf '%s\n' "$d"
            return 0
        fi
        if declare -F log_warning >/dev/null 2>&1; then
            log_warning "WVA_SATURATION_OVERLAY=${overlay} has no kustomization at ${d}; using base controller bundle"
        fi
    fi

    case "$platform" in
    openshift) printf '%s\n' "${WVA_PROJECT}/config/deploy/wva-controller-openshift" ;;
    *) printf '%s\n' "${WVA_PROJECT}/config/deploy/wva-controller" ;;
    esac
}

# Sanitized suffix token for multi-instance installs (e.g. secondary controller in e2e).
# Prints "-ci-<slug>" or nothing.
# Slug length 24: prefixed metrics Service name is 35 chars (workload-variant-autoscaler-cm-mon);
# Kubernetes Service metadata.name must be <=63, so suffix max is 28 (= 4 for "-ci-" + 24 slug).
wva_kustomize_ci_suffix() {
    local raw ci
    raw="${CONTROLLER_INSTANCE:-}"
    raw=$(echo "$raw" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9-' '-')
    raw=$(echo "$raw" | sed 's/-\{2,\}/-/g;s/^-//;s/-$//')
    ci=$(echo "$raw" | cut -c1-24)
    if [ -z "$ci" ]; then
        return 0
    fi
    printf '%s' "-ci-${ci}"
}

# Controller Deployment metadata.name after config/default namePrefix and optional wva_kustomize_apply nameSuffix.
wva_kustomize_manager_deployment_name() {
    printf '%s%s' 'workload-variant-autoscaler-controller-manager' "$(wva_kustomize_ci_suffix)"
}

# Saturation ConfigMap metadata.name (namePrefix + config/manager/configmap-saturation-scaling.yaml + nameSuffix).
wva_kustomize_saturation_configmap_name() {
    printf '%s%s' 'workload-variant-autoscaler-saturation-scaling-config' "$(wva_kustomize_ci_suffix)"
}

# When set, suffix cluster-scoped RBAC etc. so a second controller install does not overwrite the primary's bindings.
wva_kustomize_namesuffix_yaml() {
    local s
    s=$(wva_kustomize_ci_suffix)
    if [ -z "$s" ]; then
        return 0
    fi
    printf '%s\n' "nameSuffix: ${s}"
}

# Writes prometheus-client-cert Secret in WVA_NS from PEM file at PROM_CA_CERT_PATH (skips if empty or missing).
wva_apply_prometheus_ca_secret() {
    if [ ! -s "${PROM_CA_CERT_PATH:-}" ]; then
        return 0
    fi
    kubectl -n "$WVA_NS" create secret generic prometheus-client-cert \
        --from-file=ca.crt="$PROM_CA_CERT_PATH" \
        --dry-run=client -o yaml | kubectl apply -f -
}

# Renders vLLM metrics Service + ServiceMonitor into LLMD_NS (chart parity).
wva_apply_vllm_metrics_manifests() {
    if [ "${VLLM_SVC_ENABLED:-false}" != "true" ]; then
        return 0
    fi
    local template="${WVA_PROJECT}/deploy/manifests/wva-vllm-metrics.yaml.in"
    if [ ! -f "$template" ]; then
        log_error "Missing vLLM metrics template: $template"
        return 1
    fi
    local prefix model port
    prefix=$(wva_resource_prefix)
    model="${LLM_D_MODELSERVICE_NAME:-}"
    if [ -z "$model" ]; then
        log_warning "LLM_D_MODELSERVICE_NAME unset; skipping vLLM metrics Service/ServiceMonitor"
        return 0
    fi
    port="${VLLM_SVC_PORT:-8200}"
    sed -e "s/__WVA_SVC_PREFIX__/${prefix}/g" \
        -e "s/__LLMD_NS__/${LLMD_NS}/g" \
        -e "s/__MODEL_NAME__/${model}/g" \
        -e "s/__PORT__/${port}/g" \
        "$template" | kubectl apply -f -
}

wva_kustomize_require_jq() {
    if ! command -v jq &>/dev/null; then
        log_error "jq is required to render WVA Kustomize patches (install jq, or use a deploy image that includes it)."
        return 1
    fi
}

# Symlink controller bundle and write kustomization header (resources + namespace + optional nameSuffix).
# Apply flows append a "patches:" block afterward; delete uses this as the complete kustomization.
wva_kustomize_runtime_overlay_base() {
    local tmp=$1 ns=$2 ctrl_bundle=$3 sfx_line=$4
    ln -sfn "${ctrl_bundle}" "${tmp}/bundle"
    cat >"${tmp}/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

resources:
- bundle

namespace: ${ns}
${sfx_line}
EOF
}

# Strategic-merge patch for the manager Deployment (JSON so POOL_GROUP / CONTROLLER_INSTANCE need no manual YAML escaping).
wva_kustomize_write_patch_deployment_json() {
    local tmp=$1 ns=$2 ci_suffix_token=$3
    wva_kustomize_require_jq || return 1

    local watch_type watch_mode="${NAMESPACE_SCOPED:-true}"
    if [ -n "${WVA_WATCH_TARGET_NS:-}" ]; then
        watch_type=1
    elif [ "$watch_mode" = "true" ] || [ "$watch_mode" = "True" ]; then
        watch_type=0
    else
        watch_type=2
    fi

    local extra_env_json='[]'
    if [ -n "$ci_suffix_token" ]; then
        extra_env_json=$(jq -n \
            --arg v "workload-variant-autoscaler-wva-variantautoscaling-config${ci_suffix_token}" \
            --arg s "$(wva_kustomize_saturation_configmap_name)" \
            '[{name:"CONFIG_MAP_NAME",value:$v},{name:"SATURATION_CONFIG_MAP_NAME",value:$s}]')
    fi

    local watch_env_json
    case "$watch_type" in
    1) watch_env_json=$(jq -n --arg v "${WVA_WATCH_TARGET_NS}" '[{name:"WATCH_NAMESPACE",value:$v}]') ;;
    0) watch_env_json='[{"name":"WATCH_NAMESPACE","valueFrom":{"fieldRef":{"fieldPath":"metadata.namespace"}}}]' ;;
    *) watch_env_json='[{"name":"WATCH_NAMESPACE","value":""}]' ;;
    esac

    jq -n \
        --arg ns "$ns" \
        --arg img "${WVA_IMAGE_REPO}:${WVA_IMAGE_TAG}" \
        --arg ipp "${WVA_IMAGE_PULL_POLICY:-Always}" \
        --arg pool "${POOL_GROUP:-}" \
        --arg ci "${CONTROLLER_INSTANCE:-}" \
        --arg ll "${WVA_LOG_LEVEL:-info}" \
        --arg ms "${WVA_METRICS_SECURE:-true}" \
        --argjson watch_env "$watch_env_json" \
        --argjson extra_env "$extra_env_json" '
      ($watch_env + [
        {name: "POOL_GROUP", value: $pool},
        {name: "CONTROLLER_INSTANCE", value: $ci},
        {name: "LOG_LEVEL", value: $ll},
        {name: "METRICS_SECURE", value: $ms}
      ] + $extra_env) as $env |
      {
        apiVersion: "apps/v1",
        kind: "Deployment",
        metadata: {name: "workload-variant-autoscaler-controller-manager", namespace: $ns},
        spec: {
          template: {
            spec: {
              containers: [{
                name: "manager",
                image: $img,
                imagePullPolicy: $ipp,
                env: $env
              }]
            }
          }
        }
      }' >"${tmp}/patch-deployment.json"
}

# Builds a temp overlay with absolute resource path + patches, then kubectl apply -k.
wva_kustomize_apply() {
    local ctrl_bundle
    ctrl_bundle=$(wva_kustomize_controller_bundle_dir)
    if [ ! -f "${ctrl_bundle}/kustomization.yaml" ]; then
        log_error "Missing Kustomize controller bundle: ${ctrl_bundle}/kustomization.yaml"
        return 1
    fi

    local tmp="${WVA_KUSTOMIZE_TMP:-}"
    if [ -z "$tmp" ]; then
        tmp=$(mktemp -d "${TMPDIR:-/tmp}/wva-kustomize.XXXXXX")
    fi

    local ns="${WVA_NS:?}"

    # --- patch: ConfigMap runtime keys (Prometheus URL, TLS skip, scale-to-zero, optional CA path on kube)
    local ca_cm_path=""
    if [ -s "${PROM_CA_CERT_PATH:-}" ] && [ "${SKIP_TLS_VERIFY:-false}" != "true" ]; then
        ca_cm_path="/etc/prometheus-certs/ca.crt"
    fi
    local kubectl_cm_args=(
        kubectl create configmap workload-variant-autoscaler-wva-variantautoscaling-config
        --namespace="$ns"
        --from-literal=PROMETHEUS_BASE_URL="${PROMETHEUS_URL:-}"
        --from-literal=PROMETHEUS_TLS_INSECURE_SKIP_VERIFY="${SKIP_TLS_VERIFY:-false}"
        --from-literal=WVA_SCALE_TO_ZERO="${ENABLE_SCALE_TO_ZERO:-false}"
        --dry-run=client
        -o yaml
    )
    if [ -n "$ca_cm_path" ]; then
        kubectl_cm_args+=(--from-literal=PROMETHEUS_CA_CERT_PATH="${ca_cm_path}")
    fi
    "${kubectl_cm_args[@]}" >"${tmp}/patch-configmap.yaml"

    # --- patch: Deployment image + pull policy + env (JSON strategic merge via jq — see wva_kustomize_write_patch_deployment_json).
    # The bundle already resolves config/manager's `images` (controller -> ghcr.io/...:v0.5.1), so a
    # top-level `images: name: controller` entry in this overlay does not match and would not update
    # the workload image. Pin the image here so WVA_IMAGE_REPO/TAG always apply.
    local sfx_line ci_suffix_token
    sfx_line=$(wva_kustomize_namesuffix_yaml)

    ci_suffix_token=$(wva_kustomize_ci_suffix)
    # nameSuffix runs after replacements in the base bundle; the epp-metrics-token Secret annotation
    # would otherwise keep the unsuffixed SA name and the token controller never populates the Secret
    # (pods stuck ContainerCreating on the epp-metrics-token volume).
    local extra_patches=""
    if [ -n "$ci_suffix_token" ]; then
        # nameSuffix renames ConfigMaps; kustomize updates configMapKeyRef names but not these literals in manager.yaml.
        local epp_sa_name="workload-variant-autoscaler-epp-metrics-reader${ci_suffix_token}"
        wva_kustomize_require_jq || return 1
        jq -n --arg sa "$epp_sa_name" \
            '[{"op":"replace","path":"/metadata/annotations/kubernetes.io~1service-account.name","value":$sa}]' \
            >"${tmp}/patch-epp-metrics-token-sa.json"
        extra_patches="- path: patch-epp-metrics-token-sa.json
  target:
    kind: Secret
    labelSelector: app.kubernetes.io/name=workload-variant-autoscaler,app.kubernetes.io/component=epp-metrics"
    fi

    wva_kustomize_write_patch_deployment_json "$tmp" "$ns" "$ci_suffix_token" || return 1

    wva_kustomize_runtime_overlay_base "$tmp" "$ns" "$ctrl_bundle" "$sfx_line"

    cat >>"${tmp}/kustomization.yaml" <<EOF
patches:
- path: patch-configmap.yaml
  target:
    kind: ConfigMap
    name: workload-variant-autoscaler-wva-variantautoscaling-config
- path: patch-deployment.json
  target:
    kind: Deployment
    name: workload-variant-autoscaler-controller-manager
${extra_patches}
EOF

    log_info "Applying CRDs (Kustomize)..."
    kubectl apply -k "${WVA_PROJECT}/config/crd"

    log_info "Validating WVA controller bundle (preflight)..."
    kubectl kustomize "${tmp}" >/dev/null

    log_info "Applying WVA controller (Kustomize): bundle=${ctrl_bundle} deployment=$(wva_kustomize_manager_deployment_name)"
    kubectl apply -k "${tmp}"

    wva_apply_prometheus_ca_secret
    wva_apply_vllm_metrics_manifests

    if [ -z "${WVA_KUSTOMIZE_TMP:-}" ]; then
        rm -rf "${tmp}"
    fi
}

wva_kustomize_delete() {
    local tmp ns ctrl_bundle sfx_line
    ns="${WVA_NS:?}"
    tmp=$(mktemp -d "${TMPDIR:-/tmp}/wva-kustomize-del.XXXXXX")

    ctrl_bundle=$(wva_kustomize_controller_bundle_dir)
    sfx_line=$(wva_kustomize_namesuffix_yaml)

    wva_kustomize_runtime_overlay_base "$tmp" "$ns" "$ctrl_bundle" "$sfx_line"

    log_info "Removing WVA controller (Kustomize delete)..."
    kubectl delete -k "${tmp}" --ignore-not-found=true --wait=false 2>/dev/null || true

    if [ "${VLLM_SVC_ENABLED:-false}" = "true" ] && [ -n "${LLM_D_MODELSERVICE_NAME:-}" ]; then
        local prefix
        prefix=$(wva_resource_prefix)
        kubectl delete service "${prefix}-vllm" -n "${LLMD_NS}" --ignore-not-found=true 2>/dev/null || true
        kubectl delete servicemonitor "${prefix}-vllm-mon" -n "${LLMD_NS}" --ignore-not-found=true 2>/dev/null || true
    fi

    rm -rf "${tmp}"
}
