#!/usr/bin/env bash
set -euo pipefail

 echo "Deploying WVA and llm-d infrastructure..."
echo "  ACCELERATOR_TYPE: $ACCELERATOR_TYPE"
echo "  LLMD_NS: $LLMD_NS"
echo "  WVA_NS: $WVA_NS"
echo "  WVA_IMAGE_TAG: $WVA_IMAGE_TAG"
echo "  CONTROLLER_INSTANCE: $CONTROLLER_INSTANCE"
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

# EPP / scheduler via install-epp.sh (handles llm-d-router-standalone chart +
# flowControl feature gate + tokenreview RBAC).
WVA_PROJECT="$GITHUB_WORKSPACE" \
LLMD_NS="$LLMD_NAMESPACE" \
GAIE_VERSION="$GAIE_VERSION" \
LLM_D_ROUTER_VERSION="$LLM_D_ROUTER_VERSION" \
ENVIRONMENT=openshift \
ENABLE_SCALE_TO_ZERO=true \
SKIP_CLUSTER_CRDS=true \
./deploy/install-epp.sh

# Tune saturation thresholds for CI simulator mode.
kubectl patch configmap wva-saturation-scaling-config \
  -n "$WVA_NAMESPACE" --type=merge \
  -p "$(printf '{"data":{"default":"kvSpareTrigger: %s\\nqueueSpareTrigger: %s\\n"}}' \
    "${KV_SPARE_TRIGGER}" "${QUEUE_SPARE_TRIGGER}")"
