# Saturation Scaling Configuration

## Overview

The Workload Variant Autoscaler supports saturation-based scaling using KV cache utilization and queue length metrics. This feature is enabled by default and configured via a ConfigMap.

**Key features:**
- ✅ ConfigMap-based configuration with global defaults and per-model overrides
- ✅ **Efficient caching** with single read on startup (zero API calls during reconciliation)
- ✅ **Automatic reload** via ConfigMap watch (immediate response to changes)
- ✅ **Thread-safe** concurrent access with RWMutex
- ✅ Graceful degradation if ConfigMap missing (V2 has hardcoded defaults; V1 requires ConfigMap — see [Default Configuration](#default-configuration))

## Configuration

### ConfigMap Structure

The saturation scaling configuration is stored in a ConfigMap named `wva-saturation-scaling-config` in the Workload Variant Autoscaler controller's namespace.

**Location:** `deploy/configmap-saturation-scaling.yaml`

### Parameters

| Parameter | Type | Description | Recommended |
|-----------|------|-------------|-------------|
| `kvCacheThreshold` | float64 | Replica is considered saturated if KV cache utilization ≥ threshold (0.0-1.0) | 0.80 |
| `queueLengthThreshold` | float64 | Replica is considered saturated if queue length ≥ threshold | 5 |
| `kvSpareTrigger` | float64 | Scale-up signal if average spare KV capacity < trigger (0.0-1.0) | 0.10 |
| `queueSpareTrigger` | float64 | Scale-up signal if average spare queue capacity < trigger | 3 |
| `scaleUpThreshold` | float64 | Model-level utilization threshold above which scale-up is triggered (0.0-1.0). Applied by the engine post-step to every analyzer's result. | 0.85 |
| `scaleDownBoundary` | float64 | Model-level utilization boundary below which scale-down is safe (0.0-1.0). Applied by the engine post-step to every analyzer's result. | 0.70 |

### V2 Analyzer Parameters

These parameters apply when `analyzerName: "saturation"` is set or when the `analyzers:` list is populated.

| Parameter | Type | Description | Default |
|-----------|------|-------------|---------|
| `analyzerName` | string | Selects the V2 token-based analyzer: set to `"saturation"`. Empty string uses V1. | `""` |
| `priority` | float64 | Multiplier for this model's scaling urgency in fair-share GPU allocation | 1.0 |
| `analyzers` | list | Multi-analyzer pipeline registration — see [Multi-Analyzer Registration](#multi-analyzer-registration) | `[{name: "saturation", score: 1.0}]` |

> `scaleUpThreshold` and `scaleDownBoundary` are honored only for saturation on this branch; see the `multi-analyzer-threshold` PR for the universal post-step that calibrates RC/SC across all analyzers.

### Default Configuration

The recommended values for the V1 (percentage-based) saturation analyzer are:

```yaml
kvCacheThreshold: 0.80
queueLengthThreshold: 5
kvSpareTrigger: 0.1
queueSpareTrigger: 3
```

> **Important:** These V1 threshold values are **not hardcoded** in the analyzer code.
> If the ConfigMap is missing or has no `default` entry, all V1 thresholds default to zero,
> which will cause every replica to appear saturated and trigger continuous scale-up.
> Always deploy the ConfigMap with a `default` entry containing valid thresholds.

### How Scale-Up Triggers Work

The saturation analyzer uses a **spare capacity model** to determine when to scale up. Instead of waiting for replicas to become fully saturated, WVA proactively scales when the average spare capacity across non-saturated replicas falls below configured thresholds.

**Scale-up logic:**

1. **Calculate spare capacity** for each non-saturated replica:
   - Spare KV capacity = `kvCacheThreshold - current_kv_usage`
   - Spare queue capacity = `queueLengthThreshold - current_queue_length`

2. **Average across non-saturated replicas**:
   - WVA computes the average spare capacity across all healthy (non-saturated) replicas

3. **Trigger scale-up when spare capacity is low**:
   - If `avg_spare_kv < kvSpareTrigger` **OR** `avg_spare_queue < queueSpareTrigger`
   - Scale-up is triggered to add capacity before existing replicas saturate

4. **Cascade scaling prevention**:
   - Variants with pending replicas (pods that exist but aren't ready yet) are skipped during scale-up
   - This prevents repeatedly scaling the same variant while previous scale-up operations complete
   - Pod startup can take 2-7 minutes (model loading, health checks)

**Example scenario:**
- `kvCacheThreshold = 0.80`, `kvSpareTrigger = 0.10`
- Replica A: 65% KV cache usage → Spare capacity: 0.15
- Replica B: 72% KV cache usage → Spare capacity: 0.08
- Average spare KV: (0.15 + 0.08) / 2 = **0.115**
- Since 0.115 ≥ 0.10, no scale-up yet (trigger uses strict `<`)
- If Replica B increases to 76%: Average spare = (0.15 + 0.04) / 2 = **0.095** → 0.095 < 0.10 → **Scale-up triggered**

This proactive approach ensures adequate headroom and prevents request drops by scaling before saturation occurs.

**For detailed implementation, see:** [Saturation Analyzer Documentation](user-guide/saturation-analyzer.md)

## Multi-Analyzer Registration

The V2 engine carries an analyzer registry so external analyzers (e.g.
throughput, SLO) can be plugged in without the engine package knowing the
concrete type. Each cycle the engine iterates every registered analyzer in
registration order and invokes its `Analyze` method.

> **Scope note.** Saturation drives every scaling decision on its own.
> Non-saturation analyzers are registered and invoked, but their results
> are **not yet consumed** — combine semantics and per-analyzer score
> consumption land in a follow-up PR (`multi-analyzer-optimizer`). The
> hooks below are wired so that follow-up can hang its behavior off them
> without further engine changes.

### Registering Analyzers

Pre-registered:

- The V2 saturation analyzer is pre-registered by `NewEngine` under
  `interfaces.SaturationAnalyzerName`. It always runs and drives the
  optimizer.

External analyzers are registered from `cmd/main.go` via:

```go
if err := engine.RegisterAnalyzer(name, analyzer); err != nil {
    // handle: duplicate name or called after StartOptimizeLoop
}
```

`RegisterAnalyzer` appends to the registry and returns an `error` for
two misuse conditions: calling it after `StartOptimizeLoop` or
re-registering an existing name. Callers must check the error. Registration
order defines processing order in the engine loop and should match the
operator's `analyzers:` config order.

`RegisterAnalyzer` must be called before `StartOptimizeLoop`.

### Configuring Analyzers

The `analyzers:` list shapes the configuration for the registered analyzers:

```yaml
analyzerName: saturation
scaleUpThreshold: 0.85
scaleDownBoundary: 0.70
analyzers:
  - name: saturation
    score: 1.0
  - name: throughput
    score: 1.0
```

When `analyzers:` is omitted, it defaults to `[{name: "saturation", score: 1.0}]`.

### AnalyzerScoreConfig Fields

| Field | Type | Description | Default |
|-------|------|-------------|---------|
| `name` | string | Analyzer name (must match a `RegisterAnalyzer` call) | required |
| `enabled` | bool | Reserved — placeholder for future combine logic | `true` |
| `score` | float64 | Reserved — placeholder for future combine logic | `1.0` |
| `scaleUpThreshold` | float64 | Per-analyzer override for the scale-up threshold; honored by the engine post-step (see Universal Threshold Post-Step below) | global `scaleUpThreshold` |
| `scaleDownBoundary` | float64 | Per-analyzer override for the scale-down boundary; honored by the engine post-step (see Universal Threshold Post-Step below) | global `scaleDownBoundary` |

### Analyzer responsibilities and the universal threshold post-step

#### Per-variant data is canonical

`interfaces.VariantCapacity` is the single source of truth for per-variant primitives. Analyzers populate it; the engine and optimizer read it.

| Field | Written by | Read by |
|---|---|---|
| `ReplicaCount`, `PendingReplicas`, `PerReplicaCapacity`, `Cost`, `Role`, `AcceleratorName` | Analyzer | Optimizer (per-variant scaling math + picker) |
| `TotalCapacity`, `TotalDemand`, `Utilization` | Analyzer | Sat_v2 internal aggregation; `Utilization` passed through to `VariantDecision.Utilization` for metric emission |
| `r.TotalSupply`, `r.TotalAnticipatedSupply`, `r.TotalDemand` | Analyzer (via shared helpers) | Engine post-step |
| `r.RoleCapacities[role].TotalSupply/TotalAnticipatedSupply/TotalDemand` | Analyzer (via shared helpers) | Engine post-step |
| `r.RequiredCapacity`, `r.SpareCapacity` | **Engine post-step only** | Optimizer |
| `r.RoleCapacities[role].RequiredCapacity/SpareCapacity` | **Engine post-step only** | Optimizer (P/D disaggregation) |

#### Analyzer inputs

`interfaces.AnalyzerInput` carries the shared inputs every analyzer reads:
replica metrics, variant states, the model's resolved config, and
`SchedulerQueue`. None of these are analyzer-specific.

`SchedulerQueue` represents requests queued upstream of any pod (in the
llm-d flow control layer). Queue items are model-scoped and **not yet
attributed to any variant or role**. Any analyzer with a demand model may
use it — sat_v2 does today; the throughput analyzer will when it lands.

**Demand extraction from the queue is per-analyzer.** Each analyzer
converts queue depth/bytes into demand in its own unit (sat_v2:
kv-tokens; throughput: tokens/sec). Each analyzer also decides how to
attribute that demand across roles or variants — sat_v2 splits it among
active roles.

#### Linearity invariant

The optimizer's per-variant scaling math (`bottleneckReplicas`, `safeRemovalReplicas`, `applyAllocation`) assumes that `n` replicas of variant `v` reduce model-level RC by exactly `n × PRC[v]`. That means `Total*` must equal the canonical sum over variants:

```
r.TotalSupply            == Σ_v vc.ReplicaCount × vc.PerReplicaCapacity
r.TotalAnticipatedSupply == Σ_v (vc.ReplicaCount + vc.PendingReplicas) × vc.PerReplicaCapacity
r.TotalDemand            == Σ_v vc.TotalDemand
r.RoleCapacities[role].* == same sums filtered by vc.Role == role
```

Use the shared helpers in `internal/engines/aggregation/` to compute these. An analyzer that doesn't use the helpers takes responsibility for producing identical math — otherwise the optimizer's per-variant allocation silently breaks.

#### Shared aggregation helpers

```go
import "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"

r.TotalSupply            = aggregation.SumTotalSupply(variantCapacities)
r.TotalAnticipatedSupply = aggregation.SumTotalAnticipatedSupply(variantCapacities)
r.TotalDemand            = aggregation.SumTotalDemand(variantCapacities)

// For P/D disaggregated models:
totals := aggregation.AggregateByRole(variantCapacities)
// → map[role]aggregation.ScopeTotals{TotalSupply, TotalAnticipatedSupply, TotalDemand}
```

`AggregateByRole` canonicalizes empty role to `interfaces.RoleBoth`.

#### Engine post-step formula

After each analyzer's `Analyze()` returns, the engine applies the universal threshold formula at **every scope** — model level and each `RoleCapacity` entry:

```
RC = max(0, TotalDemand / scaleUpThreshold − TotalAnticipatedSupply)
SC = max(0, TotalSupply  − TotalDemand / scaleDownBoundary)
```

`TotalAnticipatedSupply` is read as-is — **zero is a literal value, not a sentinel**. For a model scaled to zero with positive demand, RC = TotalDemand/scaleUp (the correct "this much capacity needed" answer). Analyzers must populate `TotalAnticipatedSupply` via `SumTotalAnticipatedSupply`; the engine does not walk `VariantCapacities` as a fallback.

The asymmetry — anticipated supply for scale-up, steady-state `TotalSupply` for scale-down — preserves the conservative "don't double-scale while replicas are launching, don't count pending as removable" stance.

**P/D disaggregation.** The same formula and the same `(scaleUp, scaleDown)` values are applied to every `RoleCapacity` entry. Threshold configuration is common regardless of role — there are no per-role overrides. The optimizer receives fully calibrated per-role RC/SC and uses them for P/D replica allocation.

**Per-analyzer threshold overrides.** `analyzers[].scaleUpThreshold` / `analyzers[].scaleDownBoundary` are resolved per analyzer: a per-entry override takes precedence over the model-level global. The resolved pair is applied uniformly at model level and every role for that analyzer. An opt-out flag for analyzers with non-universal calibration math is deferred to a follow-up.

### Saturation Always Runs

The saturation analyzer executes on every cycle regardless of any future
`enabled` flag — its `VariantCapacities` carry `Cost` and `AcceleratorName`
required by the optimizer for variant selection and GPU accounting. On
this branch saturation's `RequiredCapacity` and `SpareCapacity` are the
only signals the optimizer consumes.

### Resilience

Each registered non-saturation analyzer runs in isolation: errors are
logged and discarded, and panics are recovered. A faulty plugin analyzer
cannot take down the optimize goroutine or block saturation from driving
the scaling decision.

## Best Practices: Coordinating with InferenceScheduler (End Point Picker)

### What is End Point Picker (EPP)?

The **End Point Picker (EPP)** is an intelligent request routing component in the InferenceScheduler that selects the optimal inference server replica to handle each incoming request. EPP monitors replica capacity metrics (KV cache utilization, queue depth), as well as other replica metrics and uses scoring algorithms to route requests to replicas.

### Deployment Architecture

**EPP Deployment Model**: Each model has a **1-to-1 relationship** with its EPP instance. Every model served by the inference infrastructure has a dedicated EPP component that routes requests specifically to that model's replicas.

**Example deployment pattern:**
- Model: `Qwen/Qwen3-0.6B` in namespace `llm-d-autoscaler` → Dedicated EPP instance `gaie-workload-autoscaler-epp`
- Model: `ibm/granite-13b` in namespace `production` → Dedicated EPP instance `gaie-production-epp`
- Each model deployment has its own EPP instance (naming follows namespace/workload convention)

This 1-to-1 architecture means that saturation detection and request routing decisions are **model-specific**, with each EPP instance monitoring only its associated model's replicas.

### Threshold Alignment Recommendation

**For optimal cluster performance, we strongly recommend using the same threshold values for both WVA (Workload Variant Autoscaler) and InferenceScheduler (End Point Picker) for each model deployment.**

Using aligned thresholds ensures consistent capacity management across the cluster and prevents request drop situations.

**Why threshold alignment matters:**

1. **Reduced Request Drop Rates**: When WVA and EPP use the same saturation thresholds, the scheduler will avoid routing requests to replicas that WVA already considers saturated. This prevents the scheduler from overloading replicas that are about to trigger scale-up.

2. **Consistent Capacity Assessment**: Both components evaluate replica capacity using the same criteria (KV cache utilization and queue length), ensuring coordinated behavior across the entire inference stack.

3. **Improved GPU Utilization**: Aligned thresholds allow the cluster to maintain optimal GPU utilization without oversaturation. The scheduler respects the same capacity boundaries that drive autoscaling decisions.

4. **Faster Response to Load Changes**: When both components agree on saturation thresholds, the system responds more quickly to load changes with coordinated routing and scaling actions.

### Configuration Comparison

#### WVA Saturation Scaling Configuration

```yaml
# WVA Configuration (wva-saturation-scaling-config ConfigMap)
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-saturation-scaling-config
  namespace: <workload-variant-autoscaler-namespace>
data:
  default: |
    kvCacheThreshold: 0.80        # Should match EPP kvCacheUtilThreshold
    queueLengthThreshold: 5       # Should match EPP queueDepthThreshold
    kvSpareTrigger: 0.10          # WVA-specific (scale-up trigger)
    queueSpareTrigger: 3          # WVA-specific (scale-up trigger)
```

#### EPP Saturation Detector Configuration

The InferenceScheduler EPP component uses the [gateway-api-inference-extension](https://github.com/kubernetes-sigs/gateway-api-inference-extension/blob/main/site-src/guides/epp-configuration/config-text.md) saturation detector to identify cluster overload.

**Per-Model Configuration**: Since each model has its own dedicated EPP instance, saturation detection is configured **per model deployment**. This allows different models to have different saturation thresholds based on their specific characteristics and SLO requirements.

```yaml
# EPP Saturation Detector Configuration (per-model EPP instance)
saturationDetector:
  ...
  queueDepthThreshold: 5          # Default: 5 - Backend waiting queue size threshold
  kvCacheUtilThreshold: 0.8       # Default: 0.8 - KV cache utilization threshold (0.0-1.0)
  ...
```

**Configuration Notes**:
- All parameters are optional; omitting them applies the documented defaults
- EPP configuration is **read only on startup** - changes require EPP pod restart
- Unlike WVA, EPP does not currently support live ConfigMap updates
- **Each EPP instance** (one per model) can have different threshold values

### Parameter Mapping and Alignment

| Concept | WVA Field | EPP Field | Aligned Default | Description |
|---------|-----------|-----------|-----------------|-------------|
| **KV Cache Saturation** | `kvCacheThreshold` | `kvCacheUtilThreshold` | **0.80** (80%) | Replica is saturated when KV cache ≥ threshold |
| **Queue Saturation** | `queueLengthThreshold` | `queueDepthThreshold` | **5** | Replica is saturated when queue length ≥ threshold |
| **Scale-Up Trigger (KV)** | `kvSpareTrigger` | *(not applicable)* | **0.10** (10%) | WVA-only: Trigger scale-up when spare KV < threshold |
| **Scale-Up Trigger (Queue)** | `queueSpareTrigger` | *(not applicable)* | **3** | WVA-only: Trigger scale-up when spare queue < threshold |

### Configuration Workflow

#### Step 1: Define Thresholds

Choose thresholds based on your workload characteristics and SLO requirements:

| Workload Type | kvCacheThreshold | queueLengthThreshold | Rationale |
|---------------|------------------|----------------------|-----------|
| **Conservative** (Default) | 0.80 | 5 | Balanced performance and utilization |
| **Aggressive** (High GPU utilization) | 0.90 | 15 | Maximize GPU usage, higher latency variance |
| **Strict** (Low latency SLO) | 0.70 | 3 | Prioritize responsiveness, lower utilization |

#### Step 2: Apply to WVA

Update `wva-saturation-scaling-config` ConfigMap:

```bash
kubectl edit cm wva-saturation-scaling-config -n <workload-variant-autoscaler-namespace>
```

Changes take effect **immediately** (WVA watches ConfigMap and auto-reloads).

#### Step 3: Apply to EPP

**Important**: Since each model has its own dedicated EPP instance (1-to-1 relationship), you must configure the EPP instance for **each specific model deployment** separately.

**Current approach:**

1. Identify the EPP instance for your target model:
   ```bash
   # Example: Find EPP deployment for a specific model in namespace
   kubectl get deployments -n llm-d-autoscaler | grep epp
   ```

2. Update the EPP instance's environment variables or configuration file for that specific model

3. Restart the EPP pod for that model:
   ```bash
   # Restart the specific model's EPP instance
   kubectl rollout restart deployment/gaie-<model-name>-epp -n <namespace>
   ```

**Example for multiple models:**
```bash
# Model 1: granite-13b in production
kubectl rollout restart deployment/gaie-granite-13b-epp -n production

# Model 2: llama-70b in lab
kubectl rollout restart deployment/gaie-llama-70b-epp -n lab
```

#### Step 4: Verify Configuration

**WVA verification:**
```bash
kubectl get cm wva-saturation-scaling-config -n <workload-variant-autoscaler-namespace> -o yaml
```

**EPP verification (per-model instance):**
```bash
# Check specific model's EPP pod logs for loaded configuration
kubectl logs -n <namespace> deployment/gaie-<model-name>-epp | grep -i "saturation\|threshold"

# Example: Verify EPP configuration for granite-13b model in production
kubectl logs -n production deployment/gaie-granite-13b-epp | grep -i "saturation\|threshold"
```

### Alignment Best Practices

1. **Core Thresholds Must Match Per Model**:
   - `kvCacheThreshold` (WVA) = `kvCacheUtilThreshold` (EPP)
   - `queueLengthThreshold` (WVA) = `queueDepthThreshold` (EPP)
   - **Important**: Since each model has its own EPP instance, ensure thresholds align for **each model deployment** individually

2. **Per-Model Configuration Strategy**:
   - Use WVA's per-model override feature to set model-specific thresholds
   - Configure the corresponding EPP instance with matching thresholds
   - Document the threshold mapping for each model deployment
   - Example: If `ibm/granite-13b` uses `kvCacheThreshold: 0.85` in WVA, its dedicated EPP must use `kvCacheUtilThreshold: 0.85`

3. **WVA-Specific Parameters** (`kvSpareTrigger`, `queueSpareTrigger`):
   - These control WVA's scale-up aggressiveness
   - Should be set **lower** than saturation thresholds
   - Provide headroom before replicas become saturated
   - Recommended: `kvSpareTrigger = kvCacheThreshold - 0.1 to 0.2`

4. **Testing Threshold Changes**:
   - Test in development environment first
   - Monitor impact on request drop rate and latency for the specific model
   - Adjust based on observed behavior
   - Remember to update both WVA and the model's EPP instance

## Usage

### 1. Using Default Configuration

> **Warning:** For the V1 (percentage-based) analyzer, deploying without a ConfigMap will
> result in zero-valued thresholds, causing all replicas to be marked as saturated.
> Always deploy the ConfigMap with a `default` entry for V1.

If the ConfigMap is missing, the system will log a warning:
```
WARN Saturation scaling ConfigMap not found
```

### 2. Customizing Global Defaults

Edit `deploy/configmap-saturation-scaling.yaml`:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-saturation-scaling-config
  namespace: <workload-variant-autoscaler-namespace>
data:
  default: |
    kvCacheThreshold: 0.75
    queueLengthThreshold: 10
    kvSpareTrigger: 0.15
    queueSpareTrigger: 5
```

Apply the ConfigMap:
```bash
kubectl apply -f deploy/configmap-saturation-scaling.yaml
```

**Note:** Changes take effect immediately! The controller watches the ConfigMap and automatically:
1. Reloads the cache when changes are detected
2. Triggers reconciliation of all VariantAutoscaling resources
3. Applies the new configuration without requiring pod restart

### 3. Per-Model Overrides

Add model-specific configuration entries to override defaults for specific model/namespace pairs.

The saturation engine resolves per-model config using a lookup key in the format
`{modelID}#{namespace}` (see `internal/engines/saturation/engine.go` — `resolveSaturationConfig()`).
The ConfigMap data key **must** match this format for overrides to take effect.
Lookup order: `modelID#namespace` → `default` → zero-value with defaults applied.

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-saturation-scaling-config
  namespace: <workload-variant-autoscaler-namespace>
data:
  default: |
    kvCacheThreshold: 0.80
    queueLengthThreshold: 5
    kvSpareTrigger: 0.1
    queueSpareTrigger: 3

  # Override for granite model in production namespace
  # Only fields specified here are overridden; the rest inherit from `default`
  "ibm/granite-13b#production": |
    kvCacheThreshold: 0.85
    kvSpareTrigger: 0.15

  # Override for llama model in lab namespace
  "meta/llama-70b#lab": |
    kvCacheThreshold: 0.80
    queueLengthThreshold: 20
    kvSpareTrigger: 0.1
    queueSpareTrigger: 10
```

**Key points:**
- Override keys **must** use the format `{modelID}#{namespace}` to match the engine's lookup
- The `model_id` and `namespace` YAML fields inside the entry are parsed but **not used for lookup**
- Overrides use **field-level merge**: only non-zero fields in the override replace the corresponding values from `default`; any field you omit (or set to its zero value) inherits from `default`. See `Merge()` in `internal/config/saturation_scaling.go`.
- Multiple overrides can exist for different model/namespace combinations

### 4. Per-Model Overrides — Field-Level Inheritance

Overrides are merged onto `default` field by field: only non-zero fields in the override
replace values from `default`, and any field you omit inherits from `default`. To change
just one threshold, specify only that field:

```yaml
  # Inherits queueLengthThreshold, kvSpareTrigger, queueSpareTrigger from `default`
  "my-org/my-model#my-namespace": |
    kvCacheThreshold: 0.90
```

> **Gotcha:** Because `Merge()` only overlays non-zero values, an override cannot force
> a field to its zero value — the override is treated as "unset" and inherits from
> `default` instead. This affects every field type, not just numeric thresholds:
>
> - `kvSpareTrigger: 0` → inherits from `default` (cannot zero out a numeric threshold)
> - `enableLimiter: false` → inherits from `default` (cannot disable a limiter that's enabled by default)
> - `analyzers: []` → inherits from `default` (cannot clear a non-empty analyzer list)
>
> See `Merge()` in `internal/config/saturation_scaling.go`.

## Validation

The controller validates all configuration entries on load. Invalid entries are logged and skipped:

### Validation Rules

1. **KvCacheThreshold:** Must be between 0.0 and 1.0
2. **QueueLengthThreshold:** Must be ≥ 0
3. **KvSpareTrigger:** Must be between 0.0 and 1.0
4. **QueueSpareTrigger:** Must be ≥ 0
5. **Consistency:** `kvCacheThreshold` must be ≥ `kvSpareTrigger`

### Example Validation Errors

**Invalid entry (logged and skipped):**
```yaml
  invalid-config: |
    model_id: test/model
    namespace: test
    kvCacheThreshold: 1.5  # ERROR: Must be ≤ 1.0
```

**Log output:**
```
WARN Invalid saturation scaling config entry, skipping key=invalid-config error=kvCacheThreshold must be between 0 and 1, got 1.50
```

## Integration with Controller

### Caching Architecture

The controller uses an **efficient caching mechanism** with ConfigMap watch for optimal performance:

**Initialization (on controller startup):**

The ConfigMap reconciler watches the `wva-saturation-scaling-config` ConfigMap and loads
configuration into the shared `Config` object. On startup, the controller bootstraps the
config cache (see `internal/controller/configmap_bootstrap.go`).

**Reconciliation (zero API calls):**

During the optimization loop, the engine reads config from the in-memory cache:
```go
// In optimize() - reads cached config (no API call)
saturationConfigMap := e.Config.SaturationConfigForNamespace(namespace)

// Resolve per-model config using "{modelID}#{namespace}" lookup
saturationConfig := resolveSaturationConfig(saturationConfigMap, modelID, namespace)

// Use saturationConfig for saturation-based scaling decisions
// (thresholds drive the analyzer's saturation detection)
```

### Automatic Cache Updates

The `ConfigMapReconciler` watches the `wva-saturation-scaling-config` ConfigMap for changes
(see `internal/controller/configmap_reconciler.go`):

1. **ConfigMap change detected** → Watch event triggered
2. **Cache automatically reloaded** → New configuration parsed and stored in `Config`
3. **Next optimization cycle** picks up the new config automatically

### Performance Characteristics

| Operation | Before (Without Cache) | After (With Cache) |
|-----------|------------------------|-------------------|
| Startup | N/A | Single ConfigMap read |
| Per Reconciliation | ConfigMap API call | Memory read only |
| Config Change | Manual pod restart needed | Automatic reload + reconcile |
| Latency Impact | Network round-trip per reconcile | Zero (memory access) |
| Concurrency | Serial API calls | Thread-safe concurrent reads |

**Cache benefits:**
- ✅ **Single read on startup** instead of per-reconciliation
- ✅ **Zero API calls during reconciliation** (cached access)
- ✅ **Event-driven updates** (immediate response to changes)
- ✅ **Thread-safe concurrent access** (RWMutex)
- ✅ **Defensive copying** prevents external modification

## Troubleshooting

### ConfigMap Not Found

**Symptom:** Warning log message
```
WARN Saturation scaling ConfigMap not found, using hardcoded defaults configmap=wva-saturation-scaling-config namespace=<workload-variant-autoscaler-namespace>
```

**Solution:** Deploy the ConfigMap:
```bash
kubectl apply -f deploy/configmap-saturation-scaling.yaml
```

### Invalid Configuration Entry

**Symptom:** Warning log message
```
WARN Invalid saturation scaling config entry, skipping key=my-config error=...
```

**Solution:** Fix the validation error in the ConfigMap entry and reapply.

### Missing Default Entry

**Symptom:** Warning log message
```
WARN No 'default' entry in saturation scaling ConfigMap, using hardcoded defaults
```

**Solution:** Add a `default` entry to the ConfigMap:
```yaml
data:
  default: |
    kvCacheThreshold: 0.80
    queueLengthThreshold: 5
    kvSpareTrigger: 0.1
    queueSpareTrigger: 3
```

### Override Not Applied

**Symptom:** Model-specific override is not being used

**Checklist:**
1. Verify the ConfigMap data key uses the format `{modelID}#{namespace}` (e.g., `"ibm/granite-13b#production"`)
2. Verify `modelID` exactly matches `va.Spec.ModelID`
3. Verify `namespace` exactly matches the VariantAutoscaling resource namespace
4. Check controller logs for validation errors
5. Ensure entry passed validation (check for WARN logs)

### Config Changes Not Taking Effect

**Symptom:** Updated ConfigMap but controller still uses old values

**Solution:** The controller watches for ConfigMap changes and automatically reloads. Check:

1. **Verify ConfigMap was updated:**
   ```bash
   kubectl get cm wva-saturation-scaling-config -n <workload-variant-autoscaler-namespace> -o yaml
   ```

2. **Check controller logs for reload confirmation:**
   ```bash
   kubectl logs -n <workload-variant-autoscaler-namespace> deployment/wva-controller | grep "Saturation scaling"
   ```

   Expected logs:
   ```
   INFO  Saturation scaling ConfigMap changed, reloading cache
   INFO  Saturation scaling config cache updated entries=2 has_default=true
   INFO  Triggering reconciliation for all VariantAutoscaling resources
   ```

3. **If no logs appear, verify watch is working:**
   - Check controller pod is running: `kubectl get pods -n <workload-variant-autoscaler-namespace>`
   - Check for errors: `kubectl logs -n <workload-variant-autoscaler-namespace> deployment/wva-controller --tail=100`

4. **Manual restart (last resort):**
   ```bash
   kubectl rollout restart deployment/wva-controller -n <workload-variant-autoscaler-namespace>
   ```

### Cache Initialization Failed

**Symptom:** Warning on controller startup
```
WARN Failed to load initial saturation scaling config, will use defaults
```

**Solution:** This is non-fatal. The controller continues with zero-valued V1 thresholds (V2 has hardcoded defaults). To fix:

1. Deploy the ConfigMap:
   ```bash
   kubectl apply -f deploy/configmap-saturation-scaling.yaml
   ```

2. The watch mechanism will automatically reload the cache once ConfigMap is available

3. Verify cache loaded:
   ```bash
   kubectl logs -n <workload-variant-autoscaler-namespace> deployment/wva-controller | grep "Saturation scaling configuration loaded"
   ```

## Example: Production Setup

**deploy/configmap-saturation-scaling.yaml:**
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-saturation-scaling-config
  namespace: <workload-variant-autoscaler-namespace>
data:
  # Conservative defaults for most workloads
  default: |
    kvCacheThreshold: 0.80
    queueLengthThreshold: 5
    kvSpareTrigger: 0.1
    queueSpareTrigger: 3

  # High-priority production workload - scale aggressively
  "ibm/granite-13b#production": |
    kvCacheThreshold: 0.70
    queueLengthThreshold: 3
    kvSpareTrigger: 0.20
    queueSpareTrigger: 5

  # Development workload - allow higher saturation
  "meta/llama-70b#development": |
    kvCacheThreshold: 0.90
    queueLengthThreshold: 15
    kvSpareTrigger: 0.05
    queueSpareTrigger: 2
```

Apply the configuration:
```bash
kubectl apply -f deploy/configmap-saturation-scaling.yaml
```

Verify deployment:
```bash
kubectl get cm wva-saturation-scaling-config -n <workload-variant-autoscaler-namespace>
kubectl describe cm wva-saturation-scaling-config -n <workload-variant-autoscaler-namespace>
```

## API Reference

### Go Structs

**SaturationScalingConfig** (defined in `internal/config/saturation_scaling.go`):
```go
type SaturationScalingConfig struct {
    ModelID              string                `yaml:"model_id,omitempty"`
    Namespace            string                `yaml:"namespace,omitempty"`
    KvCacheThreshold     float64               `yaml:"kvCacheThreshold"`
    QueueLengthThreshold float64               `yaml:"queueLengthThreshold"`
    KvSpareTrigger       float64               `yaml:"kvSpareTrigger"`
    QueueSpareTrigger    float64               `yaml:"queueSpareTrigger"`
    EnableLimiter        bool                  `yaml:"enableLimiter,omitempty"`
    AnalyzerName         string                `yaml:"analyzerName,omitempty"`
    ScaleUpThreshold     float64               `yaml:"scaleUpThreshold,omitempty"`   // default 0.85
    ScaleDownBoundary    float64               `yaml:"scaleDownBoundary,omitempty"`  // default 0.70
    Priority             float64               `yaml:"priority,omitempty"`           // default 1.0
    Analyzers            []AnalyzerScoreConfig `yaml:"analyzers,omitempty"`
}

// AnalyzerScoreConfig configures one analyzer in the multi-analyzer pipeline.
// On this branch, only `Name` is consumed (it must match a RegisterAnalyzer
// call); `Enabled`, `Score`, and the per-analyzer threshold overrides are
// reserved for follow-up PRs.
type AnalyzerScoreConfig struct {
    Name              string   `yaml:"name"`
    Enabled           *bool    `yaml:"enabled,omitempty"`           // default true
    Score             float64  `yaml:"score,omitempty"`             // default 1.0
    ScaleUpThreshold  *float64 `yaml:"scaleUpThreshold,omitempty"`  // overrides global (saturation only today)
    ScaleDownBoundary *float64 `yaml:"scaleDownBoundary,omitempty"` // overrides global (saturation only today)
}
```

**Methods:**
- `ApplyDefaults()` - Fills in zero-valued V2 fields with their defaults and seeds the `Analyzers` list when empty (V1 fields have no hardcoded defaults)
- `Validate() error` - Validates configuration values (thresholds in range, consistency checks, per-analyzer overrides)

## Architecture Notes

### Caching Implementation Details

The caching mechanism uses the following components:

**Thread Safety:**
- Uses `sync.RWMutex` for concurrent access control
- Multiple reconciliation loops can read cache simultaneously
- Write operations (cache reload) are exclusive

**Defensive Copy:**
- `SaturationConfigForNamespace()` returns a deep copy
- Prevents external code from modifying cached configuration
- Each caller gets an independent copy

**Watch Mechanism:**
- Kubernetes watch on `wva-saturation-scaling-config` ConfigMap
- Predicate filters to only relevant ConfigMap events
- Event handler reloads cache and triggers reconciliation

**Graceful Degradation:**
- Controller starts successfully even if ConfigMap missing
- V2 analyzer uses hardcoded defaults (`scaleUpThreshold: 0.85`, `scaleDownBoundary: 0.70`)
- V1 analyzer has **no hardcoded defaults** — all thresholds will be zero without ConfigMap
- Automatically loads config once ConfigMap becomes available

