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

## Table of Contents

- [Overview](#overview)
- [Analysis Algorithm](#analysis-algorithm)
  - [Identify Non-Saturated Replicas](#identify-non-saturated-replicas)
  - [Calculate Spare Capacity](#calculate-spare-capacity)
  - [Average Spare Capacity](#average-spare-capacity)
  - [Scale-Up Decision](#scale-up-decision)
  - [Scale-Down Safety Simulation](#scale-down-safety-simulation)
- [Decision Logic](#decision-logic)
  - [Calculate Saturation Targets](#calculate-saturation-targets)
  - [Model-Level Transition Blocking](#model-level-transition-blocking)
  - [Scaling Decision Table](#scaling-decision-table-when-model-is-stable)
- [Configuration](#configuration)
  - [Cascade Scaling Prevention](#cascade-scaling-prevention)
- [Architecture](#architecture)
  - [Components](#components)
  - [Data Flow](#data-flow)
- [References](#references)

## Analysis Algorithm

### Identify Non-Saturated Replicas

A replica is **non-saturated** if:
```
kv_cache_usage < kvCacheThreshold AND queue_length < queueLengthThreshold
```

**Recommended thresholds** (set via ConfigMap — see [Configuration](#configuration)):
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

**Recommended triggers** (set via ConfigMap — see [Configuration](#configuration)):
- `kvSpareTrigger`: 0.1 (10%)
- `queueSpareTrigger`: 3

> **Note:** These V1 thresholds are **not hardcoded** in the analyzer. They must be provided
> via the `wva-saturation-scaling-config` ConfigMap. If the ConfigMap is missing or has no
> `default` entry, all thresholds default to zero, which will cause every replica to appear
> saturated. Always deploy the ConfigMap with a `default` entry.

### Scale-Down Safety Simulation

Before allowing scale-down, the analyzer simulates load redistribution using a scale-factor
approach. Instead of summing raw per-replica loads, it derives the current average load from
the already-computed average spare capacity and then applies a `N/(N-1)` scale factor to
predict load after removing one replica.

See: `internal/saturation/analyzer.go` — `isScaleDownSafe()`

```
// Pre-condition: require at least 2 non-saturated replicas
if N_non_sat < 2:
    return unsafe

// Derive current average load from average spare capacity
// (Load = Threshold - Spare)
avg_kv_load = kvCacheThreshold - avg_spare_kv
avg_queue_load = queueLengthThreshold - avg_spare_queue

// Simulate removing one replica: load increases by factor N/(N-1)
scale_factor = N_non_sat / (N_non_sat - 1)
avg_kv_after_removal = avg_kv_load * scale_factor
avg_queue_after_removal = avg_queue_load * scale_factor

// Calculate remaining spare capacity after redistribution
remaining_spare_kv = kvCacheThreshold - avg_kv_after_removal
remaining_spare_queue = queueLengthThreshold - avg_queue_after_removal
```

**Scale-down is safe if:**
```
remaining_spare_kv >= kvSpareTrigger AND
remaining_spare_queue >= queueSpareTrigger
```

> **Note:** The minimum-replicas check (`N_non_sat >= 2`) is enforced as a pre-condition
> before the simulation runs, defined by `MinNonSaturatedReplicasForScaleDown` in
> `internal/saturation/constants.go`.

## Decision Logic

### Calculate Saturation Targets

`CalculateSaturationTargets(saturationAnalysis, variantStates) → map[variantName]targetReplicas`

For each variant, determines target replicas based on **saturation analysis**:

#### Model-Level Transition Blocking

Before any scaling decisions are made, the analyzer checks whether the **entire model** is
in a transitional state. If **any** variant of the model is transitioning, **all** scaling
decisions for the model are blocked. This prevents decisions based on incomplete capacity data.

See: `internal/saturation/analyzer.go` — `CalculateSaturationTargets()`, Step 1

A variant is considered transitioning if **either** condition is true:

| Check | Condition | Meaning |
|-------|-----------|---------|
| **Desired vs Current** | `DesiredReplicas ≠ 0 AND DesiredReplicas ≠ CurrentReplicas` | A previous scaling decision is still being applied |
| **Metrics vs Current** | `MetricsCount ≠ CurrentReplicas` | Not all pods are ready and reporting metrics |

When transition is detected:
- Variants with a desired/current mismatch preserve their `DesiredReplicas` as the target
- All other variants preserve their `CurrentReplicas` as the target
- No new scale-up or scale-down decisions are made for any variant of the model

#### Scaling Decision Table (when model is stable)

| Condition | Target Replicas | Rationale |
|-----------|----------------|-----------|
| Capacity needs scale-up | **Cheapest** eligible variant: readyReplicas + 1 | Cost-optimized capacity expansion (deterministic: alphabetically first variant on tie) |
| Capacity allows scale-down | **Most expensive** eligible variant (with target > 1): readyReplicas - 1 | Cost-optimized capacity reduction (deterministic: alphabetically last variant on tie) |
| Otherwise | target = readyReplicas | No capacity action needed |

After scaling decisions, targets are clamped to `[minReplicas, maxReplicas]` bounds from the
VariantAutoscaling spec (if specified).

**Note:** `readyReplicas` = number of replicas reporting capacity metrics (from `VariantSaturationAnalysis.ReplicaCount`). This prevents excessive scale-up when replicas are still starting up.

**Cascade Scaling Prevention:** Variants with pending replicas (pods that exist but are not yet ready) are skipped during scale-up selection. This prevents the controller from repeatedly scaling up the same variant while previous scale-up operations are still in progress. Pod startup can take 2-7 minutes depending on model size and hardware (container initialization, model loading, health checks).

> **Important:** Cascade prevention (pending replica checks) only applies when the model is
> **not** in transition. If any variant triggers model-level transition blocking (above),
> all decisions are blocked before the per-variant cascade prevention logic runs.

**Example Output (model stable — all variants have metrics == current and desired == current or 0):**
```
Model: llama-70b
Variants:
  - v1-l4 (cost=$5): current=2, ready=2, desired=0 → target=3 (cheapest, scaled up for capacity)
  - v2-a100 (cost=$20): current=2, ready=2, desired=0 → target=2 (no change)
```

**Example Output (model in transition — scaling blocked):**
```
Model: llama-70b
Variants:
  - v1-l4 (cost=$5): current=2, ready=2, desired=0 → target=2 (blocked: model transitioning)
  - v2-a100 (cost=$20): current=4, ready=3, desired=0 → target=4 (preserved current)

Note: v2-a100 has metrics(3) ≠ current(4) — only 3 of 4 pods are reporting.
      This triggers model-level transition, blocking all variants from new decisions.
```

**Key Principles:**
1. **Model-level transition blocking**: If any variant is transitioning, block all scaling for the model
2. **Ready replicas only**: Use replicas reporting metrics to avoid scaling up for not-yet-ready pods
3. **Cost-aware selection**: Cheapest variant for scale-up, most expensive for scale-down
4. **Deterministic tie-breaking**: When variants have equal costs, alphabetically first for scale-up, last for scale-down
5. **Pending replica awareness**: Skip variants with pending replicas during scale-up to prevent cascade scaling

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

**Solution:** WVA uses two layers of protection:
1. **Model-level transition blocking** (primary): If any variant has `desired ≠ current` or `metrics ≠ current`, all scaling is blocked for the entire model (see [Transition Blocking](#model-level-transition-blocking) above).
2. **Pending replica checks** (secondary): When the model is stable, variants with pending replicas are skipped during scale-up selection.

**How It Works:**
1. **Replica State Tracking**: Controller maintains `VariantReplicaState` with:
   - `CurrentReplicas`: Total pods (from Deployment)
   - `DesiredReplicas`: Target from previous run (from CRD status)
   - `PendingReplicas`: Pods that exist but aren't ready (`CurrentReplicas - ReadyReplicas`)

2. **Model-Level Transition Check** (Step 1 in `CalculateSaturationTargets`):
   ```
   // If ANY variant is transitioning, block ALL scaling for the model
   for each variant:
       if variant.DesiredReplicas ≠ 0 AND DesiredReplicas ≠ CurrentReplicas:
           model_in_transition = true
       if variant.MetricsCount ≠ CurrentReplicas:
           model_in_transition = true

   if model_in_transition:
       return preserved targets (no new decisions)
   ```

3. **Scale-Up Selection** (Step 4, only when model is stable):
   ```go
   // Pseudo-code from internal/saturation/analyzer.go
   for each variant:
       if variant.PendingReplicas > 0:
           skip  // Wait for pending pods to become ready
       if variant.Cost < cheapest.Cost:
           cheapest = variant

   scale_up(cheapest)  // Only if no pending replicas
   ```

4. **Per-Variant Tracking**: Each variant is tracked independently. If variant-1 has pending replicas, variant-2 can still scale up if it's the cheapest eligible variant.

**Timeline Example (With Protection):**
```
T+0s:   Saturation detected → Scale up variant-1 from 2 to 3
T+30s:  variant-1: current=3, but only 2 pods reporting metrics
        Check 2 triggers: metrics(2) ≠ current(3)
        → Model in transition, ALL scaling blocked
T+90s:  variant-1: all 3 pods ready and reporting metrics
        metrics(3) == current(3), desired reset to 0
        → Model stable, new scaling decisions allowed
```

**Benefits:**
- ✅ Prevents excessive scale-up during model loading periods (2-7 minutes)
- ✅ Reduces infrastructure costs by avoiding over-provisioning
- ✅ Maintains cost-optimized scaling across multiple variants

**Note:** Scale-down operations are not affected by pending replicas, as removing capacity is always safe when replicas are starting up.

Saturation scaling thresholds are configured via ConfigMap (see [../saturation-scaling-config.md](../saturation-scaling-config.md)):

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: wva-saturation-scaling-config
  namespace: workload-variant-autoscaler-system
data:
  default: |
    kvCacheThreshold: 0.80
    queueLengthThreshold: 5
    kvSpareTrigger: 0.1
    queueSpareTrigger: 3
```

**Per-model overrides:**

The saturation engine resolves per-model config using a lookup key in the format
`{modelID}#{namespace}` (see `internal/engines/saturation/engine.go` — `resolveSaturationConfig()`).
The ConfigMap data key **must** match this format for overrides to take effect.
Lookup order: `modelID#namespace` → `default` → zero-value with defaults applied.

```yaml
  "meta/llama-70b#production": |
    kvCacheThreshold: 0.85
    queueLengthThreshold: 5
    kvSpareTrigger: 0.15
    queueSpareTrigger: 3
```

> **Note:** Overrides **fully replace** the `default` config — there is no field-level
> inheritance. Always specify all required threshold fields. The `model_id` and `namespace`
> YAML fields inside the entry are parsed into the config struct but are **not used for
> lookup**. The ConfigMap data key itself determines which model/namespace the override
> applies to.

## Architecture

### Components

**1. Saturation Analyzer (`internal/saturation/analyzer.go`)**
- Core analysis logic for saturation-based scaling decisions
- Implements spare capacity calculations
- Performs worst-case scale-down safety simulation
- Makes **per-variant** scaling decisions with cost-awareness

**2. Metrics Collector (`internal/collector/replica_metrics.go`)**
- Collects vLLM metrics from Prometheus using `max_over_time[1m]` queries
- Queries `constants.VLLMKvCacheUsagePerc` and `constants.VLLMNumRequestsWaiting`
- Uses peak values over 1 minute for safety-first capacity analysis
- Enriches metrics with pod metadata (variant name, accelerator type)

**3. Interfaces (`internal/interfaces/analyzer.go`, `internal/interfaces/saturation_analyzer.go`)**
- Defines data structures for replica metrics (including variant cost)
- Defines analysis results and per-variant decision types
- Provides interface for capacity analysis
- Defines `VariantDecision` for per-variant scaling decisions
- Defines `VariantReplicaState` for current/desired replica tracking

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
│ AnalyzeModelSaturation   │  ← SaturationScalingConfig
└────────┬─────────────────┘
         │ ModelSaturationAnalysis (with per-variant breakdown)
         ↓
┌─────────────────────────────────┐
│ CalculateSaturationTargets      │  ← VariantReplicaState[] (current/desired from CRD)
│ - Model-level transition blocking     │
│ - Cost-aware variant selection        │
└────────┬────────────────────────────┘
         │ Saturation Targets: map[variantName]targetReplicas
         ↓
┌──────────────────┐
│    Controller    │
└──────────────────┘
```


## References
- Related: [Saturation Scaling Configuration](../saturation-scaling-config.md)
