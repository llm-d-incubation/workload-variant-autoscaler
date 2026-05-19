#!/usr/bin/env bash
#
# Shared llm-d infrastructure deployment helpers for deploy/install-llmd-infra.sh.
# Requires vars: LLMD_NS, WVA_NS, EXAMPLE_DIR, WVA_PROJECT, GATEWAY_PROVIDER,
# LLMD_REMOVE_EMULATED_DECODE_DEPLOYMENTS, LLM_D_* values, model/latency knobs.
# Requires funcs: log_info/log_warning/log_success/log_error,
# containsElement(), wait_deployment_available_nonfatal(), detect_inference_pool_api_group().
#

# Post-deploy cleanup for emulated clusters (e.g. Kind): v0.7.0 deploys model server via Kustomize
# (single decode Deployment, no prefill). Remove it so e2e tests can deploy their own workloads.
apply_llm_d_infrastructure_fixes() {
    log_info "Applying llm-d infrastructure fixes for emulated cluster..."
    # Skip cleanup when the model server Deployment was not deployed (LLMD_SKIP_DEFAULT_MODELSERVICE=true).
    if ! kubectl get deployment "$LLM_D_MODELSERVICE_NAME" -n "$LLMD_NS" &>/dev/null; then
        log_info "Model server deployment $LLM_D_MODELSERVICE_NAME not found in $LLMD_NS; skipping cleanup"
        return
    fi

    if [ "${LLMD_REMOVE_EMULATED_DECODE_DEPLOYMENTS:-true}" = "true" ]; then
        log_info "Deleting decode deployment $LLM_D_MODELSERVICE_NAME (LLMD_REMOVE_EMULATED_DECODE_DEPLOYMENTS=true; tests deploy their own workloads)..."
        kubectl delete deployments.apps \
            "$LLM_D_MODELSERVICE_NAME" \
            --ignore-not-found -n "$LLMD_NS"
    fi
}

deploy_llm_d_infrastructure() {
    log_info "Deploying llm-d infrastructure..."

    # Clone llm-d guide repo. If the directory already exists (e.g., checked out
    # by CI), align it to the requested ref.
    local llm_d_repo_url="https://github.com/$LLM_D_OWNER/$LLM_D_PROJECT.git"
    local llm_d_clone_dir="$LLM_D_PROJECT"

    if [ ! -d "$llm_d_clone_dir" ]; then
        log_info "Cloning $llm_d_repo_url (release: $LLM_D_RELEASE) into $llm_d_clone_dir"
        git clone -b "$LLM_D_RELEASE" -- "$llm_d_repo_url" "$llm_d_clone_dir" &> /dev/null
    else
        log_info "Found existing $llm_d_clone_dir directory; aligning llm-d checkout to ref '$LLM_D_RELEASE'"
        if [ ! -d "$llm_d_clone_dir/.git" ]; then
            log_error "$llm_d_clone_dir exists but is not a git repository. Remove it or set LLM_D_PROJECT to a clean directory."
            exit 1
        fi
        git -C "$llm_d_clone_dir" fetch --all --tags --prune &>/dev/null || true
        # Use -f to discard any local modifications in the script-managed clone dir.
        if ! git -C "$llm_d_clone_dir" checkout -f "$LLM_D_RELEASE" &>/dev/null; then
            log_warning "Failed to align $llm_d_clone_dir to ref '$LLM_D_RELEASE' — recloning fresh"
            rm -rf "$llm_d_clone_dir"
            log_info "Cloning $llm_d_repo_url (release: $LLM_D_RELEASE) into $llm_d_clone_dir"
            git clone -b "$LLM_D_RELEASE" -- "$llm_d_repo_url" "$llm_d_clone_dir" &> /dev/null
        fi
    fi

    # Check for HF_TOKEN (use dummy for emulated deployments)
    if [ -z "$HF_TOKEN" ]; then
        if ! containsElement "$ENVIRONMENT" "${NON_EMULATED_ENV_LIST[@]}"; then
            log_warning "HF_TOKEN not set - using dummy token for emulated deployment"
            export HF_TOKEN="dummy-token"
        else
            log_error "HF_TOKEN is required for non-emulated deployments. Please set HF_TOKEN and try again."
        fi
    fi

    # Create HF token secret
    log_info "Creating HuggingFace token secret"
    kubectl create secret generic llm-d-hf-token \
        --from-literal="HF_TOKEN=${HF_TOKEN}" \
        --namespace "${LLMD_NS}" \
        --dry-run=client -o yaml | kubectl apply -f -

    # On OpenShift, skip base Gateway API CRDs (managed by Ingress Operator via
    # ValidatingAdmissionPolicy "openshift-ingress-operator-gatewayapi-crd-admission").
    # Only install Gateway API Inference Extension (GAIE) CRDs directly.
    if [[ "$ENVIRONMENT" == "openshift" ]]; then
        log_info "Skipping Gateway API base CRDs on OpenShift (managed by Ingress Operator)"
        GAIE_CRD_REV=${GATEWAY_API_INFERENCE_EXTENSION_CRD_REVISION:-"$GAIE_VERSION"}
        log_info "Installing Gateway API Inference Extension CRDs (${GAIE_CRD_REV})"
        kubectl apply -k "https://github.com/kubernetes-sigs/gateway-api-inference-extension/config/crd/?ref=${GAIE_CRD_REV}" \
            && log_success "GAIE CRDs installed" \
            || log_warning "Failed to install GAIE CRDs (may already exist or network issue)"
    else
        bash "$GATEWAY_PREREQ_DIR/install-gateway-provider-dependencies.sh"
    fi

    # Install Gateway provider (if kgateway, use v2.0.3)
    if [ "$GATEWAY_PROVIDER" == "kgateway" ]; then
        log_info "Installing $GATEWAY_PROVIDER v2.0.3"
        yq eval '.releases[].version = "v2.0.3"' -i "$GATEWAY_PREREQ_DIR/$GATEWAY_PROVIDER.helmfile.yaml"
    fi

    # Install Gateway control plane if enabled
    if [[ "$INSTALL_GATEWAY_CTRLPLANE" == "true" ]]; then
        log_info "Installing Gateway control plane ($GATEWAY_PROVIDER)"
        helmfile apply -f "$GATEWAY_PREREQ_DIR/$GATEWAY_PROVIDER.helmfile.yaml"
    else
        log_info "Skipping Gateway control plane installation (INSTALL_GATEWAY_CTRLPLANE=false)"
    fi

    # v0.7.0+: Deploy EPP + InferencePool via GAIE standalone Helm chart.
    # The single release named $GUIDE_NAME replaces the three-chart helmfile pattern from v0.6.0.
    local recipe_scheduler_values="$WVA_PROJECT/$LLM_D_PROJECT/guides/recipes/scheduler/base.values.yaml"
    local guide_scheduler_values="$EXAMPLE_DIR/scheduler/$GUIDE_NAME.values.yaml"
    log_info "Deploying GAIE standalone chart (release=$GUIDE_NAME, version=$GAIE_VERSION)"
    helm upgrade --install "$GUIDE_NAME" \
        oci://registry.k8s.io/gateway-api-inference-extension/charts/standalone \
        -f "$recipe_scheduler_values" \
        -f "$guide_scheduler_values" \
        -n "$LLMD_NS" \
        --version "$GAIE_VERSION"

    # Post-deploy workaround: patch Role for inferencemodelrewrites if missing.
    log_info "Patching Role $LLM_D_EPP_NAME to include inferencemodelrewrites"
    if kubectl get role "$LLM_D_EPP_NAME" -n "$LLMD_NS" &> /dev/null; then
        kubectl patch role "$LLM_D_EPP_NAME" -n "$LLMD_NS" --type='json' -p='[{"op": "add", "path": "/rules/0/resources/-", "value": "inferencemodelrewrites"}]' && \
            log_success "Patched Role $LLM_D_EPP_NAME successfully" || \
            log_warning "Failed to patch Role $LLM_D_EPP_NAME"
    else
        log_warning "Role $LLM_D_EPP_NAME not found, skipping RBAC patch"
    fi

    # Post-deploy workaround: grant EPP SA permission to create tokenreviews so its metrics endpoint
    # can authenticate incoming requests (controller-runtime RBAC auth filter requirement).
    local epp_sa_name="$LLM_D_EPP_NAME"
    log_info "Ensuring $epp_sa_name SA can create tokenreviews (required for metrics endpoint auth)"
    kubectl apply -f - <<RBAC_EOF
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: ${epp_sa_name}-tokenreview-creator
rules:
- apiGroups:
  - authentication.k8s.io
  resources:
  - tokenreviews
  verbs:
  - create
- apiGroups:
  - authorization.k8s.io
  resources:
  - subjectaccessreviews
  verbs:
  - create
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ${epp_sa_name}-tokenreview-creator
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: ${epp_sa_name}-tokenreview-creator
subjects:
- kind: ServiceAccount
  name: $epp_sa_name
  namespace: $LLMD_NS
RBAC_EOF

    # v0.7.0+: Deploy model server via Kustomize overlay (replaces llm-d-modelservice Helm chart).
    if [ "${LLMD_SKIP_DEFAULT_MODELSERVICE:-false}" = "true" ]; then
        log_info "Skipping model server deploy (LLMD_SKIP_DEFAULT_MODELSERVICE=true); e2e tests will create their own workloads"
    else
        local modelserver_overlay="$EXAMPLE_DIR/modelserver/gpu/vllm/$INFRA_PROVIDER"
        log_info "Deploying model server via Kustomize: $modelserver_overlay"
        kubectl apply -n "$LLMD_NS" -k "$modelserver_overlay"

        # Post-apply patches for model server customization.
        # (In v0.7.0 configuration is in Kustomize overlays; we patch the Deployment for runtime overrides.)

        # Inference simulator: replace vLLM image + args with simulator image + latency config.
        if [ "$DEPLOY_LLM_D_INFERENCE_SIM" == "true" ]; then
            log_info "Replacing model server image with inference simulator: $LLM_D_INFERENCE_SIM_IMG_REPO:$LLM_D_INFERENCE_SIM_IMG_TAG"
            local container_name
            container_name=$(kubectl get deployment "$LLM_D_MODELSERVICE_NAME" -n "$LLMD_NS" \
                -o jsonpath='{.spec.template.spec.containers[0].name}')
            kubectl set image deployment/"$LLM_D_MODELSERVICE_NAME" \
                "${container_name}=$LLM_D_INFERENCE_SIM_IMG_REPO:$LLM_D_INFERENCE_SIM_IMG_TAG" \
                -n "$LLMD_NS"
            local sim_args
            sim_args=$(printf '["--model","%s","--port","8000","--time-to-first-token=%sms","--inter-token-latency=%sms"]' \
                "$MODEL_ID" "$TTFT_AVERAGE_LATENCY_MS" "$ITL_AVERAGE_LATENCY_MS")
            if [ -n "$VLLM_MAX_NUM_SEQS" ]; then
                sim_args=$(echo "$sim_args" | sed "s/\]$/,\"--max-num-seqs=$VLLM_MAX_NUM_SEQS\"]/")
            fi
            kubectl patch deployment "$LLM_D_MODELSERVICE_NAME" -n "$LLMD_NS" --type=json \
                -p="[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args\",\"value\":$sim_args}]"
        else
            log_info "Using real vLLM model server (DEPLOY_LLM_D_INFERENCE_SIM=false)"
            # Patch model ID if it differs from the guide's default (Qwen/Qwen3-32B).
            if [ -n "$MODEL_ID" ] && [ "$MODEL_ID" != "Qwen/Qwen3-32B" ]; then
                log_info "Patching model server to use MODEL_ID=$MODEL_ID"
                kubectl patch deployment "$LLM_D_MODELSERVICE_NAME" -n "$LLMD_NS" --type=json \
                    -p="[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args/1\",\"value\":\"$MODEL_ID\"}]"
            fi
            if [ -n "$VLLM_MAX_NUM_SEQS" ]; then
                log_info "Appending --max-num-seqs=$VLLM_MAX_NUM_SEQS to model server args"
                kubectl patch deployment "$LLM_D_MODELSERVICE_NAME" -n "$LLMD_NS" --type=json \
                    -p="[{\"op\":\"add\",\"path\":\"/spec/template/spec/containers/0/args/-\",\"value\":\"--max-num-seqs=$VLLM_MAX_NUM_SEQS\"}]"
            fi
            if [ -n "$VLLM_GPU_MEM_UTIL" ]; then
                log_info "Appending --gpu-memory-utilization=$VLLM_GPU_MEM_UTIL to model server args"
                kubectl patch deployment "$LLM_D_MODELSERVICE_NAME" -n "$LLMD_NS" --type=json \
                    -p="[{\"op\":\"add\",\"path\":\"/spec/template/spec/containers/0/args/-\",\"value\":\"--gpu-memory-utilization=$VLLM_GPU_MEM_UTIL\"}]"
            fi
            if [ -n "$VLLM_MAX_MODEL_LEN" ]; then
                log_info "Appending --max-model-len=$VLLM_MAX_MODEL_LEN to model server args"
                kubectl patch deployment "$LLM_D_MODELSERVICE_NAME" -n "$LLMD_NS" --type=json \
                    -p="[{\"op\":\"add\",\"path\":\"/spec/template/spec/containers/0/args/-\",\"value\":\"--max-model-len=$VLLM_MAX_MODEL_LEN\"}]"
            fi
            if [ -n "$VLLM_BLOCK_SIZE" ]; then
                log_info "Appending --block-size=$VLLM_BLOCK_SIZE to model server args"
                kubectl patch deployment "$LLM_D_MODELSERVICE_NAME" -n "$LLMD_NS" --type=json \
                    -p="[{\"op\":\"add\",\"path\":\"/spec/template/spec/containers/0/args/-\",\"value\":\"--block-size=$VLLM_BLOCK_SIZE\"}]"
            fi
            if [ -n "$VLLM_ENFORCE_EAGER" ] && [ "$VLLM_ENFORCE_EAGER" = "true" ]; then
                log_info "Appending --enforce-eager to model server args"
                kubectl patch deployment "$LLM_D_MODELSERVICE_NAME" -n "$LLMD_NS" --type=json \
                    -p="[{\"op\":\"add\",\"path\":\"/spec/template/spec/containers/0/args/-\",\"value\":\"--enforce-eager\"}]"
            fi
        fi

        # Scale decode replicas if requested.
        if [ -n "$DECODE_REPLICAS" ]; then
            log_info "Scaling $LLM_D_MODELSERVICE_NAME to $DECODE_REPLICAS replicas"
            kubectl scale deployment "$LLM_D_MODELSERVICE_NAME" -n "$LLMD_NS" --replicas="$DECODE_REPLICAS"
        fi
    fi

    # Patch EPP CPU requests before any rollout restart so the first scheduled pod already uses the reduced value.
    # The GAIE standalone chart embeds EPP + Envoy in the same pod; patch all containers without --containers.
    log_info "Patching EPP CPU requests for resource-constrained environments (standalone chart: EPP + Envoy in one pod)..."
    if kubectl get deployment "$LLM_D_EPP_NAME" -n "$LLMD_NS" &>/dev/null; then
        local current_epp_cpu=$(kubectl get deployment "$LLM_D_EPP_NAME" -n "$LLMD_NS" -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}' 2>/dev/null || echo "unknown")
        log_info "Current EPP CPU request (container 0): $current_epp_cpu"
        kubectl set resources deployment "$LLM_D_EPP_NAME" -n "$LLMD_NS" \
            --requests='cpu=500m,memory=256Mi' 2>/dev/null \
            && log_success "EPP all-container resources patched to cpu=500m,memory=256Mi" \
            || log_warning "Failed to patch EPP resources"
    else
        log_warning "EPP deployment not found: $LLM_D_EPP_NAME"
    fi

    # EPP (inference scheduler) image comes from the llm-d guide helmfile at LLM_D_RELEASE — not overridden here.
    # Enable flowControl in the EPP ConfigMap when scale-to-zero or e2e tests require it (queue metrics path).
    if [ "$ENABLE_SCALE_TO_ZERO" == "true" ] || [ "${LLMD_PATCH_EPP_FLOW_CONTROL:-false}" == "true" ]; then
        if kubectl get deployment "$LLM_D_EPP_NAME" -n "$LLMD_NS" &> /dev/null; then
        
            # Enable flowControl feature gate in the EPP ConfigMap.
            # The GAIE standalone chart names the active config key after the release:
            # e.g. LLM_D_EPP_NAME=optimized-baseline-epp → key=optimized-baseline-plugins.yaml
            # Strip the "-epp" suffix to derive the config key used by --config-file in the EPP container.
            local EPP_PLUGINS_KEY="${LLM_D_EPP_NAME%-epp}-plugins.yaml"
            if kubectl get configmap "$LLM_D_EPP_NAME" -n "$LLMD_NS" &> /dev/null; then
                # Read the config key the EPP actually loads (derived from release name, not "default-plugins.yaml")
                local CURRENT_CONFIG=$(kubectl get configmap "$LLM_D_EPP_NAME" -n "$LLMD_NS" -o json | yq eval ".data[\"${EPP_PLUGINS_KEY}\"]" -)

                if echo "$CURRENT_CONFIG" | yq eval '.featureGates // [] | contains(["flowControl"])' - | grep -q 'true'; then
                    log_info "flowControl feature gate already enabled in EPP ConfigMap ($EPP_PLUGINS_KEY)"
                else
                    log_info "Enabling flowControl feature gate in EPP ConfigMap $LLM_D_EPP_NAME ($EPP_PLUGINS_KEY)"

                    # Use yq to properly add flowControl to featureGates array (creates array if missing, appends if exists)
                    local UPDATED_CONFIG=$(echo "$CURRENT_CONFIG" | yq eval '.featureGates += ["flowControl"] | .featureGates |= unique' -)

                    # Validate that flowControl was successfully added
                    if echo "$UPDATED_CONFIG" | yq eval '.featureGates // [] | contains(["flowControl"])' - | grep -q 'true'; then
                        # Apply the updated config — JSON Pointer path: dots in key name need no escaping (only ~ and /)
                        kubectl patch configmap "$LLM_D_EPP_NAME" -n "$LLMD_NS" --type='json' -p='[
                            {
                                "op": "replace",
                                "path": "/data/'"$EPP_PLUGINS_KEY"'",
                                "value": "'"$(echo "$UPDATED_CONFIG" | sed 's/"/\\"/g' | tr '\n' '\r' | sed 's/\r/\\n/g')"'"
                            }
                        ]'

                        # Restart deployment to pick up the config change
                        log_info "Restarting $LLM_D_EPP_NAME deployment to apply flowControl feature gate"
                        kubectl rollout restart deployment "$LLM_D_EPP_NAME" -n "$LLMD_NS"
                    else
                        log_error "Failed to add flowControl to featureGates in EPP ConfigMap - YAML structure may be invalid or unexpected"
                        log_error "Current config structure: $(echo "$CURRENT_CONFIG" | yq eval '.' - 2>&1 | head -5)"
                        exit 1
                    fi
                fi
            else
                log_warning "ConfigMap $LLM_D_EPP_NAME not found in $LLMD_NS"
            fi
        else
            log_warning "Skipping inference-scheduler patch: Deployment $LLM_D_EPP_NAME not found in $LLMD_NS"
        fi
    fi

    # Deploy InferenceObjective for GIE queuing when flow control is enabled (scale-from-zero).
    # E2E applies e2e-default from Go (test/e2e/fixtures) so tests do not depend on install.sh for this CR.
    if [ "${LLMD_SKIP_INFERENCE_OBJECTIVE:-false}" != "true" ] && [ "$ENABLE_SCALE_TO_ZERO" == "true" ]; then
        if kubectl get crd inferenceobjectives.inference.networking.x-k8s.io &>/dev/null; then
            local infobj_file="${WVA_PROJECT}/deploy/inference-objective-e2e.yaml"
            if [ -f "$infobj_file" ]; then
                # v0.7.0+: InferencePool name matches the GAIE Helm release name ($GUIDE_NAME).
                local pool_ref_name="${RELEASE_NAME_POSTFIX:-$WELL_LIT_PATH_NAME}"
                log_info "Applying InferenceObjective e2e-default (poolRef.name=$pool_ref_name) for GIE queuing"
                if sed -e "s/NAMESPACE_PLACEHOLDER/${LLMD_NS}/g" -e "s/POOL_NAME_PLACEHOLDER/${pool_ref_name}/g" "$infobj_file" | kubectl apply -f -; then
                    log_success "InferenceObjective e2e-default applied"
                else
                    log_warning "Failed to apply InferenceObjective (pool $pool_ref_name may not exist yet)"
                fi
            else
                log_warning "InferenceObjective manifest not found at $infobj_file"
            fi
        else
            log_warning "InferenceObjective CRD not found; GIE may not support InferenceObjective yet"
        fi
    fi

    # For deterministic e2e infra-only runs, avoid waiting on all llm-d deployments.
    # The full wait often blocks on modelservice decode/prefill readiness, which is
    # unnecessary for the e2e suite because tests create/manage their own workloads.
    if [ "${LLMD_WAIT_FOR_ESSENTIAL_LLM_D_ONLY:-false}" = "true" ]; then
        local E2E_DEPLOY_WAIT_TIMEOUT="${E2E_DEPLOY_WAIT_TIMEOUT:-120s}"
        log_info "Waiting for essential llm-d components only (timeout=${E2E_DEPLOY_WAIT_TIMEOUT}, LLMD_WAIT_FOR_ESSENTIAL_LLM_D_ONLY=true)..."

        if kubectl get deployment "$LLM_D_EPP_NAME" -n "$LLMD_NS" &>/dev/null; then
            kubectl wait --for=condition=Available "deployment/$LLM_D_EPP_NAME" -n "$LLMD_NS" --timeout="$E2E_DEPLOY_WAIT_TIMEOUT" || \
                log_warning "EPP deployment not ready yet: $LLM_D_EPP_NAME"
        else
            log_warning "EPP deployment not found: $LLM_D_EPP_NAME"
        fi

        # Gateway deployment name includes release prefix and can vary by environment.
        # Patch Gateway CPU request before waiting
        local gateway_deploy
        gateway_deploy=$(kubectl get deployment -n "$LLMD_NS" -o name 2>/dev/null | grep "inference-gateway-istio" | head -1 || true)
        if [ -n "$gateway_deploy" ]; then
            local gateway_name=$(echo "$gateway_deploy" | sed 's|deployment.apps/||')
            local current_gw_cpu=$(kubectl get deployment "$gateway_name" -n "$LLMD_NS" -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}' 2>/dev/null || echo "unknown")
            log_info "Current Gateway CPU request: $current_gw_cpu"
            
            if [ "$current_gw_cpu" != "500m" ] && [ "$current_gw_cpu" != "unknown" ]; then
                log_info "Patching Gateway deployment to request 500m CPUs instead of $current_gw_cpu"
                kubectl set resources deployment "$gateway_name" -n "$LLMD_NS" \
                    --requests='cpu=500m,memory=256Mi' 2>/dev/null \
                    && log_success "Gateway resources patched to cpu=500m,memory=256Mi" \
                    || log_warning "Failed to patch Gateway resources"
            else
                log_info "Gateway already requesting 500m CPUs or resources not set, skipping patch"
            fi
            
            # Now wait for Gateway to be available
            kubectl wait --for=condition=Available "$gateway_deploy" -n "$LLMD_NS" --timeout="$E2E_DEPLOY_WAIT_TIMEOUT" || \
                log_warning "Gateway deployment not ready yet: $gateway_deploy"
        else
            log_warning "Gateway deployment not found in $LLMD_NS"
        fi
    else
        # Model-serving pods (vLLM) can take several minutes to download and load
        # large models into GPU memory. The startupProbe allows up to 30m, so the
        # wait timeout here must be long enough for the model to finish loading.
        local DEPLOY_WAIT_TIMEOUT="${DEPLOY_WAIT_TIMEOUT:-600s}"
        log_info "Waiting for all llm-d Deployments to become Available (timeout=${DEPLOY_WAIT_TIMEOUT}; for CI/e2e use LLMD_WAIT_FOR_ESSENTIAL_LLM_D_ONLY=true to wait only EPP + gateway)..."
        kubectl wait --for=condition=Available deployment --all -n "$LLMD_NS" --timeout="$DEPLOY_WAIT_TIMEOUT" || \
            log_warning "llm-d components are not ready yet - check 'kubectl get pods -n $LLMD_NS'"
    fi

    # Align WVA with the InferencePool API group in use (scale-from-zero requires WVA to watch the same group).
    # llm-d version determines whether pools are inference.networking.k8s.io (v1) or inference.networking.x-k8s.io (v1alpha2).
    if [ "$DEPLOY_WVA" == "true" ]; then
        detect_inference_pool_api_group
        if [ -n "$DETECTED_POOL_GROUP" ]; then
            log_info "Detected InferencePool API group: $DETECTED_POOL_GROUP; upgrading WVA to watch it (scale-from-zero)"
            if kubectl patch deployment workload-variant-autoscaler-controller-manager \
                -n "$WVA_NS" --type=json \
                -p="[{\"op\":\"add\",\"path\":\"/spec/template/spec/containers/0/env/-\",\"value\":{\"name\":\"POOL_GROUP\",\"value\":\"${DETECTED_POOL_GROUP}\"}}]"; then
                log_success "WVA patched with POOL_GROUP=$DETECTED_POOL_GROUP"
                # Wait for the rolling restart to complete so the datastore is populated
                kubectl rollout status deployment/workload-variant-autoscaler-controller-manager \
                    -n "$WVA_NS" --timeout=120s \
                    && log_success "WVA rollout complete" \
                    || log_warning "WVA rollout status timed out - datastore may not be ready"
            else
                log_warning "WVA pool-group patch failed - scale-from-zero may not see the InferencePool"
            fi
        else
            log_warning "Could not detect InferencePool API group - WVA may have empty datastore for scale-from-zero"
        fi
    fi
    
    # List all pods in llm-d namespace
    log_info "Listing all pods in $LLMD_NS namespace:"
    kubectl get pods -n "$LLMD_NS" -o wide || log_warning "Failed to list pods in $LLMD_NS"

    if ! containsElement "$ENVIRONMENT" "${NON_EMULATED_ENV_LIST[@]}"; then
        apply_llm_d_infrastructure_fixes
    fi

    cd "$WVA_PROJECT"
    
    log_success "llm-d infrastructure deployment complete"
}
