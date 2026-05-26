#!/usr/bin/env bash
set -euo pipefail

echo "Applying latest VariantAutoscaling CRD..."
# Apply CRDs from the Kustomize source of truth (config/crd/bases/)
kubectl apply -f config/crd/bases/
