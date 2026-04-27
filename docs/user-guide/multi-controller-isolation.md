# Multi-Controller Isolation

This guide explains how to run multiple WVA controller instances in the same Kubernetes cluster with proper isolation.

## Overview

By default, WVA operates as a single cluster-wide controller that manages all VariantAutoscaling (VA) resources and emits metrics to Prometheus. When running multiple WVA controller instances simultaneously (e.g., for parallel end-to-end tests, multi-tenant environments, or A/B testing), controllers may emit conflicting metrics that confuse HPA scaling decisions.

The **controller instance isolation** feature allows multiple WVA controllers to coexist in the same cluster by:

1. **Labeling metrics** with a unique `controller_instance` identifier
2. **Filtering VA resources** so each controller only manages explicitly assigned VAs
3. **Scoping HPA queries** to select metrics from specific controller instances

## Use Cases

### Parallel E2E Testing

Run multiple independent test suites simultaneously without metric conflicts:

```bash
# Test suite A with controller instance "test-a"
CONTROLLER_INSTANCE=test-a make test-e2e-full

# Test suite B with controller instance "test-b" (runs in parallel)
CONTROLLER_INSTANCE=test-b make test-e2e-full
```

Each test suite:
- Deploys its own WVA controller with a unique instance ID
- Creates VAs labeled with matching `controller_instance`
- HPA reads metrics filtered by `controller_instance` label
- No interference between test suites

### Multi-Tenant Environments

Isolate autoscaling for different teams or environments:

```yaml
# Team A controller in namespace wva-team-a
wva:
  controllerInstance: "team-a"

# Team B controller in namespace wva-team-b  
wva:
  controllerInstance: "team-b"
```

Each team's controller only manages VAs in their designated namespace with matching labels.

### Adding Models to an Existing Controller

The most common multi-model pattern uses a **single controller** with multiple model
installations. Install the controller once, then add models using `controller.enabled=false`:

```bash
# Step 1: Install the WVA controller (once per cluster or namespace)
helm upgrade -i wva-controller ./charts/workload-variant-autoscaler \
  --namespace wva-system \
  --create-namespace \
  --set controller.enabled=true \
  --set va.enabled=false \
  --set hpa.enabled=false \
  --set vllmService.enabled=false
```

```bash
# Step 2: Add Model A (only VA + HPA resources, no controller)
helm upgrade -i wva-model-a ./charts/workload-variant-autoscaler \
  --namespace wva-system \
  --set controller.enabled=false \
  --set va.enabled=true \
  --set hpa.enabled=true \
  --set llmd.namespace=team-a \
  --set llmd.modelName=my-model-a \
  --set llmd.modelID="meta-llama/Llama-3.1-8B"
```

```bash
# Step 3: Add Model B (same controller manages both models)
helm upgrade -i wva-model-b ./charts/workload-variant-autoscaler \
  --namespace wva-system \
  --set controller.enabled=false \
  --set va.enabled=true \
  --set hpa.enabled=true \
  --set llmd.namespace=team-b \
  --set llmd.modelName=my-model-b \
  --set llmd.modelID="meta-llama/Llama-3.1-70B"
```

With `controller.enabled=false`, the chart deploys only:

- **VariantAutoscaling** CR (if `va.enabled=true`)
- **HorizontalPodAutoscaler** (if `hpa.enabled=true`)
- **Service** and **ServiceMonitor** for vLLM metrics (if `vllmService.enabled=true`)
- **RBAC** ClusterRoles for VA resources (viewer, editor, admin)

It skips all controller infrastructure: Deployment, ServiceAccount, ConfigMaps, RBAC
bindings, leader election roles, and prometheus CA certificates.

> **Tip:** If using `controllerInstance` for metric isolation, set the same value on both the
> controller install and all model installs so the HPA metric selectors match.

### Canary/Blue-Green Deployments

Test new WVA versions alongside production:

```yaml
# Production controller
wva:
  controllerInstance: "production"

# Canary controller with new version
wva:
  controllerInstance: "canary"
  image:
    tag: v0.5.0-rc1
```

## Configuration

### Helm Values

Enable controller instance isolation by setting `wva.controllerInstance`:

```yaml
# values.yaml
wva:
  controllerInstance: "my-instance-id"
```

Install with Helm:

```bash
helm upgrade -i workload-variant-autoscaler ./charts/workload-variant-autoscaler \
  --namespace workload-variant-autoscaler-system \
  --set wva.controllerInstance=my-instance-id
```

### Environment Variable

The controller instance is configured via the `CONTROLLER_INSTANCE` environment variable:

```yaml
# deployment.yaml
spec:
  template:
    spec:
      containers:
      - name: manager
        env:
        - name: CONTROLLER_INSTANCE
          value: "my-instance-id"
```

### VariantAutoscaling Labels

When `controllerInstance` is set, the Helm chart automatically adds the label to VA resources:

```yaml
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: llama-8b-autoscaler
  labels:
    wva.llmd.ai/controller-instance: "my-instance-id"
spec:
  modelId: "meta-llama/Llama-3.1-8B"
```

**Important:** Each controller only reconciles VAs with a matching `controller-instance` label. VAs without this label are managed by controllers without `CONTROLLER_INSTANCE` set.

## How It Works

### Metric Labeling

When `CONTROLLER_INSTANCE` is set, all emitted metrics include a `controller_instance` label:

```promql
# Without controller instance isolation
wva_desired_replicas{variant_name="llama-8b",namespace="llm-d",accelerator_type="H100"}

# With controller instance isolation  
wva_desired_replicas{variant_name="llama-8b",namespace="llm-d",accelerator_type="H100",controller_instance="my-instance-id"}
```

Affected metrics:
- `wva_replica_scaling_total`
- `wva_desired_replicas`
- `wva_current_replicas`
- `wva_desired_ratio`

### VA Resource Filtering

The controller uses a predicate filter to watch only VAs with matching labels:

```go
// Controller watches VAs where:
// - Label wva.llmd.ai/controller-instance == CONTROLLER_INSTANCE (if set)
// - Label is absent (if CONTROLLER_INSTANCE is not set)
```

This ensures complete isolation - each controller only reconciles its assigned VAs.

### HPA Metric Selection

The HPA template automatically filters metrics by `controller_instance` when set:

```yaml
# HPA with controller instance filtering
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
spec:
  metrics:
  - type: Pods
    pods:
      metric:
        name: wva_desired_replicas
        selector:
          matchLabels:
            variant_name: "llama-8b"
            exported_namespace: "llm-d"
            controller_instance: "my-instance-id"
```

## Backwards Compatibility

The feature is **fully backwards compatible**:

- **When `controllerInstance` is NOT set:**
  - No `controller_instance` label added to metrics
  - Controller manages all VAs (no label filtering)
  - HPA queries metrics without `controller_instance` selector
  - Behavior identical to previous versions

- **When `controllerInstance` IS set:**
  - `controller_instance` label added to all metrics
  - Controller only manages VAs with matching label
  - HPA queries metrics filtered by `controller_instance`

Upgrading existing deployments requires no changes unless you want to enable multi-controller isolation.

## Best Practices

### Naming Conventions

Use descriptive controller instance identifiers:

```yaml
# ✅ Good - clear purpose
controllerInstance: "prod"
controllerInstance: "staging"  
controllerInstance: "e2e-test-12345"
controllerInstance: "team-ml-inference"

# ❌ Avoid - unclear purpose
controllerInstance: "c1"
controllerInstance: "test"
```

### Label Management

**Do NOT manually add/remove controller-instance labels** on VA resources managed by Helm. The Helm chart manages these labels automatically.

For manually created VAs, ensure labels match the target controller:

```yaml
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  labels:
    wva.llmd.ai/controller-instance: "my-instance-id"  # Must match controller
```

### Monitoring

Query metrics for specific controller instances:

```promql
# Check desired replicas for specific controller instance
wva_desired_replicas{controller_instance="prod"}

# Compare scaling events across instances
sum by (controller_instance, direction) (
  rate(wva_replica_scaling_total[5m])
)

# Alert on missing controller instance metrics
absent(wva_current_replicas{controller_instance="prod"})
```

### Cleanup

When removing a controller instance, clean up associated resources:

```bash
# Delete controller deployment
helm uninstall workload-variant-autoscaler-instance-a

# Clean up orphaned VAs with instance label
kubectl delete va -l wva.llmd.ai/controller-instance=instance-a

# Clean up HPAs
kubectl delete hpa -l wva.llmd.ai/controller-instance=instance-a
```

## Troubleshooting

### VA Not Being Reconciled

**Symptom:** VA status shows `ObservedGeneration: 0` or conditions never update.

**Cause:** Label mismatch between VA and controller instance.

**Solution:**
1. Check controller instance configuration:
   ```bash
   kubectl get deploy -n workload-variant-autoscaler-system \
     workload-variant-autoscaler-controller-manager \
     -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="CONTROLLER_INSTANCE")].value}'
   ```

2. Check VA label:
   ```bash
   kubectl get va llama-8b-autoscaler \
     -o jsonpath='{.metadata.labels.wva\.llmd\.ai/controller-instance}'
   ```

3. Ensure labels match or add missing label:
   ```bash
   kubectl label va llama-8b-autoscaler \
     wva.llmd.ai/controller-instance=my-instance-id
   ```

### HPA Not Scaling

**Symptom:** HPA shows `<unknown>` for custom metric or doesn't scale deployment.

**Cause:** HPA metric selector doesn't match controller instance label.

**Solution:**
1. Check HPA metric selector:
   ```bash
   kubectl get hpa llama-8b-hpa -o yaml | grep -A 10 selector
   ```

2. Verify metrics exist with expected labels:
   ```promql
   wva_desired_replicas{
     variant_name="llama-8b",
     controller_instance="my-instance-id"
   }
   ```

3. Update HPA selector to include `controller_instance` label.

### Metric Conflicts

**Symptom:** Multiple controllers emit metrics for same variant, causing erratic HPA behavior.

**Cause:** Multiple controllers running without proper instance isolation.

**Solution:**
1. Set unique `controllerInstance` for each controller
2. Ensure VA labels match respective controller instances
3. Verify HPA selectors filter by `controller_instance`

## Examples

### Example 1: Parallel Testing

Deploy two test environments simultaneously:

```bash
# Environment A
helm upgrade -i wva-test-a ./charts/workload-variant-autoscaler \
  --namespace wva-test-a \
  --create-namespace \
  --set wva.controllerInstance=test-a \
  --set llmd.namespace=llm-test-a

# Environment B
helm upgrade -i wva-test-b ./charts/workload-variant-autoscaler \
  --namespace wva-test-b \
  --create-namespace \
  --set wva.controllerInstance=test-b \
  --set llmd.namespace=llm-test-b
```

Each environment operates independently with isolated metrics and scaling decisions.

### Example 2: Gradual Rollout

Test new WVA version for subset of workloads:

```yaml
# Production controller (v0.4.1) manages production VAs
wva:
  controllerInstance: "prod"
  image:
    tag: v0.4.1

---
# Canary controller (v0.5.0) manages canary VAs  
wva:
  controllerInstance: "canary"
  image:
    tag: v0.5.0-rc1
```

Create canary VAs with `controller-instance: canary` label to test new version.

### Example 3: Multi-Tenant Platform

Isolate autoscaling for different teams:

```bash
# Deploy per-team controllers
for team in ml-research ml-production data-science; do
  helm upgrade -i wva-${team} ./charts/workload-variant-autoscaler \
    --namespace wva-${team} \
    --create-namespace \
    --set wva.controllerInstance=${team} \
    --set llmd.namespace=${team}
done
```

Each team's workloads are managed by their dedicated controller instance.

## Related Documentation

- [Installation Guide](installation.md) - Setting up WVA
- [Configuration Guide](configuration.md) - Configuring VariantAutoscaling resources
- [HPA Integration](hpa-integration.md) - Integrating with Horizontal Pod Autoscaler
- [Testing Guide](../developer-guide/testing.md) - Running E2E tests with controller isolation
