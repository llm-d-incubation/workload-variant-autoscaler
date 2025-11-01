# VariantAutoscaling Scaling Behavior

This document explains how the VariantAutoscaling controller makes scaling decisions and provides guidance for administrators on how to properly manage replica counts.

## Overview

The VariantAutoscaling controller manages the number of replicas for model variant deployments by analyzing traffic patterns, performance metrics, and cost considerations. It uses a hierarchical decision-making process that prioritizes optimal solutions when metrics are available and provides intelligent fallback behavior when they are not.

## Scaling Decision Hierarchy

The controller follows this decision hierarchy (in order of priority):

1. **Optimizer Solution** (when metrics are available and optimizer runs)
2. **Fallback Allocation** (when optimizer doesn't run but fallback allocation exists)
   - Includes retention period awareness and scale-to-zero logic
3. **Last Resort** (when no optimizer solution or fallback allocation)
   - Uses controller-centric approach with retention period awareness

All paths enforce `minReplicas` and `maxReplicas` bounds and apply scale-to-zero logic when the retention period is exceeded.

## 1. Optimizer Solution

When metrics are available, the controller uses a mathematical optimizer to determine the optimal replica allocation across all variants of a model. The optimizer considers:

- **Performance constraints**: TTFT (Time To First Token) and ITL (Inter-Token Latency) SLO requirements
- **Cost optimization**: Minimizing total cost across all variants
- **Variant characteristics**: Performance profiles and cost per replica
- **Configuration bounds**: `minReplicas` and `maxReplicas` settings

**Result**: The optimizer produces a cost-optimal allocation that meets all performance constraints.

### Retention Period Protection

Even when the optimizer runs successfully, the controller applies an additional time-based retention period check to prevent premature scale-to-zero:

- **If optimizer returns 0 replicas**:
  - Controller checks if retention period has expired since `lastUpdate`
  - **First run** (when `lastUpdate` is zero): Preserves current deployment replicas as grace period for Prometheus discovery
  - **Retention period NOT exceeded**: Preserves previous allocation (doesn't scale to zero yet)
  - **Retention period EXCEEDED**: Applies optimizer's 0 replicas recommendation

This ensures consistent retention period semantics across all decision paths and prevents the optimizer's metrics-based scale-to-zero logic from conflicting with time-based retention requirements.

**Status indicators**: `desiredOptimizedAlloc.reason` will show:
```
# Normal optimizer solution
Optimizer solution: cost and latency optimized allocation

# First run grace period
First run: preserving current replicas for Prometheus discovery grace period

# When retention period blocks scale-to-zero
Optimizer returned 0 but retention period not exceeded (1m30s < 5m), preserving allocation
```

## 2. Fallback Allocation (Controller-Centric Approach)

When the optimizer doesn't run but a fallback allocation exists (from preparation phase or previous reconciliation), the controller enters fallback mode to maintain service stability.

### Behavior

The controller uses a **controller-centric approach** with two stages:

#### Stage 1: Check Retention Period

First, the controller checks if the retention period has been exceeded since the last allocation update:

- **Retention period NOT exceeded**: Use cached allocation with controller-centric approach
  - **First run**: Uses current deployment replica count as baseline
  - **Subsequent runs**: Uses previous optimized allocation to maintain controller intent
  - **Bounds enforcement**: Applies `minReplicas` and `maxReplicas` bounds

- **Retention period EXCEEDED**: Apply scale-to-zero logic (same as Retention Period Logic below)

#### Stage 2: Bounds Re-enforcement

Even when using cached allocations, bounds are re-applied to respect any CRD changes:
- If admin changed `minReplicas` or `maxReplicas`, allocation is clamped immediately
- Ensures administrator bounds are always respected

### Why Controller-Centric?

This approach ensures that:
- Temporary metric unavailability doesn't cause unnecessary scaling changes
- The controller's optimization intent is preserved during transient issues
- External manual scaling changes are eventually reconciled back to the controller's decision
- Service stability is maintained during metric collection gaps
- Scale-to-zero still occurs when no activity is detected for extended periods

### Example

```yaml
# Current state
currentAlloc: 5 replicas
desiredOptimizedAlloc: 8 replicas (lastUpdate: 2 minutes ago)
minReplicas: 2
retentionPeriod: 5m

# Optimizer doesn't run, fallback allocation exists
# Retention period NOT exceeded (2m < 5m)
# Result: maintains 8 replicas (controller intent)
```

**Status indicator**: `desiredOptimizedAlloc.reason` will show:
```
Fallback: preserving previous allocation (no optimizer solution)
```

**If retention period exceeded**: See Retention Period Logic section below.

## 3. Retention Period Logic

When metrics remain unavailable for an extended period (default: 5 minutes), the controller applies retention period logic to conserve resources while maintaining essential capacity.

The retention period is measured from `desiredOptimizedAlloc.lastUpdate`, which tracks when the allocation decision (NumReplicas or Reason) last changed.

### Three Retention Scenarios

#### Scenario A: Scale-to-Zero Enabled + All Variants Have minReplicas=0

**Condition**:
- `scaleToZero.enabled: true`
- All variants of the model have `minReplicas: 0` (or unset, defaulting to 0)
- Time since last update > retention period

**Action**: Scale all variants to 0 replicas

**Reasoning**: No traffic is expected, and configuration allows full scale-down to conserve resources.

**Status indicator**:
```
Fallback: scale-to-zero after retention period (no metrics for >5m)
```

#### Scenario B: Scale-to-Zero Disabled + All Variants Have minReplicas=0

**Condition**:
- `scaleToZero.enabled: false`
- All variants of the model have `minReplicas: 0`
- Time since last update > retention period

**Action**:
- Cheapest variant: 1 replica
- All other variants: 0 replicas

**Reasoning**: Maintain minimal capacity for incoming requests using the most cost-efficient option.

**Status indicator** (cheapest variant):
```
Fallback: cheapest variant with 1 replica after retention period (no metrics, scaleToZero disabled)
```

**Status indicator** (other variants):
```
Fallback: non-cheapest variant scaled to 0 after retention period (no metrics, scaleToZero disabled)
```

#### Scenario C: Any Variant Has minReplicas > 0

**Condition**:
- At least one variant has `minReplicas > 0`
- Time since last update > retention period

**Action**: Set each variant to its configured `minReplicas` value

**Reasoning**: Honor the administrator's minimum capacity requirements.

**Status indicator**:
```
Fallback: using minReplicas=2 after retention period (no metrics for >5m)
```

### First Run Grace Period

On first reconciliation, `lastUpdate` is zero, so retention period checks are skipped. This provides a grace period for:
- Prometheus to discover ServiceMonitors
- Metrics to begin flowing
- The controller to establish baseline state

## 4. Last Resort Allocation

If no optimizer solution or fallback allocation is provided, the controller applies the same logic as Fallback Allocation with additional deployment discovery protection.

### Behavior

The controller uses a **controller-centric approach** with retention period awareness and smart first-run handling:

#### When Retention Period NOT Exceeded

The controller follows a multi-stage decision process:

**Stage 1: Check if deployment was discovered late**
```
IF previousOptimized exists AND current > previous:
    # Deployment discovered after first run
    baseline = currentDeploymentReplicas    # Reset to actual deployment state
```

This handles the case where a VariantAutoscaling resource is created for an existing deployment with N replicas, but the deployment isn't discovered in the first reconciliation.

**Stage 2: Use previous optimized allocation**
```
IF previousOptimized exists AND current <= previous:
    baseline = previousOptimized.NumReplicas  # Maintain controller intent
```

**Stage 3: First run with deployment discovery protection**
```
IF previousOptimized does NOT exist:
    IF current=0 AND minReplicas=0:
        # Check for other running variants of same model
        IF other variants running:
            baseline = 0  # Safe to start at 0
        ELSE:
            baseline = 1  # Safe default to prevent premature scale-to-zero
    ELSE:
        baseline = currentDeploymentReplicas  # Use discovered replicas

desiredReplicas = clamp(baseline, minReplicas, maxReplicas)
```

This ensures:
- If deployment isn't discovered yet (`current=0`), use safe default of 1 (unless other variants are handling load)
- If deployment is discovered (`current>=1`), use actual replica count
- Prevents premature scale-to-zero before deployment discovery

#### When Retention Period EXCEEDED

Applies scale-to-zero logic (same as Retention Period Logic):
- All minReplicas=0 + scaleToZero enabled → scale to 0
- All minReplicas=0 + scaleToZero disabled → cheapest to 1, others to 0
- Some minReplicas>0 → use minReplicas

### Example

```yaml
# Scenario 1: Retention period NOT exceeded
previousOptimized: 10 replicas (lastUpdate: 2 minutes ago)
currentDeployment: 5 replicas
minReplicas: 2
maxReplicas: 12
retentionPeriod: 5m

# Result: 10 replicas (maintains controller intent, not current state)
```

```yaml
# Scenario 2: Retention period EXCEEDED
previousOptimized: 10 replicas (lastUpdate: 6 minutes ago)
minReplicas: 0 (all variants)
scaleToZero: enabled
retentionPeriod: 5m

# Result: 0 replicas (scale-to-zero logic applied)
```

**Status indicators**:
```
# Late deployment discovery (current > previous)
Last resort: deployment discovered late, using current=5 (was 0)

# Retention period not exceeded (has previous optimized)
Last resort: maintaining controller intent: max(minReplicas=2, previousOptimized=10) = 10

# First run with other variants running
Last resort: first run, other variants handling load, starting at 0

# First run without other variants
Last resort: first run with current=0, using safe default of 1 (waiting for deployment discovery)

# Retention period exceeded with scale-to-zero
Last resort: retention period exceeded (>5m), scale-to-zero enabled, scaling to 0

# Retention period exceeded without scale-to-zero
Last resort: retention period exceeded (>5m), cheapest variant getting 1 replica (scaleToZero disabled)
```

## Administrator Guidelines

### How to Control Replica Counts

**DO** use these CRD fields to control scaling:

1. **`minReplicas`**: Set minimum replica count for a variant
   ```yaml
   spec:
     minReplicas: 2  # Never scale below 2 replicas
   ```

2. **`maxReplicas`**: Set maximum replica count for a variant
   ```yaml
   spec:
     maxReplicas: 10  # Never scale above 10 replicas
   ```

3. **`scaleToZero.enabled`**: Control whether variants can scale to zero
   ```yaml
   # In ConfigMap referenced by sloClassRef
   scaleToZero:
     enabled: true
     retentionPeriod: "5m"
   ```

### Why Not Direct Deployment Scaling?

**DO NOT** manually scale deployments using `kubectl scale` or similar commands.

**Why?** The VariantAutoscaling controller continuously reconciles deployment replica counts based on its optimization decisions. Manual scaling changes will be:

- **Overridden** on the next reconciliation cycle (typically within seconds)
- **Ignored** by the controller's optimization logic
- **Lost** as the controller enforces its desired state

### Proper Intervention Methods

If you need to intervene in scaling:

1. **Set bounds**: Use `minReplicas` and `maxReplicas` to constrain the optimizer
2. **Disable scale-to-zero**: Ensure variants maintain minimum capacity
3. **Temporarily pause**: Delete or suspend the VariantAutoscaling resource (not recommended for production)
4. **Adjust SLO configuration**: Modify performance targets to influence optimizer decisions

### Example: Ensuring Minimum Capacity

```yaml
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: llama-3-1-8b-a100-4
spec:
  modelID: "meta/llama-3.1-8b"
  variantID: "meta/llama-3.1-8b-A100-4"
  minReplicas: 3      # Ensure at least 3 replicas always
  maxReplicas: 20     # Cap at 20 replicas
  # ... other fields
```

## Monitoring Scaling Decisions

### Check Current State

```bash
kubectl get variantautoscaling -A
```

Output shows:
- `CurrentReplicas`: Current deployment replica count
- `Optimized`: Desired optimized replica count
- `MetricsReady`: Whether metrics are available

### View Detailed Status

```bash
kubectl describe variantautoscaling <name> -n <namespace>
```

Key fields to monitor:
- `status.desiredOptimizedAlloc.reason`: Explanation of the current allocation decision
- `status.desiredOptimizedAlloc.lastUpdate`: When the allocation decision last changed
- `status.conditions`: MetricsAvailable condition status

### Prometheus Metrics

The controller exports Prometheus metrics for observability:

```promql
# View desired replica counts with reasons
va_desired_optimized_replicas{variant_name="llama-3-1-8b-a100-4"}

# Track when allocations change
rate(va_allocation_changes_total[5m])
```

## Troubleshooting

### Variants Not Scaling as Expected

1. **Check metrics availability**:
   ```bash
   kubectl get variantautoscaling <name> -o jsonpath='{.status.conditions[?(@.type=="MetricsAvailable")].status}'
   ```

2. **Review allocation reason**:
   ```bash
   kubectl get variantautoscaling <name> -o jsonpath='{.status.desiredOptimizedAlloc.reason}'
   ```

3. **Verify bounds configuration**:
   ```bash
   kubectl get variantautoscaling <name> -o jsonpath='{.spec.minReplicas}{"\n"}{.spec.maxReplicas}'
   ```

### Unexpected Scale-to-Zero

If variants are scaling to zero unexpectedly:

1. **Check scaleToZero configuration** in your SLO ConfigMap
2. **Verify minReplicas** is set if you want guaranteed capacity
3. **Review retention period**: Ensure metrics are flowing regularly

### Manual Scaling Gets Overridden

This is **expected behavior**. Use `minReplicas`/`maxReplicas` instead of manual scaling.

## Integration with HPA

The VariantAutoscaling controller works alongside Horizontal Pod Autoscaler (HPA):

- **WVA Role**: Manages replica counts based on optimization decisions
- **HPA Role**: Can be configured to handle rapid scaling based on CPU/memory metrics if needed

Both controllers can coexist, with WVA managing the strategic allocation based on cost and SLO optimization.

## Summary

The VariantAutoscaling controller provides intelligent, cost-optimized scaling with robust fallback behavior:

**Key Features:**
- **Consistent retention period enforcement**: All decision paths (optimizer, fallback, last resort) use time-based retention checks to prevent premature scale-to-zero
- **Controller-centric approach**: Maintains optimization intent during transient issues
- **Deployment discovery protection**: Smart first-run logic prevents premature scale-to-zero before deployment discovery, checks for other running variants, and handles late discovery gracefully
- **Retention period awareness**: Scales to zero only after retention period expires with no activity
- **Bounds enforcement**: Always respects `minReplicas` and `maxReplicas` across all paths
- **Scale-to-zero integration**: Conserves resources while maintaining essential capacity

**Administrator Guidelines:**
- ✅ Use `minReplicas`, `maxReplicas`, and `scaleToZero` for capacity control
- ✅ Monitor `desiredOptimizedAlloc.reason` to understand scaling decisions
- ✅ Monitor `desiredOptimizedAlloc.lastUpdate` to track allocation changes
- ✅ Configure retention period appropriately for your environment
- ❌ Avoid manual deployment scaling (will be overridden)
- ❌ Don't rely on external scaling tools that conflict with WVA's decisions

For more information, see:
- [CRD Reference](crd-reference.md) - Detailed field documentation
- [Configuration Guide](configuration.md) - SLO and ConfigMap setup
- [Scale-to-Zero Feature](../features/scale-to-zero.md) - Scale-to-zero configuration
- [Replica Bounds](../features/replica-bounds.md) - Min/max replica configuration
