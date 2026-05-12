# Scale to Zero

The Scale-to-Zero feature enables automatic scaling of idle model deployments to zero replicas, freeing GPU resources when models are not receiving traffic. This is particularly valuable in multi-tenant environments where GPU resources are expensive and shared across multiple models.

## Overview

### The Problem

Without scale-to-zero, idle model deployments continue consuming GPU resources even when receiving no traffic:

- **Wasted GPU resources**: Models consume GPUs 24/7 even during periods of no activity
- **Higher infrastructure costs**: Paying for GPUs that aren't processing requests
- **Reduced cluster capacity**: Idle models block other workloads from accessing GPUs
- **No automatic resource reclamation**: Manual intervention required to free resources

### The Solution

Scale-to-zero automatically scales model deployments to zero replicas after a configurable retention period of inactivity. It:

1. **Monitors request traffic** via Prometheus metrics
2. **Tracks idle duration** using a configurable retention period
3. **Scales to zero** when no requests are received within the retention period
4. **Preserves minimum replicas** for models with scale-to-zero disabled

## How It Works

### Scaling Pipeline

Scale-to-zero operates as an enforcer in the scaling pipeline:

```
┌─────────────────────┐
│ Saturation Analyzer │  Determines target replicas based on
│                     │  workload metrics
└──────────┬──────────┘
           │
           ▼
┌─────────────────────┐
│ Scale-to-Zero       │  Enforces scale-to-zero or minimum
│ Enforcer            │  replica constraints
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
│ Scaling Decision    │  Applied to cluster via HPA
└─────────────────────┘
```

### Decision Logic

The scale-to-zero enforcer applies the following logic:

**When scale-to-zero is enabled:**
1. Query request count for the model over the retention period
2. If requests > 0: Keep saturation analyzer's targets unchanged
3. If requests = 0: Scale all variants to zero replicas
4. On query failure: Keep current targets (safety mechanism)

**When scale-to-zero is disabled:**
1. Calculate total replicas across all variants
2. If total > 0: Keep targets unchanged
3. If total = 0: Set cheapest variant to 1 replica (minimum preservation)

### Retention Period

The retention period defines how long a model must be idle before scaling to zero:

```
Timeline:
─────────────────────────────────────────────────────────►
│                                                        │
│                  Retention Period                      │
│     ├────────────────────────────────────────────┤     │
│     ●────────────────────────────────────────────●     │
│     ↑                                            │     │
│ Last Request                             Scale to Zero │
```

**Example with 15-minute retention:**
- Last request received at 10:00
- No requests between 10:00 and 10:15
- At 10:15, model scales to zero

## Configuration

### ConfigMap Structure

Scale-to-zero is configured via the `model-scale-to-zero-config` ConfigMap:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: model-scale-to-zero-config
  namespace: workload-variant-autoscaler-system
data:
  # Global defaults for all models
  default: |
    enable_scale_to_zero: true
    retention_period: "15m"

  # Per-model override (shorter retention)
  llama-8b-override: |
    model_id: meta/llama-3.1-8b
    retention_period: "5m"

  # Disable for critical model
  critical-model: |
    model_id: production/critical-model
    enable_scale_to_zero: false

  # Namespace-scoped override (same model, different config per namespace)
  llama-production: |
    model_id: meta/llama-3.1-8b
    namespace: production
    retention_period: "30m"

  llama-development: |
    model_id: meta/llama-3.1-8b
    namespace: development
    retention_period: "2m"
```

### Configuration Options

| Option | Type | Default | Description |
|--------|------|---------|-------------|
| `enable_scale_to_zero` | boolean | `false` | Enable automatic scaling to zero for idle models |
| `retention_period` | string | `"10m"` | Duration of inactivity before scaling to zero (e.g., "5m", "1h", "30s"). Note: This is unrelated to HPA's `stabilizationWindowSeconds` - retention period tracks how long a model has no requests, while stabilizationWindowSeconds prevents HPA flapping after scaling decisions |
| `model_id` | string | - | Model identifier for per-model overrides (required for non-default entries) |
| `namespace` | string | - | Namespace scope for per-model overrides. When specified, the override only applies to the model in that specific namespace. This is important when the same model is deployed in multiple namespaces with different requirements |

### Configuration Priority

Configuration is resolved in the following order (highest to lowest priority):

1. **Per-model configuration** matching `model_id` (and optionally `namespace`)
2. **Global defaults** from the `default` entry in ConfigMap
3. **Environment variable** `WVA_SCALE_TO_ZERO` (true/false)
4. **System default**: disabled with 10-minute retention

### Per-Model Overrides

Per-model entries can override specific fields while inheriting others from defaults:

```yaml
data:
  default: |
    enable_scale_to_zero: true
    retention_period: "15m"

  # Override only retention period, inherit enable_scale_to_zero
  fast-reclaim: |
    model_id: dev/test-model
    retention_period: "2m"

  # Disable scale-to-zero for this model only
  always-on: |
    model_id: production/critical-api
    enable_scale_to_zero: false

  # Namespace-scoped override: same model with different settings per namespace
  # This is useful when the same model is deployed across dev/staging/prod
  model-in-prod: |
    model_id: my-org/my-model
    namespace: production
    enable_scale_to_zero: false    # Never scale to zero in production

  model-in-dev: |
    model_id: my-org/my-model
    namespace: development
    retention_period: "2m"          # Quick reclaim in development
```

## Prerequisites

### HPA Scale-to-Zero Feature Gate

Scale-to-zero requires Kubernetes HPA to support `minReplicas: 0`. This is controlled by the `HPAScaleToZero` feature gate:

- **Kubernetes 1.27+**: Feature gate is beta and enabled by default
- **Kubernetes < 1.27**: Feature gate must be explicitly enabled

Verify the feature is available:

```bash
# Check if HPA can be configured with minReplicas: 0
kubectl get hpa -n your-namespace -o yaml | grep minReplicas
```

### Prometheus Metrics

Scale-to-zero relies on vLLM request metrics to determine idle state:

- **Metric**: `vllm:request_success_total`
- **Labels**: `namespace`, `model_name`

Verify metrics are available:

```bash
# Query Prometheus for request metrics
curl -s "http://prometheus:9090/api/v1/query?query=vllm:request_success_total" | jq
```

### VariantAutoscaling Resource

Each model must have a `VariantAutoscaling` resource. The scale-to-zero configuration is applied based on the `modelID` in the VA spec:

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
  modelID: "my-org/my-model"  # Used for scale-to-zero config lookup
  variantCost: "10.0"
```

## Example Scenarios

### Scenario 1: Development Environment

Scale down quickly to free resources for other developers:

```yaml
data:
  default: |
    enable_scale_to_zero: true
    retention_period: "2m"
```

**Behavior:**
- Models scale to zero after 2 minutes of inactivity
- Quick resource reclamation for shared development clusters
- Fast iteration cycles

### Scenario 2: Production with Mixed Criticality

Critical models stay always-on, others scale to zero:

```yaml
data:
  default: |
    enable_scale_to_zero: true
    retention_period: "30m"

  critical-api: |
    model_id: production/customer-facing-api
    enable_scale_to_zero: false

  batch-model: |
    model_id: production/batch-processing
    retention_period: "5m"
```

**Behavior:**
- Critical API never scales to zero (always has 1+ replica)
- Batch processing model reclaims resources quickly (5 min)
- Other models use 30-minute default retention

### Scenario 3: Cost Optimization

Aggressive resource reclamation for cost-sensitive environments:

```yaml
data:
  default: |
    enable_scale_to_zero: true
    retention_period: "5m"
```

**Behavior:**
- All models scale to zero after 5 minutes of inactivity
- Maximum GPU resource utilization
- Cold start latency acceptable for the use case

## Observability

### Logs

The controller logs scale-to-zero decisions:

```
INFO  Scale-to-zero enforced
      modelID=my-org/my-model
      action=scale-to-zero
      retentionPeriod=15m
      requestCount=0

INFO  Scale-to-zero skipped (requests detected)
      modelID=my-org/my-model
      retentionPeriod=15m
      requestCount=42

INFO  Minimum replicas preserved (scale-to-zero disabled)
      modelID=production/critical-api
      variant=variant-a100
      replicas=1
```

### VariantAutoscaling Status

The VA status reflects scale-to-zero state:

```yaml
status:
  desiredOptimizedAlloc:
    numReplicas: 0      # Scaled to zero
    lastRunTime: "2024-01-15T10:30:00Z"
  currentAlloc:
    numReplicas: 0
```

## Troubleshooting

### Model Not Scaling to Zero

1. **Check if scale-to-zero is enabled**:
   ```bash
   kubectl get cm model-scale-to-zero-config -n workload-variant-autoscaler-system -o yaml
   ```

2. **Verify HPA minReplicas allows zero**:
   ```bash
   kubectl get hpa -n your-namespace -o yaml | grep minReplicas
   ```

3. **Check request metrics exist**:
   ```bash
   # Query for model request metrics
   kubectl exec -n monitoring prometheus-0 -- \
     curl -s "localhost:9090/api/v1/query?query=vllm:request_success_total{model_name='your-model'}"
   ```

4. **Check controller logs for errors**:
   ```bash
   kubectl logs -n workload-variant-autoscaler-system \
     deployment/workload-variant-autoscaler-controller-manager | grep -i "scale-to-zero"
   ```

### Model Scaling to Zero Too Quickly

1. **Increase retention period**:
   ```yaml
   data:
     your-model: |
       model_id: your-org/your-model
       retention_period: "30m"
   ```

2. **Verify request metrics are being recorded**:
   - Check vLLM is exporting `vllm:request_success_total` metric
   - Verify Prometheus is scraping the metrics endpoint
   - Confirm metric labels match the model ID

### Model Not Scaling Back Up

Scale-up from zero requires the EPP (End Point Picker) queue metrics to trigger when traffic arrives.

If scale-up isn't working:

1. **Verify EPP queue metrics are available**: The controller monitors `inference_extension_flow_control_queue_size` metric
2. **Check saturation configuration**: See [Saturation Scaling Configuration](../saturation-scaling-config.md)
3. **Verify HPA is receiving external metrics**: Check `kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1"`

## Best Practices

1. **Start with conservative retention periods**: Use longer periods (15-30 min) initially, then tune based on traffic patterns

2. **Disable for latency-critical models**: Models requiring instant availability should have scale-to-zero disabled

3. **Consider cold start time**: Factor in model loading time when setting retention periods - too short may cause frequent cold starts

4. **Monitor request patterns**: Use Prometheus/Grafana to understand traffic patterns before configuring retention periods

5. **Use per-model overrides**: Different models have different usage patterns - configure accordingly

6. **Test in non-production first**: Validate scale-to-zero behavior in development/staging before production

## Limitations

- **Metrics dependency**: Requires Prometheus metrics to be available; metrics unavailability prevents scale-to-zero (safety mechanism)
- **No predictive scaling**: Scale-to-zero is reactive, not predictive - it cannot anticipate traffic spikes
- **Single retention period per model**: Cannot configure different retention periods for different time windows (e.g., shorter during business hours)

## Related Documentation

- [Saturation Scaling Configuration](../saturation-scaling-config.md) - Configure saturation thresholds for scaling behavior
- [GPU Limiter](gpu-limiter.md) - Resource-aware scaling constraints applied after scale-to-zero enforcement
