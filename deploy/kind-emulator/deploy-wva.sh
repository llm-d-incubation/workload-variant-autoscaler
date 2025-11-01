#!/usr/bin/env bash

set -eou pipefail

KIND=${KIND:-kind}
KUBECTL=${KUBECTL:-kubectl}
KIND_NAME=${KIND_NAME:-"kind-wva-gpu-cluster"}
KIND_CONTEXT=kind-${KIND_NAME}
NAMESPACE=${NAMESPACE:-"workload-variant-autoscaler-system"}
MONITORING_NAMESPACE=${MONITORING_NAMESPACE:-"workload-variant-autoscaler-monitoring"}
KIND_NODE_NAME=${KIND_NODE_NAME:-"kind-wva-gpu-cluster-control-plane"}
WEBHOOK_TIMEOUT=${WEBHOOK_TIMEOUT:-2m}

_kubectl() {
        ${KUBECTL} --context ${KIND_CONTEXT} $@
}

_kind() {
	${KIND} $@
}

# Check if the Kind cluster exists, if not, create it
if ! _kind get kubeconfig --name "${KIND_NAME}" &>/dev/null; then
  echo "Kind cluster '${KIND_NAME}' does not exist. Creating..."
  deploy/kind-emulator/setup.sh "$@"
else
  echo "Kind cluster '${KIND_NAME}' is already running."
fi

# Load the Docker image into the Kind cluster
echo "Loading Docker image '${IMG}' into Kind cluster '${KIND_NAME}'..."

# Try to pull the image, or use local image if pull fails
if ! docker pull "${IMG}"; then
  echo "Warning: Failed to pull image '${IMG}' from registry. Attempting to use local image..."
  
  # Check if the image exists locally
  if ! docker image inspect "${IMG}" >/dev/null 2>&1; then
    echo "Error: Image '${IMG}' not found locally either. Please build the image or check the registry."
    exit 1
  else
    echo "Using local image '${IMG}'"
  fi
else
  echo "Successfully pulled image '${IMG}' from registry"
fi

_kind load docker-image ${IMG} --name ${KIND_NAME}

echo "Creating namespace ${NAMESPACE}"
_kubectl create ns ${NAMESPACE} 2>/dev/null || true

echo "Creating monitoring namespace ${MONITORING_NAMESPACE}"
_kubectl create ns ${MONITORING_NAMESPACE} 2>/dev/null || true

echo "Installing inferno CRD"
# Delete old CRDs to ensure clean installation (handles field renames like modelProfile -> variantProfile)
_kubectl delete crd variantautoscalings.llmd.ai --ignore-not-found=true
_kubectl delete crd inferencemodels.llmd.ai --ignore-not-found=true
make install
sleep 10

${KUBECTL} config set-context ${KIND_CONTEXT}

# Install the configmap service class
_kubectl apply -f deploy/configmap-serviceclass.yaml

# Install the configmap for the accelerator unit cost
_kubectl apply -f deploy/configmap-accelerator-unitcost.yaml

# deploy emulated vllme server (includes Prometheus with TLS)
# Export cluster name so the deploy script uses the same cluster
export KIND_NAME=${KIND_NAME}
deploy/examples/vllm-emulator/deploy.sh

echo "Deploying Inferno controller-manager"
make deploy-emulated
echo "Inferno controller-manager Installed"

echo "Inferno Deployment complete"