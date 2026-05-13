# Prometheus Integration

WVA integrates with Prometheus to collect metrics from vLLM inference servers and expose custom autoscaling metrics. In addition, WVA also emits internal metrics for observability (See [Internal Metrics](#internal-metrics)). This guide covers Prometheus configuration, metric collection, and security best practices.

## Configuration

WVA supports two methods for configuring Prometheus connectivity:

### 1. Environment Variables (Recommended)

Set Prometheus configuration via environment variables in the WVA deployment:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: workload-variant-autoscaler-controller-manager
spec:
  template:
    spec:
      containers:
      - name: manager
        env:
        # Required: Prometheus server URL
        - name: PROMETHEUS_BASE_URL
          value: "https://prometheus-k8s.monitoring.svc.cluster.local:9091"

        # Optional: TLS configuration
        - name: PROMETHEUS_TLS_INSECURE_SKIP_VERIFY
          value: "false"  # Set to "true" only for testing/development

        - name: PROMETHEUS_CA_CERT_PATH
          value: "/etc/prometheus-certs/ca.crt"

        - name: PROMETHEUS_CLIENT_CERT_PATH
          value: "/etc/prometheus-certs/client.crt"

        - name: PROMETHEUS_CLIENT_KEY_PATH
          value: "/etc/prometheus-certs/client.key"

        - name: PROMETHEUS_SERVER_NAME
          value: "prometheus-k8s.monitoring.svc.cluster.local"

        # Optional: Bearer token authentication
        - name: PROMETHEUS_BEARER_TOKEN
          valueFrom:
            secretKeyRef:
              name: prometheus-token
              key: token
```

**Environment Variable Reference:**

| Variable | Required | Description | Default |
|----------|----------|-------------|---------|
| `PROMETHEUS_BASE_URL` | Yes | Prometheus server URL (HTTPS only in production) | - |
| `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY` | No | Skip TLS certificate verification (dev/test only) | `false` |
| `PROMETHEUS_CA_CERT_PATH` | No | Path to CA certificate for TLS verification | - |
| `PROMETHEUS_CLIENT_CERT_PATH` | No | Path to client certificate for mutual TLS | - |
| `PROMETHEUS_CLIENT_KEY_PATH` | No | Path to client private key for mutual TLS | - |
| `PROMETHEUS_SERVER_NAME` | No | Expected server name in TLS certificate | - |
| `PROMETHEUS_BEARER_TOKEN` | No | Bearer token for Prometheus authentication | - |

### 2. ConfigMap Configuration

Alternatively, configure Prometheus via the controller's ConfigMap:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-variantautoscaling-config
  namespace: workload-variant-autoscaler-system
data:
  PROMETHEUS_BASE_URL: "https://prometheus-k8s.monitoring.svc.cluster.local:9091"
  PROMETHEUS_TLS_INSECURE_SKIP_VERIFY: "false"
  PROMETHEUS_CA_CERT_PATH: "/etc/prometheus-certs/ca.crt"
  PROMETHEUS_CLIENT_CERT_PATH: "/etc/prometheus-certs/client.crt"
  PROMETHEUS_CLIENT_KEY_PATH: "/etc/prometheus-certs/client.key"
  PROMETHEUS_SERVER_NAME: "prometheus-k8s.monitoring.svc.cluster.local"
  PROMETHEUS_BEARER_TOKEN: "your-bearer-token"  # Not recommended - use Secret instead
```

**Configuration Priority:**
1. Environment variables (checked first)
2. ConfigMap values (fallback)
3. Error if neither provides `PROMETHEUS_BASE_URL`

## Security Considerations

### TLS Configuration

**Production Deployments:**
- Always use HTTPS endpoints (`https://`)
- Provide CA certificate via `PROMETHEUS_CA_CERT_PATH`
- Never set `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true` in production

**Development/Testing:**
- You may set `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true` for local clusters
- Example (port-forwarding to Prometheus):
  ```bash
  # Terminal 1: Port forward Prometheus
  kubectl port-forward -n monitoring svc/prometheus-k8s 9091:9091

  # Terminal 2: Set environment for local development
  export PROMETHEUS_BASE_URL=https://127.0.0.1:9091
  export PROMETHEUS_TLS_INSECURE_SKIP_VERIFY=true
  ```

### PromQL Injection Prevention

WVA implements security measures to prevent PromQL injection attacks:

1. **Parameter Escaping**: All query parameters (namespace, model ID, variant name) are automatically escaped:
   - Backslashes are escaped: `\` → `\\`
   - Double quotes are escaped: `"` → `\"`

2. **Namespace Validation**: Namespace values are validated before use in PromQL queries to prevent malicious label matchers

**Example - Safe Query Construction:**
```go
// User input (potentially malicious)
namespace := `prod",malicious="value`

// WVA automatically escapes the value
escapedNamespace := EscapePromQLValue(namespace)
// Result: `prod\",malicious=\"value`

// Safe PromQL query
query := fmt.Sprintf(`vllm_kv_cache_usage{namespace="%s"}`, escapedNamespace)
// Result: vllm_kv_cache_usage{namespace="prod\",malicious=\"value"}
// Prometheus treats this as a literal string, preventing injection
```

**Why This Matters:**
- Prevents unauthorized access to metrics from other namespaces
- Blocks label injection attacks that could manipulate query results
- Ensures multi-tenant deployments remain isolated

## Custom Metrics Documentation

## WVA Custom Metrics

WVA exposes custom metrics that provide insights into autoscaling behavior and optimization performance. These metrics are exposed via Prometheus at the `/metrics` endpoint.

### Metrics Overview

All custom metrics are prefixed with `inferno_` and include labels for `variant_name`, `namespace`, and other relevant dimensions.

### Optimization Metrics

*No optimization metrics are currently exposed. Optimization timing is logged at DEBUG level.*

### Operational Metrics
### `wva_available_gpus`
- **Type**: Gauge
- **Description**: Number of currently available GPUs group by accelerator type (e.g. "H100", "A100"). Only available in clusters such as OpenShift where WVA can iterate over node objects. In addition, WVA only iterates over node objects when configuration such as `enableLimiter` is `true`.
- **Labels**:
  - `accelerator_vendor`: Name of the GPU vendor
  - `accelerator_model`: Full name of the accelerator
  - `accelerator_type`: Type of accelerator (short name of the accelerator)
- **Use Case**: Show number of GPUs discovered by WVA

### Replica Management Metrics

### `wva_current_replicas`
- **Type**: Gauge
- **Description**: Current number of replicas for each variant
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `accelerator_type`: Type of accelerator being used
- **Use Case**: Monitor current number of replicas per variant

### `wva_desired_replicas`
- **Type**: Gauge
- **Description**: Desired number of replicas for each variant
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `accelerator_type`: Type of accelerator being used
- **Use Case**: Expose the desired optimized number of replicas per variant

### `wva_desired_ratio`
- **Type**: Gauge
- **Description**: Ratio of the desired number of replicas and the current number of replicas for each variant
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `accelerator_type`: Type of accelerator being used
- **Use Case**: Compare the desired and current number of replicas per variant, for scaling purposes

### `wva_replica_scaling_total`
- **Type**: Counter
- **Description**: Total number of replica scaling operations
- **Labels**:
  - `variant_name`: Name of the variant
  - `namespace`: Kubernetes namespace
  - `direction`: Direction of scaling (up, down)
  - `reason`: Reason for scaling
- **Use Case**: Track scaling frequency and reasons

## Configuration

### Metrics Endpoint
The metrics are exposed at the `/metrics` endpoint on port 8080 (HTTP).

### ServiceMonitor Configuration

WVA metrics are exposed on port 8080 (HTTP):
```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: workload-variant-autoscaler
  namespace: workload-variant-autoscaler-system
  labels:
    release: kube-prometheus-stack
spec:
  selector:
    matchLabels:
      control-plane: controller-manager
  endpoints:
  - port: http
    scheme: http
    interval: 30s
    path: /metrics
```

## Example Queries

### Basic Queries
```promql
# Current replicas by variant
wva_current_replicas

# Scaling frequency
rate(wva_replica_scaling_total[5m])

# Desired replicas by variant
wva_desired_replicas
```

### Advanced Queries
```promql
# Scaling frequency by direction
rate(wva_replica_scaling_total{direction="scale_up"}[5m])

# Replica count mismatch
abs(wva_desired_replicas - wva_current_replicas)

# Scaling frequency by reason
rate(wva_replica_scaling_total[5m]) by (reason)
```

## Internal Metrics
### `wva_errors_total`
- **Type**: Counter
- **Description**: Total number of errors by WVA components
- **Labels**:
  - `component`: Name of the component. The components are `collector`, `analyzer`, `optimizer`, `limiter`, `enforcer`, and `controller`. Currently, this metric is available for `collector`, `enforcer`, `controller`. It will be available for other components as these components handle errors
  - `error_type`: Short description of the error
- **Use Case**: Track errors in WVA by components
