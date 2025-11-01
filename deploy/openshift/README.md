# Workload-Variant-Autoscaler OpenShift Deployment Script

Automated deployment script for WVA and llm-d infrastructure on OpenShift clusters.

## Overview

This script automates the complete deployment process on OpenShift cluster including:

- Workload-Variant-Autoscaler controller
- llm-d infrastructure (Gateway, Scheduler, vLLM)
- Prometheus Adapter for external metrics
- HPA integration
- All required ConfigMaps and RBAC
- Automatic GPU detection
- Deployment verification

## Prerequisites

### Required Tools

- **oc** (OpenShift CLI)
- **kubectl**
- **helm** (v3+)
- **yq** (v4+)
- **jq**
- **git**

### Required Access

- OpenShift cluster with **admin** privileges
- Logged in via `oc login`
- GPUs available in the cluster (H100, A100, L40S...)
*Note* to check the available GPU types on your OCP cluster, you can run:

```bash
kubectl get nodes -o jsonpath='{range .items[?(@.status.allocatable.nvidia\.com/gpu)]}{.metadata.name}{"\t"}{.metadata.labels.nvidia\.com/gpu\.product}{"\n"}{end}'
```

### Required Tokens

- **HuggingFace token** for model downloads

## Quick Start

### 1. Set Environment Variables

```bash
# Required: Set your HuggingFace token
export HF_TOKEN="your-hf-token-here"

# Optional: Customize deployment
export BASE_NAME="inference-scheduling"              # Default
export MODEL_ID="unsloth/Meta-Llama-3.1-8B"         # Default
export WVA_IMAGE="ghcr.io/llm-d/workload-variant-autoscaler:v0.0.1"  # Default
```

### 2. Deploy the Workload Variant Autoscaler and llm-d using Make

```bash
make deploy-wva-on-openshift
```

That's it! The script will:

1. Check prerequisites

2. Detect GPU types on your OpenShift cluster

3. Deploy all components, including WVA, llm-d, and the Prometheus-Adapter for HPA

4. Verify the deployment

5. Print a summary with next steps

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
| `GATEWAY_PROVIDER` | Deployed Gateway API implementation | `istio` |
| `BENCHMARK_MODE` | Deploying using benchmark configuration for Istio | `true` |
| `INSTALL_GATEWAY_CTRLPLANE` | Need to install Gateway Control Plane | `false` |

*Note*: when `true`, the `BENCHMARK_MODE` will **override** any `GATEWAY_PROVIDER` previously set to use the benchmark configuration for Istio.

### Deployment Flags

Control which components to deploy:

```bash
# Deploy only specific components
export DEPLOY_WVA=true                    # Deploy WVA controller
export DEPLOY_LLM_D=true                  # Deploy llm-d infrastructure
export DEPLOY_PROMETHEUS_ADAPTER=true     # Deploy Prometheus Adapter
export SKIP_CHECKS=false                  # Skip prerequisite checks
```

## Usage Examples

### Example 1: Full Deployment (Default)

```bash
export HF_TOKEN="hf_xxxxx"
make deploy-llm-d-wva-on-openshift
```

### Example 2: Custom Model and Namespace

```bash
export HF_TOKEN="hf_xxxxx"
export BASE_NAME="my-inference"
export MODEL_ID="meta-llama/Llama-2-7b-hf"
make deploy-llm-d-wva-on-openshift
```

### Example 3: Deploy Only WVA (llm-d Already Deployed)

```bash
export DEPLOY_WVA=true
export DEPLOY_LLM_D=false
export DEPLOY_PROMETHEUS_ADAPTER=false
make deploy-llm-d-wva-on-openshift
```

### Example 4: Re-run with Different GPU Type

```bash
export HF_TOKEN="hf_xxxxx"
export ACCELERATOR_TYPE="A100"
make deploy-llm-d-wva-on-openshift
```

## Script Features

### Automatic Detection

- **GPU Type**: Automatically detects H100, A100, L40S etc... GPUs
- **Thanos URL**: Finds the correct Prometheus/Thanos endpoint
- **OpenShift Connection**: Verifies cluster connectivity

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

- llm-d infrastructure is deployed

- Prometheus Adapter is running

- VariantAutoscaling resource exists

- HPA is configured

- External metrics API is accessible

### Summary Report

Displays:

- All deployed components

- Resource names and namespaces

- Next steps and useful commands

- How to verify and test

## What Gets Deployed

### 1. Workload-Variant-Autoscaler

- **Namespace**: `workload-variant-autoscaler-system`
- **Components**:
  - Controller manager deployment
  - Service for metrics
  - ServiceMonitor for Prometheus
  - ConfigMaps (service classes, accelerator costs)
  - RBAC (roles, bindings, service account)

### 2. llm-d Infrastructure

- **Namespace**: `llm-d-inference-scheduling` (default)
- **Components**:
  - Gateway (kgateway)
  - Inference Scheduler (GAIE)
  - vLLM deployment with model
  - Service for vLLM
  - ServiceMonitor for vLLM metrics
  - HuggingFace token secret

### 3. Prometheus Adapter

- **Namespace**: `openshift-user-workload-monitoring`
- **Components**:
  - Prometheus Adapter deployment (2 replicas)
  - ConfigMap with CA certificate
  - RBAC for cluster monitoring
  - External metrics API configuration

### 4. Autoscaling Resources

- **VariantAutoscaling**: Custom resource for WVA optimization
- **HPA**: HorizontalPodAutoscaler for deployment scaling
- **Probes**: Health checks for vLLM pods

## Troubleshooting

### Script Fails: Missing Prerequisites

```bash
[ERROR] Missing required tools: yq helm
```

**Solution**: Install missing tools:

```bash
# macOS
brew install yq helm

# Linux
# Follow official installation guides for yq and helm
```

### Script Fails: Not Logged Into OpenShift

```bash
[ERROR] Not logged into OpenShift cluster
```

**Solution**: Log in first:

```bash
oc login --token=<your-token> --server=<your-server>
```

### Script Fails: HF_TOKEN Not Set

```bash
[ERROR] HF_TOKEN environment variable is not set
```

**Solution**: Set your HuggingFace token:

```bash
export HF_TOKEN="hf_xxxxxxxxxxxxxxxxxxxxx"
```

### Deployment Succeeds But Metrics Not Available

**Wait 1-2 minutes** for:

- Prometheus to scrape metrics

- Prometheus Adapter to process them

- External metrics API to update

**Check status**:

```bash
kubectl get pods -n openshift-user-workload-monitoring | grep prometheus-adapter
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/llm-d-inference-scheduling/inferno_desired_replicas" | jq
```

### vLLM Pods Not Starting

**Check logs**:

```bash
kubectl logs -n llm-d-inference-scheduling deployment/ms-inference-scheduling-llm-d-modelservice-decode
```

**Common issues**:

- Insufficient GPU resources

- HuggingFace token invalid/expired

- Model download timeout

- Inappropriate SLOs for the deployed model and GPU types: update the `SLO_TPOT` and `SLO_TTFT` variables with appropriate SLOs given the model and employed GPU type

## Post-Deployment

### Verify Deployment

```bash
# Check all components
kubectl get pods -n workload-variant-autoscaler-system
kubectl get pods -n llm-d-inference-scheduling
kubectl get variantautoscaling -n llm-d-inference-scheduling
kubectl get hpa -n llm-d-inference-scheduling

# Check external metrics
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/llm-d-inference-scheduling/inferno_desired_replicas" | jq
```

### Monitor WVA Logs

```bash
kubectl logs -n workload-variant-autoscaler-system \
  deployment/workload-variant-autoscaler-controller-manager \
  -f
```

### Run E2E Tests

```bash
make test-e2e-openshift
```

### Generate Load

```bash
# Create a load generation job
kubectl apply -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: vllm-bench-test
  namespace: llm-d-inference-scheduling
spec:
  template:
    spec:
      containers:
      - name: vllm-bench
        image: vllm/vllm-openai:latest
        command: ["/bin/sh", "-c"]
        args:
        - |
          python3 -m vllm.entrypoints.cli.main bench serve \
            --backend openai \
            --base-url http://infra-inference-scheduling-inference-gateway:80 \
            --model unsloth/Meta-Llama-3.1-8B \
            --request-rate 20 \
            --num-prompts 1000
      restartPolicy: Never
EOF
```

## Cleanup

To remove all deployed components:

```bash
# Delete llm-d infrastructure
helm uninstall infra-inference-scheduling -n llm-d-inference-scheduling
helm uninstall gaie-inference-scheduling -n llm-d-inference-scheduling
helm uninstall ms-inference-scheduling -n llm-d-inference-scheduling

# Delete Prometheus Adapter
helm uninstall prometheus-adapter -n openshift-user-workload-monitoring

# Delete WVA
make undeploy

# Delete namespaces
kubectl delete namespace llm-d-inference-scheduling
```

## Script Structure

```bash
deploy-llmd+wva-openshift.sh
├── Prerequisites Check
│   ├── Tool availability (oc, kubectl, helm, yq)
│   └── OpenShift connection
├── GPU Detection
│   └── Automatic GPU type identification
├── Configuration
│   ├── Find Thanos URL
│   └── Create namespace
├── Deploy WVA Controller
│   ├── Update Prometheus URL
│   ├── Deploy via make
│   ├── Create ConfigMaps
│   ├── Deploy ServiceMonitor
│   └── Add RBAC
├── Deploy llm-d Infrastructure
│   ├── Create HF token secret
│   ├── Clone llm-d-infra repo
│   ├── Install dependencies
│   ├── Install kgateway
│   ├── Configure and deploy llm-d
│   ├── Deploy Service/ServiceMonitor
│   └── Apply EPP ConfigMap fix
├── Deploy Prometheus Adapter
│   ├── Extract Thanos certificate
│   ├── Create CA ConfigMap
│   ├── Deploy via Helm
│   └── Add RBAC
├── Create Resources
│   ├── VariantAutoscaling
│   ├── HPA
│   └── vLLM probes
├── Verification
│   └── Check all components
└── Summary Report
```

## Contributing

When modifying the script:

1. Follow the existing function structure
2. Add error handling for new operations
3. Update the documentation
4. Test on a clean OpenShift cluster
5. Maintain backward compatibility

## Support

For issues or questions:

1. Check the [troubleshooting section](#troubleshooting)
2. Check WVA and llm-d logs
3. Open an issue on GitHub

## Related Documentation

- `test/e2e-openshift/README.md`: E2E testing documentation
- `README.md`: Main project documentation
