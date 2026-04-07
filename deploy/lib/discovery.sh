#!/usr/bin/env bash
#
# Runtime environment discovery helpers for deploy/install.sh.
# Requires vars: LLMD_NS, POOL_DETECT_RETRIES, DEFAULT_MODEL_ID.
# Requires funcs: log_info/log_warning/log_success.
# Sets/exports: ACCELERATOR_TYPE, DEPLOY_LLM_D_INFERENCE_SIM, DETECTED_POOL_GROUP.
#

detect_gpu_type() {
    log_info "Detecting GPU type in cluster..."

    # Check if GPUs are visible
    local gpu_count
    gpu_count=$(kubectl get nodes -o json | jq -r '.items[].status.allocatable["nvidia.com/gpu"]' | grep -v null | head -1)

    if [ -z "$gpu_count" ] || [ "$gpu_count" == "null" ]; then
        log_warning "No GPUs visible"
        log_warning "GPUs may exist on host but need NVIDIA Device Plugin or GPU Operator"

        # Check if GPUs exist on host
        if nvidia-smi &> /dev/null; then
            log_info "nvidia-smi detected GPUs on host:"
            nvidia-smi --query-gpu=name,memory.total --format=csv,noheader | head -5
            log_warning "Install NVIDIA GPU Operator"
        else
            log_warning "No GPUs detected on host either"
            log_info "Setting DEPLOY_LLM_D_INFERENCE_SIM=true for demo mode"
            DEPLOY_LLM_D_INFERENCE_SIM=true
        fi
    else
        log_success "GPUs visible: $gpu_count GPU(s) per node"

        # Detect GPU type from labels
        local gpu_product
        gpu_product=$(kubectl get nodes -o json | jq -r '.items[] | select(.status.allocatable["nvidia.com/gpu"] != null) | .metadata.labels["nvidia.com/gpu.product"]' | head -1)

        if [ -n "$gpu_product" ]; then
            log_success "Detected GPU: $gpu_product"

            # Map GPU product to accelerator type
            case "$gpu_product" in
                *H100*)
                    ACCELERATOR_TYPE="H100"
                    ;;
                *A100*)
                    ACCELERATOR_TYPE="A100"
                    ;;
                *L40S*)
                    ACCELERATOR_TYPE="L40S"
                    ;;
                *)
                    log_warning "Unknown GPU type: $gpu_product, using default: $ACCELERATOR_TYPE"
                    ;;
            esac
        fi
    fi

    export ACCELERATOR_TYPE
    export DEPLOY_LLM_D_INFERENCE_SIM
    log_info "Using detected accelerator type: $ACCELERATOR_TYPE"
}

# Detect which InferencePool API group is in use in the cluster (v1 vs v1alpha2).
# Sets DETECTED_POOL_GROUP to inference.networking.k8s.io or inference.networking.x-k8s.io
# so WVA can be upgraded to watch the correct group (required for scale-from-zero datastore).
# Retries up to POOL_DETECT_RETRIES times (default 6, 10s apart) to handle the race where
# InferencePool instances haven't been created yet after helmfile deploy.
detect_inference_pool_api_group() {
    DETECTED_POOL_GROUP=""
    local max_retries=${POOL_DETECT_RETRIES:-6}
    local retry_interval_s=10
    local attempt=0
    # Search in the target namespace first (avoids cluster-wide RBAC issues), then fall back to -A.
    while [ "$attempt" -lt "$max_retries" ]; do
        # Try namespace-scoped first if LLMD_NS is set
        if [ -n "${LLMD_NS:-}" ]; then
            if [ -n "$(kubectl get inferencepools.inference.networking.k8s.io -n "$LLMD_NS" -o name --request-timeout=10s 2>/dev/null | head -1)" ]; then
                DETECTED_POOL_GROUP="inference.networking.k8s.io"
                return
            elif [ -n "$(kubectl get inferencepools.inference.networking.x-k8s.io -n "$LLMD_NS" -o name --request-timeout=10s 2>/dev/null | head -1)" ]; then
                DETECTED_POOL_GROUP="inference.networking.x-k8s.io"
                return
            fi
        fi
        # Fall back to cluster-wide search
        if [ -n "$(kubectl get inferencepools.inference.networking.k8s.io -A -o name --request-timeout=10s 2>/dev/null | head -1)" ]; then
            DETECTED_POOL_GROUP="inference.networking.k8s.io"
            return
        elif [ -n "$(kubectl get inferencepools.inference.networking.x-k8s.io -A -o name --request-timeout=10s 2>/dev/null | head -1)" ]; then
            DETECTED_POOL_GROUP="inference.networking.x-k8s.io"
            return
        fi
        attempt=$((attempt + 1))
        if [ "$attempt" -lt "$max_retries" ]; then
            log_info "InferencePool not found yet, retrying in ${retry_interval_s}s ($attempt/$max_retries)..."
            sleep "$retry_interval_s"
        fi
    done
}
