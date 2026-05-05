# Troubleshooting Guide

This guide helps you diagnose and resolve common issues with the Workload Variant Autoscaler (WVA).

## Why is my VariantAutoscaling not being reconciled?

If your VariantAutoscaling resource is not being reconciled by the controller, check the following potential causes in order:

### 1. Namespace Exclusion Annotation

The controller respects the `wva.llmd.ai/exclude: "true"` annotation on namespaces. If this annotation is set on a namespace, the controller will **NOT** reconcile any VariantAutoscaling resources in that namespace.

**Check for exclusion annotation:**

```bash
kubectl get namespace <namespace-name> -o jsonpath='{.metadata.annotations.wva\.llmd\.ai/exclude}'
```

**Resolution:**

Remove the exclusion annotation if you want the controller to manage VAs in this namespace:

```bash
kubectl annotate namespace <namespace-name> wva.llmd.ai/exclude-
```

**Important:** The exclusion annotation is **ignored** if you're running the controller with the `--watch-namespace` flag set to that specific namespace. In single-namespace mode, the explicit CLI flag takes precedence over annotation-based filtering.

---

### 2. Controller Instance Label (Multi-Controller Setup)

If you're running multiple controller instances (using the `CONTROLLER_INSTANCE` environment variable), each VariantAutoscaling resource must have the `wva.llmd.ai/controller-instance` label matching the controller instance that should manage it.

**Check your VA's controller-instance label:**

```bash
kubectl get va <va-name> -n <namespace> \
  -o jsonpath='{.metadata.labels.wva\.llmd\.ai/controller-instance}'
```

**Check what controller instance is configured:**

```bash
kubectl get deployment -n <controller-namespace> workload-variant-autoscaler-controller-manager \
  -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="CONTROLLER_INSTANCE")].value}'
```

**Resolution:**

Add or update the label on your VariantAutoscaling resource:

```yaml
apiVersion: llm-d.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: my-va
  namespace: my-namespace
  labels:
    wva.llmd.ai/controller-instance: "my-controller-instance"
spec:
  # ... your VA spec ...
```

For more information on multi-controller setups, see the [Multi-Controller Isolation guide](multi-controller-isolation.md).

---

### 3. Single-Namespace Mode

If the controller is running with the `--watch-namespace` flag, it will **ONLY** watch that specific namespace and ignore all others.

**Check if single-namespace mode is enabled:**

```bash
kubectl get deployment -n <controller-namespace> workload-variant-autoscaler-controller-manager \
  -o jsonpath='{.spec.template.spec.containers[0].args}' | grep watch-namespace
```

**Resolution:**

Either:

**Option A:** Remove the `--watch-namespace` flag to watch all namespaces:

Edit the controller deployment and remove `--watch-namespace` from the container args.

**Option B:** Set `--watch-namespace` to the namespace containing your VA:

```yaml
spec:
  template:
    spec:
      containers:
      - name: manager
        args:
        - --watch-namespace=my-namespace
        # ... other args ...
```

---

### 4. Target Deployment Not Found

The VariantAutoscaling resource must reference an existing Deployment. If the target deployment doesn't exist, the VA will not scale.

**Check if target deployment exists:**

```bash
kubectl get deployment <deployment-name> -n <namespace>
```

**Check VA status for error messages:**

```bash
kubectl get va <va-name> -n <namespace> -o yaml
```

Look for status conditions indicating missing target deployment.

**Resolution:**

Create the target deployment, or update the VA's `scaleTargetRef` to reference the correct deployment:

```yaml
spec:
  scaleTargetRef:
    name: my-deployment  # Must match an existing deployment name
```

---

### 5. Invalid VariantAutoscaling Configuration

The VA resource may have validation errors preventing reconciliation.

**Check for validation errors:**

```bash
kubectl describe va <va-name> -n <namespace>
```

Look for events indicating validation failures.

**Common validation issues:**

- Invalid `scaleTargetRef` (missing `name` or `kind`)
- Invalid metric names in `metrics` section
- Missing required fields

**Resolution:**

Fix the validation errors and reapply the resource.

---

### 6. Controller Not Running

The controller itself may not be running or may be experiencing errors.

**Check controller pod status:**

```bash
kubectl get pods -n <controller-namespace> \
  -l control-plane=controller-manager
```

**Check controller logs for errors:**

```bash
kubectl logs -n <controller-namespace> \
  deployment/workload-variant-autoscaler-controller-manager \
  --tail=100
```

**Resolution:**

If the controller pod is not running:

1. Check for resource constraints (CPU/memory limits)
2. Check for image pull errors
3. Review controller logs for startup errors

Restart the controller if necessary:

```bash
kubectl rollout restart deployment/workload-variant-autoscaler-controller-manager \
  -n <controller-namespace>
```

---

## Why is my namespace-local ConfigMap not being used?

Namespace-local ConfigMaps (for saturation configuration or scale-to-zero settings) may not be applied if certain conditions aren't met.

### 1. Namespace Not Tracked

By default, namespace-local ConfigMaps are only watched in namespaces that:

- Have at least one VariantAutoscaling resource, **OR**
- Have the `wva.llmd.ai/config-enabled: "true"` label

**Check if namespace is tracked:**

```bash
# Check for VAs in the namespace
kubectl get va -n <namespace>

# Check for opt-in label
kubectl get namespace <namespace> \
  -o jsonpath='{.metadata.labels.wva\.llmd\.ai/config-enabled}'
```

**Resolution:**

Either:

**Option A:** Create a VariantAutoscaling resource in the namespace (automatic tracking)

**Option B:** Add the opt-in label to the namespace:

```bash
kubectl label namespace <namespace> wva.llmd.ai/config-enabled=true
```

---

### 2. Namespace Excluded

As with VariantAutoscaling resources, namespace-local ConfigMaps are **not** watched in excluded namespaces.

**Check for exclusion annotation:**

```bash
kubectl get namespace <namespace> \
  -o jsonpath='{.metadata.annotations.wva\.llmd\.ai/exclude}'
```

**Resolution:**

Remove the exclusion annotation:

```bash
kubectl annotate namespace <namespace> wva.llmd.ai/exclude-
```

**Note:** In single-namespace mode (`--watch-namespace` set), exclusion is ignored for the watched namespace.

---

### 3. ConfigMap Name Mismatch

Namespace-local ConfigMaps must use one of the well-known names:

- `wva-saturation-config` (for saturation configuration)
- `wva-scale-to-zero-config` (for scale-to-zero configuration)

**Check ConfigMap name:**

```bash
kubectl get configmap -n <namespace>
```

**Resolution:**

Rename your ConfigMap to use the correct well-known name.

---

## Why are my optimizations not taking effect?

### 1. Global Optimization Disabled

Check if global optimization is enabled in the controller configuration.

**Check controller configuration:**

```bash
kubectl get configmap wva-variantautoscaling-config \
  -n <controller-namespace> \
  -o jsonpath='{.data.GLOBAL_OPT_ENABLE}'
```

**Resolution:**

Enable global optimization by setting `GLOBAL_OPT_ENABLE: "true"` in the ConfigMap.

---

### 2. Optimization Interval Too Long

The controller reconciles VAs periodically. If the optimization interval is very long, changes may take time to apply.

**Check optimization interval:**

```bash
kubectl get configmap wva-variantautoscaling-config \
  -n <controller-namespace> \
  -o jsonpath='{.data.GLOBAL_OPT_INTERVAL}'
```

Default is `60s` (60 seconds).

**Resolution:**

Reduce the interval for faster reconciliation (not recommended for production):

```yaml
data:
  GLOBAL_OPT_INTERVAL: "30s"
```

---

### 3. Prometheus Metrics Not Available

The optimizer relies on Prometheus metrics. If metrics are unavailable, optimization cannot occur.

**Check Prometheus connectivity:**

```bash
# Check controller logs for Prometheus errors
kubectl logs -n <controller-namespace> \
  deployment/workload-variant-autoscaler-controller-manager \
  | grep -i prometheus
```

**Check ServiceMonitor:**

```bash
kubectl get servicemonitor -n <controller-namespace> \
  workload-variant-autoscaler-controller-manager-metrics-monitor
```

**Resolution:**

Ensure:
1. Prometheus is deployed and accessible
2. ServiceMonitor is created and configured correctly
3. Prometheus is scraping the controller metrics endpoint

---

## Additional Resources

- [Configuration Guide](configuration.md)
- [Multi-Controller Isolation](multi-controller-isolation.md)
- [CRD Reference](crd-reference.md)
- [Developer Guide](../developer-guide/)

---

## Getting Help

If you're still experiencing issues after following this guide:

1. **Check the logs:** Controller logs often contain detailed error messages
2. **Review the status:** Use `kubectl describe va` to see status conditions and events
3. **Open an issue:** Report bugs or request help at [GitHub Issues](https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues)
