# Replica Bounds Configuration

## Overview

Replica bounds allow you to control the minimum and maximum number of replicas for each VariantAutoscaling resource. This provides fine-grained control over GPU resource allocation and ensures predictable scaling behavior.

## Features

- **Per-Variant Control**: Set bounds independently for each model variant
- **Optional Configuration**: Both bounds are optional with sensible defaults
- **Optimizer Integration**: The optimizer respects bounds when calculating optimal allocations
- **Scale-to-Zero Compatibility**: Works seamlessly with the scale-to-zero feature

## Configuration

### Field Definitions

```yaml
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: my-variant
spec:
  # ... other spec fields ...

  # Minimum number of replicas (optional, default: 0)
  minReplicas: 1

  # Maximum number of replicas (optional, default: unlimited)
  maxReplicas: 10
```

#### `minReplicas` (optional)

- **Type**: integer
- **Default**: 0
- **Minimum**: 0
- **Description**: The optimizer will never scale below this value

**Important Considerations**:
- Setting `minReplicas > 0` ensures at least this many replicas are always running
- Setting `minReplicas > 0` prevents the variant from scaling to zero, even if scale-to-zero is enabled for the model
- Setting `minReplicas > 0` for multiple variants of the same model may lead to unnecessary GPU utilization

#### `maxReplicas` (optional)

- **Type**: integer
- **Default**: nil (unlimited)
- **Minimum**: 1
- **Description**: The optimizer will never scale above this value

**Important Considerations**:
- If not specified, no upper bound is enforced
- Useful for limiting resource consumption or respecting quota limits
- Must be >= `minReplicas` if both are specified

## Usage Examples

### Example 1: Basic Bounds

```yaml
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: llama-a100-variant
  namespace: llm-inference
spec:
  modelID: "meta/llama-3.1-8b"
  variantID: "meta/llama-3.1-8b-A100-1"
  accelerator: "A100"
  acceleratorCount: 1

  # Scale between 1 and 10 replicas
  minReplicas: 1
  maxReplicas: 10

  # ... other spec fields ...
```

### Example 2: Prevent Scale-to-Zero

```yaml
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: critical-model
spec:
  modelID: "critical-model"
  variantID: "critical-model-H100-4"

  # Always keep at least 2 replicas running
  minReplicas: 2
  # No upper limit

  # ... other spec fields ...
```

### Example 3: Fixed Replica Count

```yaml
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: fixed-capacity
spec:
  modelID: "fixed-model"
  variantID: "fixed-model-L40S-2"

  # Always run exactly 5 replicas
  minReplicas: 5
  maxReplicas: 5

  # ... other spec fields ...
```

### Example 4: Limit Maximum Only

```yaml
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: quota-limited
spec:
  modelID: "quota-model"
  variantID: "quota-model-A100-2"

  # Can scale to zero, but limited to 20 replicas max
  minReplicas: 0  # or omit for default
  maxReplicas: 20

  # ... other spec fields ...
```

### Example 5: Multiple Variants with Different Bounds

```yaml
---
# High-priority variant: always available, can scale high
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: llama-a100-high-priority
spec:
  modelID: "meta/llama-3.1-8b"
  variantID: "meta/llama-3.1-8b-A100-1"
  accelerator: "A100"
  minReplicas: 2    # Always keep 2 running
  maxReplicas: 50   # Can scale high
  # ... other spec fields ...

---
# Cost-efficient variant: scale to zero when idle
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: llama-l40s-cost-efficient
spec:
  modelID: "meta/llama-3.1-8b"
  variantID: "meta/llama-3.1-8b-L40S-2"
  accelerator: "L40S"
  minReplicas: 0    # Can scale to zero
  maxReplicas: 10   # Limited capacity
  # ... other spec fields ...
```

## Behavior and Warnings

### Validation Warnings

The system logs warnings for potentially problematic configurations:

#### Multiple Variants with minReplicas > 0

```
WARN: Multiple variants with minReplicas > 0 may lead to unnecessary GPU utilization
  modelID: meta/llama-3.1-8b
  variants: [variant-a, variant-b]
  count: 2
```

**Explanation**: Having multiple variants with `minReplicas > 0` means multiple variant implementations are always running, which may waste GPU resources if only one variant typically handles the load.

**Recommendation**: Consider setting `minReplicas > 0` for only one variant per model, or ensure there's a valid reason for keeping multiple variants running simultaneously.

#### Conflict with Scale-to-Zero

```
WARN: Model has scaleToZero enabled but variants have minReplicas > 0, preventing scale-to-zero
  modelID: meta/llama-3.1-8b
  variants: [variant-a]
  scaleToZeroEnabled: true
```

**Explanation**: Scale-to-zero is enabled for the model, but one or more variants have `minReplicas > 0`, which prevents the model from ever scaling to zero replicas.

**Recommendation**: Either:
- Set `minReplicas: 0` to allow scale-to-zero, or
- Disable scale-to-zero if you intentionally want to keep replicas running

### Optimizer Behavior

The optimizer applies replica bounds in the following order:

1. **Optimization**: Calculate optimal replica count based on load and SLO requirements
2. **Minimum Enforcement**: If optimized count < `minReplicas`, set to `minReplicas`
3. **Maximum Enforcement**: If optimized count > `maxReplicas`, set to `maxReplicas`

Example optimizer log:
```
INFO: Clamping replicas to maxReplicas
  variant: my-variant
  namespace: default
  optimized: 15
  maxReplicas: 10
```

### Zero-Rate Handling

When a model has zero traffic, the optimizer considers replica bounds:

- **With scale-to-zero enabled + minReplicas=0**: Scales all variants to zero
- **With scale-to-zero enabled + minReplicas>0**: Keeps `minReplicas` running
- **With scale-to-zero disabled**: Keeps at least one replica running (respecting `minReplicas`)

## Integration with Scale-to-Zero

Replica bounds work seamlessly with the [scale-to-zero feature](./scale-to-zero.md):

| Configuration | Behavior |
|---------------|----------|
| `minReplicas: 0` + scale-to-zero enabled | Can scale to zero when idle |
| `minReplicas: 0` + scale-to-zero disabled | Keeps minimum 1 replica |
| `minReplicas: N (>0)` + scale-to-zero enabled | Keeps minimum N replicas (warning logged) |
| `minReplicas: N (>0)` + scale-to-zero disabled | Keeps minimum N replicas |

## Best Practices

### 1. Start with Defaults

Begin without setting bounds and observe the optimizer's behavior:

```yaml
# No bounds specified - optimizer has full flexibility
spec:
  modelID: "my-model"
  variantID: "my-model-A100-1"
  # minReplicas and maxReplicas omitted
```

### 2. Set minReplicas Only When Necessary

Only set `minReplicas > 0` if:
- Cold start latency is critical and unacceptable
- The model must be highly available at all times
- You've measured that scale-to-zero doesn't meet your SLO

### 3. Use maxReplicas for Cost Control

Set `maxReplicas` to:
- Prevent runaway scaling and cost
- Respect GPU quotas
- Limit resource consumption during unexpected traffic spikes

### 4. Per-Variant Strategy

For models with multiple variants:
- Set `minReplicas > 0` for at most one variant (typically the most cost-efficient one)
- Allow other variants to scale from zero based on demand
- This provides availability while minimizing idle GPU costs

### 5. Monitor and Adjust

- Check optimizer logs for clamping messages
- Monitor metrics to see if bounds are too restrictive
- Adjust bounds based on observed traffic patterns

## Troubleshooting

### Problem: Model Never Scales to Zero

**Symptom**: Model always has running replicas despite no traffic and scale-to-zero enabled.

**Possible Causes**:
1. One or more variants have `minReplicas > 0`
2. Check VariantAutoscaling resources for the model

**Solution**:
```bash
kubectl get va -l model-id=<your-model> -o yaml | grep -A 1 minReplicas
```

Set `minReplicas: 0` or omit the field to allow scale-to-zero.

### Problem: Optimizer Keeps Hitting maxReplicas Limit

**Symptom**: Frequent log messages about clamping to `maxReplicas`, potential SLO violations.

**Possible Causes**:
1. `maxReplicas` is set too low for the traffic pattern
2. Traffic has increased since bounds were configured

**Solution**:
- Increase `maxReplicas` or remove it entirely
- Review SLO metrics to determine appropriate upper bound

### Problem: Unexpected GPU Utilization

**Symptom**: High GPU utilization despite low traffic.

**Possible Causes**:
1. Multiple variants have `minReplicas > 0`
2. Check for warning logs about multiple variants

**Solution**:
- Set `minReplicas > 0` for only one variant per model
- Allow other variants to scale from zero

## Related Features

- [Scale-to-Zero Configuration](./scale-to-zero.md)
- [CRD Reference](../user-guide/crd-reference.md)
- [Configuration Guide](../user-guide/configuration.md)

## See Also

- [Modeling and Optimization Design](../design/modeling-optimization.md)
- [Prometheus Integration](../integrations/prometheus.md)
