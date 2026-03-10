# Saturation Analyzer

## Overview

The Saturation Analyzer is a **fast, reactive, and safe saturation guardrail** that prevents capacity exhaustion by monitoring live vLLM metrics.

**Key Features:**
- ✅ Operates from live vLLM metrics (no offline profiling required)
- ✅ Detects imminent capacity exhaustion (KV-cache or request queue)
- ✅ Makes **per-variant** target replica calculations with cost-awareness
- ✅ Uses ready replicas (those reporting metrics) to avoid excessive scale-up
- ✅ **Prevents cascade scaling** by blocking scale-up when replicas are pending
- ✅ Preserves desired replicas from previous runs
- ✅ Analyzes capacity across all variants of the same model

## Architecture

### Components

**1. Saturation Analyzer (`internal/saturation/analyzer.go`)**
- Core analysis logic for saturation-based scaling decisions
- Implements spare capacity calculations
- Performs worst-case scale-down safety simulation
- Makes **per-variant** scaling decisions with cost-awareness

**2. Metrics Collector**

- **Query registration** (`internal/collector/registration/saturation.go`): defines the `max_over_time[1m]` PromQL templates for `vllm:kv_cache_usage_perc` and `vllm:num_requests_waiting`
- **Prometheus source** (`internal/collector/source/prometheus/prometheus_source.go`): executes queries and caches results
- **Replica metrics** (`internal/collector/replica_metrics.go`): enriches raw query results with pod metadata (variant name, accelerator type)

**3. Interfaces (`internal/interfaces/saturation_analyzer.go`)**

- Defines `ReplicaMetrics` data structure (with variant cost, KV cache, queue fields)
- Defines analysis results: `ModelSaturationAnalysis`, `VariantSaturationAnalysis`
- Defines `VariantDecision` for per-variant scaling decisions (with pipeline step history)
- Defines `VariantReplicaState` for current/desired/pending replica tracking
- Defines `SaturationAnalyzer` interface (`AnalyzeModelSaturation`, `CalculateSaturationTargets`)
- Sibling `analyzer.go` holds the generic `Analyzer` interface, `AnalyzerInput`, `AnalyzerResult`, and `VariantCapacity` (used by the V2 engine)

### Data Flow

```
┌─────────────┐
│  Prometheus │
└──────┬──────┘
       │ vLLM metrics (KV cache, queue length)
       ↓
┌──────────────────┐
│ MetricsCollector │
└────────┬─────────┘
         │ ReplicaMetrics[] (with cost)
         ↓
┌──────────────────────────┐
│ AnalyzeModelCapacity     │  ← CapacityScalingConfig
└────────┬─────────────────┘
         │ ModelCapacityAnalysis (with per-variant breakdown)
         ↓
┌─────────────────────────────┐
│ CalculateCapacityTargets    │  ← VariantReplicaState[] (current/desired from CRD)
│ - Preserves desired replicas      │
│ - Cost-aware variant selection    │
└────────┬──────────────────────────┘
         │ Capacity Targets: map[variantName]targetReplicas
         ↓
┌──────────────────┐
│    Controller    │
└──────────────────┘
```

## Analysis Algorithm

### Identify Non-Saturated Replicas

A replica is **non-saturated** if:
```
kv_cache_usage < kvCacheThreshold AND queue_length < queueLengthThreshold
```

**Default thresholds:**
- `kvCacheThreshold`: 0.80 (80%)
- `queueLengthThreshold`: 5

### Calculate Spare Capacity

For each non-saturated replica:
```
spare_kv_i = kvCacheThreshold - kv_cache_usage_i
spare_queue_i = queueLengthThreshold - queue_length_i
```

### Average Spare Capacity

Across all non-saturated replicas:
```
avg_spare_kv = Σ spare_kv_i / N_non_sat
avg_spare_queue = Σ spare_queue_i / N_non_sat
```

### Scale-Up Decision

Trigger scale-up if:
```
avg_spare_kv < kvSpareTrigger OR avg_spare_queue < queueSpareTrigger
```

**Default triggers:**
- `kvSpareTrigger`: 0.1 (10%)
- `queueSpareTrigger`: 3

### Scale-Down Safety Simulation

Before allowing scale-down, simulate total load redistribution across remaining replicas:

```
remaining_replicas = N_non_sat - 1

// Calculate total load across all non-saturated replicas
total_kv_load = Σ kv_cache_usage_i (for all non-saturated replicas)
total_queue_load = Σ queue_length_i (for all non-saturated replicas)

// Simulate removing one replica: redistribute total load
avg_kv_after_removal = total_kv_load / remaining_replicas
avg_queue_after_removal = total_queue_load / remaining_replicas

// Calculate remaining spare capacity
remaining_spare_kv = kvCacheThreshold - avg_kv_after_removal
remaining_spare_queue = queueLengthThreshold - avg_queue_after_removal
```

**Scale-down is safe if:**
```
remaining_spare_kv >= kvSpareTrigger AND
remaining_spare_queue >= queueSpareTrigger AND
N_non_sat >= 2
```

## Decision Logic

### Calculate Capacity Targets

`CalculateCapacityTargets(capacityAnalysis, variantStates) → map[variantName]targetReplicas`

For each variant, determines target replicas based on **capacity needs only**:

| Condition | Target Replicas | Rationale |
|-----------|----------------|-----------|
| **desired ≠ 0 AND desired ≠ current** | target = **desired** | Preserve previous decision (from CRD status) |
| Capacity needs scale-up | **Cheapest** non-preserved variant: readyReplicas + 1 | Cost-optimized capacity expansion (deterministic: alphabetically first variant on tie) |
| Capacity allows scale-down | **Most expensive** non-preserved variant: readyReplicas - 1 | Cost-optimized capacity reduction (deterministic: alphabetically last variant on tie) |
| Otherwise | target = readyReplicas | No capacity action needed |

**Note:** `readyReplicas` = number of replicas reporting capacity metrics (from `VariantCapacityAnalysis.ReplicaCount`). This prevents excessive scale-up when replicas are still starting up.

**Cascade Scaling Prevention:** Variants with pending replicas (pods that exist but are not yet ready) are skipped during scale-up selection. This prevents the controller from repeatedly scaling up the same variant while previous scale-up operations are still in progress. Pod startup can take 2-7 minutes depending on model size and hardware (container initialization, model loading, health checks).

**Example Output:**
```
Model: llama-70b
Variants:
  - v1-l4 (cost=$5): current=2, ready=2, desired=0 → target=3 (cheapest, scaled up for capacity)
  - v2-a100 (cost=$20): current=4, ready=3, desired=4 → target=4 (preserved desired)

Note: v2-a100 has 4 current replicas but only 3 are ready (reporting metrics).
      Target is set to desired=4 because desired ≠ current.
```

**Key Principles:**
1. **Ready replicas only**: Use replicas reporting metrics to avoid scaling up for not-yet-ready pods
2. **Preserve desired replicas**: When desired ≠ current, always use desired as capacity target
3. **Cost-aware selection**: Cheapest variant for scale-up, most expensive for scale-down
4. **Deterministic tie-breaking**: When variants have equal costs, alphabetically first for scale-up, last for scale-down
5. **Pending replica awareness**: Skip variants with pending replicas during scale-up to prevent cascade scaling

## Usage Examples

### Complete Flow

```go
import (
    "context"
    "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/saturation"
    "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector"
    "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
)

// Create analyzer
analyzer := saturation.NewAnalyzer()

// Collect metrics (uses max_over_time[1m] for safety-first analysis)
// Note: Cost should be populated from CRD spec (default 10)
metricsCollector := collector.NewReplicaMetricsCollector(metricsSource, k8sClient)
replicaMetrics, err := metricsCollector.CollectReplicaMetrics(ctx, modelID, namespace)

// Get capacity config
config := interfaces.DefaultCapacityScalingConfig()

// Analyze capacity across all variants
analysis, err := analyzer.AnalyzeModelCapacity(ctx, modelID, namespace, replicaMetrics, config)

// Build variant states (current replicas from pod count, desired from CRD status)
// PendingReplicas = CurrentReplicas - ReadyReplicas (pods that exist but aren't ready yet)
variantStates := []interfaces.VariantReplicaState{
    {VariantName: "v1-l4", CurrentReplicas: 2, DesiredReplicas: 0, PendingReplicas: 0},    // no previous run, all ready
    {VariantName: "v2-a100", CurrentReplicas: 4, DesiredReplicas: 4, PendingReplicas: 1},  // wanted 4, 1 pod still starting
}

// Calculate saturation-based targets
capacityTargets := analyzer.CalculateCapacityTargets(analysis, variantStates)

log.Printf("Capacity targets: %+v", capacityTargets)
// Output: map[v1-l4:3 v2-a100:4]
// - v1-l4: scaled to 3 (cheapest variant, capacity needs scale-up)
// - v2-a100: preserved at 4 (desired ≠ current)
```

### Applying Saturation-Based Targets

```go
// Calculate capacity targets
capacityTargets := analyzer.CalculateCapacityTargets(analysis, variantStates)

// Convert to decisions and apply
for variantName, target := range capacityTargets {
    state := getVariantState(variantName, variantStates) // helper function

    if target > state.CurrentReplicas {
        log.Printf("Scale-up %s: %d → %d", variantName, state.CurrentReplicas, target)
        // Apply scale-up
    } else if target < state.CurrentReplicas {
        log.Printf("Scale-down %s: %d → %d", variantName, state.CurrentReplicas, target)
        // Apply scale-down
    } else {
        log.Printf("No change for %s: %d", variantName, target)
    }
}
```

## Multi-Variant Analysis

The saturation analyzer aggregates metrics **across all variants of the same model**:

```go
// Example: Model "llama-70b" with 2 variants
// - variant-1 (A100, cost: $20, 2 replicas)
// - variant-2 (H100, cost: $15, 3 replicas)

replicaMetrics := []interfaces.ReplicaMetrics{
    // Variant 1 (more expensive)
    {PodName: "v1-pod-1", VariantName: "variant-1", ModelID: "llama-70b",
     AcceleratorName: "A100", Cost: 20, KvCacheUsage: 0.70, QueueLength: 2},
    {PodName: "v1-pod-2", VariantName: "variant-1", ModelID: "llama-70b",
     AcceleratorName: "A100", Cost: 20, KvCacheUsage: 0.75, QueueLength: 3},

    // Variant 2 (cheaper)
    {PodName: "v2-pod-1", VariantName: "variant-2", ModelID: "llama-70b",
     AcceleratorName: "H100", Cost: 15, KvCacheUsage: 0.60, QueueLength: 1},
    {PodName: "v2-pod-2", VariantName: "variant-2", ModelID: "llama-70b",
     AcceleratorName: "H100", Cost: 15, KvCacheUsage: 0.65, QueueLength: 2},
    {PodName: "v2-pod-3", VariantName: "variant-2", ModelID: "llama-70b",
     AcceleratorName: "H100", Cost: 15, KvCacheUsage: 0.55, QueueLength: 1},
}

// Analyzer aggregates across all 5 replicas
analysis, _ := analyzer.AnalyzeModelCapacity(ctx, "llama-70b", "prod", replicaMetrics, config)

// Results include per-variant breakdown with cost
fmt.Printf("Total replicas: %d\n", analysis.TotalReplicas) // 5
fmt.Printf("Non-saturated: %d\n", analysis.NonSaturatedCount) // 5
fmt.Printf("Variants analyzed: %d\n", len(analysis.VariantAnalyses)) // 2

for _, va := range analysis.VariantAnalyses {
    fmt.Printf("Variant: %s, Replicas: %d, Accelerator: %s, Cost: %.2f\n",
        va.VariantName, va.ReplicaCount, va.AcceleratorName, va.Cost)
    // Note: va.ReplicaCount = ready replicas (those reporting metrics)
}

// If capacity needs scale-up and no optimizer guidance:
// → variant-2 (H100) will be scaled up (cheaper at $15 vs $20)
// → Target = readyReplicas + 1 (prevents excessive scale-up for not-yet-ready pods)
//
// If capacity allows scale-down in saturation mode:
// → variant-1 (A100) will be scaled down (more expensive at $20)
```

## Configuration

### Cascade Scaling Prevention

**Problem:** Without pending replica awareness, the saturation analyzer could repeatedly trigger scale-up for the same variant before previous scale-up operations complete, leading to excessive replica counts.

**Timeline Example (Without Protection):**
```
T+0s:  Saturation detected → Scale up variant-1 from 2 to 3 replicas
T+30s: New pod created but not ready yet (still loading model)
       Saturation still detected (only 2 ready replicas) → Scale up to 4 replicas
T+60s: Both new pods still starting, saturation persists → Scale up to 5 replicas
T+90s: All 5 pods now ready, but we have 3 extra replicas (over-provisioned)
```

**Solution:** WVA tracks **pending replicas** (`CurrentReplicas - ReadyReplicas`) per variant and skips variants with pending replicas during scale-up selection.

**How It Works:**
1. **Replica State Tracking**: Controller maintains `VariantReplicaState` with:
   - `CurrentReplicas`: Total pods (from Deployment)
   - `DesiredReplicas`: Target from previous run (from CRD status)
   - `PendingReplicas`: Pods that exist but aren't ready (`CurrentReplicas - ReadyReplicas`)

2. **Scale-Up Selection**: When saturation triggers scale-up:
   ```go
   // Pseudo-code from internal/saturation/analyzer.go
   for each variant:
       if variant has preserved desired replicas:
           skip  // Already has decision from previous run
       if variant.PendingReplicas > 0:
           skip  // Wait for pending pods to become ready
       if variant.Cost < cheapest.Cost:
           cheapest = variant

   scale_up(cheapest)  // Only if no pending replicas
   ```

3. **Per-Variant Tracking**: Each variant is tracked independently. If variant-1 has pending replicas, variant-2 can still scale up if it's the cheapest eligible variant.

**Timeline Example (With Protection):**
```
T+0s:  Saturation detected → Scale up variant-1 from 2 to 3 (PendingReplicas=1)
T+30s: Saturation still detected, but variant-1 skipped (has 1 pending replica)
       If variant-2 is cheaper and has no pending replicas → Scale up variant-2
T+90s: variant-1 pod becomes ready (PendingReplicas=0), now eligible for scale-up again
```

**Benefits:**
- ✅ Prevents excessive scale-up during model loading periods (2-7 minutes)
- ✅ Reduces infrastructure costs by avoiding over-provisioning
- ✅ Maintains cost-optimized scaling across multiple variants

**Note:** Scale-down operations are not affected by pending replicas, as removing capacity is always safe when replicas are starting up.

Saturation scaling thresholds are configured via ConfigMap (see [saturation-scaling-config.md](saturation-scaling-config.md)):

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: capacity-scaling-config
  namespace: workload-variant-autoscaler-system
data:
  default: |
    kvCacheThreshold: 0.80
    queueLengthThreshold: 5
    kvSpareTrigger: 0.1
    queueSpareTrigger: 3
```

**Per-model overrides:**
```yaml
llama-70b-prod: |
  model_id: meta/llama-70b
  namespace: production
  kvCacheThreshold: 0.85
  kvSpareTrigger: 0.15
```

## Testing

Comprehensive unit tests are provided in `internal/saturation/analyzer_test.go`:

```bash
go test ./internal/saturation/...
```

**Test coverage:**
- ✅ Scale-up trigger conditions
- ✅ Scale-down safety simulation (total load redistribution)
- ✅ Multi-variant aggregation
- ✅ **CalculateCapacityTargets**
  - ✅ Scale-up cheapest variant
  - ✅ Scale-down most expensive variant
  - ✅ Preserve desired replicas (desired ≠ current)
  - ✅ All variants preserved scenario
  - ✅ Equal costs with deterministic tie-breaking
  - ✅ Scale-down below minimum (prevents scaling to 0)
- ✅ Saturated replica identification
- ✅ Edge cases (empty metrics, single replica, nil analysis)

## Observability

### Log Messages

**Saturation analysis:**
```
DEBUG Saturation analysis completed
  modelID=llama-70b
  namespace=prod
  totalReplicas=5
  nonSaturated=4
  avgSpareKv=0.150
  avgSpareQueue=2.5
  shouldScaleUp=true
  scaleDownSafe=false
```

**Scale-down safety:**
```
DEBUG Scale-down unsafe: insufficient headroom after redistribution
  remainingSpareKv=0.050
  kvTrigger=0.100
  kvSafe=false
  remainingSpareQueue=1.0
  queueTrigger=3
  queueSafe=false
```

**Capacity target calculation:**
```
INFO Capacity target: scale-up cheapest variant
  variant=v1-l4
  cost=5.00
  currentReplicas=2     (total replicas per CRD)
  readyReplicas=2       (replicas reporting metrics)
  target=3
  reason=KV spare capacity low
```

## Performance Characteristics

### Computational Complexity

- **Per-replica analysis:** O(N) where N = number of replicas
- **Variant aggregation:** O(V) where V = number of variants
- **Overall:** O(N + V), typically O(N) as V << N

### Prometheus Queries

**Two queries per model** (registered in `internal/collector/registration/saturation.go`):

1. `max by (pod) (max_over_time(vllm:kv_cache_usage_perc{namespace="prod",model_name="llama-70b"}[1m]))` — peak KV cache utilization per pod
2. `max by (pod) (max_over_time(vllm:num_requests_waiting{namespace="prod",model_name="llama-70b"}[1m]))` — peak queue length per pod

**Query strategy:** Uses `max_over_time[1m]` to capture peak capacity usage in the last minute, providing conservative safety-first analysis that prevents missing saturation events between queries. The `model_id` filter ensures metrics are scoped to the specific model being analyzed, preventing cross-model metric pollution.

**Query frequency:** Once per reconciliation loop (typically every 60s)

## Integration Notes

### Controller Integration

The saturation analyzer is integrated into the controller's reconciliation loop:

1. **Collect metrics** for all pods of a model (across all variants)
   - Enrich with cost from CRD spec (default: 10)

2. **Analyze capacity** using `AnalyzeModelCapacity`
   - Aggregates metrics across all variants
   - Produces `ModelCapacityAnalysis` with per-variant breakdown and cost

3. **Build variant states** with current and desired replicas
   - Current replicas: from actual pod count
   - Desired replicas: from CRD status field (previous run), 0 if not set

4. **Calculate capacity targets** using `CalculateCapacityTargets`
   - Preserves desired replicas when desired ≠ current
   - Uses cost-based selection (cheapest/most expensive) for capacity actions
   - Returns `map[variantName]targetReplicas`

5. **Apply decisions** per variant
   - Scale each variant to its target replicas

### Metrics Requirements

The analyzer requires these Prometheus metrics from vLLM. Queries are registered in `internal/collector/registration/saturation.go`:

- `vllm:kv_cache_usage_perc` — KV cache utilization (0.0-1.0)
- `vllm:num_requests_waiting` — Queue length (integer)

These metrics must include the following labels:
- `pod` or `pod_name` — Pod identification
- `model_id` — Model identification (to prevent cross-model metric pollution)
- `namespace` — Kubernetes namespace

### CRD Requirements

The analyzer requires two fields from the CRD:

**Spec fields:**
- `cost` (float64, optional): Cost per replica for this variant (default: 10)
  - Used for cost-aware variant selection
  - Cheapest variant scaled up, most expensive scaled down

**Status fields:**
- `desiredReplicas` (int, optional): Target replicas from previous run
  - Set to 0 or omit if not set yet
  - Used to preserve previous decisions
  - When `desired ≠ 0 AND desired ≠ current`: sets `capacityTarget = desired`

## Limitations

1. **Minimum replicas:** Scale-down requires ≥2 non-saturated replicas for safety simulation; variants cannot be scaled below 1 replica
2. **Metric availability:** Assumes vLLM metrics are available in Prometheus
3. **Pod identification:** Requires pod and model_id labels in Prometheus metrics
4. **No model profiling:** Does not account for model-specific capacity curves
5. **Cost field:** Currently uses constant value (DefaultReplicaCost = 10.0); CRD integration pending

## Future Enhancements

Potential improvements:
- Per-accelerator type threshold overrides
- Historical capacity trend analysis
- Predictive capacity planning
- Integration with Inference Scheduler thresholds
- Metric-based cache invalidation

## References
- Related: [Saturation Scaling Configuration](saturation-scaling-config.md)

