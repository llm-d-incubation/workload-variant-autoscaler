# Configuration Guide

This guide explains how to configure Workload-Variant-Autoscaler for your workloads.

## Deployment Lifecycle Management

### Creating VariantAutoscaling Resources

WVA automatically handles the relationship between VariantAutoscaling (VA) resources and their target Deployments:

**Recommended Order:**
```bash
# 1. Create and verify the deployment is ready
kubectl apply -f deployment.yaml
kubectl wait --for=condition=available deployment/llama-8b --timeout=300s

# 2. Create the VariantAutoscaling resource
kubectl apply -f variantautoscaling.yaml
```

**Race Condition Protection:**

WVA handles the race condition where a VA is created before its target deployment exists. If you create a VA before its deployment:

1. VA is created with status indicating the deployment is not found
2. When the deployment is created, WVA automatically detects it
3. VA immediately begins monitoring and autoscaling (no wait for periodic reconciliation)

```yaml
# VA created first - will automatically detect deployment when it appears
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: llama-8b-autoscaler
spec:
  scaleTargetRef:
    name: llama-8b  # Deployment doesn't exist yet
  # ... other config
```

### Deployment Deletion Handling

When a target deployment is deleted, WVA immediately:

1. **Updates VA Status**: Marks the VA as not ready with reason `DeploymentNotFound`
2. **Clears Metrics**: Removes stale metrics to prevent incorrect autoscaling decisions
3. **Maintains VA Resource**: The VA itself is not deleted and will resume operation when deployment is recreated

**Example Status After Deployment Deletion:**

```yaml
status:
  conditions:
  - type: Ready
    status: "False"
    reason: "DeploymentNotFound"
    message: "Target deployment 'llama-8b' no longer exists"
  desiredOptimizedAlloc: {}  # Cleared to reflect no deployment
```

**Recovery Process:**

When the deployment is recreated, WVA automatically:
1. Detects the new deployment immediately (via Create event)
2. Updates VA status to Ready
3. Resumes monitoring and autoscaling

No manual intervention required!

### Best Practices

1. **Always specify scaleTargetRef explicitly** when the deployment name differs from the model ID:
   ```yaml
   spec:
     scaleTargetRef:
       name: my-custom-deployment-name
     modelId: "meta-llama/Llama-3.1-8B"
   ```

2. **Monitor VA status** to detect deployment issues:
   ```bash
   kubectl get va llama-8b-autoscaler -o jsonpath='{.status.conditions[?(@.type=="Ready")]}'
   ```

3. **Use consistent naming** - naming your deployment and VA with related names helps with operational clarity.

## VariantAutoscaling Resource

The `VariantAutoscaling` CR is the primary configuration interface for WVA.

### Basic Example

```yaml
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: llama-8b-autoscaler
  namespace: llm-inference
spec:
  scaleTargetRef:
    kind: Deployment
    name: llama-8b
  modelID: "meta/llama-3.1-8b"
  variantCost: "10.0"  # Optional, defaults to "10.0"
```

### Complete Reference

For complete field documentation, see the [CRD Reference](crd-reference.md).

## Operating Mode

WVA operates in **saturation mode**.

### Saturation Mode

- **Behavior**: Reactive scaling based on saturation detection
- **How It Works**: Monitors KV cache usage and queue lengths, scales when thresholds exceeded
- **Configuration**: Uses `capacity-scaling-config` ConfigMap
- **Pros**: Fast response (<30s), predictable, no model training needed
- **Cons**: Reactive (scales after saturation detected)

See [Saturation Analyzer Documentation](../../docs/saturation-analyzer.md) for configuration details.

## ConfigMaps

WVA uses ConfigMaps for cluster-wide configuration.

### Configuration Precedence

Configuration values are resolved with following precedence (highest to lowest):

1. **CLI Flags** — only when explicitly set on the command line (highest priority)
2. **Environment Variables**
3. **ConfigMap** (in `workload-variant-autoscaler-system` namespace)
4. **Defaults** (lowest priority)

> **Note:** CLI flag defaults do **not** override environment variables or ConfigMap values.
> Only flags that are explicitly passed on the command line take precedence.
> For example, if `--leader-elect` is not passed but `LEADER_ELECT=true` is set in
> the environment, the environment value (`true`) is used.

**Example:**
```bash
# CLI flag explicitly set (highest priority)
--metrics-bind-address=":8443"

# Environment variable (used when flag is not explicitly set)
export METRICS_BIND_ADDRESS=":8080"

# ConfigMap (used when neither flag nor env is set)
# wva-variantautoscaling-config
data:
  METRICS_BIND_ADDRESS: ":9090"

# Default (used if none of the above are set)
# Default: "0" (disabled)
```

### Immutable vs Mutable Parameters

#### Immutable Parameters (Require Restart)

These settings **cannot** be changed at runtime via ConfigMap updates. Attempts to change them will:
- Be rejected by the controller
- Emit a Warning Kubernetes event
- Require a controller restart to take effect

**Immutable Parameters:**
- `PROMETHEUS_BASE_URL` - Prometheus connection endpoint
- `METRICS_BIND_ADDRESS` - Metrics bind address
- `HEALTH_PROBE_BIND_ADDRESS` - Health probe bind address
- `LEADER_ELECTION_ID` - Leader election coordination ID
- TLS certificate paths (webhook and metrics certificates)

**Example - Attempting to Change Immutable Parameter:**
```yaml
# This will be rejected and emit a Warning event
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-variantautoscaling-config
  namespace: workload-variant-autoscaler-system
data:
  PROMETHEUS_BASE_URL: "https://new-prometheus:9090"  # Requires restart
```

**Check for Rejected Changes:**
```bash
# View Warning events
kubectl get events -n workload-variant-autoscaler-system \
  --field-selector reason=ImmutableConfigChangeRejected

# Controller logs
kubectl logs -n workload-variant-autoscaler-system \
  deployment/workload-variant-autoscaler-controller-manager | \
  grep "Attempted to change immutable parameters"
```

#### Mutable Parameters (Runtime Updates)

These settings **can** be changed at runtime via ConfigMap updates without restarting the controller:

**Mutable Parameters:**
- `GLOBAL_OPT_INTERVAL` - Optimization interval (default: `60s`)
- Saturation scaling configuration (via `wva-saturation-scaling-config` ConfigMap)
- Scale-to-zero configuration (via `wva-model-scale-to-zero-config` ConfigMap)
- Prometheus cache settings

**Example - Runtime Configuration Update:**
```yaml
# This will be applied immediately without restart
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-variantautoscaling-config
  namespace: workload-variant-autoscaler-system
data:
  GLOBAL_OPT_INTERVAL: "120s"  # Applied immediately
```

### Immutable ConfigMap (Security Hardening)

For enhanced security, you can make the entire ConfigMap immutable using the Helm chart option `wva.configMap.immutable: true`. This provides additional protection beyond the controller's runtime validation.

**Security Benefits:**
- **Prevents accidental changes**: Kubernetes will reject any update attempts
- **Protects against malicious modifications**: Even with RBAC access, the ConfigMap cannot be modified
- **Ensures configuration integrity**: Configuration can only be changed through controlled Helm upgrades
- **Reduces attack surface**: Eliminates runtime configuration as a potential attack vector

**Trade-offs:**
- **Runtime updates disabled**: All configuration changes (including mutable parameters) require ConfigMap recreation
- **Change process**: To update configuration:
  1. Delete the ConfigMap: `kubectl delete configmap <name> -n <namespace>`
  2. Update Helm values and upgrade: `helm upgrade ... --set wva.configMap.immutable=false ...`
  3. Restart the controller pod

**Enable Immutable ConfigMap:**
```bash
# Via Helm values
helm install workload-variant-autoscaler ./charts/workload-variant-autoscaler \
  -n workload-variant-autoscaler-system \
  --set wva.configMap.immutable=true
```

**When to Use:**
- **Production environments** with strict security requirements
- **Multi-tenant clusters** where configuration tampering is a concern
- **Compliance requirements** that mandate immutable infrastructure
- **High-security deployments** where configuration changes should be audited and controlled

**When NOT to Use:**
- **Development environments** where rapid iteration is needed
- **Scenarios requiring frequent runtime config updates** (e.g., A/B testing, dynamic tuning)
- **Environments where ConfigMap updates are part of normal operations**

### Namespace-Local ConfigMap Overrides

WVA supports namespace-local ConfigMap overrides that allow different namespaces to have different configuration settings without requiring separate controller instances. This provides a middle ground between global configuration and full multi-controller isolation.

**Use Cases:**
- **Different teams sharing a cluster** with different SLO requirements
- **Staging vs production namespaces** with different scaling thresholds
- **Gradual rollout** of new thresholds in one namespace before applying cluster-wide
- **Environment-specific tuning** without operational overhead

**How It Works:**

1. **Global ConfigMap** (in controller namespace): Provides default configuration for all namespaces
2. **Namespace-Local ConfigMap** (in target namespace): Overrides global settings for that namespace only
3. **Resolution Order**: Namespace-local > Global (automatic fallback if namespace-local doesn't exist)

**Well-Known ConfigMap Names:**

The following ConfigMap names are recognized for namespace-local overrides:
- `wva-saturation-scaling-config` - Saturation scaling thresholds
- `wva-model-scale-to-zero-config` - Scale-to-zero configuration

**Example: Namespace-Local Saturation Config**

```yaml
# Global ConfigMap (in workload-variant-autoscaler-system namespace)
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-saturation-scaling-config
  namespace: workload-variant-autoscaler-system
data:
  default: |
    kvCacheThreshold: 0.80
    queueLengthThreshold: 5
    kvSpareTrigger: 0.10
    queueSpareTrigger: 3
```

```yaml
# Namespace-Local Override (in production namespace)
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-saturation-scaling-config  # Same well-known name
  namespace: production  # Different namespace
data:
  default: |
    kvCacheThreshold: 0.70  # More aggressive for production
    queueLengthThreshold: 3
    kvSpareTrigger: 0.20
    queueSpareTrigger: 5
```

**Result**: VAs in the `production` namespace use production thresholds (0.70), while VAs in other namespaces use global defaults (0.80).

**Example: Namespace-Local Scale-to-Zero Config**

```yaml
# Global ConfigMap
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-model-scale-to-zero-config
  namespace: workload-variant-autoscaler-system
data:
  model1: |
    model_id: model1
    enable_scale_to_zero: true
    retention_period: 10m
```

```yaml
# Namespace-Local Override
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-model-scale-to-zero-config
  namespace: staging
data:
  model1: |
    model_id: model1
    enable_scale_to_zero: false  # Disable scale-to-zero in staging
    retention_period: 5m
```

**ConfigMap Deletion:**

When a namespace-local ConfigMap is deleted, WVA automatically falls back to the global configuration. No restart required - the fallback happens immediately.

```bash
# Delete namespace-local ConfigMap
kubectl delete configmap wva-saturation-scaling-config -n production

# VAs in production namespace now use global config
```

**Namespace Discovery:**

WVA uses a hybrid approach to discover namespaces for namespace-local ConfigMap watching:

1. **Automatic (VA-based)**: WVA automatically tracks namespaces that have VariantAutoscaling resources. This is the default behavior - no configuration needed.

2. **Explicit Opt-in (Label-based)**: You can opt-in namespaces by adding the label `wva.llmd.ai/config-enabled=true` to a namespace. This enables namespace-local ConfigMap watching even before VariantAutoscaling resources are created, avoiding race conditions.

**Example: Opt-in a namespace for namespace-local ConfigMaps:**

```bash
# Label a namespace to enable namespace-local ConfigMap watching
kubectl label namespace production wva.llmd.ai/config-enabled=true

# Now you can create namespace-local ConfigMaps before VAs exist
kubectl apply -f - <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-saturation-scaling-config
  namespace: production
data:
  default: |
    kvCacheThreshold: 0.70
    queueLengthThreshold: 3
EOF
```

**When to use label-based opt-in:**
- Creating namespace-local ConfigMaps before VariantAutoscaling resources exist
- Explicitly controlling which namespaces can have overrides (security/audit)
- Multi-controller isolation (each controller can watch different label values)

**Limitations:**

- **Main ConfigMap** (`wva-variantautoscaling-config`) is only supported globally, not as namespace-local override
- **Optimization interval** (`GLOBAL_OPT_INTERVAL`) is global only
- **Prometheus cache settings** are global only

**Relationship with Multi-Controller Isolation:**

Namespace-local ConfigMaps are **complementary** to multi-controller isolation:
- **Namespace-local ConfigMaps**: Single controller, configuration isolation only
- **Multi-controller isolation**: Multiple controllers, complete operational isolation

They can be used together - you can have multiple controller instances, each using namespace-local configs within their scope.

### Main Configuration ConfigMap

The main configuration ConfigMap (`wva-variantautoscaling-config`) supports both static and dynamic settings:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-variantautoscaling-config
  namespace: workload-variant-autoscaler-system
data:
  # Mutable: Optimization interval (can be changed at runtime)
  GLOBAL_OPT_INTERVAL: "60s"

  # Immutable: Prometheus connection (requires restart if changed)
  PROMETHEUS_BASE_URL: "https://prometheus:9090"

  # Immutable: Feature flags (require restart if changed)
  WVA_SCALE_TO_ZERO: "true"
  WVA_LIMITED_MODE: "false"
```

**Note:** The ConfigMap name is auto-generated by Helm based on the release name. For Kustomize deployments, set the `CONFIG_MAP_NAME` environment variable in the deployment manifest.

### Configuration via Environment Variables

Many settings can be configured via environment variables (useful for containerized deployments):

```yaml
# Deployment manifest
env:
  # Prometheus connection (immutable - requires restart to change)
  - name: PROMETHEUS_BASE_URL
    value: "https://prometheus:9090"

  # Optional: Override ConfigMap name
  - name: CONFIG_MAP_NAME
    value: "my-custom-config"

  # Optional: Override namespace
  - name: POD_NAMESPACE
    value: "workload-variant-autoscaler-system"
```

**See:** [Prometheus Integration](../integrations/prometheus.md) for complete Prometheus configuration options.

### Configuration via CLI Flags

Infrastructure settings can be configured via CLI flags. Only flags explicitly passed on the command line take highest precedence; unset flags fall through to environment variables, ConfigMap, and then defaults.

```bash
# Start controller with custom settings
./manager \
  --metrics-bind-address=":8443" \
  --health-probe-bind-address=":8081" \
  --leader-elect \
  --leader-election-lease-duration=60s \
  --leader-election-renew-deadline=50s \
  --leader-election-retry-period=10s \
  --rest-client-timeout=60s
```

### Configuration Parameter Reference

The following table lists all static configuration parameters with their CLI flag, environment variable, ConfigMap key, type, and default value. All three sources share the same key name (except CLI flags which use kebab-case).

**Note:** CLI flags are typically set in the Helm chart or deployment manifest, not directly.

| Parameter | CLI Flag | Env Var / ConfigMap Key | Type | Default | Description |
|-----------|----------|------------------------|------|---------|-------------|
| Metrics bind address | `--metrics-bind-address` | `METRICS_BIND_ADDRESS` | string | `0` | Metrics endpoint bind address (`:8443` for HTTPS, `:8080` for HTTP, `0` to disable) |
| Health probe address | `--health-probe-bind-address` | `HEALTH_PROBE_BIND_ADDRESS` | string | `:8081` | Health probe endpoint bind address |
| Leader election | `--leader-elect` | `LEADER_ELECT` | bool | `false` | Enable leader election for HA |
| Leader election ID | — | `LEADER_ELECTION_ID` | string | `72dd1cf1.llm-d.ai` | Leader election coordination ID |
| Lease duration | `--leader-election-lease-duration` | `LEADER_ELECTION_LEASE_DURATION` | duration | `60s` | Duration non-leaders wait before force-acquiring leadership |
| Renew deadline | `--leader-election-renew-deadline` | `LEADER_ELECTION_RENEW_DEADLINE` | duration | `50s` | Duration the leader retries refreshing before giving up |
| Retry period | `--leader-election-retry-period` | `LEADER_ELECTION_RETRY_PERIOD` | duration | `10s` | Duration between retry attempts |
| REST timeout | `--rest-client-timeout` | `REST_CLIENT_TIMEOUT` | duration | `60s` | Timeout for Kubernetes API server REST calls |
| Secure metrics | `--metrics-secure` | `METRICS_SECURE` | bool | `true` | Serve metrics endpoint via HTTPS |
| Enable HTTP/2 | `--enable-http2` | `ENABLE_HTTP2` | bool | `false` | Enable HTTP/2 for metrics and webhook servers |
| Watch namespace | `--watch-namespace` | `WATCH_NAMESPACE` | string | `""` | Namespace to watch (empty = all namespaces) |
| Log verbosity | `-v` | `V` | int | `2` | Log level verbosity |
| Webhook cert path | `--webhook-cert-path` | `WEBHOOK_CERT_PATH` | string | `""` | Directory containing the webhook certificate |
| Webhook cert name | `--webhook-cert-name` | `WEBHOOK_CERT_NAME` | string | `tls.crt` | Webhook certificate file name |
| Webhook cert key | `--webhook-cert-key` | `WEBHOOK_CERT_KEY` | string | `tls.key` | Webhook key file name |
| Metrics cert path | `--metrics-cert-path` | `METRICS_CERT_PATH` | string | `""` | Directory containing the metrics server certificate |
| Metrics cert name | `--metrics-cert-name` | `METRICS_CERT_NAME` | string | `tls.crt` | Metrics server certificate file name |
| Metrics cert key | `--metrics-cert-key` | `METRICS_CERT_KEY` | string | `tls.key` | Metrics key file name |
| Scale to zero | — | `WVA_SCALE_TO_ZERO` | bool | `false` | Enable scale-to-zero feature |
| Limited mode | — | `WVA_LIMITED_MODE` | bool | `false` | Enable limited mode |
| Scale-from-zero concurrency | — | `SCALE_FROM_ZERO_ENGINE_MAX_CONCURRENCY` | int | `10` | Max concurrent scale-from-zero operations |

### Fail-Fast Validation

WVA implements **fail-fast** validation: if required configuration is missing or invalid, the controller will:
- **Not start** (exits with error code 1)
- Log clear error messages indicating what's missing
- Prevent running with invalid configuration

**Required Configuration:**
- `PROMETHEUS_BASE_URL` - Must be set via environment variable or ConfigMap

**Check Startup Errors:**
```bash
# View controller logs for validation errors
kubectl logs -n workload-variant-autoscaler-system \
  deployment/workload-variant-autoscaler-controller-manager | \
  grep -i "config\|validation\|error"

# Check pod status
kubectl get pods -n workload-variant-autoscaler-system
# If CrashLoopBackOff, check logs for config errors
```

### Configuration Update Behavior

**Static Config Updates:**
- Changes to immutable parameters are **rejected** at runtime
- Controller emits Warning events and logs errors
- **Action Required:** Restart the controller to apply changes

**Dynamic Config Updates:**
- Changes to mutable parameters are **applied immediately**
- Controller logs the changes (old → new values)
- No restart required

**Monitor Configuration Changes:**
```bash
# Watch for config update logs
kubectl logs -n workload-variant-autoscaler-system \
  deployment/workload-variant-autoscaler-controller-manager -f | \
  grep "Updated.*config"

# Example output:
# "Updated optimization interval" old=60s new=120s
# "Updated saturation config" oldEntries=2 newEntries=3
```

## Configuration Options

### Required Fields

The VariantAutoscaling CR has the following required fields:

- **scaleTargetRef**: Reference to the target Deployment to scale (follows HPA pattern)
  - **kind**: Resource kind (e.g., "Deployment")
  - **name**: Name of the deployment
- **modelID**: OpenAI API compatible identifier for your model (e.g., "meta/llama-3.1-8b")

### Optional Fields

- **variantCost**: Cost per replica for saturation-based cost optimization (default: "10.0")
  - Must be a string matching pattern `^\d+(\.\d+)?$` (numeric string)
  - Used by capacity analyzer when multiple variants can handle the load

### Cost Configuration

#### variantCost (Optional)

Specifies the cost per replica for this variant, used in saturation-based cost optimization.

```yaml
spec:
  modelID: "meta/llama-3.1-8b"
  variantCost: "15.5"  # Cost per replica (default: "10.0")
```

**Default:** "10.0"
**Validation:** Must be a string matching pattern `^\d+(\.\d+)?$` (numeric string)

**Use Cases:**
- **Differentiated Pricing**: Higher cost for premium accelerators (H100) vs. standard (A100)
- **Multi-Tenant Cost Tracking**: Assign different costs per customer/tenant
- **Cost-Based Optimization**: Saturation analyzer prefers lower-cost variants when multiple variants can handle load

**Example:**
```yaml
# Premium variant (H100, higher cost)
spec:
  modelID: "meta/llama-3.1-70b"
  variantCost: "80.0"

# Standard variant (A100, lower cost)
spec:
  modelID: "meta/llama-3.1-70b"
  variantCost: "40.0"
```

**Behavior:**
- Saturation analyzer uses `variantCost` when deciding which variant to scale
- If costs are equal, chooses variant with most available capacity
- Does not affect model-based optimization

### Advanced Options

See [CRD Reference](crd-reference.md) for advanced configuration options.

## Best Practices

### Environment Variables

WVA supports configuration via environment variables for operational settings:

**Prometheus Configuration:**
- `PROMETHEUS_BASE_URL`: Prometheus server URL (required for metrics collection)
- `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY`: Skip TLS verification (development only)
- `PROMETHEUS_CA_CERT_PATH`: CA certificate path for TLS
- `PROMETHEUS_CLIENT_CERT_PATH`: Client certificate for mutual TLS
- `PROMETHEUS_CLIENT_KEY_PATH`: Client key for mutual TLS
- `PROMETHEUS_SERVER_NAME`: Expected server name in TLS certificate
- `PROMETHEUS_BEARER_TOKEN`: Bearer token for authentication

**Other Configuration:**
- `CONFIG_MAP_NAME`: ConfigMap name (default: auto-generated from Helm release)
- `POD_NAMESPACE`: Controller namespace (auto-injected by Kubernetes)

See [Prometheus Integration](../integrations/prometheus.md) for detailed Prometheus configuration.

### Cost Optimization

- Assign higher costs to premium accelerators (H100) and lower costs to standard ones (A100)
- Use consistent cost values across variants of the same model to enable fair comparison
- The saturation analyzer will prefer scaling lower-cost variants when multiple can handle the load

### Deployment Configuration

- Always specify `scaleTargetRef` explicitly to avoid ambiguity
- Use descriptive names that indicate the model and accelerator type
- Add labels to deployments and VAs for easier operational management
- Monitor VA status conditions to detect issues with target deployments

## Monitoring Configuration

WVA exposes metrics for monitoring and integrates with HPA for automatic scaling.

### Safety Net Behavior

WVA includes a **safety net** that prevents HPA from using stale metrics during failures:

1. **Normal Operation**: Emits `wva_desired_replicas` with optimized targets
2. **Capacity Analysis Fails**:
   - Uses previous desired replicas (from last successful run)
   - If unavailable, uses current replicas (safe no-op)
3. **Log Messages**: Watch for `"Safety net activated"` in controller logs

**Check Safety Net Activation:**
```bash
# Controller logs
kubectl logs -n llm-d-scheduler deployment/wva-controller | grep "Safety net activated"

# Should see:
# "Safety net activated: emitted fallback metrics"
#   variant=my-va
#   currentReplicas=2
#   desiredReplicas=2
#   fallbackSource=current-replicas
```

**Why This Matters:**
- Prevents HPA from scaling based on stale metrics
- Provides graceful degradation during Prometheus outages
- Emits safe no-op signals (current=desired) when no history available

### Prometheus Metrics

See:
- [Prometheus Integration](../integrations/prometheus.md)
- [Custom Metrics](../integrations/prometheus.md#custom-metrics)

## Examples

More configuration examples in:
- [config/samples/](../../config/samples/)
- [Tutorials](../tutorials/)

## Multi-Controller Environments

When running multiple WVA controller instances in the same cluster (e.g., for parallel testing, multi-tenant setups, or canary deployments), use the **controller instance isolation** feature to prevent metric conflicts and ensure proper VA resource management.

### Quick Example

```yaml
# Helm values for controller instance A
wva:
  controllerInstance: "instance-a"

---
# Helm values for controller instance B
wva:
  controllerInstance: "instance-b"
```

Each controller will:
- Only manage VAs with matching `wva.llmd.ai/controller-instance` label
- Emit metrics with `controller_instance` label
- Have HPAs that filter metrics by `controller_instance`

For complete documentation, see [Multi-Controller Isolation Guide](multi-controller-isolation.md).

## Troubleshooting Configuration

### Common Issues

**Deployment Not Found:**
- Verify the deployment name in `scaleTargetRef` matches exactly
- Check that the deployment exists in the same namespace as the VA
- Review VA status conditions: `kubectl get va <name> -o yaml`

**Metrics Not Available:**
- Ensure Prometheus is properly configured and scraping vLLM metrics
- Verify ServiceMonitor is created for the vLLM deployment
- Check VA status condition `MetricsAvailable`

**Cost Optimization Not Working:**
- Verify `variantCost` is specified for all variants of the same model
- Check that variants have different costs to enable cost-based selection
- Review saturation analyzer logs for decision-making process
- Check if min replicas can be reduced

## Next Steps

- [Run the Quick Start Demo](../tutorials/demo.md)
- [Integrate with HPA](../integrations/hpa-integration.md)
- [Set up Prometheus monitoring](../integrations/prometheus.md)
