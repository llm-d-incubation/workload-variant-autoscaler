# Controller Event Handling and Reconciliation Behavior

## Overview

The WVA controller uses Kubernetes controller-runtime to watch and respond to various resource events in the cluster. This document describes how the controller handles different types of events and triggers reconciliation.

## Event Filtering Philosophy

WVA employs a **selective event filtering** strategy to optimize controller performance and reduce unnecessary reconciliation cycles:

- **Allow events that require action**: Only events that necessitate immediate controller action trigger reconciliation
- **Block redundant events**: Events that don't change the controller's decision-making are filtered out
- **Periodic reconciliation handles drift**: The controller reconciles all VariantAutoscaling resources periodically (default: 60s), ensuring eventual consistency without relying on every individual event

## Watched Resources

### 1. VariantAutoscaling (Primary Resource)

**Events Handled:**
- **Create**: ✅ Triggers reconciliation to initialize the resource
- **Update**: ❌ Blocked - Periodic reconciliation handles all updates
- **Delete**: ❌ Blocked - Periodic reconciliation detects deleted resources
- **Generic**: ❌ Blocked

**Rationale:**
The controller reconciles all VariantAutoscaling resources on a periodic interval (60s by default). Individual Update and Delete events would only cause unnecessary reconciliation cycles since:
- Updates are handled in the next periodic reconciliation
- Deleted resources are filtered out in `filterActiveVariantAutoscalings()`

### 2. Deployment

**Events Handled:**
- **Create**: ✅ Triggers reconciliation for VAs referencing this deployment
- **Update**: ❌ Blocked
- **Delete**: ✅ Triggers reconciliation for VAs referencing this deployment
- **Generic**: ❌ Blocked

**Create Event - Race Condition Handling:**

Deployment Create events handle a critical race condition:

```
Timeline:
T0: User creates VariantAutoscaling CR for deployment "llama-8b"
T1: VA reconciliation runs - deployment doesn't exist yet
T2: User creates Deployment "llama-8b"
T3: ??? Without Create event handling, VA waits until next periodic cycle
```

When a Deployment is created:
1. Controller lists all VAs in the same namespace
2. Finds VAs with matching `scaleTargetRef` or inferred target name
3. Triggers immediate reconciliation of those VAs

This ensures VAs become operational immediately when their target deployment appears, rather than waiting up to 60 seconds for the next periodic reconciliation.

**Delete Event - Status Update and Cleanup:**

Deployment Delete events allow the VA to respond immediately when its target deployment is removed:

```yaml
# Before deployment deletion
status:
  conditions:
  - type: Ready
    status: "True"
  desiredOptimizedAlloc:
    numReplicas: 3
    accelerator: "A100"

# After deployment deletion (immediate update)
status:
  conditions:
  - type: Ready
    status: "False"
    reason: "DeploymentNotFound"
    message: "Target deployment no longer exists"
  desiredOptimizedAlloc: {}
```

When a Deployment is deleted:
1. Controller identifies all VAs referencing the deleted deployment
2. Triggers reconciliation for those VAs
3. VA status is updated to reflect the missing deployment
4. Associated metrics are cleared or updated
5. VA is marked as not ready until deployment is recreated

This provides immediate visibility into the deployment's absence and prevents stale metrics from affecting autoscaling decisions.

**Implementation:**

```go
// DeploymentPredicate returns a predicate that filters Deployment events.
// It allows Create and Delete events for all Deployments to trigger VA reconciliation:
// - Create: handles the race condition where VA is created before its target deployment
// - Delete: allows VA to update status and clear metrics when target deployment is removed
func DeploymentPredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			return true  // Allow all Deployment create events
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return false  // Block Deployment update events
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return true  // Allow all Deployment delete events
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false  // Block generic events
		},
	}
}
```

### 3. ConfigMap

**Events Handled:**
- **Create**: ✅ Processed by event handler
- **Update**: ✅ Processed by event handler (required for config changes)
- **Delete**: ❌ Blocked
- **Generic**: ❌ Blocked

**Watched ConfigMaps:**
- `wva-variantautoscaling-config` (default name)
  - Contains global optimization configuration (e.g., `GLOBAL_OPT_INTERVAL`)
- `wva-saturation-scaling-config` (default name)
  - Contains per-accelerator saturation scaling thresholds

**Rationale:**
ConfigMap updates need to be processed immediately to apply new configuration. However, ConfigMap changes update the global configuration cache and don't trigger individual VA reconciliation - the Engine loop reads the updated configuration on its next cycle.

**Predicate:**

```go
// ConfigMapPredicate returns a predicate that filters ConfigMap events to only the target ConfigMaps.
// It accepts a namespaceChecker function to filter ConfigMaps in tracked namespaces (prevents cluster-wide watching).
func ConfigMapPredicate(namespaceChecker func(string) bool) predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		name := obj.GetName()
		namespace := obj.GetNamespace()
		// Well-known ConfigMap names in controller namespace (global) or tracked namespaces (namespace-local)
		return (name == getConfigMapName() || name == getSaturationConfigMapName()) && 
		       obj.GetNamespace() == configMapNamespace
	})
}
```

### 4. ServiceMonitor

**Events Handled:**
- **Create**: ✅ Processed (no reconciliation triggered)
- **Update**: ✅ When `deletionTimestamp` is set (finalizers cause deletion to emit Update events)
- **Delete**: ✅ Processed (no reconciliation triggered)
- **Generic**: ❌ Blocked

**Purpose:**
The controller watches its own ServiceMonitor (label `control-plane=controller-manager` in the controller namespace; default Kustomize name `workload-variant-autoscaler-cm-mon`) for observability purposes. When the ServiceMonitor is deleted:

1. Prometheus stops scraping controller metrics
2. External autoscalers (HPA/KEDA) can't access optimized replica metrics
3. Controller logs warnings and emits Kubernetes events to alert operators

**Important:** ServiceMonitor events do NOT trigger VA reconciliation. The ServiceMonitor affects metrics scraping, not optimization logic. The handler exists solely for observability.

**Predicate:**

```go
func ServiceMonitorPredicate() predicate.Predicate {
	return predicate.NewPredicateFuncs(func(obj client.Object) bool {
		if obj.GetNamespace() != config.SystemNamespace() {
			return false
		}
		labels := obj.GetLabels()
		if labels == nil {
			return false
		}
		return labels["control-plane"] == "controller-manager" &&
			labels["app.kubernetes.io/name"] == "workload-variant-autoscaler"
	})
}
```

## Periodic Reconciliation

In addition to event-driven reconciliation, the controller performs **periodic reconciliation** of all VariantAutoscaling resources:

- **Default Interval**: 60 seconds
- **Configurable**: Via `GLOBAL_OPT_INTERVAL` in ConfigMap
- **Purpose**: Ensures eventual consistency and handles:
  - Metric collection and analysis
  - Optimization decisions
  - Status updates
  - Detection of deleted resources

This periodic reconciliation is why many Update and Delete events can be safely filtered - the controller will process changes in the next cycle.

## Event Flow Examples

### Example 1: Deployment Created Before VA

```
1. User creates Deployment "llama-8b"
   → No VA exists yet, no action taken

2. User creates VariantAutoscaling "llama-8b-autoscaler"
   → Create event triggers reconciliation
   → Controller finds deployment, begins monitoring
```

### Example 2: VA Created Before Deployment (Race Condition)

```
1. User creates VariantAutoscaling "llama-8b-autoscaler"
   → Create event triggers reconciliation
   → Deployment doesn't exist, VA status reflects this

2. User creates Deployment "llama-8b"
   → Deployment Create event triggers VA reconciliation
   → Controller finds matching VA, updates status
   → VA becomes operational immediately (no 60s wait)
```

### Example 3: Deployment Deleted

```
1. Deployment "llama-8b" is running with VA "llama-8b-autoscaler"
   → VA status shows Ready=True, 3 replicas

2. User deletes Deployment "llama-8b"
   → Deployment Delete event triggers VA reconciliation
   → Controller updates VA status: Ready=False, DeploymentNotFound
   → Metrics are cleared
   → Operators are immediately aware of the issue

3. User recreates Deployment "llama-8b"
   → Deployment Create event triggers VA reconciliation
   → VA becomes operational again
```

### Example 4: ConfigMap Updated

```
1. Admin updates wva-saturation-scaling-config ConfigMap
   → Update event processed by ConfigMap handler
   → Global configuration cache updated
   → Engine loop reads new config on next cycle
   → No individual VA reconciliation triggered
```

## Best Practices

### For Operators

1. **Create VAs after deployments are ready**: While the controller handles the race condition, creating VAs after deployments are fully initialized avoids unnecessary early reconciliation cycles.

2. **Monitor ServiceMonitor health**: If the controller's ServiceMonitor is deleted, external autoscalers can't access metrics. Watch for `ServiceMonitorDeleted` events.

3. **Understand reconciliation timing**: 
   - Critical events (Deployment create/delete) trigger immediate reconciliation
   - Other changes are handled in the next periodic cycle (≤60s)

### For Developers

1. **Add predicates for new watches**: Always implement a predicate when watching new resource types to avoid unnecessary reconciliation.

2. **Consider periodic reconciliation**: Before handling an event type, ask: "Does this need immediate action, or will the next periodic reconciliation handle it?"

3. **Document event handling decisions**: Explain why specific events are allowed or blocked in code comments.

## Troubleshooting

### VA Not Responding to Deployment

**Symptom**: VA exists but doesn't process new deployment

**Diagnosis**:
```bash
# Check if deployment name matches VA's target
kubectl get va llama-8b-autoscaler -o jsonpath='{.spec.scaleTargetRef.name}'
kubectl get deployment llama-8b

# Check controller logs for deployment events
kubectl logs -n workload-variant-autoscaler-system deployment/workload-variant-autoscaler-controller-manager | grep "Deployment created"
```

**Solution**: Ensure VA's `scaleTargetRef` or inferred target matches the deployment name.

### Metrics Not Available

**Symptom**: External autoscaler can't read `wva_optimized_replicas` metric

**Diagnosis**:
```bash
# Check if ServiceMonitor exists
kubectl get servicemonitor -n workload-variant-autoscaler-system -l control-plane=controller-manager

# Check controller events
kubectl get events -n workload-variant-autoscaler-system --field-selector involvedObject.kind=ServiceMonitor
```

**Solution**: Recreate the ServiceMonitor if it was deleted.

## Related Documentation

- [Architecture Overview](modeling-optimization.md)
- [Configuration Guide](../user-guide/configuration.md)
- [CRD Reference](../user-guide/crd-reference.md)
- [Debugging Guide](../developer-guide/debugging.md)
