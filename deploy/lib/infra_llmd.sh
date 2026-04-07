#!/usr/bin/env bash
#
# Shared llm-d infrastructure deployment helpers for deploy/install.sh.
# Requires vars: LLMD_NS, WVA_NS, EXAMPLE_DIR, WVA_PROJECT, GATEWAY_PROVIDER,
# LLM_D_* values, model/latency knobs.
# Requires funcs: log_info/log_warning/log_success/log_error,
# containsElement(), wait_deployment_available_nonfatal(), detect_inference_pool_api_group().
#

deploy_llm_d_infrastructure() {
    log_info "Deploying llm-d infrastructure..."

     # Clone llm-d repo if not exists
    if [ ! -d "$LLM_D_PROJECT" ]; then
        log_info "Cloning $LLM_D_PROJECT repository (release: $LLM_D_RELEASE)"
        git clone -b $LLM_D_RELEASE -- https://github.com/$LLM_D_OWNER/$LLM_D_PROJECT.git $LLM_D_PROJECT &> /dev/null
    else
        log_warning "$LLM_D_PROJECT directory already exists, skipping clone"
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
    bash $CLIENT_PREREQ_DIR/install-deps.sh

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
        bash $GATEWAY_PREREQ_DIR/install-gateway-provider-dependencies.sh
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

    # Update model ID if different from the guide's actual default
    if [ "$MODEL_ID" != "$ACTUAL_DEFAULT_MODEL" ] ; then
        log_info "Updating deployment to use model: $MODEL_ID (replacing guide default: $ACTUAL_DEFAULT_MODEL)"
        yq eval "(.. | select(. == \"$ACTUAL_DEFAULT_MODEL\")) = \"$MODEL_ID\" | (.. | select(. == \"hf://$ACTUAL_DEFAULT_MODEL\")) = \"hf://$MODEL_ID\"" -i "$LLM_D_MODELSERVICE_VALUES"

        # Increase model-storage volume size
        log_info "Increasing model-storage volume size for model: $MODEL_ID"
        yq eval '.modelArtifacts.size = "100Gi"' -i "$LLM_D_MODELSERVICE_VALUES"
    else
        log_info "Model ID matches guide default ($ACTUAL_DEFAULT_MODEL), no replacement needed"
    fi

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

    # Configure vLLM max-num-seqs if set (useful for e2e testing to force saturation)
    if [ -n "$VLLM_MAX_NUM_SEQS" ]; then
      log_info "Setting vLLM max-num-seqs to $VLLM_MAX_NUM_SEQS for decode containers"
      yq eval ".decode.containers[0].args += [\"--max-num-seqs=$VLLM_MAX_NUM_SEQS\"]" -i "$LLM_D_MODELSERVICE_VALUES"
    fi

    # Configure decode replicas if set (useful for e2e testing with limited GPUs)
    if [ -n "$DECODE_REPLICAS" ]; then
      log_info "Setting decode replicas to $DECODE_REPLICAS"
      yq eval ".decode.replicas = $DECODE_REPLICAS" -i "$LLM_D_MODELSERVICE_VALUES"
    fi

    # Check if the guide's llm-d.ai/model label differs from what WVA's vllm-service expects.
    # If so, we'll patch pod labels post-deploy (not pre-deploy) to avoid violating the
    # llm-d-modelservice chart schema which disallows extra properties under modelArtifacts.
    CURRENT_MODEL_LABEL=$(yq eval '.modelArtifacts.labels."llm-d.ai/model"' "$LLM_D_MODELSERVICE_VALUES" 2>/dev/null || echo "")
    NEEDS_LABEL_ALIGNMENT=false
    if [ -n "$CURRENT_MODEL_LABEL" ] && [ "$CURRENT_MODEL_LABEL" != "null" ] && [ "$CURRENT_MODEL_LABEL" != "$LLM_D_MODELSERVICE_NAME" ]; then
      log_info "Will align llm-d.ai/model label post-deploy: '$CURRENT_MODEL_LABEL' -> '$LLM_D_MODELSERVICE_NAME'"
      NEEDS_LABEL_ALIGNMENT=true
    fi

    # Auto-detect vLLM port from guide configuration and update WVA vllm-service.
    # When routing proxy is disabled, vLLM serves directly on containerPort (typically 8000).
    # When proxy is enabled, vLLM serves on proxy.targetPort (typically 8200).
    PROXY_ENABLED=$(yq eval '.routing.proxy.enabled // true' "$LLM_D_MODELSERVICE_VALUES" 2>/dev/null || echo "true")
    if [ "$PROXY_ENABLED" == "false" ]; then
      DETECTED_PORT=$(yq eval '.decode.containers[0].ports[0].containerPort // 8000' "$LLM_D_MODELSERVICE_VALUES" 2>/dev/null || echo "8000")
      if [ "$VLLM_SVC_PORT" != "$DETECTED_PORT" ]; then
        log_info "Routing proxy disabled - updating vLLM service port: $VLLM_SVC_PORT -> $DETECTED_PORT"
        VLLM_SVC_PORT=$DETECTED_PORT
        # Update the WVA vllm-service port (WVA was deployed before llm-d infra)
        if [ "$DEPLOY_WVA" == "true" ] && [ "$VLLM_SVC_ENABLED" == "true" ]; then
          helm upgrade "$WVA_RELEASE_NAME" ${WVA_PROJECT}/charts/workload-variant-autoscaler \
            -n "$WVA_NS" --reuse-values \
            --set wva.namespaceScoped="${NAMESPACE_SCOPED:-true}" \
            --set vllmService.port="$VLLM_SVC_PORT" \
            --set vllmService.targetPort="$VLLM_SVC_PORT"
        fi
      fi
    fi

    # Deploy llm-d core components
    log_info "Deploying llm-d core components"
    # When DEPLOY_WVA is true, skip WVA in helmfile — install.sh deploys it
    # separately using the local chart (supports dev/test of chart changes).
    # The helmfile's WVA release uses the published OCI chart which may not
    # have the latest fixes and uses KIND-specific defaults (e.g. monitoringNamespace).
    local -a helmfile_selector_exprs=()
    if [ "$DEPLOY_WVA" == "true" ]; then
      helmfile_selector_exprs+=("kind!=autoscaling")
      log_info "Skipping WVA in helmfile (will be deployed separately from local chart)"
    fi
    if [ "$E2E_TESTS_ENABLED" = "true" ] && [ "$INFRA_ONLY" = "true" ]; then
      # E2E infra-only tests create scenario-specific modelservice workloads
      # themselves. Skip the default llm-d-modelservice release so baseline
      # infrastructure is clean and we avoid create-then-delete churn.
      helmfile_selector_exprs+=("chart!=llm-d-modelservice")
      log_info "E2E infra-only mode: skipping llm-d-modelservice release in helmfile"
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

    if [ "$E2E_TESTS_ENABLED" = "true" ] && [ "$INFRA_ONLY" = "true" ]; then
      if helm list -n "$LLMD_NS" --short 2>/dev/null | grep -q '^ms-'; then
        log_warning "Modelservice release still present in $LLMD_NS despite e2e selector; tests may need extra cleanup"
      fi
    fi

    # Post-deploy: align the WVA vllm-service selector and ServiceMonitor to match
    # the actual pod labels. The llm-d-modelservice chart sets pod labels from
    # modelArtifacts.labels (e.g. "Qwen3-32B"), but the WVA chart's Service selector
    # uses llmd.modelName (e.g. "ms-inference-scheduling-llm-d-modelservice").
    # We patch the Service/ServiceMonitor selectors (which ARE mutable) rather than
    # the deployment labels (which have immutable selectors).
    if [ "$NEEDS_LABEL_ALIGNMENT" == "true" ]; then
      # Compute the chart fullname (mirrors _helpers.tpl logic)
      local chart_name="workload-variant-autoscaler"
      local wva_fullname
      if echo "$WVA_RELEASE_NAME" | grep -q "$chart_name"; then
        wva_fullname="$WVA_RELEASE_NAME"
      else
        wva_fullname="${WVA_RELEASE_NAME}-${chart_name}"
      fi
      wva_fullname=$(echo "$wva_fullname" | cut -c1-63 | sed 's/-$//')
      local svc_name="${wva_fullname}-vllm"
      local svcmon_name="${wva_fullname}-vllm-mon"
      log_info "Aligning WVA Service/ServiceMonitor selectors: llm-d.ai/model=$CURRENT_MODEL_LABEL"
      # Patch Service selector
      kubectl patch service "$svc_name" -n "$LLMD_NS" --type=merge -p "{
        \"spec\": {\"selector\": {\"llm-d.ai/model\": \"$CURRENT_MODEL_LABEL\"}}
      }" && log_success "Patched Service $svc_name selector" \
         || log_warning "Failed to patch Service $svc_name selector"
      # Patch ServiceMonitor matchLabels
      kubectl patch servicemonitor "$svcmon_name" -n "$LLMD_NS" --type=merge -p "{
        \"spec\": {\"selector\": {\"matchLabels\": {\"llm-d.ai/model\": \"$CURRENT_MODEL_LABEL\"}}}
      }" && log_success "Patched ServiceMonitor $svcmon_name selector" \
         || log_warning "Failed to patch ServiceMonitor $svcmon_name selector"
      # Also patch the Service labels so the ServiceMonitor can find it
      kubectl label service "$svc_name" -n "$LLMD_NS" "llm-d.ai/model=$CURRENT_MODEL_LABEL" --overwrite \
        && log_success "Patched Service $svc_name label" \
        || log_warning "Failed to patch Service $svc_name label"
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

    # Patch llm-d-inference-scheduler deployment image and enable flowControl when scale-to-zero or e2e tests are enabled
    # (required for scale-from-zero: the image must support flow control for queue metrics).
    if [ "$ENABLE_SCALE_TO_ZERO" == "true" ] || [ "$E2E_TESTS_ENABLED" == "true" ]; then
        if kubectl get deployment "$LLM_D_EPP_NAME" -n "$LLMD_NS" &> /dev/null; then
            # Get the current image from the deployment
            local CURRENT_IMAGE=$(kubectl get deployment "$LLM_D_EPP_NAME" -n "$LLMD_NS" -o jsonpath='{.spec.template.spec.containers[0].image}')
            
            # Only patch if the image is different
            if [ "$CURRENT_IMAGE" != "$LLM_D_INFERENCE_SCHEDULER_IMG" ]; then
                log_info "Patching llm-d-inference-scheduler deployment: updating image from $CURRENT_IMAGE to $LLM_D_INFERENCE_SCHEDULER_IMG"
                kubectl patch deployment "$LLM_D_EPP_NAME" -n "$LLMD_NS" --type='json' -p='[
                    {
                        "op": "replace",
                        "path": "/spec/template/spec/containers/0/image",
                        "value": "'$LLM_D_INFERENCE_SCHEDULER_IMG'"
                    }
                ]'
            else
                log_info "Skipping image patch: llm-d-inference-scheduler already using $LLM_D_INFERENCE_SCHEDULER_IMG"
            fi

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
    if [ "$E2E_TESTS_ENABLED" != "true" ] && [ "$ENABLE_SCALE_TO_ZERO" == "true" ]; then
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

    # For deterministic e2e infra-only runs, avoid waiting on all llm-d deployments.
    # The full wait often blocks on modelservice decode/prefill readiness, which is
    # unnecessary for the e2e suite because tests create/manage their own workloads.
    if [ "$E2E_TESTS_ENABLED" = "true" ] && [ "$INFRA_ONLY" = "true" ]; then
        local E2E_DEPLOY_WAIT_TIMEOUT="${E2E_DEPLOY_WAIT_TIMEOUT:-120s}"
        log_info "E2E infra-only mode: waiting for essential llm-d components (timeout=${E2E_DEPLOY_WAIT_TIMEOUT})..."

        if kubectl get deployment "$LLM_D_EPP_NAME" -n "$LLMD_NS" &>/dev/null; then
            kubectl wait --for=condition=Available "deployment/$LLM_D_EPP_NAME" -n "$LLMD_NS" --timeout="$E2E_DEPLOY_WAIT_TIMEOUT" || \
                log_warning "EPP deployment not ready yet: $LLM_D_EPP_NAME"
        else
            log_warning "EPP deployment not found: $LLM_D_EPP_NAME"
        fi

        # Gateway deployment name includes release prefix and can vary by environment.
        # Wait only if we can detect one, otherwise continue.
        local gateway_deploy
        gateway_deploy=$(kubectl get deployment -n "$LLMD_NS" -o name 2>/dev/null | grep "inference-gateway-istio" | head -1 || true)
        if [ -n "$gateway_deploy" ]; then
            kubectl wait --for=condition=Available "$gateway_deploy" -n "$LLMD_NS" --timeout="$E2E_DEPLOY_WAIT_TIMEOUT" || \
                log_warning "Gateway deployment not ready yet: $gateway_deploy"
        fi
    else
        # Model-serving pods (vLLM) can take several minutes to download and load
        # large models into GPU memory. The startupProbe allows up to 30m, so the
        # wait timeout here must be long enough for the model to finish loading.
        local DEPLOY_WAIT_TIMEOUT="${DEPLOY_WAIT_TIMEOUT:-600s}"
        log_info "Waiting for llm-d components to initialize (timeout=${DEPLOY_WAIT_TIMEOUT})..."
        kubectl wait --for=condition=Available deployment --all -n "$LLMD_NS" --timeout="$DEPLOY_WAIT_TIMEOUT" || \
            log_warning "llm-d components are not ready yet - check 'kubectl get pods -n $LLMD_NS'"
    fi

    # Align WVA with the InferencePool API group in use (scale-from-zero requires WVA to watch the same group).
    # llm-d version determines whether pools are inference.networking.k8s.io (v1) or inference.networking.x-k8s.io (v1alpha2).
    if [ "$DEPLOY_WVA" == "true" ]; then
        detect_inference_pool_api_group
        if [ -n "$DETECTED_POOL_GROUP" ]; then
            log_info "Detected InferencePool API group: $DETECTED_POOL_GROUP; upgrading WVA to watch it (scale-from-zero)"
            if helm upgrade "$WVA_RELEASE_NAME" ${WVA_PROJECT}/charts/workload-variant-autoscaler \
                -n "$WVA_NS" --reuse-values \
                --set wva.namespaceScoped="${NAMESPACE_SCOPED:-true}" \
                --set wva.poolGroup="$DETECTED_POOL_GROUP" --wait --timeout=60s; then
                log_success "WVA upgraded with wva.poolGroup=$DETECTED_POOL_GROUP"
            else
                log_warning "WVA upgrade with poolGroup failed - scale-from-zero may not see the InferencePool"
            fi
        else
            log_warning "Could not detect InferencePool API group - WVA may have empty datastore for scale-from-zero"
        fi
    fi

    cd "$WVA_PROJECT"
    log_success "llm-d infrastructure deployment complete"
}
