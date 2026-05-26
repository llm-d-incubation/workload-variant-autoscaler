#!/usr/bin/env bash
set -euo pipefail

echo "Deploying WVA and llm-d infrastructure..."
echo "  MODEL_ID: $MODEL_ID"
echo "  ACCELERATOR_TYPE: $ACCELERATOR_TYPE"
echo "  LLMD_NS: $LLMD_NS"
echo "  WVA_NS: $WVA_NS"
echo "  WVA_IMAGE_TAG: $WVA_IMAGE_TAG"
echo "  CONTROLLER_INSTANCE: $CONTROLLER_INSTANCE"
echo "  VLLM_MAX_NUM_SEQS: $VLLM_MAX_NUM_SEQS"
echo "  KV_SPARE_TRIGGER: ${KV_SPARE_TRIGGER:-<default>}"
echo "  QUEUE_SPARE_TRIGGER: ${QUEUE_SPARE_TRIGGER:-<default>}"
echo "  HF token configuration: ✓"
# Base monitoring + WVA + scaler backend.
./deploy/install.sh --environment openshift

# Create llm-d namespace and HuggingFace token secret.
kubectl create namespace "$LLMD_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
kubectl create secret generic llm-d-hf-token \
  --from-literal="HF_TOKEN=${HF_TOKEN}" \
  -n "$LLMD_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

# OpenShift: install GAIE CRDs before standalone chart.
kubectl apply -k "https://github.com/kubernetes-sigs/gateway-api-inference-extension/config/crd/?ref=${GAIE_VERSION}" || true

# Model server via Kustomize (reduce replicas for CI).
yq ".spec.replicas = 1" -i llm-d/guides/optimized-baseline/modelserver/gpu/vllm/base/patch-vllm.yaml
kubectl apply -k llm-d/guides/optimized-baseline/modelserver/gpu/vllm/base -n "$LLMD_NAMESPACE"

# Post-apply: patch model ID and vLLM batch size.
MODELSERVICE="optimized-baseline-nvidia-gpu-vllm-decode"
kubectl patch deployment "$MODELSERVICE" -n "$LLMD_NAMESPACE" --type=json \
  -p="[{\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args/1\",\"value\":\"$MODEL_ID\"}]"
[ -n "${VLLM_MAX_NUM_SEQS:-}" ] && kubectl patch deployment "$MODELSERVICE" -n "$LLMD_NAMESPACE" --type=json \
  -p="[{\"op\":\"add\",\"path\":\"/spec/template/spec/containers/0/args/-\",\"value\":\"--max-num-seqs=$VLLM_MAX_NUM_SEQS\"}]"

# EPP / scheduler via install-epp.sh (handles GAIE standalone chart +
# flowControl feature gate + tokenreview RBAC).  llm-d is already
# checked out at $GITHUB_WORKSPACE/llm-d so the script reuses it.
WVA_PROJECT="$GITHUB_WORKSPACE" \
LLMD_NS="$LLMD_NAMESPACE" \
GAIE_VERSION="$GAIE_VERSION" \
LLM_D_RELEASE="$LLM_D_RELEASE" \
ENVIRONMENT=openshift \
ENABLE_SCALE_TO_ZERO=true \
./deploy/install-epp.sh

# Tune saturation thresholds for CI simulator mode.
kubectl patch configmap workload-variant-autoscaler-saturation-scaling-config \
  -n "$WVA_NAMESPACE" --type=merge \
  -p "$(printf '{"data":{"default":"kvSpareTrigger: %s\\nqueueSpareTrigger: %s\\n"}}' \
    "${KV_SPARE_TRIGGER}" "${QUEUE_SPARE_TRIGGER}")"
