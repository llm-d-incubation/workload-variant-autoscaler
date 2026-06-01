# Throughput Analyzer

## Overview

The Throughput Analyzer is a **model-driven, proactive scaling analyzer** that estimates decode
token throughput supply (μ_dec) and compares it against decode token demand (λ_dec) to drive scaling decisions.

Where the Saturation Analyzer reacts to observed capacity exhaustion, the Throughput Analyzer
predicts how much decode throughput the current replica fleet can sustain at a given KV cache
operating point, and scales before demand exceeds that supply.

**Key concepts:**
- **μ_dec** — decode token supply: how many tokens/sec the fleet can generate, estimated from
  KV cache occupancy and a calibrated inter-token latency (ITL) model
- **λ_dec** — decode token demand: how many tokens/sec the scheduler is dispatching to this model
- **ITL(k)** — inter-token latency as a function of KV utilization k: fitted as `A·k + B` via OLS

> **Implementation status:** Query registration, collector wiring (three PromQL queries +
> `GenerationTokenRate`, `KvUsageInstant`, `VLLMRequestRate` fields), ShapeTracker,
> ObservationWindow, and SanityReport are implemented. ITL model fitting, supply/demand estimation,
> scaling signal (full `ThroughputAnalyzer`), and engine wiring are not yet implemented.

## Table of Contents

- [Overview](#overview)
- [Metrics](#metrics)
  - [Throughput Analyzer Queries](#throughput-analyzer-queries)
  - [Shared Fields from Collector](#shared-fields-from-collector)
  - [Query Design Decisions](#query-design-decisions)
- [Architecture](#architecture)
  - [Package Structure](#package-structure)
  - [Components](#components)
  - [Data Flow](#data-flow)
- [ITL Model Calibration](#itl-model-calibration)
  - [Tier 1 — OLS Fit](#tier-1--ols-fit)
  - [Tier 2 — Constrained OLS](#tier-2--constrained-ols)
- [Supply Estimation](#supply-estimation)
- [Demand Estimation](#demand-estimation)
  - [Priority Chain](#priority-chain)
  - [Scheduler Queue Demand](#scheduler-queue-demand)
- [Scaling Signal](#scaling-signal)
  - [Model-Level Aggregation](#model-level-aggregation)
  - [Role-Aware Aggregation](#role-aware-aggregation)
- [Constants and Tuning](#constants-and-tuning)
- [References](#references)

## Metrics

`RegisterThroughputAnalyzerQueries` (in `internal/collector/registration/throughput_analyzer.go`)
registers three queries that are genuinely new and not covered by other analyzer registrations.
All other TA inputs are read from `interfaces.ReplicaMetrics` fields populated by the saturation
and queueing model registrations.

### Throughput Analyzer Queries

#### QueryGenerationTokenRate (`generation_token_rate`)

```promql
sum by (pod) (rate(vllm:request_generation_tokens_sum{namespace="...",model_name="..."}[1m]))
```

**What it measures:** Observed generation (decode) token rate per pod in tokens/sec.

**TA notation:** μ_dec^obs — the directly observable supply proxy. Included for observability;
the analyzer derives supply from the ITL model rather than using this value directly.

**ReplicaMetrics field:** `GenerationTokenRate`

---

#### QueryKvUsageInstant (`kv_usage_instant`)

```promql
max by (pod) (vllm:kv_cache_usage_perc{namespace="...",model_name="..."})
```

**What it measures:** Instantaneous KV cache utilization fraction per pod (0.0–1.0).

**TA notation:** k* — the current operating point in the ITL model `ITL(k) = A·k + B`.

**ReplicaMetrics field:** `KvUsageInstant`

**Same underlying metric as `QueryKvCacheUsage`:** Both queries hit `vllm:kv_cache_usage_perc`.
`QueryKvCacheUsage` (saturation) wraps it in `max_over_time[1m]` to get the 1-minute peak —
a conservative bound for capacity guardrails. `QueryKvUsageInstant` reads the raw gauge so the
Throughput Analyzer sees the current operating point k*, not a high-water mark from a transient
spike that has since subsided. Using the peak would overestimate load and cause premature
scale-up. Both fields coexist on `ReplicaMetrics` for their respective purposes.

**Why `max by (pod)` and not `avg by (pod)`:** `vllm:kv_cache_usage_perc` is a scalar gauge
per vLLM process, so there is one Prometheus series per pod in normal deployment. The
`max by (pod)` clause is purely deduplication: if the same pod is scraped by multiple targets
(e.g., a PodMonitor and a ServiceMonitor), duplicate series with identical values appear under
the same pod label. `max` collapses them. Since duplicates carry the same value, `max = avg`
— the choice has no effect on correctness. This follows the convention used by every other
per-pod query in this codebase.

---

#### QueryVLLMRequestRate (`vllm_request_rate`)

```promql
sum by (pod) (rate(vllm:request_generation_tokens_count{namespace="...",model_name="..."}[1m]))
```

**What it measures:** vLLM-side request completion rate per pod (req/s), derived from the
generation tokens histogram `_count` counter (increments once per completed request).

**TA notation:** fallback λ_req — used when `ArrivalRate == 0` for all pods (EPP not deployed).
Per variant V, the analyzer computes `λ_dec_fallback = Σ_{r∈V} VLLMRequestRate_r × AvgOutputTokens_r` over that variant's replicas.

**ReplicaMetrics field:** `VLLMRequestRate`

**Note:** This also serves as a throughput proxy weight for histogram averaging. When computing
variant-average IL, OL, and prefix hit rate across replicas, each replica is weighted by its
`VLLMRequestRate` to prevent low-throughput replicas from distorting the shape estimate.

**Important:** This measures *completed* (served) requests, not *arriving* requests. It
undercounts when requests are queued in the scheduler. Use `ArrivalRate` (primary) first;
fall back to this only when the EPP is not deployed.

---

### Shared Fields from Collector

The following TA inputs are already collected via other analyzer registrations. The TA reads
these fields directly rather than registering duplicate queries.

| TA notation | Field | Query | Registration |
|---|---|---|---|
| KV_max (token capacity) | `ReplicaMetrics.TotalKvCapacityTokens` | `QueryCacheConfigInfo` | `RegisterSaturationQueries` |
| ITL_obs (observed ITL) | `ReplicaMetrics.AvgITL` | `QueryAvgITL` | `RegisterQueueingModelQueries` |
| OL (avg output tokens) | `ReplicaMetrics.AvgOutputTokens` | `QueryAvgOutputTokens` | `RegisterSaturationQueries` |
| IL (avg input tokens) | `ReplicaMetrics.AvgInputTokens` | `QueryAvgInputTokens` | `RegisterSaturationQueries` |
| H％ (prefix hit rate) | `ReplicaMetrics.PrefixCacheHitRate` | `QueryPrefixCacheHitRate` | `RegisterSaturationQueries` |
| λ_req (per-pod, req/s) | `ReplicaMetrics.ArrivalRate` | `QuerySchedulerDispatchRate` | `RegisterQueueingModelQueries` |
| Q (scheduler queue size) | `SchedulerQueueMetrics.QueueSize` (model-level) | `QuerySchedulerQueueSize` | `RegisterSaturationQueries` |

**λ_dec primary (per variant V):** `Σ_{r∈V} ArrivalRate_r × AvgOutputTokens_r` over the variant's replicas (EPP deployed).  
**λ_dec fallback (per variant V):** `Σ_{r∈V} VLLMRequestRate_r × AvgOutputTokens_r` (EPP absent, all ArrivalRate == 0).

**Note on arrival rate:** `ArrivalRate` comes from `QuerySchedulerDispatchRate` which is per-pod,
namespaced, and model-scoped — correctly isolating traffic to a specific variant. The TA sums
per-replica λ_dec in the analyzer rather than using a model-level query, which avoids the
namespace filtering limitation of the scheduler metric.

---

### Query Design Decisions

| Query / Field | Source | Aggregation | Window | Purpose in TA |
|---|---|---|---|---|
| `QueryGenerationTokenRate` | vLLM | `sum by (pod)` | 1m rate | μ_dec^obs per pod (observability) |
| `QueryKvUsageInstant` | vLLM | `max by (pod)` | instant | k* (no max_over_time) |
| `QueryVLLMRequestRate` | vLLM | `sum by (pod)` | 1m rate | Fallback λ_req; histogram weight |
| `TotalKvCapacityTokens` | `KvCacheConfigInfo` labels | derived | static | KV_max = blocks × block_size |
| `AvgITL` | `QueryAvgITL` | `max by (pod)` | 1m rate | ITL_obs for OLS calibration |
| `AvgOutputTokens` | `QueryAvgOutputTokens` | `max by (pod)` | 5m rate | OL for KV_req and λ_dec |
| `AvgInputTokens` | `QueryAvgInputTokens` | `max by (pod)` | 5m rate | IL for IL_eff = IL × (1−H％) |
| `PrefixCacheHitRate` | `QueryPrefixCacheHitRate` | `max by (pod)` | 5m rate | H％ for IL_eff |
| `ArrivalRate` | `QuerySchedulerDispatchRate` | `sum by (pod_name, namespace)` | 1m rate | λ_req per pod (primary) |

## Architecture

> **Note:** Query Registration, Metrics Collector, Package Structure, ShapeTracker,
> ObservationWindow, and SanityReport are implemented. ITLModel, full ThroughputAnalyzer,
> Analysis Pipeline, and Data Flow are **not yet implemented**.

### Package Structure

```
internal/engines/analyzers/throughput/
├── constants.go               thresholds, window params, tuning defaults
├── types.go                   WorkloadShape, ITLObservation, SanityIssue, SanityReport,
│                              ThroughputVariantState
├── shape_tracker.go           ShapeTracker: (IL,OL) bucket + change detection
├── observation_window.go      ObservationWindow: rolling (k,ITL) pairs, Ready flag
├── sanity.go                  CheckModelMetrics: 6 SanityIssue types
├── itl_model.go               ITLModel{A,B}, FitITLModel (OLS), ITLAt(k)
├── itl_knowledge_store.go     itlKnowledgeStore: tier-3 skeleton (not yet wired)
└── analyzer.go                ThroughputAnalyzer: Observe() + full Analyze()
```

### Components

**Query Registration (`internal/collector/registration/throughput_analyzer.go`)**  
Registers three PromQL templates exclusive to the throughput analyzer:
`QueryGenerationTokenRate`, `QueryKvUsageInstant`, `QueryVLLMRequestRate`.
`RegisterThroughputAnalyzerQueries` must be called once at startup alongside
`RegisterSaturationQueries` and `RegisterQueueingModelQueries`.

**Metrics Collector (`internal/collector/replica_metrics.go`)**  
Populates all `interfaces.ReplicaMetrics` fields in a single `Refresh()` call covering all
12 registered queries. The three TA-exclusive fields are:
`GenerationTokenRate`, `KvUsageInstant`, `VLLMRequestRate`.
The remaining TA fields (`TotalKvCapacityTokens`, `AvgITL`, `AvgOutputTokens`, `AvgInputTokens`,
`PrefixCacheHitRate`, `ArrivalRate`) are populated by saturation and queueing model queries.

**ShapeTracker (`shape_tracker.go`)**  
Maintains the current workload shape bucket `(IL, OL, H%, IL_eff, KVreq)`. Detects shape changes
(>20% shift in IL or OL) and triggers observation window reset.

- `IL_eff = IL × (1 − H%)` — effective input length after prefix cache
- `KVreq = IL_eff + OL/2` — time-averaged KV footprint per decode request

**Shape dimensions — design note:**  
The tracker stores IL, OL, and H% but change detection uses only IL and OL (see `Within()`).
H% is stored because it feeds IL_eff and KVreq; a H%-only change does not reset the window.

The minimal sufficient representation for KVreq is `(OL, IL_eff)` rather than `(IL, OL, H%)`
separately — we track all three because it is not yet clear whether IL and H% should be treated
as independent shape dimensions or collapsed into IL_eff alone.

Likewise, the slope A in `ITL(k) = A·k + B` may be independent of H%: A captures how quickly
decode latency grows with KV load, which is driven by hardware and concurrency, not by the
fraction of input tokens served from cache. We do not have enough data to confirm this yet.
If it holds, a H%-only shift would warrant updating only B (via a new Tier-2 constrained fit)
while carrying over the previous A — avoiding a full window reset.  
*(This is a potential future optimization; the current implementation resets nothing on H%-only changes.)*

**ObservationWindow (`observation_window.go`)**  
Rolling window of `(k*, ITL_obs)` pairs collected per replica per cycle. Filters observations
to `k ∈ [0.15, 0.85]` (reliable linear-model range). Reports `Ready()` when ≥ 10 samples with
≥ 0.30 k-spread are accumulated within the 30-minute default window.

**ITLModel (`itl_model.go`)**  
Two-tier calibration of `ITL(k) = A·k + B`. See [ITL Model Calibration](#itl-model-calibration).

**ThroughputAnalyzer (`analyzer.go`)**  
Implements `interfaces.Analyzer`. Groups replicas by `VariantName`, runs sanity checks,
updates per-variant shape tracker and observation window in `Observe()`, then computes
supply, demand, and model-level RC/SC signals in `Analyze()`.

### State and High Availability

`ThroughputAnalyzer` is stateful across reconcile cycles: it accumulates `(k*, ITL)` observations until the window is ready to fit the ITL model. The state is **in-memory only** — a `map[string]*variantState` held inside the analyzer instance, with no persistence to etcd or Kubernetes.

Per-variant state is minimal:

| Field | What it holds |
|---|---|
| `ShapeTracker` | Current workload shape snapshot (IL, OL, hit rate); overwritten each cycle |
| `ObservationWindow` | Rolling slice of ≤ 20 `(k*, ITL_obs)` pairs + timestamps |
| `lastSanityReport` | Most recent sanity check result |
| `lastObservedAt` | Timestamp of last observation |

**In HA mode**, the engine reconciliation loops run only on the elected leader (gated in `main.go`). The `ThroughputAnalyzer` instance lives inside that loop — state is local to the leader process and is never shared across replicas.

On leader failover the incoming leader starts with an empty analyzer. During warm-up (until the observation window re-accumulates ≥ 10 samples with ≥ 0.30 k-spread), the TA emits no scaling signal (`RC = 0, SC = 0`). The saturation analyzer runs unaffected and provides coverage throughout. Warm-up completes within a few minutes at normal traffic levels.

**No external state store is needed.** State loss on failover is equivalent to a workload shape change (which already clears the window by design). The gap is bounded and temporary; adding a ConfigMap or lease annotation to persist calibration state would not be worth the added complexity at this stage.

### Analysis Pipeline

```
  ┌──────────────────── Per-Variant Processing (each variant v) ──────────────────┐
  │                                                                               │
  │  []ReplicaMetrics (replicas of variant v)                                     │
  │        │                                                                      │
  │        ├─(IL, OL, H%) [VLLMRequestRate-weighted]──► ShapeTracker              │
  │        │                                               │                      │
  │        │                                         KVreq, IL_eff                │
  │        │                                         shape change──► Window.Clear │
  │        │                                                                      │
  │        ├─(k*, ITL_obs per replica)──────────────► ObservationWindow           │
  │        │                                               │                      │
  │        │                                        Ready? yes──► OLS fit         │
  │        │                                               │    no──► constrained │
  │        │                                               └──► ITLModel{A, B}    │
  │        │                                                         │            │
  │        ├─(KV_max)─────────────────────────────► computeVariantSupply          │
  │        │                                        [ITL(k_sat) = A·k_sat + B]    │
  │        │                                        → μ_dec_sat, perReplicaSupply │
  │        │                                                                      │
  │        └─(ArrivalRate / VLLMRequestRate)─────► computeDemand                  │
  │                                                 → λ_dec, isEPP                │
  └──────────────────────────────────┬────────────────────────────────────────────┘
                                     │ per-variant outputs accumulated
  ┌──────────────────────────────────▼────────────────────────────────────────────┐
  │  Model-Level Aggregation                                                      │
  │                                                                               │
  │  totalSupply      = Σ μ_dec_sat                                               │
  │  totalDemand      = Σ λ_dec  +  QueueSize / (QueueDrainFactor × ITL(k_sat))   │
  │  totalAnticipated = Σ (current + pending) × perReplicaSupply                  │
  │                                                                               │
  │  RequiredCapacity = max(0, totalDemand − totalAnticipated)                    │
  │  SpareCapacity    = max(0, totalSupply  − totalDemand)    [if anyEPP]         │
  │  RoleCapacities                                           [if P/D roles]      │
  └───────────────────────────────────────────────────────────────────────────────┘
```

### Data Flow

```
┌────────────┐
│ Prometheus │
└──────┬─────┘
       │ vllm:request_generation_tokens_sum      (QueryGenerationTokenRate   → GenerationTokenRate)
       │ vllm:kv_cache_usage_perc                (QueryKvUsageInstant          → KvUsageInstant)
       │ vllm:request_generation_tokens_count    (QueryVLLMRequestRate       → VLLMRequestRate)
       │ vllm:cache_config_info                  (QueryCacheConfigInfo       → TotalKvCapacityTokens)
       │ vllm:inter_token_latency_seconds_*      (QueryAvgITL               → AvgITL)
       │ vllm:request_generation_tokens_*        (QueryAvgOutputTokens       → AvgOutputTokens)
       │ vllm:request_prompt_tokens_*            (QueryAvgInputTokens        → AvgInputTokens)
       │ vllm:prefix_cache_hits/queries          (QueryPrefixCacheHitRate    → PrefixCacheHitRate)
       │ inference_extension_scheduler_*         (QuerySchedulerDispatchRate → ArrivalRate)
       │ inference_extension_scheduler_*         (QuerySchedulerQueueSize    → QueueSize)
       ↓
┌─────────────────────────┐
│ ReplicaMetricsCollector │  ← internal/collector/replica_metrics.go
│ CollectReplicaMetrics() │     single Refresh() call, 12 queries
└──────┬──────────────────┘
       │ []interfaces.ReplicaMetrics + SchedulerQueueMetrics
       ↓
┌──────────────────────────────────────────────────────────┐
│ ThroughputAnalyzer.Analyze()                             │
│                                                          │
│  per variant:                                            │
│    ShapeTracker → KVreq                                  │
│    ObservationWindow → (k*, ITL) pairs                   │
│    ITLModel (tier-1 OLS or tier-2 constrained)           │
│    supply: μ_dec_sat = k_sat×KV_max / KVreq / ITL(k_sat) │
│    demand: EPP primary → vLLM fallback → k*-local        │
│                                                          │
│  model-level:                                            │
│    + queue demand from QueueSize / (factor×ITL)          │
│    RC = max(0, totalDemand − totalAnticipated)           │
│    SC = max(0, totalSupply − totalDemand)  [EPP]         │
└──────┬───────────────────────────────────────────────────┘
       │ AnalyzerResult{RequiredCapacity, SpareCapacity, VariantCapacities, RoleCapacities}
       ↓
┌──────────────────────────────────────┐
│ combineAnalyzerResults()             │  ← any-up / all-down with saturation
│ (internal/engines/saturation)        │
└──────────────────┬───────────────────┘
                   │ combined AnalyzerResult
                   ↓
┌────────────────────┐
│ ScalingOptimizer   │  → VariantDecisions → Controller
└────────────────────┘
```

## ITL Model Calibration

> **Not yet implemented.**

The ITL model `ITL(k) = A·k + B` captures how inter-token latency grows with KV cache
utilization k. It is calibrated independently per variant (different hardware → different A, B).

### Tier 1 — OLS Fit

When `ObservationWindow.Ready()` is true (≥ 10 samples spanning ≥ 30% of the k range),
`FitITLModel` fits A and B by ordinary least squares, minimizing `Σ(ITL_i − A·k_i − B)²`.
The fit is accepted only when A > 0 (physically required: more concurrent requests → higher
latency). On success, the fitted model is used for both supply and demand estimation this cycle.

### Tier 2 — Constrained OLS

When the window is not ready, A is estimated with B pinned to `DefaultBaselineITLSec` (0.006 s —
H100 hardware baseline at near-zero load):

```
A = Σ((ITL_i − B) · k_i) / Σ(k_i²)
```

This is least-squares with B fixed, applied to all replicas with k* > 0. For a single replica
it reduces to the single-point formula `A = (ITL − B) / k*`. For multiple replicas it is
strictly better — same OLS criterion as tier-1 but with one fewer degree of freedom.
Accepted only when A > 0.

**Tier 3 (not yet wired):** `itlKnowledgeStore` is present in the package for a future
zero-replica fallback using the last successful tier-1 fit. It is not wired into the current
`Analyze()` loop because that loop only iterates variants with active replica metrics.

## Supply Estimation

> **Not yet implemented.**

Per replica `r`:

```
IL_eff    = AvgInputTokens × (1 − PrefixCacheHitRate)
KVreq     = IL_eff + AvgOutputTokens / 2      # time-averaged KV footprint per request
N_dec_sat = DefaultKSat × KV_max / KVreq      # in-flight requests at k_sat
μ_dec_sat = N_dec_sat / ITL(k_sat)            # decode tokens/sec at saturation operating point
```

Per-variant totals: `totalSupply = Σ μ_dec_sat`, `perReplicaSupply = totalSupply / n`.

`DefaultKSat = 0.85` — the KV utilization at which μ_dec_sat is evaluated. This is a
per-analyzer constant pending alignment with the EPP system-wide k_sat (see open items).

## Demand Estimation

> **Not yet implemented.**

### Priority Chain

Demand is resolved in priority order per variant. The first non-zero source wins.

**1. EPP primary** (isEPP = true)  
When any replica has `ArrivalRate > 0`:
```
λ_dec = Σ_{r∈V} ArrivalRate_r × AvgOutputTokens_r
```
Each replica of variant V contributes its own arrival rate × output length. This avoids
averaging-the-averages when replicas have different throughput.

**2. vLLM fallback** (isEPP = false)  
When EPP is absent but `VLLMRequestRate > 0`:
```
λ_dec = Σ_{r∈V} VLLMRequestRate_r × AvgOutputTokens_r
```
Same structure as primary but using the vLLM-side completion rate. SpareCapacity (scale-down)
is suppressed when isEPP is false — the vLLM rate only counts served requests, not arriving ones.

**3. k\*-based local** (scale-up only)  
When both EPP and vLLM rates are zero, demand is derived from the current KV utilization:
```
λ_local = Σ_{r∈V}  k_r* × KV_max_r / KVreq / ITL(k_r*)
```
Each replica's in-flight request count `N_r = k_r* × KV_max / KVreq` is divided by `ITL(k_r*)`
to approximate its current throughput. Scale-down is still gated on EPP when this path is used.

### Scheduler Queue Demand

After all per-variant contributions are summed, scheduler queue demand is added to model-level
`totalDemand` (non-prefill roles only):

```
avgDecodeITLSat  = mean(ITL(k_sat)) over decode/both variants
queueDemand      = QueueSize / (DefaultQueueDrainFactor × avgDecodeITLSat)
```

`DefaultQueueDrainFactor = 2.0` bounds per-request queueing time to
≤ 2 × ITL(k_sat) × avgOL. The output-length factor cancels in the derivation, so the result
is independent of OL.

Queue demand appears in model-level `TotalDemand` but is **not attributed to any specific
variant** — `Σ VariantCapacity.TotalDemand ≤ result.TotalDemand` when a queue is present.

## Scaling Signal

> **Not yet implemented.**

### Model-Level Aggregation

`RequiredCapacity` and `SpareCapacity` are computed from model-level totals, not accumulated
per-variant. This prevents simultaneous conflicting signals when variant A is overloaded and
variant B has spare.

```
totalAnticipated = Σ_v (current_replicas_v + pending_replicas_v) × perReplicaSupply_v
requiredCapacity = max(0, totalDemand − totalAnticipated)
spareCapacity    = max(0, totalSupply − totalDemand)   if anyEPP else 0
```

`PendingReplicas` counts replicas that have been provisioned but not yet in service. Including
them in `totalAnticipated` suppresses redundant scale-up requests while pods are starting.

By construction, `requiredCapacity` and `spareCapacity` cannot both be non-zero in the same
cycle: if demand exceeds anticipated supply then spare = max(0, supply−demand) = 0.

### Role-Aware Aggregation

Roles are read from `AnalyzerInput.VariantStates` and stored in per-variant state. All roles
use the same decode-rate framework.

- `RequiredCapacity` is **suppressed for the prefill role**: decode rate is never the bottleneck
  for a prefill-only pod. Prefill-specific rate signals (based on prefill token throughput) are
  deferred to a later PR.
- `SpareCapacity` for a role requires EPP on at least one variant of that role.
- `RoleCapacities` is nil when all variants have role `""` or `"both"` (non-disaggregated model).

## Constants and Tuning

| Constant | Default | Description |
|---|---|---|
| `DefaultKSat` | 0.85 | KV utilization at which μ_dec_sat is evaluated |
| `DefaultBaselineITLSec` | 0.006 | B in tier-2 ITL model (H100 near-zero-load baseline) |
| `DefaultQueueDrainFactor` | 2.0 | Bounds queueing time to ≤ factor × ITL(k_sat) × OL |
| `DefaultWindowMaxSize` | 20 | Max (k*, ITL) pairs in ObservationWindow |
| `DefaultObservationMaxAge` | 30m | Observations older than this are pruned |
| `DefaultMinSamples` | 10 | Minimum samples for OLS Ready flag |
| `DefaultMinKSpread` | 0.30 | Minimum k-spread for OLS Ready flag |
| `DefaultMinObservableK` | 0.15 | Lower k* filter for ObservationWindow |
| `DefaultMaxObservableK` | 0.85 | Upper k* filter for ObservationWindow |
| `DefaultShapeChangeTolerance` | 0.20 | IL or OL shift that triggers window reset |

**Open items:**
- `DefaultKSat = 0.85` is per-analyzer; needs alignment with EPP system-wide k_sat
- `DefaultBaselineITLSec = 0.006` is H100-specific; may need hardware-aware defaults

## References

- Related: [Saturation Analyzer](../user-guide/saturation-analyzer.md)
- Design: `ideas/TA-Plan.md`, `ideas/TA-supply.md`, `ideas/TA-demand.md`, `ideas/TA-PR4-plan.md`
