# GPU Limiter (Experimental)

> **Note**: The GPU Limiter is an experimental feature. Its API and behavior may change in future releases.

The GPU Limiter is a resource-aware scaling feature that constrains autoscaling decisions based on actual GPU availability in your cluster. It ensures that scale-up decisions are feasible and fairly distributed across multiple models competing for limited GPU resources.

## Overview

### The Problem

Without the GPU limiter, the saturation-based autoscaler makes scaling decisions based purely on workload metrics (KV cache utilization, queue length) without considering actual GPU availability. This can lead to:

- **Unfulfillable scale-up requests**: Autoscaler requests 5 replicas but only 3 GPUs are available
- **Unfair resource distribution**: One model consumes all GPUs while others starve
- **Scheduling failures**: Pods stuck in Pending state due to insufficient GPU resources
- **No cross-model coordination**: Multiple models scale independently without awareness of shared constraints

### The Solution

The GPU Limiter sits between the saturation analyzer and the scaling decision executor. It:

1. **Tracks GPU capacity** per accelerator type (H100, A100, MI300X, etc.)
2. **Constrains scale-up decisions** to available resources
3. **Prioritizes allocation** to the most saturated models
4. **Ensures fairness** when multiple models compete for limited GPUs

## How It Works

### Scaling Pipeline

```
┌─────────────────────┐
│ Saturation Analyzer │  Determines target replicas based on
│                     │  KV cache usage and queue length
└──────────┬──────────┘
           │
           ▼
┌─────────────────────┐
│    GPU Limiter      │  Constrains targets based on
│    (if enabled)     │  available GPU capacity
└──────────┬──────────┘
           │
           ▼
┌─────────────────────┐
│ Scale-to-Zero       │  Applies safety constraints
│ Enforcer            │  for idle workloads
└──────────┬──────────┘
           │
           ▼
┌─────────────────────┐
│ Scaling Decision    │  Applied to cluster via HPA
└─────────────────────┘
```

### Allocation Algorithm: Greedy by Saturation

When GPU resources are limited, the limiter uses a **greedy-by-saturation** algorithm:

1. **Sort models by saturation** (most saturated first, based on spare capacity)
2. **Allocate GPUs sequentially** in priority order
3. **Partial allocation** if full request cannot be met

This ensures that the most overloaded models get resources first.

**Example:**

```
Cluster: 4 H100 GPUs available

Models wanting to scale up:
  Model-A: SpareCapacity=5%, needs 4 GPUs  (very saturated)
  Model-B: SpareCapacity=15%, needs 3 GPUs (moderately saturated)

Allocation:
  1. Model-A (5% spare) → Gets 4 GPUs (fully satisfied)
  2. Model-B (15% spare) → Gets 0 GPUs (H100 pool exhausted)
     ↳ Marked as "limited", scale-up deferred
```

### GPU Type Awareness

The limiter tracks GPUs **per accelerator type**. Models requesting H100 GPUs only compete with other H100 workloads, not with A100 or MI300X workloads.

```
Cluster Inventory:
  H100: 8 total, 6 used, 2 available
  A100: 4 total, 2 used, 2 available
  MI300X: 4 total, 0 used, 4 available

Model-A (H100): Can scale up by 1 replica (2 GPUs available)
Model-B (A100): Can scale up by 2 replicas (2 GPUs available)
Model-C (MI300X): Can scale up by 4 replicas (4 GPUs available)
```

## Enabling the GPU Limiter

### Configuration

The GPU Limiter is controlled via the `saturation-scaling-config` ConfigMap:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: saturation-scaling-config
  namespace: workload-variant-autoscaler-system
data:
  default: |
    kvCacheThreshold: 0.80
    queueLengthThreshold: 5
    kvSpareTrigger: 0.1
    queueSpareTrigger: 3
    enableLimiter: true
```

### Configuration Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `enableLimiter` | boolean | `false` | Enable GPU-aware scaling constraints |

The other parameters (`kvCacheThreshold`, `queueLengthThreshold`, `kvSpareTrigger`, `queueSpareTrigger`) control saturation-based scaling behavior. See [Saturation Scaling Configuration](../saturation-scaling-config.md) for detailed documentation.

## Prerequisites

### GPU Operator

The limiter requires GPU information from your cluster. It discovers GPU resources via the GPU operator's node labels and allocatable resources. Supported GPU operators:

- **NVIDIA GPU Operator**: Discovers NVIDIA GPUs (H100, A100, L40S, etc.)
- **AMD GPU Operator**: Discovers AMD GPUs (MI300X, MI250, etc.)

Ensure your GPU operator is installed and nodes report GPU resources correctly:

```bash
# Verify GPU resources are visible
kubectl get nodes -o json | jq '.items[].status.allocatable | with_entries(select(.key | contains("gpu")))'
```

### Deployment GPU Requirements

Model deployments must specify GPU resource requests so the limiter can track usage:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-model-decode
spec:
  template:
    spec:
      containers:
      - name: vllm
        resources:
          requests:
            nvidia.com/gpu: "2"  # 2 GPUs per replica
          limits:
            nvidia.com/gpu: "2"
```

### VariantAutoscaling Resources

Each model must have a `VariantAutoscaling` resource:

```yaml
apiVersion: autoscaling.llm-d.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: my-model
  namespace: llm-d-inference
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: my-model-decode
  modelID: "my-org/my-model"
  variantCost: "10.0"
```

The accelerator type is automatically discovered from the cluster based on the deployment's GPU resource requests and node GPU labels.

## Observability

### Logs

When the limiter constrains a decision, the controller logs:

```
INFO  Decision was limited by GPU availability
      variant=my-model
      originalTarget=5
      limitedTarget=3
      limitedBy=gpu-limiter
```

### VariantAutoscaling Status

The VA status shows the optimized allocation including accelerator type:

```yaml
status:
  desiredOptimizedAlloc:
    numReplicas: 3
    accelerator: "H100"
    lastRunTime: "2024-01-15T10:30:00Z"
```

## Example Scenarios

### Scenario 1: Single Model with Limited GPUs

```
Cluster: 4 H100 GPUs (1 node)
Model: llama-8b (2 GPUs per replica)

Without Limiter:
  Saturation analyzer requests 5 replicas (10 GPUs needed)
  HPA tries to scale, pods stuck Pending

With Limiter:
  Saturation analyzer requests 5 replicas
  Limiter: Only 4 GPUs available → max 2 replicas
  Decision: Scale to 2 replicas
  Result: All pods schedulable
```

### Scenario 2: Multiple Models Competing

```
Cluster: 8 H100 GPUs
Model-A (2 GPUs/replica): 1 replica running, 95% saturated
Model-B (1 GPU/replica): 2 replicas running, 80% saturated

Without Limiter:
  Model-A wants 4 replicas (+6 GPUs)
  Model-B wants 5 replicas (+3 GPUs)
  Total: 9 GPUs needed, only 6 available (8-2=6 free)
  Random scheduling race

With Limiter:
  Priority: Model-A (95% saturated) > Model-B (80% saturated)
  Model-A: Gets 6 GPUs → scales to 4 replicas
  Model-B: 0 GPUs left → stays at 2 replicas (marked limited)
  Result: Most critical model served first
```

### Scenario 3: Heterogeneous GPU Types

```
Cluster:
  4 H100 GPUs (2 free)
  4 A100 GPUs (4 free)

Model-A (H100, 1 GPU/replica): wants 3 more replicas
Model-B (A100, 2 GPU/replica): wants 2 more replicas

With Limiter:
  Model-A: Requests 3 H100 GPUs, gets 2 → scales by 2
  Model-B: Requests 4 A100 GPUs, gets 4 → scales by 2
  Result: Each model limited by its GPU type availability
```

## Troubleshooting

For GPU limiter troubleshooting, see the [Saturation Scaling Configuration - GPU Limiter Issues](../saturation-scaling-config.md#gpu-limiter-issues) section.

## Best Practices

1. **Start with limiter disabled**: Validate saturation-based scaling first, then enable limiter
2. **Monitor GPU utilization**: Use cluster monitoring to track GPU allocation efficiency
3. **Configure saturation thresholds**: See [Saturation Scaling Configuration](../saturation-scaling-config.md) for threshold tuning guidance
4. **Plan for headroom**: Don't run at 100% GPU capacity; leave room for scale-up

## Limitations

- **Experimental feature**: API and behavior may change in future releases
- **No preemption**: The limiter doesn't scale down lower-priority models to make room
- **No reservation**: GPUs are allocated on demand, not reserved in advance
- **Single cluster**: Cross-cluster GPU coordination not supported
- **Greedy algorithm**: May not find globally optimal allocation in complex scenarios

