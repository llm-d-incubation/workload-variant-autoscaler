# Changelog for v0.5.0

This document details the key changes and improvements introduced in WVA v0.5.0, released as part of PR #549.

## Major Features

### 1. Pending Replica Awareness & Cascade Scaling Prevention

**Problem Solved:**
Previously, the saturation engine could repeatedly trigger scale-up for the same variant before previous scale-up operations completed, leading to over-provisioning.

**Solution:**
WVA now tracks pending replicas (pods that exist but are not yet ready) per variant and blocks scale-up for variants with pending replicas.

**Technical Details:**
- Added `PendingReplicas` field to `VariantReplicaState` interface
- `PendingReplicas = CurrentReplicas - ReadyReplicas`
- Scale-up variant selection skips variants with `PendingReplicas > 0`
- Per-variant tracking allows other variants to scale up if eligible

**Timeline Impact:**

*Before (without protection):*
```
T+0s:  Saturation detected → Scale variant-1: 2 → 3
T+30s: Pod not ready yet, saturation persists → Scale: 3 → 4
T+60s: Pods still starting → Scale: 4 → 5
T+90s: All ready, but over-provisioned by 2-3 replicas
```

*After (with protection):*
```
T+0s:  Saturation detected → Scale variant-1: 2 → 3 (PendingReplicas=1)
T+30s: variant-1 skipped (has pending), variant-2 scaled if cheaper
T+90s: variant-1 ready (PendingReplicas=0), eligible again
```

**Benefits:**
- ✅ Prevents excessive scale-up during model loading (2-7 minutes)
- ✅ Reduces infrastructure costs
- ✅ Maintains cost-optimized scaling across variants

**Documentation:**
- [Saturation Analyzer - Cascade Scaling Prevention](user-guide/saturation-analyzer.md#cascade-scaling-prevention)
- [Saturation Scaling Config](saturation-scaling-config.md#how-scale-up-triggers-work)

### 2. Prometheus Configuration via Environment Variables

**Enhancement:**
WVA now supports Prometheus configuration through environment variables, providing more flexible deployment options.

**Configuration Methods:**

1. **Environment Variables (New, Recommended):**
   ```yaml
   env:
   - name: PROMETHEUS_BASE_URL
     value: "https://prometheus-k8s.monitoring.svc:9091"
   - name: PROMETHEUS_TLS_INSECURE_SKIP_VERIFY
     value: "false"
   - name: PROMETHEUS_CA_CERT_PATH
     value: "/etc/prometheus-certs/ca.crt"
   ```

2. **ConfigMap (Existing, Fallback):**
   ```yaml
   data:
     PROMETHEUS_BASE_URL: "https://prometheus-k8s.monitoring.svc:9091"
     PROMETHEUS_TLS_INSECURE_SKIP_VERIFY: "false"
   ```

**Configuration Priority:**
1. Environment variables (checked first)
2. ConfigMap values (fallback)
3. Error if neither provides `PROMETHEUS_BASE_URL`

**Available Environment Variables:**
- `PROMETHEUS_BASE_URL` - Prometheus server URL (required)
- `PROMETHEUS_TLS_INSECURE_SKIP_VERIFY` - Skip TLS verification (dev/test only)
- `PROMETHEUS_CA_CERT_PATH` - CA certificate path
- `PROMETHEUS_CLIENT_CERT_PATH` - Client certificate for mTLS
- `PROMETHEUS_CLIENT_KEY_PATH` - Client private key for mTLS
- `PROMETHEUS_SERVER_NAME` - Expected server name in TLS certificate
- `PROMETHEUS_BEARER_TOKEN` - Bearer token authentication

**Benefits:**
- ✅ Easier configuration in containerized environments
- ✅ Better secret management (use Kubernetes Secrets via env)
- ✅ Simpler Helm chart customization
- ✅ Backward compatible (ConfigMap still supported)

**Documentation:**
- [Prometheus Integration](integrations/prometheus.md#configuration)
- [Configuration Guide - Environment Variables](user-guide/configuration.md#environment-variables)

### 3. PromQL Injection Prevention

**Security Enhancement:**
Added automatic parameter escaping and validation to prevent PromQL injection attacks.

**Protection Mechanisms:**

1. **Parameter Escaping:**
   - All query parameters automatically escaped before use in PromQL
   - Backslashes: `\` → `\\`
   - Double quotes: `"` → `\"`

2. **Namespace Validation:**
   - Namespace values validated before PromQL construction
   - Prevents malicious label matchers

**Example Attack Prevention:**
```go
// Malicious input attempt
namespace := `prod",malicious="attack`

// WVA automatically escapes
escapedNamespace := EscapePromQLValue(namespace)
// Result: `prod\",malicious=\"attack`

// Safe query
query := fmt.Sprintf(`vllm_kv_cache_usage{namespace="%s"}`, escapedNamespace)
// Prometheus treats as literal string, injection blocked
```

**Why This Matters:**
- Prevents unauthorized access to metrics from other namespaces
- Blocks label injection attacks
- Ensures multi-tenant isolation

**Implementation:**
- `internal/collector/v2/query_template.go`: `EscapePromQLValue()` function
- `internal/collector/v2/prometheus_source.go`: Automatic escaping in `executeQuery()`

**Documentation:**
- [Prometheus Integration - PromQL Injection Prevention](integrations/prometheus.md#promql-injection-prevention)

## Minor Improvements

### Helper Functions

**`getVariantKey()` Function:**
- Added namespace-safe variant identification
- Format: `namespace/variantName`
- Prevents collisions when multiple namespaces have deployments with same name

**Location:** `internal/engines/saturation/engine.go`

### Type Safety Improvements

**Prometheus Query Results:**
- Added safe type assertions for Prometheus query results
- Prevents runtime panics from unexpected metric types
- Better error handling and logging

## E2E Test Improvements

### Load Generation Tuning

**Enhancements:**
- Tuned load generation parameters for consistent ~2-3 replica scale-up
- Added per-model token configuration for sustained saturation testing
- Improved test stability and reliability

**Impact:**
- More predictable E2E test outcomes
- Better validation of saturation-based scaling
- Reduced flaky test failures

## Breaking Changes

### VariantAutoscaling CRD: Required `scaleTargetRef` Field

**Impact:** VariantAutoscaling resources created before v0.5.0 that do not have `scaleTargetRef` must be updated before upgrading.

**What Changed:**
- The `scaleTargetRef` field is now **required** in the VariantAutoscaling CRD (enforced via kubebuilder validation).
- The controller skips processing VAs without `scaleTargetRef.name` (see `internal/utils/variant.go`).

**Impact on Scale-to-Zero:**
- **Critical**: v0.4.1 VAs without `scaleTargetRef` will **not scale to zero** properly, even if:
  - HPAScaleToZero feature gate is enabled
  - HPA has `minReplicas: 0` configured
  - Scale-to-zero is enabled in WVA configuration
- This occurs because the HPA cannot properly reference the target deployment without `scaleTargetRef`.

**Migration Required:**
1. **Before upgrading to v0.5.0**, update all existing VariantAutoscaling resources:
   ```yaml
   apiVersion: llmd.ai/v1alpha1
   kind: VariantAutoscaling
   metadata:
     name: <your-va-name>
     namespace: <your-namespace>
   spec:
     scaleTargetRef:
       kind: Deployment
       name: <your-deployment-name>  # Required: must match your deployment
     modelID: <your-model-id>
   ```

2. **After CRD update**, VAs without `scaleTargetRef` will fail validation and cannot be created or updated.

3. **Verify your VAs** have `scaleTargetRef` before upgrading:
   ```bash
   kubectl get va -A -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\t"}{.spec.scaleTargetRef.name}{"\n"}{end}' | grep -v "^\s*$"
   ```

**Why This Change:**
- Provides explicit, unambiguous target deployment reference (follows HPA pattern)
- Enables proper scale-to-zero functionality with HPA
- Prevents controller from skipping VAs due to missing target reference

## Upgrade Notes

1. **Pending Replicas Tracking:**
   - Automatically enabled, no configuration required
   - May observe different scaling behavior during pod startup periods
   - This is expected and prevents over-provisioning

2. **Prometheus Configuration:**
   - Existing ConfigMap configuration continues to work
   - Consider migrating to environment variables for better secret management
   - See [Prometheus Integration docs](integrations/prometheus.md)

3. **Security:**
   - PromQL injection prevention is automatic
   - No action required, but validates multi-tenant deployment security

## Testing

All changes include comprehensive unit tests:
- `internal/saturation/analyzer_test.go` - Pending replica handling
- `internal/config/prometheus_test.go` - Environment variable config
- `internal/collector/v2/query_template.go` - Query escaping

E2E tests updated:
- `test/e2e-saturation-based/` - Enhanced load generation
- `test/e2e-openshift/` - Cross-platform validation

## Contributors

- Andrew Anderson (@clubanderson)

## References

- PR #549: https://github.com/llm-d/llm-d-workload-variant-autoscaler/pull/549
- Commit: 14e2bd88 - fix: pending-aware scaling and E2E test stability improvements

---

For detailed implementation, see:
- [Saturation Analyzer Documentation](user-guide/saturation-analyzer.md)
- [Prometheus Integration](integrations/prometheus.md)
- [Configuration Guide](user-guide/configuration.md)
