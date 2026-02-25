# ScaleFromZero Feature Guide

## Overview

The ScaleFromZero feature enables the Workload-Variant-Autoscaler (WVA) to automatically scale up inactive model deployments (with 0 replicas) when there are pending inference requests. This feature helps optimize resource utilization by allowing deployments to scale down to zero during periods of inactivity, while ensuring rapid scale-up when demand returns.

## How It Works

The ScaleFromZero engine continuously monitors inactive VariantAutoscaling resources (those with `replicas == 0`) and checks for pending requests in the inference gateway's flow control queue. When pending requests are detected for a specific model, the engine automatically scales the corresponding deployment from 0 to 1 replica.

### Architecture

```
┌─────────────────────────────────────────────────────────────┐
│  ScaleFromZero Engine (Polling Loop - 100ms interval)     │
└─────────────────────────────────────────────────────────────┘
                          │
                          ▼
┌─────────────────────────────────────────────────────────────┐
│  1. Query all inactive VariantAutoscaling resources         │
│     (replicas == 0)                                          │
└─────────────────────────────────────────────────────────────┘
                          │
                          ▼
┌─────────────────────────────────────────────────────────────┐
│  2. For each inactive variant (concurrent processing):      │
│     - Find associated InferencePool via labels              │
│     - Query EPP metrics source for queue metrics            │
│     - Check inference_extension_flow_control_queue_size     │
└─────────────────────────────────────────────────────────────┘
                          │
                          ▼
┌─────────────────────────────────────────────────────────────┐
│  3. If pending requests exist for the model:                │
│     - Scale target deployment from 0 → 1 replica            │
│     - Update VariantDecision cache                          │
│     - Update VariantAutoscaling status                      │
│     - Trigger reconciler                                    │
└─────────────────────────────────────────────────────────────┘
```

## Prerequisites

- WVA and llm-d installed and running - deployment options available for [kind](https://github.com/llm-d/llm-d-workload-variant-autoscaler/blob/main/deploy/kind-emulator/README.md), [OpenShift](https://github.com/llm-d/llm-d-workload-variant-autoscaler/blob/main/deploy/openshift/README.md) and [Kubernetes](https://github.com/llm-d/llm-d-workload-variant-autoscaler/blob/main/deploy/kubernetes/README.md)
- EndpointPicker (EPP) configured with flowcontrol enabled - required for queue metrics collection (set EPP env variable `ENABLE_EXPERIMENTAL_FLOW_CONTROL_LAYER`)


## Usage

### Basic Setup

1. **Deploy your model server with 0 replicas** (or let it scale down to 0):

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: llama-8b-deployment
spec:
  replicas: 0  # Start at zero
  selector:
    matchLabels:
      app: llama-8b
      model: meta-llama-3.1-8b
  template:
    metadata:
      labels:
        app: llama-8b
        model: meta-llama-3.1-8b
    spec:
      containers:
      - name: vllm
        image: vllm/vllm-openai:latest
        # ... container configuration
```

2. **Create a VariantAutoscaling resource**:

```yaml
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: llama-8b-autoscaler
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: llama-8b-deployment
  modelID: "meta/llama-3.1-8b"
  variantCost: "10.0"
```

3. **Create an InferencePool** that references your deployment:

```yaml
apiVersion: inference.networking.x-k8s.io/v1alpha2
kind: InferencePool
metadata:
  name: llama-pool
spec:
  selector:
    matchLabels:
      app: llama-8b
      model: meta-llama-3.1-8b
  targetPorts:
  - 8080
  endpointPickerRef:
    name: epp-service
```

4. **Send inference requests**:

   For example: expose the inference gateway service port to the host machine:
   ```bash
   kubectl port-forward svc/infra-inference-gateway 8080:80
   ```

   Then, send an inference request:

   ```bash
   curl -X POST http://localhost:8080/v1/completions \
     -H "Content-Type: application/json" \
     -d '{
       "model": "meta/llama-3.1-8b",
       "prompt": "The capital of France is",
       "max_tokens": 10
     }' | jq
   ```

   The ScaleFromZero engine will automatically detect pending requests and scale up the deployment.

## Monitoring

### Key Metrics

Monitor the ScaleFromZero feature using these metrics:

1. **Queue Size Metric** (from EPP):
   ```promql
   inference_extension_flow_control_queue_size{target_model_name="meta/llama-3.1-8b"}
   ```

2. **VariantAutoscaling Status**:
   ```bash
   kubectl get va
   ```
   
   Output:
   ```console
   NAME                  TARGET                  MODEL                    OPTIMIZED   METRICSREADY   AGE
   llama-8b-autoscaler   llama-8b-deployment     meta/llama-3.1-8b           1           True         5m
   ```

3. **Deployment Replicas**:
   ```bash
   kubectl get deployment llama-8b-deployment
   ```

### Logs

Enable debug logging to see ScaleFromZero operations:

```bash
# View controller logs
kubectl logs -n workload-variant-autoscaler-system \
  deployment/workload-variant-autoscaler-controller-manager \
  -f --tail=100
```

Look for log messages like:
```
Found inactive VariantAutoscaling resources count=3
Processing variant name=llama-8b-autoscaler
Target workload has pending requests, scaling up from zero metricName=inference_extension_flow_control_queue_size value=5
Successfully scaled up Target Workload variant=llama-8b-autoscaler target VA model=meta/llama-3.1-8b
```