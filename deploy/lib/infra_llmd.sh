#!/usr/bin/env bash
#
# Shared llm-d infrastructure deployment helpers for deploy/install-llmd-infra.sh.
# Requires vars: LLMD_NS, WVA_NS, EXAMPLE_DIR, WVA_PROJECT, GATEWAY_PROVIDER,
# LLMD_REMOVE_EMULATED_DECODE_DEPLOYMENTS, LLM_D_* values, model/latency knobs.
# Requires funcs: log_info/log_warning/log_success/log_error,
# containsElement(), wait_deployment_available_nonfatal(), detect_inference_pool_api_group().
#

# Post-deploy cleanup for emulated clusters (e.g. Kind): chart ModelService ships both prefill and decode;
# current WVA flows expect a single user-owned decode workload. Always remove prefill; decode removal is gated
# by LLMD_REMOVE_EMULATED_DECODE_DEPLOYMENTS (default true for e2e / local emulated flows).
apply_llm_d_infrastructure_fixes() {
    log_info "Applying llm-d infrastructure fixes for emulated cluster..."
    # Skip cleanup when modelservice release is not installed (e.g. e2e infra-only path excludes it).
    if ! helm list -n "$LLMD_NS" --short 2>/dev/null | grep -q '^ms-'; then
        log_info "No llm-d modelservice release detected in $LLMD_NS; skipping prefill/decode cleanup"
        return
    fi

    log_info "Deleting prefill deployments..."
    kubectl delete deployments.apps \
        "$LLM_D_MODELSERVICE_NAME-prefill" \
        --ignore-not-found -n "$LLMD_NS"

    if [ "${LLMD_REMOVE_EMULATED_DECODE_DEPLOYMENTS:-true}" = "true" ]; then
        log_info "Deleting decode deployments (LLMD_REMOVE_EMULATED_DECODE_DEPLOYMENTS=true; tests deploy their own workloads)..."
        kubectl delete deployments.apps \
            "$LLM_D_MODELSERVICE_NAME-decode" \
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
        if ! git -C "$llm_d_clone_dir" checkout "$LLM_D_RELEASE" &>/dev/null; then
            log_error "Failed to align $llm_d_clone_dir to ref '$LLM_D_RELEASE'"
            exit 1
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

    # Install dependencies
    log_info "Installing llm-d dependencies"
    if [ ! -f "$CLIENT_PREREQ_DIR/install-deps.sh" ]; then
        log_error "Missing llm-d dependency installer at $CLIENT_PREREQ_DIR/install-deps.sh"
        exit 1
    fi
    bash "$CLIENT_PREREQ_DIR/install-deps.sh"

    # On OpenShift, skip base Gateway API CRDs (managed by Ingress Operator via
    # ValidatingAdmissionPolicy "openshift-ingress-operator-gatewayapi-crd-admission").
    # Only install Gateway API Inference Extension (GAIE) CRDs directly.
    if [[ "$ENVIRONMENT" == "openshift" ]]; then
        log_info "Skipping Gateway API base CRDs on OpenShift (managed by Ingress Operator)"
        GAIE_CRD_REV=${GATEWAY_API_INFERENCE_EXTENSION_CRD_REVISION:-"v1.3.0"}
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

    # Configuring llm-d before installation
    cd "$EXAMPLE_DIR"
    log_info "Configuring llm-d infrastructure"

    # Detect the actual default model from the values file (not the hardcoded DEFAULT_MODEL_ID)
    ACTUAL_DEFAULT_MODEL=$(yq eval '.modelArtifacts.name' "$LLM_D_MODELSERVICE_VALUES" 2>/dev/null || echo "$DEFAULT_MODEL_ID")
    if [ -z "$ACTUAL_DEFAULT_MODEL" ] || [ "$ACTUAL_DEFAULT_MODEL" == "null" ]; then
        ACTUAL_DEFAULT_MODEL="$DEFAULT_MODEL_ID"
    fi

    # Update model ID if different from the guide's actual default.
    # Use strenv() so MODEL_ID / ACTUAL_DEFAULT_MODEL are never interpolated into yq
    # expression text (avoids breakage on quotes, pipes, etc. in HuggingFace IDs).
    if [ "$MODEL_ID" != "$ACTUAL_DEFAULT_MODEL" ] ; then
        log_info "Updating deployment to use model: $MODEL_ID (replacing guide default: $ACTUAL_DEFAULT_MODEL)"
        MODEL_ID="$MODEL_ID" ACTUAL_DEFAULT_MODEL="$ACTUAL_DEFAULT_MODEL" yq eval -i \
            '(.. | select(. == strenv(ACTUAL_DEFAULT_MODEL))) = strenv(MODEL_ID) | (.. | select(. == ("hf://" + strenv(ACTUAL_DEFAULT_MODEL)))) = ("hf://" + strenv(MODEL_ID))' \
            "$LLM_D_MODELSERVICE_VALUES"

        # Increase model-storage volume size
        log_info "Increasing model-storage volume size for model: $MODEL_ID"
        yq eval '.modelArtifacts.size = "100Gi"' -i "$LLM_D_MODELSERVICE_VALUES"
    else
        log_info "Model ID matches guide default ($ACTUAL_DEFAULT_MODEL), no replacement needed"
    fi

    # Ensure llm-d.ai/model label is unique per model for multi-model HPA isolation.
    # The chart sets a default (e.g. "Qwen3-32B") that all deployments share,
    # which causes AmbiguousSelector errors when multiple HPAs target different
    # deployments with identical pod selectors.
    local model_label_value
    model_label_value=$(echo "$MODEL_ID" | sed 's|.*/||; s|\.|-|g')  # e.g. "Qwen3-0-6B", "Meta-Llama-3.1-8B"
    log_info "Setting llm-d.ai/model label to: $model_label_value (unique per model)"
    yq eval ".modelArtifacts.labels.\"llm-d.ai/model\" = \"$model_label_value\"" -i "$LLM_D_MODELSERVICE_VALUES"
    # Configure llm-d-inference-simulator if needed
    if [ "$DEPLOY_LLM_D_INFERENCE_SIM" == "true" ]; then
      log_info "Deploying llm-d-inference-simulator..."
        yq eval ".decode.containers[0].image = \"$LLM_D_INFERENCE_SIM_IMG_REPO:$LLM_D_INFERENCE_SIM_IMG_TAG\" | \
                 .prefill.containers[0].image = \"$LLM_D_INFERENCE_SIM_IMG_REPO:$LLM_D_INFERENCE_SIM_IMG_TAG\" | \
                 .decode.containers[0].args = [\"--time-to-first-token=$TTFT_AVERAGE_LATENCY_MS\", \"--inter-token-latency=$ITL_AVERAGE_LATENCY_MS\"] | \
                 .prefill.containers[0].args = [\"--time-to-first-token=$TTFT_AVERAGE_LATENCY_MS\", \"--inter-token-latency=$ITL_AVERAGE_LATENCY_MS\"]" \
                 -i "$LLM_D_MODELSERVICE_VALUES"
    else
        log_info "Skipping llm-d-inference-simulator deployment (DEPLOY_LLM_D_INFERENCE_SIM=false)"
    fi

    # Override llm-d container image tags if set (e.g. upgrade from v0.3.0 to v0.6.0)
    if [ -n "$LLMD_IMAGE_TAG" ]; then
      log_info "Overriding llm-d image tags to $LLMD_IMAGE_TAG"
      yq eval ".decode.containers[0].image = \"ghcr.io/llm-d/llm-d-cuda:${LLMD_IMAGE_TAG}\"" -i "$LLM_D_MODELSERVICE_VALUES"
      yq eval ".routing.proxy.image = \"ghcr.io/llm-d/llm-d-routing-sidecar:${LLMD_IMAGE_TAG}\"" -i "$LLM_D_MODELSERVICE_VALUES"
    fi

    # Configure vLLM max-num-seqs if set (useful for e2e testing to force saturation)
    if [ -n "$VLLM_MAX_NUM_SEQS" ]; then
      log_info "Setting vLLM max-num-seqs to $VLLM_MAX_NUM_SEQS for decode containers"
      yq eval ".decode.containers[0].args += [\"--max-num-seqs=$VLLM_MAX_NUM_SEQS\"]" -i "$LLM_D_MODELSERVICE_VALUES"
    fi

    # Configure vLLM GPU memory utilization if set
    if [ -n "$VLLM_GPU_MEM_UTIL" ]; then
      log_info "Setting vLLM gpu-memory-utilization to $VLLM_GPU_MEM_UTIL"
      yq eval ".decode.containers[0].args += [\"--gpu-memory-utilization=$VLLM_GPU_MEM_UTIL\"]" -i "$LLM_D_MODELSERVICE_VALUES"
    fi

    # Configure vLLM max-model-len if set
    if [ -n "$VLLM_MAX_MODEL_LEN" ]; then
      log_info "Setting vLLM max-model-len to $VLLM_MAX_MODEL_LEN"
      yq eval ".decode.containers[0].args += [\"--max-model-len=$VLLM_MAX_MODEL_LEN\"]" -i "$LLM_D_MODELSERVICE_VALUES"
    fi

    # Configure vLLM block-size if set
    if [ -n "$VLLM_BLOCK_SIZE" ]; then
      log_info "Setting vLLM block-size to $VLLM_BLOCK_SIZE"
      yq eval ".decode.containers[0].args += [\"--block-size=$VLLM_BLOCK_SIZE\"]" -i "$LLM_D_MODELSERVICE_VALUES"
    fi

    # Configure vLLM enforce-eager if set
    if [ -n "$VLLM_ENFORCE_EAGER" ] && [ "$VLLM_ENFORCE_EAGER" = "true" ]; then
      log_info "Setting vLLM enforce-eager"
      yq eval ".decode.containers[0].args += [\"--enforce-eager\"]" -i "$LLM_D_MODELSERVICE_VALUES"
    fi

    # Configure decode replicas if set (useful for e2e testing with limited GPUs)
    if [ -n "$DECODE_REPLICAS" ]; then
      log_info "Setting decode replicas to $DECODE_REPLICAS"
      yq eval ".decode.replicas = $DECODE_REPLICAS" -i "$LLM_D_MODELSERVICE_VALUES"
    fi

    # Deploy llm-d core components
    log_info "Deploying llm-d core components"
    local -a helmfile_selector_exprs=()
    if [ "${LLMD_SKIP_DEFAULT_MODELSERVICE:-false}" = "true" ]; then
      # E2E and similar flows create scenario-specific ModelService workloads;
      # skip the default llm-d-modelservice release so baseline infra is clean.
      helmfile_selector_exprs+=("chart!=llm-d-modelservice")
      log_info "Skipping llm-d-modelservice release in helmfile (LLMD_SKIP_DEFAULT_MODELSERVICE=true)"
    fi
    local selector_csv=""
    if [ "${#helmfile_selector_exprs[@]}" -gt 0 ]; then
      selector_csv=$(IFS=,; echo "${helmfile_selector_exprs[*]}")
      log_info "helmfile selector: $selector_csv"
      helmfile apply -e "$GATEWAY_PROVIDER" -n "${LLMD_NS}" --selector "$selector_csv"
    else
      log_info "helmfile selector: (none)"
      helmfile apply -e "$GATEWAY_PROVIDER" -n "${LLMD_NS}"
    fi

    # Post-deploy workaround: Upstream EPP chart (v1.0.1) is missing RBAC for inferencemodelrewrites
    log_info "Patching Role $LLM_D_EPP_NAME to include inferencemodelrewrites"
    if kubectl get role "$LLM_D_EPP_NAME" -n "$LLMD_NS" &> /dev/null; then
        kubectl patch role "$LLM_D_EPP_NAME" -n "$LLMD_NS" --type='json' -p='[{"op": "add", "path": "/rules/0/resources/-", "value": "inferencemodelrewrites"}]' && \
            log_success "Patched Role $LLM_D_EPP_NAME successfully" || \
            log_warning "Failed to patch Role $LLM_D_EPP_NAME"
    else
        log_warning "Role $LLM_D_EPP_NAME not found, skipping RBAC patch"
    fi

    if [ "${LLMD_SKIP_DEFAULT_MODELSERVICE:-false}" = "true" ]; then
      if helm list -n "$LLMD_NS" --short 2>/dev/null | grep -q '^ms-'; then
        log_warning "Modelservice release still present in $LLMD_NS despite selector; tests may need extra cleanup"
      fi
    fi

    # Apply HTTPRoute with correct resource name references.
    # The static httproute.yaml uses resource names matching the helmfile's default
    # RELEASE_NAME_POSTFIX (e.g. "workload-autoscaler"). When RELEASE_NAME_POSTFIX
    # is overridden (e.g. in CI), gateway and InferencePool names change, so we
    # must template the HTTPRoute references to match the actual deployed resources.
    # RELEASE_NAME_POSTFIX is set by the reusable nightly workflow
    # (llm-d-infra reusable-nightly-e2e-openshift.yaml) via the guide_name input.
    if [ -f httproute.yaml ]; then
        local rn="${RELEASE_NAME_POSTFIX:-}"
        if [ -n "$rn" ]; then
            local gw_name="infra-${rn}-inference-gateway"
            local pool_name="gaie-${rn}"
            log_info "Applying HTTPRoute (gateway=$gw_name, pool=$pool_name)"
            if ! yq eval "
                .spec.parentRefs[0].name = \"${gw_name}\" |
                .spec.rules[0].backendRefs[0].name = \"${pool_name}\"
            " httproute.yaml | kubectl apply -f - -n ${LLMD_NS}; then
                log_error "Failed to apply templated HTTPRoute for gateway=${gw_name}, pool=${pool_name}"
                exit 1
            fi
        else
            if ! kubectl apply -f httproute.yaml -n ${LLMD_NS}; then
                log_error "Failed to apply HTTPRoute from httproute.yaml"
                exit 1
            fi
        fi
    fi

    # EPP (inference scheduler) image comes from the llm-d guide helmfile at LLM_D_RELEASE — not overridden here.
    # Enable flowControl in the EPP ConfigMap when scale-to-zero or e2e tests require it (queue metrics path).
    if [ "$ENABLE_SCALE_TO_ZERO" == "true" ] || [ "${LLMD_PATCH_EPP_FLOW_CONTROL:-false}" == "true" ]; then
        if kubectl get deployment "$LLM_D_EPP_NAME" -n "$LLMD_NS" &> /dev/null; then
        
            # Enable flowControl feature gate in the EPP ConfigMap
            if kubectl get configmap "$LLM_D_EPP_NAME" -n "$LLMD_NS" &> /dev/null; then
                # Check if flowControl is already enabled
                local CURRENT_CONFIG=$(kubectl get configmap "$LLM_D_EPP_NAME" -n "$LLMD_NS" -o jsonpath='{.data.default-plugins\.yaml}')
                
                if echo "$CURRENT_CONFIG" | yq eval '.featureGates // [] | contains(["flowControl"])' - | grep -q 'true'; then
                    log_info "flowControl feature gate already enabled in EPP ConfigMap"
                else
                    log_info "Enabling flowControl feature gate in EPP ConfigMap $LLM_D_EPP_NAME"
                    
                    # Use yq to properly add flowControl to featureGates array (creates array if missing, appends if exists)
                    local UPDATED_CONFIG=$(echo "$CURRENT_CONFIG" | yq eval '.featureGates += ["flowControl"] | .featureGates |= unique' -)
                    
                    # Validate that flowControl was successfully added
                    if echo "$UPDATED_CONFIG" | yq eval '.featureGates // [] | contains(["flowControl"])' - | grep -q 'true'; then
                        # Apply the updated config
                        kubectl patch configmap "$LLM_D_EPP_NAME" -n "$LLMD_NS" --type='json' -p='[
                            {
                                "op": "replace",
                                "path": "/data/default-plugins.yaml",
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
                local pool_ref_name="${RELEASE_NAME_POSTFIX:+gaie-$RELEASE_NAME_POSTFIX}"
                pool_ref_name="${pool_ref_name:-gaie-$WELL_LIT_PATH_NAME}"
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

    # Patch EPP and Gateway CPU requests for resource-constrained environments (e.g., KIND)
    # This reduces CPU requests from 4 cores to 500m (0.5 cores) each to allow scheduling on smaller clusters
    log_info "Patching EPP and Gateway CPU requests for resource-constrained environments..."
    
    if kubectl get deployment "$LLM_D_EPP_NAME" -n "$LLMD_NS" &>/dev/null; then
        local current_epp_cpu=$(kubectl get deployment "$LLM_D_EPP_NAME" -n "$LLMD_NS" -o jsonpath='{.spec.template.spec.containers[0].resources.requests.cpu}' 2>/dev/null || echo "unknown")
        log_info "Current EPP CPU request: $current_epp_cpu"
        
        if [ "$current_epp_cpu" != "500m" ]; then
            log_info "Patching EPP deployment to request 500m CPUs instead of $current_epp_cpu"
            kubectl patch deployment "$LLM_D_EPP_NAME" -n "$LLMD_NS" --type='json' -p='[
                {
                    "op": "replace",
                    "path": "/spec/template/spec/containers/0/resources/requests/cpu",
                    "value": "500m"
                }
            ]' && log_success "EPP CPU request patched to 500m" || log_warning "Failed to patch EPP CPU request"
        else
            log_info "EPP already requesting 500m CPUs, skipping patch"
        fi
    else
        log_warning "EPP deployment not found: $LLM_D_EPP_NAME"
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
                kubectl patch deployment "$gateway_name" -n "$LLMD_NS" --type='json' -p='[
                    {
                        "op": "replace",
                        "path": "/spec/template/spec/containers/0/resources/requests/cpu",
                        "value": "500m"
                    }
                ]' && log_success "Gateway CPU request patched to 500m" || log_warning "Failed to patch Gateway CPU request"
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
