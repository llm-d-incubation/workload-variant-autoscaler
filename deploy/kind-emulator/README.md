# Local Development with Kind Emulator

Quick start guide for local development using Kind (Kubernetes in Docker) with emulated GPU resources.

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
make deploy-llm-d-wva-emulated-on-kind
```

This creates:

- Kind cluster with 3 nodes, emulated GPUs (mixed vendors)
- WVA controller
- llm-d infrastructure (simulation mode)
- Prometheus monitoring
- vLLM emulator

## Configuration Options

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `HF_TOKEN` | HuggingFace token (required) | - |
| `WELL_LIT_PATH_NAME` | Name of the deployed well-lit path | `inference-scheduling` |
| `LLMD_NS` | llm-d namespace | `llm-d-$WELL_LIT_PATH_NAME` |
| `MONITORING_NAMESPACE` | Prometheus monitoring namespace | `openshift-user-workload-monitoring` |
| `MODEL_ID` | Model to deploy | `unsloth/Meta-Llama-3.1-8B` |
| `ACCELERATOR_TYPE` | GPU type (auto-detected) | `H100` |
| `SLO_TPOT` | Time per Output Token SLO target for the deployed model and GPU type | `9` |
| `SLO_TTFT` | Time to First Token SLO target for the deployed model and GPU type | `1000` |
| `WVA_IMAGE_REPO` | WVA controller image base repository | `ghcr.io/llm-d/workload-variant-autoscaler` |
| `WVA_IMAGE_TAG` | WVA controller image tag | `v0.0.1` |
| `LLM_D_RELEASE` | llm-d release version | `v0.3.0` |
| `LLM_D_MODELSERVICE_NAME` | Name of the ModelService deployed by llm-d | `ms-$WELL_LIT_PATH_NAME-llm-d-modelservice-decode` |
| `PROM_CA_CERT_PATH` | Path for the Prometheus certificate | `/tmp/prometheus-ca.crt` |
| `VLLM_SVC_ENABLED` | Flag to enable deployment of the Service exposing vLLM Deployment | `true` |
| `VLLM_SVC_NODEPORT` | Port used as NodePort by the Service | `ms-$WELL_LIT_PATH_NAME-llm-d-modelservice-decode` |
| `GATEWAY_PROVIDER` | Deployed Gateway API implementation | `kgateway` |
| `INSTALL_GATEWAY_CTRLPLANE` | Need to install Gateway Control Plane | `false` |

*Note*: the emulated environment currently uses the `llm-d-infra v1.3.1` release, using kgateway, for retrocompatibility with vLLM-emulator. This will change once the `llm-d-inference-sim` implements the required metrics needed for scaling with WVA. For more information, see this [issue](https://github.com/llm-d/llm-d-inference-sim/issues/211).

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

**2. Deploy WVA only:**

```bash
export DEPLOY_WVA=true
export DEPLOY_LLM_D=false
export DEPLOY_PROMETHEUS=true # Prometheus is needed for WVA to scrape metrics
export VLLM_SVC_ENABLED=true
export DEPLOY_PROMETHEUS_ADAPTER=false
export DEPLOY_HPA=false
make deploy-llm-d-wva-emulated-on-kind
```

**3. Deploy with llm-d (by default):**

```bash
make deploy-llm-d-wva-emulated-on-kind
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

### deploy-wva.sh

Deploys WVA controller to existing cluster.

```bash
./deploy-wva.sh
```

### deploy-llm-d.sh

Deploys WVA with llm-d infrastructure.

```bash
./deploy-llm-d.sh -i <your-image>
```

### undeploy-llm-d.sh

Removes WVA and llm-d infrastructure.

```bash
./undeploy-llm-d.sh
```

### teardown.sh

Destroys the Kind cluster.

```bash
./teardown.sh
```

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

**Port-forward vLLM emulator:**

```bash
kubectl port-forward -n llm-d-sim svc/vllme-service 8000:80
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

### 3. Generate Load

```bash
cd ../../tools/vllm-emulator

# Install dependencies
pip install -r requirements.txt

# Run load generator
python loadgen.py \
  --model default/default \
  --rate '[[120, 60]]' \
  --url http://localhost:8000/v1 \
  --content 50
```

### 4. Monitor

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
make undeploy-llm-d-wva-emulated-on-kind
```

**Remove deployments and delete the Kind cluster:**

```bash
make undeploy-llm-d-wva-emulated-on-kind-delete-cluster
```

**Destroy cluster:**

```bash
make destroy-kind-cluster
```

**Or use scripts directly:**

```bash
./undeploy-llm-d.sh
./teardown.sh
```

## Next Steps

- [Run E2E tests](../../docs/developer-guide/testing.md#e2e-tests)
- [Development Guide](../../docs/developer-guide/development.md)
- [Testing Guide](../../docs/developer-guide/testing.md)
