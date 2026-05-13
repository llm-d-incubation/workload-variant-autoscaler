# Local Development with Kind Emulator

Quick start guide for local development using Kind (Kubernetes in Docker) with emulated GPU resources.

> **Note**: This guide covers Kind-specific deployment for local testing. For a complete overview of deployment methods, Helm chart configuration, and the full configuration reference, see the [main deployment guide](../README.md).

## Table of Contents

- [Prerequisites](#prerequisites)
- [Quick Start](#quick-start)
- [Configuration Options](#configuration-options)
- [Scripts](#scripts)
- [Cluster Configuration](#cluster-configuration)
- [Testing Locally](#testing-locally)
- [Troubleshooting](#troubleshooting)
- [Development Workflow](#development-workflow)
- [Clean Up](#clean-up)
- [Next Steps](#next-steps)

## Prerequisites

- Docker
- Kind
- kubectl
- Helm

## Quick Start

### One-Command Setup

Deploy WVA with full llm-d infrastructure:

```bash
# From project root
make deploy-wva-emulated-on-kind
```

This creates:

- Kind cluster with 3 nodes, emulated GPUs (mixed vendors)
- WVA controller
- llm-d infrastructure (simulation mode)
- Prometheus monitoring
- vLLM emulator

## Configuration Options

For a complete list of environment variables and configuration options, see the [Configuration Reference](../README.md#configuration-reference) in the main deployment guide.

**Key environment variables for Kind emulator**:

```bash
export HF_TOKEN="hf_xxxxx"                  # Required: HuggingFace token
export MODEL_ID="unsloth/Meta-Llama-3.1-8B" # Model to deploy
export ACCELERATOR_TYPE="H100"              # Emulated GPU type
export GATEWAY_PROVIDER="kgateway"          # Gateway for Kind (kgateway recommended)
export ITL_AVERAGE_LATENCY_MS=20            # Average inter-token latency for the llm-d-inference-sim
export TTFT_AVERAGE_LATENCY_MS=200          # Average time-to-first-token for the llm-d-inference-sim

# Performance tuning (optional)
export VLLM_MAX_NUM_SEQS=64                 # vLLM max concurrent sequences (batch size)
export HPA_STABILIZATION_SECONDS=240        # HPA stabilization window

# Image load (optional; auto-detected if unset)
export KIND_IMAGE_PLATFORM=linux/amd64      # Single platform for kind load (avoids "digest not found")
```

**Deployment flags**:

```bash
export DEPLOY_PROMETHEUS=true         # Deploy Prometheus stack
export DEPLOY_WVA=true                # Deploy WVA controller
export DEPLOY_PROMETHEUS_ADAPTER=true # Deploy Prometheus Adapter
# llm-d: `make deploy-wva-emulated-on-kind` runs install.sh then install-llmd-infra.sh
```

### Step-by-Step Setup

**1. Create Kind cluster:**

```bash
make create-kind-cluster

# With custom configuration
make create-kind-cluster KIND_ARGS="-t mix -n 4 -g 2"
# -t: vendor type (nvidia, amd, intel, mix)
# -n: number of nodes
# -g: GPUs per node
```

**2. Deploy WVA + monitoring only (no llm-d):**

```bash
cd /path/to/repo
export ENVIRONMENT=kind-emulator
./deploy/install.sh
```

**3. Full stack (WVA + llm-d emulated):**

```bash
make deploy-wva-emulated-on-kind
```

**4. Testing configuration with fast saturation:**

```bash
export DEPLOY_VA=true
export DEPLOY_HPA=true
export VLLM_MAX_NUM_SEQS=8              # Low batch size for easy saturation
export HPA_STABILIZATION_SECONDS=30     # Fast scaling for testing
make deploy-wva-emulated-on-kind
```

## Scripts

### setup.sh

Creates Kind cluster with emulated GPU support.

```bash
./setup.sh -t mix -n 3 -g 2
```

**Options:**

- `-t`: Vendor type (nvidia|amd|intel|mix) - default: mix
- `-n`: Number of nodes - default: 3
- `-g`: GPUs per node - default: 2

### teardown.sh

Destroys the Kind cluster.

```bash
./teardown.sh
```

### install.sh (Kind environment plugin)

`deploy/kind-emulator/install.sh` is **sourced** by `deploy/install-llmd-infra.sh` when `ENVIRONMENT=kind-emulator`. It handles Kind-specific setup (namespaces, image load, monitoring wiring, and related helpers). **llm-d ModelService post-deploy cleanup** for emulated clusters (remove chart prefill; optionally remove chart decode when `LLMD_REMOVE_EMULATED_DECODE_DEPLOYMENTS=true`, the default) runs from **`deploy/lib/infra_llmd.sh`** inside `deploy_llm_d_infrastructure`, not from this `install.sh`.

## Cluster Configuration

Default cluster created by `setup.sh`:

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraMounts:
      - hostPath: /dev/null
        containerPath: /dev/nvidia0
  - role: worker
  - role: worker
```

GPUs are emulated using extended resources:

- `nvidia.com/gpu`
- `amd.com/gpu`
- `intel.com/gpu`

## Testing Locally

### 1. Access metrics, Services and Pods

**Port-forward WVA metrics:**

```bash
kubectl port-forward -n workload-variant-autoscaler-system \
  svc/workload-variant-autoscaler-controller-manager-metrics 8080:8080
```

**Port-forward Prometheus:**

```bash
kubectl port-forward -n workload-variant-autoscaler-monitoring \
  svc/prometheus-operated 9090:9090
```

**Port-forward Inference Gateway:**

```bash
kubectl port-forward -n llm-d-sim svc/infra-sim-inference-gateway 8000:80
```

### 2. Create Test Resources

```bash
# Apply sample VariantAutoscaling
kubectl apply -f ../../config/samples/
```

### 3. Run E2E test

**Option A — Run E2E tests (recommended)**  
The consolidated e2e suite (`test/e2e/`) exercises infra-only deploy, resource wiring, reconciliation, and deterministic correctness checks. For sustained load or benchmarking, use **Option B** or separate perf workflows — not required for e2e.

```bash
# From repo root, after deploying (e.g. make deploy-wva-emulated-on-kind)
make deploy-e2e-infra   # if not already done
make test-e2e-smoke    # quick validation
# or
make test-e2e-full     # full suite (`full && !flaky`)
```

See [Testing Guide](../../docs/developer-guide/testing.md) and [E2E Test Suite README](../../test/e2e/README.md).

### 4. Generate Load

**Option B — Manual load with burst script**  
Use the script in the e2e fixtures (requires only `curl`; no Python). After port-forwarding the inference gateway or vLLM service to `localhost:8000`:

```bash
# From repo root
export TARGET_URL="http://localhost:8000/v1/chat/completions"
export MODEL_ID="unsloth/Meta-Llama-3.1-8B"
export TOTAL_REQUESTS=100
export BATCH_SIZE=10
./hack/burst_load_generator.sh
```

Tune load with `TOTAL_REQUESTS`, `BATCH_SIZE`, and optional `BATCH_SLEEP`, `MAX_TOKENS`, `CURL_TIMEOUT` (see script header).

### 5. Monitor

```bash
# Watch deployments scale
watch kubectl get deploy -n llm-d-sim

# Watch VariantAutoscaling status
watch kubectl get variantautoscalings.llmd.ai -A

# View controller logs
kubectl logs -n workload-variant-autoscaler-system \
  -l control-plane=controller-manager -f
```

## Troubleshooting

### Cluster Creation Fails

```bash
# Clean up and retry
kind delete cluster --name kind-wva-gpu-cluster
make create-kind-cluster
```

### Controller Not Starting

```bash
# Check controller logs
kubectl logs -n workload-variant-autoscaler-system \
  deployment/workload-variant-autoscaler-controller-manager

# Verify CRDs installed
kubectl get crd variantautoscalings.llmd.ai

# Check RBAC
kubectl get clusterrole,clusterrolebinding -l app=workload-variant-autoscaler
```

### GPUs Not Appearing

```bash
# Verify GPU labels on nodes
kubectl get nodes -o json | jq '.items[].status.capacity'

# Should see nvidia.com/gpu, amd.com/gpu, or intel.com/gpu
```

### Port-Forward Issues

```bash
# Kill existing port-forwards
pkill -f "kubectl port-forward"

# Verify pod is running before port-forwarding
kubectl get pods -n <namespace>
```

### `kind load` fails with "content digest ... not found"

This can happen when loading a multi-platform image into Kind: the image manifest references blobs for multiple platforms (e.g. `linux/arm64`, `linux/amd64`), but the stream that `kind load` feeds into containerd does not include all of them, so `ctr` reports a missing digest. See [kubernetes-sigs/kind#3795](https://github.com/kubernetes-sigs/kind/issues/3795) and [kubernetes-sigs/kind#3845](https://github.com/kubernetes-sigs/kind/issues/3845). The install script works around it by pulling a single-platform image before loading. If you still see the error or need a specific architecture, set the platform explicitly:

```bash
# Force linux/amd64 (e.g. for Intel or emulated nodes)
KIND_IMAGE_PLATFORM=linux/amd64 make deploy-wva-emulated-on-kind CREATE_CLUSTER=true

# Force linux/arm64 (e.g. for Apple Silicon with native arm64 nodes)
KIND_IMAGE_PLATFORM=linux/arm64 make deploy-wva-emulated-on-kind CREATE_CLUSTER=true
```

Alternatively, build the image locally and deploy with `IfNotPresent` so the script skips the registry pull and loads your local single-platform image:

```bash
make docker-build IMG=ghcr.io/llm-d/llm-d-workload-variant-autoscaler:latest
WVA_IMAGE_PULL_POLICY=IfNotPresent make deploy-wva-emulated-on-kind CREATE_CLUSTER=true
```

## Development Workflow

1. **Make code changes**
2. **Build new image:**

   ```bash
   make docker-build IMG=localhost:5000/wva:dev
   ```

3. **Load image to Kind:**

   ```bash
   kind load docker-image localhost:5000/wva:dev --name kind-inferno-gpu-cluster
   ```

4. **Update deployment:**

   ```bash
   kubectl set image deployment/workload-variant-autoscaler-controller-manager \
     -n workload-variant-autoscaler-system \
     manager=localhost:5000/wva:dev
   ```

5. **Verify changes:**

   ```bash
   kubectl logs -n workload-variant-autoscaler-system \
     deployment/workload-variant-autoscaler-controller-manager -f
   ```

## Clean Up

**Remove deployments:**

```bash
make undeploy-wva-emulated-on-kind
```

**Remove deployments and delete the Kind cluster:**

```bash
make undeploy-wva-emulated-on-kind-delete-cluster
```

**Destroy cluster:**

```bash
make destroy-kind-cluster
```

## Next Steps

- [Run E2E tests](../../docs/developer-guide/testing.md#e2e-tests)
- [Development Guide](../../docs/developer-guide/development.md)
- [Testing Guide](../../docs/developer-guide/testing.md)
