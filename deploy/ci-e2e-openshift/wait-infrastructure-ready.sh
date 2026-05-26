#!/usr/bin/env bash
set -euo pipefail

echo "Waiting for WVA controller to be ready..."
kubectl rollout status deployment -l app.kubernetes.io/name=workload-variant-autoscaler -n "$WVA_NAMESPACE" --timeout=300s || true
kubectl get pods -n "$WVA_NAMESPACE"

# Wait for model server decode Deployment to exist (Kustomize apply can lag; names derive from guide namePrefix).
CANONICAL_DECODE="optimized-baseline-nvidia-gpu-vllm-decode"
DECODE_DEPLOY=""
for i in $(seq 1 90); do
  if kubectl get deployment "$CANONICAL_DECODE" -n "$LLMD_NAMESPACE" &>/dev/null; then
    DECODE_DEPLOY="$CANONICAL_DECODE"
    break
  fi
  DECODE_DEPLOY=$(kubectl get deploy -n "$LLMD_NAMESPACE" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -E -- '-decode$' | head -1 || true)
  if [ -n "$DECODE_DEPLOY" ]; then
    break
  fi
  echo "Waiting for ModelService decode Deployment in $LLMD_NAMESPACE ($i/90)..."
  sleep 10
done
if [ -z "$DECODE_DEPLOY" ]; then
  echo "::error::No decode Deployment found in $LLMD_NAMESPACE (check llm-d kustomize/helm deploy)"
  kubectl get deploy -n "$LLMD_NAMESPACE" -o wide || true
  exit 1
fi
echo "Using Model A1 decode Deployment: $DECODE_DEPLOY"

# Ensure the vLLM deployment has the correct replica count.
# A previous failed run's "Scale down GPU workloads" step may have set replicas=0
# and kustomize apply doesn't override manually-changed replicas on re-deploy.
# kubectl rollout status returns instantly on 0-replica deployments, so we must
# ensure replicas > 0 before waiting.
DESIRED_REPLICAS="${DECODE_REPLICAS:-1}"
CURRENT_REPLICAS=$(kubectl get deployment "$DECODE_DEPLOY" -n "$LLMD_NAMESPACE" -o jsonpath='{.spec.replicas}' 2>/dev/null || true)
if ! [[ "${CURRENT_REPLICAS:-}" =~ ^[0-9]+$ ]]; then
  CURRENT_REPLICAS=0
fi
if [ "${CURRENT_REPLICAS}" -eq 0 ]; then
  echo "WARNING: Model A1 deployment has 0 replicas (likely from previous failed run cleanup)"
  echo "Scaling to $DESIRED_REPLICAS replica(s)..."
  kubectl scale "deployment/$DECODE_DEPLOY" -n "$LLMD_NAMESPACE" --replicas="$DESIRED_REPLICAS" || {
    echo "ERROR: Failed to scale Model A1 deployment"
    exit 1
  }
fi

echo "Waiting for Model A1 vLLM deployment to be ready (up to 25 minutes for model loading)..."
# kubectl rollout status waits for all replicas to be Ready, unlike
# --for=condition=available which is satisfied even at 0 ready replicas.
# vLLM model loading takes 15-20 minutes, so we use a 25-minute timeout.
kubectl rollout status "deployment/$DECODE_DEPLOY" -n "$LLMD_NAMESPACE" --timeout=1500s || {
  echo "WARNING: Model A1 deployment not ready after 25 minutes"
  echo "=== Pod status ==="
  kubectl get pods -n "$LLMD_NAMESPACE"
  echo "=== Deployment conditions ==="
  kubectl get deployment "$DECODE_DEPLOY" -n "$LLMD_NAMESPACE" -o jsonpath='{.status.conditions}' | jq . || true
  echo "=== Recent events ==="
  kubectl get events -n "$LLMD_NAMESPACE" --sort-by='.lastTimestamp' | tail -20
}
kubectl get pods -n "$LLMD_NAMESPACE"
