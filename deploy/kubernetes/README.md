# Workload-Variant-Autoscaler Kubernetes Deployment Script

Automated deployment script for WVA, llm-d infrastructure, Prometheus, and HPA on Kubernetes clusters.

## Overview

This script automates the complete deployment process on kubernetes cluster including:

- **kube-prometheus-stack** (Prometheus + Grafana + Alertmanager)
- **Workload-Variant-Autoscaler** controller with metrics validation
- **llm-d infrastructure** (Gateway, Scheduler, vLLM)
- **Prometheus Adapter** for external metrics
- **HPA** integration for autoscaling
- **ServiceMonitors** for metrics collection
- **VariantAutoscaling** custom resources
- All required ConfigMaps and RBAC

## Prerequisites

### Required Tools

- **kubectl** - Kubernetes CLI
- **helm** (v3+) - Package manager
- **git** - Version control
- **jq** - JSON processor (optional, for verification)
- **yq** (v4+) - YAML processor (optional, used if available)

### Required Access

- Kubernetes cluster with **cluster-admin** privileges (or sufficient RBAC)
- `kubectl` configured and connected to your cluster
- GPUs available in the cluster (or use emulator mode for demo)

### Required Tokens

- **HuggingFace token** for model downloads (required for llm-d deployment)

## Quick Start

### 1. Set Environment Variables

```bash
# Required: Set your HuggingFace token
export HF_TOKEN="your-hf-token-here"

# Optional: Customize deployment
export WELL_LIT_PATH_NAME="inference-scheduling"                          # Default
export MODEL_ID="unsloth/Meta-Llama-3.1-8B"                               # Default
export WVA_IMAGE_REPO="ghcr.io/llm-d/workload-variant-autoscaler"         # Default
export WVA_IMAGE_TAG="v0.0.1"                                             # Default
export ACCELERATOR_TYPE="A100"                                            # Auto-detected or default
```

### 2. Run the Deployment Script using Make

```bash
make deploy-wva-on-k8s
```

That's it! The script will:

1. Check prerequisites
2. Detect GPU types
3. Create namespaces  (by default)
4. Deploy Prometheus stack  (by default)
5. Deploy WVA controller  (by default)
6. Deploy llm-d infrastructure  (by default)
7. Deploy the Prometheus Adapter (by default)
8. Create the VariantAutoscaling resource for the vLLM deployment  (by default)
9. Deploy HPA  (by default)
10. Verify the deployment process
11. Print a summary with next steps

## Configuration Options

### Environment Variables

| Variable | Description | Default |
|----------|-------------|---------|
| `HF_TOKEN` | HuggingFace token (required) | - |
| `WELL_LIT_PATH_NAME` | Name of the deployed well-lit path | `inference-scheduling` |
| `LLMD_NS` | llm-d namespace | `llm-d-$WELL_LIT_PATH_NAME` |
| `MONITORING_NAMESPACE` | Prometheus monitoring namespace | `openshift-user-workload-monitoring` |
| `MODEL_ID` | Model to deploy | `unsloth/Meta-Llama-3.1-8B` |
| `ACCELERATOR_TYPE` | GPU type (auto-detected) | `A100` |
| `SLO_TPOT` | Time per Output Token SLO target for the deployed model and GPU type | `9` |
| `SLO_TTFT` | Time to First Token SLO target for the deployed model and GPU type | `1000` |
| `WVA_IMAGE_REPO` | WVA controller image base repository | `ghcr.io/llm-d/workload-variant-autoscaler` |
| `WVA_IMAGE_TAG` | WVA controller image tag | `v0.0.1` |
| `LLM_D_RELEASE` | llm-d release version | `v0.3.0` |
| `LLM_D_MODELSERVICE_NAME` | Name of the ModelService deployed by llm-d | `ms-$WELL_LIT_PATH_NAME-llm-d-modelservice-decode` |
| `PROM_CA_CERT_PATH` | Path for the Prometheus certificate | `/tmp/prometheus-ca.crt` |
| `VLLM_SVC_ENABLED` | Flag to enable deployment of the Service exposing vLLM Deployment | `true` |
| `VLLM_SVC_NODEPORT` | Port used as NodePort by the Service | `ms-$WELL_LIT_PATH_NAME-llm-d-modelservice-decode` |
| `GATEWAY_PROVIDER` | Deployed Gateway API implementation | `istio` |
| `BENCHMARK_MODE` | Deploying using benchmark configuration for Istio | `true` |
| `INSTALL_GATEWAY_CTRLPLANE` | Need to install Gateway Control Plane | `false` |

### Deployment Flags

Control which components to deploy:

```bash
# Deploy specific components
export DEPLOY_PROMETHEUS=true          # Deploy kube-prometheus-stack
export DEPLOY_WVA=true                # Deploy WVA controller
export DEPLOY_LLM_D=true              # Deploy llm-d infrastructure
export DEPLOY_PROMETHEUS_ADAPTER=true # Deploy Prometheus Adapter
export DEPLOY_HPA=true                # Deploy HPA
export DEPLOY_VLLM_EMULATOR=false     # Use emulator if no GPUs (auto-detected)
export SKIP_CHECKS=false              # Skip prerequisite checks
```

## Usage Examples

### Example 1: Full Deployment (Default)

```bash
export HF_TOKEN="hf_xxxxx"
make deploy-wva-on-k8s
```

### Example 2: Custom Model and Namespace

```bash
export HF_TOKEN="hf_xxxxx"
export BASE_NAME="my-inference"
export MODEL_ID="meta-llama/Llama-2-7b-hf"
make deploy-wva-on-k8s
```

### Example 3: Deploy Only WVA (llm-d Already Deployed)

```bash
export DEPLOY_WVA=true
export DEPLOY_LLM_D=false
export DEPLOY_PROMETHEUS=true # Prometheus is needed for WVA to scrape metrics
export VLLM_SVC_ENABLED=true
export DEPLOY_PROMETHEUS_ADAPTER=false
export DEPLOY_HPA=false
make deploy-wva-on-k8s
```

### Example 4: Demo Mode (No GPUs Available)

```bash
export HF_TOKEN="hf_xxxxx"
export USE_VLLM_EMULATOR=true
make deploy-wva-on-k8s
```

### Example 5: Deploy with Different WVA Image

```bash
export HF_TOKEN="hf_xxxxx"
export IMG="ghcr.io/yourorg/workload-variant-autoscaler:latest"
make deploy-wva-on-k8s
```

## Script Features

### Automatic Detection

- **GPU Type**: Automatically detects A100, H100, L40S, etc.
- **GPU Availability**: Detects if GPUs are visible to Kubernetes
- **Emulator Mode**: Auto-enables if no GPUs detected
- **Kubernetes Connection**: Verifies cluster connectivity

### Error Handling

- Exits on any error (`set -e`)
- Validates prerequisites before starting
- Checks for required environment variables
- Provides detailed error messages

### Progress Tracking

- Color-coded output (INFO, SUCCESS, WARNING, ERROR)
- Step-by-step progress indicators
- Detailed logging of each operation

### Deployment Verification

After deployment, the script verifies:

- WVA controller is running
- Prometheus stack is deployed
- llm-d infrastructure is deployed
- VariantAutoscaling resource exists
- HPA is configured
- ServiceMonitors are created

### Summary Report

Displays:

- All deployed components
- Resource names and namespaces
- Next steps and useful commands
- Troubleshooting tips

## What Gets Deployed

### 1. kube-prometheus-stack

- **Namespace**: `workload-variant-autoscaler-monitoring`
- **Components**:
  - Prometheus server (HTTPS/TLS enabled)
  - Grafana dashboards
  - Alertmanager
  - kube-state-metrics
  - node-exporter
  - ServiceMonitor CRDs and operators

### 2. Workload-Variant-Autoscaler

- **Namespace**: `workload-variant-autoscaler-system`
- **Components**:
  - Controller manager deployment (2 replicas)
  - Service for metrics (port 8443)
  - ServiceMonitor for WVA metrics
  - ConfigMaps (service classes, accelerator costs, config)
  - RBAC (roles, bindings, service account)

### 3. llm-d Infrastructure

- **Namespace**: `llm-d-inference-scheduling` (default)
- **Components**:
  - Gateway (kgateway v2.0.3)
  - Inference Scheduler (GAIE/EPP)
  - vLLM deployment with model
  - Service for vLLM (port 8200)
  - ServiceMonitor for vLLM metrics
  - HuggingFace token secret

### 4. Prometheus Adapter

- **Namespace**: `workload-variant-autoscaler-monitoring`
- **Components**:
  - Prometheus Adapter deployment
  - External metrics API configuration
  - RBAC for cluster monitoring
  - TLS configuration for Prometheus access

### 5. Autoscaling Resources

- **VariantAutoscaling**: Custom resource for WVA optimization
- **HPA**: HorizontalPodAutoscaler for deployment scaling
- **ServiceMonitors**: Metrics collection configuration

## Namespaces Overview

```
┌─────────────────────────────────────────────────────────────┐
│ workload-variant-autoscaler-system                          │
│   └─ WVA Controller (watches VariantAutoscaling CRs)        │
├─────────────────────────────────────────────────────────────┤
│ workload-variant-autoscaler-monitoring                      │
│   ├─ Prometheus (scrapes metrics)                           │
│   ├─ Prometheus Adapter (external metrics API)              │
│   ├─ ServiceMonitor: WVA metrics                            │
│   └─ ServiceMonitor: vLLM metrics                           │
├─────────────────────────────────────────────────────────────┤
│ llm-d-inference-scheduling                                  │
│   ├─ vLLM Deployment (exposes /metrics)                     │
│   ├─ Gateway (request routing)                              │
│   ├─ GAIE/EPP (endpoint picking)                            │
│   ├─ VariantAutoscaling CR                                  │
│   └─ HPA (scales based on inferno_desired_replicas)         │
├─────────────────────────────────────────────────────────────┤
│ Istio                                    │
└─────────────────────────────────────────────────────────────┘
```

## Troubleshooting

### Script Fails: Missing Prerequisites

```bash
[ERROR] Missing required tools: helm kubectl
```

**Solution**: Install missing tools:

```bash
# Using package manager
sudo apt-get install helm kubectl  # Debian/Ubuntu
brew install helm kubectl          # macOS

# Or download from official sites
```

### Script Fails: Cannot Connect to Kubernetes

```bash
[ERROR] Cannot connect to Kubernetes cluster
```

**Solution**: Check kubectl configuration:

```bash
kubectl cluster-info
kubectl get nodes
```

### Script Fails: HF_TOKEN Not Set

```bash
[ERROR] HF_TOKEN environment variable is not set
```

**Solution**: Set your HuggingFace token:

```bash
export HF_TOKEN="hf_xxxxxxxxxxxxxxxxxxxxx"
```

### GPUs Not Visible to Kubernetes

**Symptoms**:

```bash
[WARNING] No GPUs visible to Kubernetes
vLLM pods: Pending
Reason: Insufficient nvidia.com/gpu
```

**Diagnosis**:

```bash
# Check if GPUs exist on host
nvidia-smi

# Check if GPUs visible to Kubernetes
kubectl get nodes -o json | jq '.items[].status.allocatable["nvidia.com/gpu"]'
```

**Solution**:

**Option 1**: Install NVIDIA GPU Operator

```bash
helm repo add nvidia https://helm.ngc.nvidia.com/nvidia
helm repo update
helm install --wait --generate-name \
  -n gpu-operator --create-namespace \
  nvidia/gpu-operator
```

**Option 2**: Install NVIDIA Device Plugin

```bash
kubectl create -f https://raw.githubusercontent.com/NVIDIA/k8s-device-plugin/v0.14.0/nvidia-device-plugin.yml
```

**Option 3**: Use Emulator Mode (for demo/testing)

```bash
export USE_VLLM_EMULATOR=true
make deploy-wva-on-k8s
```

**Note for KIND clusters**: KIND (Kubernetes IN Docker) requires special configuration for GPU passthrough. Consider using a real Kubernetes cluster or emulator mode.

### Metrics Not Available After Deployment

**Wait 1-2 minutes** for:

- vLLM pods to start
- Prometheus to scrape metrics
- Prometheus Adapter to process them
- External metrics API to update

**Check metrics availability**:

```bash
# Check WVA logs
kubectl logs -n workload-variant-autoscaler-system -l control-plane=controller-manager --tail=50

# Look for metrics validation
kubectl logs -n workload-variant-autoscaler-system -l control-plane=controller-manager | grep "Metrics unavailable"

# Check if vLLM is exposing metrics
kubectl port-forward -n llm-d-inference-scheduling <vllm-pod> 8200:8200
curl http://localhost:8200/metrics | grep vllm:
```

### vLLM Pods Not Starting

**Check logs**:

```bash
kubectl logs -n llm-d-inference-scheduling deployment/ms-inference-scheduling-llm-d-modelservice-decode
kubectl describe pod -n llm-d-inference-scheduling -l llm-d.ai/model
```

**Common issues**:

- Insufficient GPU resources
- HuggingFace token invalid/expired
- Model download timeout
- Image pull errors

### ServiceMonitor Not Scraping

**Verify ServiceMonitor exists**:

```bash
kubectl get servicemonitor -n workload-variant-autoscaler-monitoring
```

**Check Prometheus targets**:

```bash
kubectl port-forward -n workload-variant-autoscaler-monitoring svc/kube-prometheus-stack-prometheus 9090:9090
# Visit http://localhost:9090/targets
```

**Verify service selector matches**:

```bash
kubectl get servicemonitor vllm-servicemonitor -n workload-variant-autoscaler-monitoring -o yaml
kubectl get svc -n llm-d-inference-scheduling --show-labels
```

## Post-Deployment

### Verify Deployment

```bash
# Check all namespaces
kubectl get pods -n workload-variant-autoscaler-system
kubectl get pods -n workload-variant-autoscaler-monitoring
kubectl get pods -n llm-d-inference-scheduling

# Check VariantAutoscaling (with NEW MetricsReady column!)
kubectl get variantautoscaling -n llm-d-inference-scheduling -o wide

# Check detailed status with conditions
kubectl describe variantautoscaling ms-inference-scheduling-llm-d-modelservice-decode -n llm-d-inference-scheduling

# Check HPA
kubectl get hpa -n llm-d-inference-scheduling

# Check external metrics
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/llm-d-inference-scheduling/inferno_desired_replicas" | jq
```

### Monitor WVA Logs (See Metrics Validation!)

```bash
# Watch live logs
kubectl logs -n workload-variant-autoscaler-system \
  -l control-plane=controller-manager \
  -f

# Filter for metrics validation messages
kubectl logs -n workload-variant-autoscaler-system \
  -l control-plane=controller-manager | \
  jq 'select(.reason=="MetricsMissing" or .reason=="MetricsFound")'
```

### Access Prometheus UI

```bash
kubectl port-forward -n workload-variant-autoscaler-monitoring \
  svc/kube-prometheus-stack-prometheus 9090:9090

# Visit http://localhost:9090
# Query: vllm:request_success_total
# Query: inferno_desired_replicas
```

### Access Grafana Dashboards

```bash
# Get admin password
kubectl get secret -n workload-variant-autoscaler-monitoring \
  kube-prometheus-stack-grafana \
  -o jsonpath="{.data.admin-password}" | base64 -d ; echo

# Port forward
kubectl port-forward -n workload-variant-autoscaler-monitoring \
  svc/kube-prometheus-stack-grafana 3000:80

# Visit http://localhost:3000
# Username: admin
# Password: <from above command>
```

### Generate Load (Test Autoscaling)

If using real vLLM with model loaded:

```bash
# Deploy guidellm load generator
kubectl apply -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: guidellm-load-test
  namespace: llm-d-inference-scheduling
spec:
  template:
    spec:
      containers:
      - name: guidellm
        image: quay.io/vishakharamani/guidellm:latest
        args:
          - benchmark
          - --target=http://infra-inference-scheduling-inference-gateway:80
          - --rate-type=constant
          - --rate=10
          - --max-seconds=300
          - --model=unsloth/Meta-Llama-3.1-8B
      restartPolicy: Never
EOF
```

Watch the autoscaling:

```bash
# Watch VariantAutoscaling status update
kubectl get variantautoscaling -n llm-d-inference-scheduling -w

# Watch HPA scaling
kubectl get hpa -n llm-d-inference-scheduling -w

# Watch pod count change
kubectl get pods -n llm-d-inference-scheduling -w
```

## Cleanup

To remove all deployed components:

```bash
make undeploy-wva-on-k8s
```

Or manually:

```bash
# Delete llm-d infrastructure
cd llm-d-infra/quickstart/examples/inference-scheduling
helmfile destroy -e kgateway

# Delete Prometheus Adapter
helm uninstall prometheus-adapter -n workload-variant-autoscaler-monitoring

# Delete kube-prometheus-stack
helm uninstall kube-prometheus-stack -n workload-variant-autoscaler-monitoring

# Delete WVA
cd /path/to/workload-variant-autoscaler
kubectl delete -k config/default

# Delete namespaces
kubectl delete namespace llm-d-inference-scheduling
kubectl delete namespace workload-variant-autoscaler-system
kubectl delete namespace workload-variant-autoscaler-monitoring
```

## Metrics Validation Feature

This deployment includes the **NEW metrics health monitoring system**:

### What It Does

1. **Validates vLLM metrics** before optimization
2. **Sets status conditions** on VariantAutoscaling:
   - `MetricsAvailable` - Are vLLM metrics available?
   - `OptimizationReady` - Can optimization run?
3. **Provides actionable errors** when ServiceMonitor isn't working
4. **Implements graceful degradation** when metrics unavailable

### Viewing Metrics Health

```bash
# See MetricsReady column
kubectl get variantautoscaling -n llm-d-inference-scheduling

# Example output:
# NAME        MODEL           ACCELERATOR  CURRENT  OPTIMIZED  METRICSREADY  AGE
# my-variant  llama-3-8b      A100         2        3          True          5m
```

### Viewing Detailed Conditions

```bash
kubectl describe variantautoscaling ms-inference-scheduling-llm-d-modelservice-decode \
  -n llm-d-inference-scheduling

# Look for:
# Status:
#   Conditions:
#     Type:     MetricsAvailable
#     Status:   True/False
#     Reason:   MetricsFound/MetricsMissing/MetricsStale/PrometheusError
#     Message:  Detailed troubleshooting information
```

### Understanding Metrics Validation Logs

When metrics are unavailable, you'll see structured logs like:

```json
{
  "level": "WARN",
  "ts": "2025-10-13T18:36:52.670Z",
  "msg": "Metrics unavailable, skipping optimization for variant",
  "variant": "ms-inference-scheduling-llm-d-modelservice-decode",
  "namespace": "llm-d-inference-scheduling",
  "model": "meta-llama/Llama-3.1-8B",
  "reason": "MetricsMissing",
  "troubleshooting": "Check: (1) ServiceMonitor exists in monitoring namespace..."
}
```

This means:

- WVA is working correctly
- Detecting no metrics available
- Skipping optimization gracefully
- Providing troubleshooting steps
- Will retry automatically

## Architecture

### ServiceMonitor Namespace Strategy

**Key Decision**: ServiceMonitors are in `workload-variant-autoscaler-monitoring`, NOT in the same namespace as the apps they monitor.

```
ServiceMonitors:     workload-variant-autoscaler-monitoring
WVA Controller:      workload-variant-autoscaler-system
vLLM Pods:           llm-d-inference-scheduling
```

### Metrics Flow

```
┌─────────────────────────────────────────────────────────────┐
│ vLLM Pods (llm-d-inference-scheduling)                      │
│   └─ Expose /metrics on port 8200                           │
└─────────────────────────────────────────────────────────────┘
                         ↓ (scraped by)
┌─────────────────────────────────────────────────────────────┐
│ ServiceMonitor (workload-variant-autoscaler-monitoring)     │
│   └─ Tells Prometheus where to scrape                       │
└─────────────────────────────────────────────────────────────┘
                         ↓ (configures)
┌─────────────────────────────────────────────────────────────┐
│ Prometheus (workload-variant-autoscaler-monitoring)         │
│   └─ Scrapes and stores vLLM metrics                        │
└─────────────────────────────────────────────────────────────┘
                         ↓ (queries)
┌─────────────────────────────────────────────────────────────┐
│ WVA Controller (workload-variant-autoscaler-system)         │
│   ├─ ValidateMetricsAvailability() ← NEW!                   │
│   ├─ Collect metrics from Prometheus                        │
│   ├─ Run WVA optimization                               │
│   ├─ Set status conditions ← NEW!                           │
│   └─ Emit inferno_desired_replicas metric                   │
└─────────────────────────────────────────────────────────────┘
                         ↓ (exposed by)
┌─────────────────────────────────────────────────────────────┐
│ Prometheus Adapter (workload-variant-autoscaler-monitoring) │
│   └─ Exposes as external.metrics.k8s.io API                 │
└─────────────────────────────────────────────────────────────┘
                         ↓ (consumed by)
┌─────────────────────────────────────────────────────────────┐
│ HPA (llm-d-inference-scheduling)                            │
│   └─ Scales vLLM deployment based on inferno_desired_replicas│
└─────────────────────────────────────────────────────────────┘
```

## Differences from OpenShift Script

### Key Changes for Kubernetes

1. **Prometheus**:
   - OpenShift: Uses Thanos querier
   - Kubernetes: Deploys kube-prometheus-stack

2. **Monitoring Namespace**:
   - OpenShift: `openshift-user-workload-monitoring`
   - Kubernetes: `workload-variant-autoscaler-monitoring`

3. **RBAC**:
   - OpenShift: Uses `oc adm policy` commands
   - Kubernetes: Standard RBAC via manifests

4. **Service Type**:
   - OpenShift: Often uses NodePort or Routes
   - Kubernetes: Uses ClusterIP (can customize)

5. **GPU Detection**:
   - Both: Auto-detect GPU type
   - Kubernetes: Also checks for device plugin/operator

## Advanced Usage

### Custom Prometheus Configuration

```bash
# Use existing Prometheus
export DEPLOY_PROMETHEUS=false
export PROMETHEUS_URL="https://my-prometheus.monitoring.svc:9090"
make deploy-wva-on-k8s
```

### Deploy to Specific Cluster Context

```bash
kubectl config use-context my-cluster
make deploy-wva-on-k8s
```

### Debug Mode

```bash
# Enable debug logging in WVA
kubectl set env deployment/workload-variant-autoscaler-controller-manager \
  LOG_LEVEL=debug \
  -n workload-variant-autoscaler-system
```

### Update WVA Image

```bash
export WVA_IMAGE="ghcr.io/yourorg/workload-variant-autoscaler:custom-tag"
export DEPLOY_LLM_D=false  # Don't redeploy llm-d
export DEPLOY_PROMETHEUS=false  # Don't redeploy Prometheus
make deploy-wva-on-k8s
```

## Performance Tuning

### Optimization Interval

```bash
# Change how often WVA runs optimization (default: 60s)
kubectl patch configmap workload-variant-autoscaler-variantautoscaling-config \
  -n workload-variant-autoscaler-system \
  --type merge \
  -p '{"data":{"GLOBAL_OPT_INTERVAL":"30s"}}'

# Restart WVA to apply
kubectl rollout restart deployment workload-variant-autoscaler-controller-manager \
  -n workload-variant-autoscaler-system
```

### HPA Tuning

```bash
# Faster scale-up
kubectl patch hpa vllm-deployment-hpa -n llm-d-inference-scheduling --type merge -p '
{
  "spec": {
    "behavior": {
      "scaleUp": {
        "stabilizationWindowSeconds": 0,
        "policies": [{"type": "Pods", "value": 10, "periodSeconds": 15}]
      }
    }
  }
}'
```

## Contributing

When modifying the script:

1. Follow the existing function structure
2. Add error handling for new operations
3. Update this documentation
4. Test on a clean Kubernetes cluster
5. Maintain compatibility with both GPU and non-GPU clusters

## Related Documentation

- `docs/metrics-health-monitoring.md`: Metrics validation feature guide
- `docs/hpa-integration.md`: HPA integration guide
- `README.md`: Main project documentation

## Support

For issues or questions:

1. Check the [troubleshooting section](#troubleshooting)
2. Check WVA and llm-d logs
3. Review `docs/metrics-health-monitoring.md` for metrics issues
4. Open an issue on GitHub
