#!/usr/bin/env bash
set -euo pipefail

echo "=== Deployment Status ==="
echo ""
echo "=== Model A1 ($LLMD_NAMESPACE) ==="
kubectl get deployment -n "$LLMD_NAMESPACE" | grep -E "decode|NAME" || true
kubectl get hpa -n "$LLMD_NAMESPACE" || true
kubectl get variantautoscaling -n "$LLMD_NAMESPACE" || true
echo ""
echo "=== WVA Controller ($WVA_NAMESPACE) ==="
kubectl get pods -n "$WVA_NAMESPACE"
