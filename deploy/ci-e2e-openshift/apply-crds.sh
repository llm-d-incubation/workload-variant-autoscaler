#!/usr/bin/env bash
set -euo pipefail

echo "Applying latest VariantAutoscaling CRD..."
# Apply CRDs from the Kustomize source of truth (config/base/crd/)
kubectl apply -k config/base/crd/
