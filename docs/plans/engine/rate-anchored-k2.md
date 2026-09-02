# Rate-anchored compute capacity (k2) — plan

## Problem

The V2 saturation analyzer models a replica's capacity as `min(k1, k2)` in KV tokens:

- `k1 = TotalKvCapacityTokens × kvCacheThreshold` — the memory bound.
- `k2` — the compute bound, currently recorded as `tokensInUse` at the moment the
  replica was seen queueing (`computeK2` priority 1), then averaged into a rolling
  history keyed by `model|accelerator|gpuCount|outputBucket`.

`k2` measures a **KV stock** while the constraint it stands for is a **rate**. On a
prefill-heavy workload the two are unrelated: the engine exhausts prompt-token
throughput and begins queueing while KV occupancy is still low. In the sustained
1000/250 comparison run the WVA leg was queueing, dropping requests, and cycling
replicas at **16.2% average KV utilization** — a regime the current model cannot
express, because at 16% occupancy `demand/supply` reads as abundant headroom.

Three consequences follow, all observed in that run:

1. **The compute bound is silently discarded.** `tokensInUse` derives from
   `kv_cache_usage` which is `max_over_time(...[1m])` — a peak. Whenever the peak
   exceeds `kvCacheThreshold` (0.80), `k2 ≥ k1` and `min(k1, k2)` returns the
   memory bound, so the compute signal never binds.
2. **Supply is over-stated exactly when load is highest**, so utilization reads low
   and `spareCapacity = totalSupply − totalDemand/scaleDownBoundary` grows.
3. **Demand deflates faster than supply.** The queue term
   (`QueueLength × (avgInput + avgOutput)`) is the dominant part of demand at peak
   and collapses to zero the moment new replicas absorb the backlog, while `k2`
   stays inflated via the historical average. The controller then sheds to one
   replica and the cycle repeats.

Tuning thresholds cannot fix this: six threshold/window/policy legs were run
against that workload and none of them broke the cycling.

## Approach

Add a second, **rate-anchored** estimator for `k2`, selected by an internal switch,
leaving the existing estimator in place and untouched when it is off.

The design separates two questions that the occupancy-based estimator conflates:

```
detector:    rates decide WHEN a replica is at its limit
measurement: tokens record WHAT that limit is
```

A replica is at its limit when it has a backlog at least `QueueLengthThreshold`
deep, or when its arrival rate has reached the service rate measured while it *was*
backlogged. At that moment its resident token count is a measurement of the limit.
That measurement is stored **per workload bucket** — model, accelerator, role,
request shape — as a running minimum, and every replica of the bucket reads the
same value.

Two properties follow, and both were learned the hard way from earlier versions of
this code:

- **The value is identical across replicas of a variant.** `aggregateByVariant`
  takes the MEDIAN of per-replica capacities. A number that varied with each
  replica's own load was not commensurable across siblings: an idle replica's
  figure blended with a backlogged one's and lifted variant capacity enough to turn
  a scale-up into a scale-down, reintroducing shed-to-one by a new route. A bucket
  ceiling makes the median a no-op.
- **The value does not move with this cycle's load.** A capacity recomputed from
  the current arrival rate changed every cycle, which is an oscillation waiting to
  happen. A stored ceiling changes only as the running minimum is lowered by a new
  measurement or relaxed upward by age — both slow by construction.

Why a limit measured at 16% KV utilization is the whole point: on a prefill-heavy
workload the engine exhausts prompt-token throughput long before memory, so the
binding constraint is invisible to an occupancy-only model. The detector notices it
from rates; the measurement records it in the units the rest of the engine speaks.

### Damping, by construction

- `SaturationEnterRatio` (0.95) means the detector fires just before arrivals cross
  the service rate, and a lambda hovering at mu does not toggle it between cycles.
- The service rate is a mean over backlogged samples with a five-minute time constant;
  the ceiling is a running miniμm with slow upward relaxation. Neither tracks a single
  cycle, and neither can move in only one direction.
- `MinServiceRateSamples` keeps one slow interval from establishing a limit.
- lambda is smoothed over a residence time, so a completion-derived rate and an
  arrival rate are compared on the same time base.

### Why not an ITL model

`ITL(k) = A·k + B` (already fitted by the throughput analyzer) describes decode
latency against KV occupancy. On prefill-heavy traffic ITL can stay flat while TTFT
and the queue explode, so it does not capture this bottleneck. It remains the better
model for decode-bound workloads; the rate anchor covers both.

## Metrics

Everything required already exists. Two of the three are registered only when the
throughput analyzer is enabled, which this plan removes as a dependency.

| Quantity | Query | Field | Today |
|---|---|---|---|
| λ arrival rate | `rate(inference_extension_scheduler_attempts_total{status="success"}[1m])` (EPP) | `ArrivalRate` | unconditional |
| μ completion rate | `rate(vllm:request_generation_tokens_count[1m])` | `RequestRate` | throughput-analyzer only |
| occupancy | `max_over_time(vllm:kv_cache_usage_perc[1m])` × capacity | `TokensInUse` | unconditional |
| queue | `max_over_time(vllm:num_requests_waiting[1m])` | `QueueLength` | unconditional |
| KV capacity | `vllm:cache_config_info` | `TotalKvCapacityTokens` | unconditional |

One new query, and it is not optional: `QueryQueueLengthInstant`, the waiting-request
count without `max_over_time`. `QueryQueueLength` is the same counter as a one-minute
peak, which the demand path wants and a capacity gate cannot use — the peak latches for
a minute after a queue drains, long enough to record a replica that is now keeping up
as though it were at its limit. The gate and the measurement have to be read at the
same instant, so both instantaneous readings are collected.

`QueryRequestRate` moves to a shared registrar called unconditionally by the saturation
engine (`QueryKvUsageInstant` moves with it, since the throughput analyzer needs it and
the registrar is shared); the throughput analyzer registers them only if absent, so
either order works and neither panics.

This is the one part of the change that is not behind the flag: with the estimator off,
the controller still issues the extra per-pod query each cycle and populates two fields
nothing reads. The cost is one range query per model per interval, and the alternative —
registering it from inside a flagged path — would mean the flag could not be flipped
without a collector restart.

SGLang equivalents (`sglang:generation_tokens_histogram_count`, `sglang:token_usage`)
are already defined and move with them.

## Work items

1. **Shared query registration** — `internal/collector/registration/rate_capacity.go`:
   single definition of the two shared templates (vLLM + SGLang) and
   `RegisterRateCapacityQueries`; `registerIfAbsent` / `registerForEngineIfAbsent`
   helpers; `RegisterThroughputAnalyzerQueries` switched to the if-absent variants.
   Called from `saturation.Engine` construction alongside `RegisterSaturationQueries`.
2. **Switch** — `EnableRateAnchoredK2`, a build-time constant in
   `saturation_v2/rate_capacity.go`. Deliberately not a ConfigMap setting: the
   estimator is under evaluation against the incumbent and is not something to
   toggle in a running cluster. With it false the service-rate store is never
   allocated and every path in the file returns immediately.
3. **Estimator** — `internal/engines/analyzers/saturation_v2/rate_capacity.go`:
   per-bucket service-rate store (running max under backlog, staleness eviction),
   bucket key extended with the **input** dimension, and `rateAnchoredK2()`.
4. **Wiring** — `computeK2` consults the rate-anchored estimator first when the flag
   is on, falling through to the existing chain when it declines to answer. New
   `k2Source` value so the active estimator is visible in logs and metrics.
5. **Tests** — store behaviour (backlog gating, max, decay, eviction), estimator
   arithmetic and clamps, flag-off equivalence with the current path.

## Arrival/completion delay

A completion happens one residence time `W` after the arrival that caused it, so a
completion-derived μ and an instantaneous λ sit on different time bases. During a
ramp, completions still reflect the lighter load of `W` ago, and μ/λ reads as
saturation on a replica that is coping. Occupancy has the same property — it is a
stock that already integrates arrivals over `W`.

λ is therefore smoothed per replica with an EWMA whose time constant is
`W = AvgTTFT + AvgOutputTokens × AvgITL`, both already collected. The weight comes
from the actual gap between samples, so an irregular optimize interval or a missed
cycle does not distort the average; an average older than
`ArrivalSmoothingResetFactor × W` is discarded rather than blended. Without latency
data the rate passes through unsmoothed rather than being averaged by a made-up
constant.

The backlog path needs none of this: it compares no rates, so it is unaffected by
the delay. That is a second reason to keep it ahead of the completions fallback.

## Detector inputs, in order

1. **Backlog** — a queue at least `QueueLengthThreshold` deep. Needs no rates at all,
   which makes it the safety net for a fleet with no EPP and no prior calibration,
   and it is exactly the prefill-heavy case the occupancy estimator misreads as idle.
   A shallower queue does not qualify: arrival jitter produces one at any load.
2. **Arrivals reaching the service rate** — λ from the EPP dispatch rate, smoothed
   over a residence time, against the bucket's μ at `SaturationEnterRatio`. This
   catches the limit before a queue forms.
3. **Completions as λ** — without EPP, and only when there is no queue, completions
   are arrivals. Invalid under backlog, which is why it sits behind that check.

The two `k2Source` labels do **not** identify which of these fired; they distinguish
a limit measured this cycle (`RATE-now`) from one carried over from an earlier one
(`RATE-learned`), which is what the offline replay needs to tell apart. A replica
declines only when nothing has been learned for its bucket and it is not at its limit
now — a state in which nothing is at risk.

## Guards

- Require `KvUsageInstant > 0` and a usable ratio; otherwise decline and fall through.
- `MinRateRatio` floors the ratio a mis-scraped rate can produce; there is no upper
  clamp, since `min(k1, k2)` already bounds the compute estimate by the memory one.
- Both stores prune themselves on insert past `BucketPruneThreshold`, because nothing
  in the engine drives eviction for the analyzer's other stores either — a store that
  relied on being swept would grow unbounded the moment the flag was flipped.
- Never exceed `k1`: `min(k1, k2)` already handles this, but the estimator also
  refuses to return a value below a floor fraction of `k1` to avoid a stalled
  replica collapsing capacity to near zero.

## Known limitations

- **EPP is no longer required.** μ comes from completions
  (`vllm:request_generation_tokens_count`), the ceiling from occupancy under backlog, and `W`
  from TTFT and ITL — all vLLM-side. The EPP dispatch rate now only distinguishes the
  `RATE-now` label from `RATE-learned`. The earlier hard dependency is gone; worth correcting
  in #1500 and #1501.
- **Needs backlog to calibrate.** A fleet that never queues never learns `μ` and
  falls back to the memory bound — acceptable, since nothing is at risk there, but
  it makes emitting `k2Source` mandatory rather than optional.
- **P/D disaggregation.** `μ` from the generation-tokens histogram is decode-centric;
  a prefill pool would need `rate(vllm:request_prompt_tokens_count[1m])` as its `μ`.
  Not addressed here.
- **Prompt-token throughput is still not collected.** Useful as a diagnostic to
  confirm the prefill-bound reading of the sustained run; not required by the
  estimator.

## Where the two estimators actually differ

Under a deep backlog they agree: the occupancy path records `TokensInUse` as k2 and
the rate path's backlog branch returns the same figure. The divergence is entirely
in the **post-drain** state — queue empty, occupancy collapsed, arrivals unchanged.
There the occupancy path answers from its inflated history and reports abundant
spare capacity (the shed-to-one), while the rate path reads λ still at the ceiling
and holds capacity at the current load. That is the behaviour the cluster legs must
confirm, and it is pinned by a test at the `computeReplicaCapacity` level.

## Round 1 result — supply was the smaller half of the problem

Measured 2026-07-31 on the sustained 1000/250 workload, two images from one commit:
the flag moved the amplitude (5 replicas at peak vs 4, 67 errors vs 132, EPP queue 2.19 vs
3.54) and left the collapse-to-one cycle intact.

A ceiling alone cannot stop that cycle. Demand is resident tokens, `λ × W × tokensPerRequest`,
so it falls when replicas are added: contention drops, residence `W` drops, and the queue
term — the dominant part at the peak — disappears outright. Supply held flat against a
shrinking demand reads as abundant spare capacity, and the fleet sheds the replicas that had
just fixed the problem. At the run's numbers: five replicas at a ~320k bound is 1.6M of
supply against ~258k of demand once the backlog cleared, so `SC = 1.6M — 258k/0.60 = 1.17M`
- over three replicas' worth of "spare", acted on in one cycle.

### The correction: capacity at the current operating point

By Little's law a replica at its limit holds `μ × W × tokensPerRequest` tokens, so that
product IS capacity in the units the engine already speaks, at whatever operating point `W`
describes:

```go
capacity = min(ceiling, μ × W × tokensPerRequest)
```

Demand is `λ × W × tokensPerRequest`, so the ratio becomes `λ / (N × μ)` — it does not
move when replicas are added, only λ per replica does. The alternative was to rewrite the
demand term as `(λ/μ) × ceiling`, the same mathematics from the other side, but that changes
a quantity the optimizer and the role-aggregation path share. This stays inside the flag.

Three properties the implementation depends on:

- **Nothing jumps when it engages.** At calibration λ = μ, so the product equals the
  occupancy that set the ceiling.
- **One direction only.** `W` comes from TTFT, which includes time queued, so a backlogged
  replica reports an inflated `W`. The clamp at the ceiling is not a safety rail — it is what
  makes the forμla correct while the queue contaminates `W`. After a drain the queue wait is
  gone, `W` is true service residence, and the scaling is exact.
- **Bucket-wide, not per-replica.** `FreezeWork` averages the previous cycle's samples across
  the bucket's replicas and publishes one value at the top of `Analyze`, so the median in
  `aggregateByVariant` stays a no-op. Averaging rather than folding each sample as it arrives
  matters: every replica reports at the same timestamp, so folding would give the first
  replica of the loop the entire weight and make capacity depend on iteration order — a
  freshly started replica with a short residence could pull the bucket down and drive a
  spurious scale-up. It costs one cycle of lag.

Three related corrections came with it:

- **Ceilings are recorded only under backlog.** Arrivals reaching the service rate still says
  the replica is at its limit — that is how the limit is caught before a queue forms — but
  with no queue, low occupancy means the replica is keeping up, not that its ceiling fell.
  Recording it ratcheted capacity down on evidence of health.
- **μ is the mean of backlogged samples, not their running maxiμm.** The ceiling is a running
  miniμm, so it errs toward less capacity and more replicas; a running maxiμm for μ erred
  the opposite way and only decayed back slowly. When prompts get heavier within a bucket the
  true μ falls, and an estimate that can only ratchet up overstates capacity and under-scales.
  Under backlog the server is never idle, so every sample is a valid reading, and their mean
  is both the better estimate and the one that moves in either direction.
- **The ceiling is measured from the instantaneous KV reading**, not `TokensInUse`
  (`max_over_time(...[1m])`). A running miniμm fed a peak is biased high in the one
  direction that costs replicas. `KvUsageInstant` is already collected; the older field
  remains the fallback.

### What this predicts for the round-1 workload

The run completed 39 600 requests (mean λ 22 req/s) on 2.09 average replicas at 0.17% errors,
so per-replica service rate is about **μ = 22/2.09 ≈ 10.5 req/s** — a throughput identity, not
an estimate. At N=2 that is ρ ≈ 1.05: throughput fine, queue unbounded, which is exactly the
measured 140-deep queue and 7.58 s TTFT p99. With `scaleUpThreshold = 0.75` the band is
`N in [λ/(0.75 μ), λ/(0.60 μ)]`, so at the sustained λ = 24 the fleet settles at
**3–4 replicas**, and shedding to one would require `λ < 0.6 μ ≈ 6.3 req/s`. The old fixed
point of 2.09 was not "enough" — it was ρ ≈ 1, reported as 12.9% utilization because a request
waiting in the queue holds no KV at all.

### Reading the limit, and reading it early

Two rules decide when a replica is at its limit, and they are read on one time base.
`QueueLength` and `TokensInUse` are collected as `max_over_time(...[1m])`, so the peak
latches for a minute; `QueryQueueLengthInstant` and `KvUsageInstant` are point samples.
Pairing a latched gate with a point sample is what let a drained replica be recorded as
its own limit, collapsing the ceiling to its floor. `limitEvidence` pairs them
explicitly — instantaneous with instantaneous, or the one-minute peak of both.

Only a replica that is both backlogged **and** completing work may define a ceiling,
and lowering one takes `MinCeilingLowerCycles` consecutive cycles of agreement. A pod
that has just become ready takes a routed burst before its cache fills; a stalled pod
queues without completing anything. Either looks backlogged at almost zero occupancy,
and since the ceiling is a minimum across the bucket, either would set capacity for
every healthy sibling.

Arrivals reaching the service rate is not a measurement of the limit — with no queue,
low occupancy means the replica is keeping up — but it is a reliable signal that the
limit has been reached. Capacity is then held at the current occupancy, so demand meets
it and the fleet scales **before** a queue forms. That matters more than it looks: a
replica takes about ninety seconds from decision to serving, so the backlog that builds
during a scale-up is set by how early the decision was made, not by how fast the loop
runs afterward.

### The bucket key is a property of the variant

The shape that keys a bucket is averaged across the variant's replicas. Replicas of one
variant serve the same traffic, so their per-replica averages differ only by sampling
noise — but that noise is enough to put two siblings either side of a threshold, where
they learn independent ceilings and service rates. `aggregateByVariant` takes the MEDIAN
of per-replica capacities, so those two figures get blended even though they measure
different things, and a variant sitting near a threshold flips its estimate every cycle
as replicas drift across it. Input and output thresholds are also separate now: prompts
run an order of magnitude longer than generations, and bucketing input at the output
thresholds put almost every real workload in one bucket.

### Residence must be service time

The operating point is `mu x W x tokensPerRequest`, and `W` has to be the time a request
spends *being served*. Reading it as `TTFT + outputTokens x ITL` does not give that:
TTFT is measured from arrival at the engine, so it carries time queued.

The validation runs made the consequence visible. `RATE-W` — the only label under which
capacity is scaled below the learned ceiling, and so the only mechanism that can prevent
the post-drain shed — fired **once in thirty-five minutes** on prefill-heavy traffic
against **twenty-seven times** on symmetric traffic in the same build. Under backlog the
inflated `W` pushes `mu x W x tokensPerRequest` past the ceiling, the clamp holds capacity
there, and the ceiling on that workload is measured while a one or two replica fleet sits
at near-full KV — so it lands at the memory bound, which is what the occupancy path
already returns. ON and OFF producing the same numbers follows from that.

The decode half was never contaminated: inter-token latency contains no queue wait. Only
prefill is, and it can be measured directly, because during a cycle with nothing queued
TTFT *is* prefill. Buckets learn it then and reuse it when the queue is deep, retaining it
for the bucket's lifetime rather than the service-rate window — prefill is a property of
model, hardware and prompt length, all of which are in the bucket key, and it can only be
observed in the unqueued cycles a busy fleet does not have.

Where vLLM publishes `request_inference_time_seconds` (time in the RUNNING phase, queue
wait already excluded) that is preferred outright, since the derived prefill is sampled
uncontended while real prefill grows under load. SGLang publishes no equivalent among its
seventeen metrics, so it takes the derived path, which needs nothing not already
collected.

Worth knowing where the derived form is weakest: long prompts with short generations,
where prefill dominates residence rather than being a couple of per cent of it. It
understates residence there, therefore understates capacity, therefore asks for more
replicas rather than fewer — the tolerable direction, and the case the vLLM metric covers.

### The scale-down floor

Capacity estimation alone cannot make a scale-down safe, because the decision is a
counterfactual — would `N−1` replicas still cope? — and the token-space figures cannot
answer it. Demand in resident tokens is measured at the current replica count and does
not survive the removal being evaluated: residence rises, occupancy per replica rises
with it, and past a point a queue appears from nothing. Arrivals are the one quantity
the decision does not move, so the same question asked in rate space has an answer that
holds.

The constraint is on the aggregate service rate within a role, not on replicas within a
variant:

```
sum over v in role of (N_v x mu_v)  >=  lambda_role / scaleDownBoundary
```

Per-variant would be wrong: shedding from one variant re-routes its traffic to a
sibling, so a variant's own arrival rate is itself a function of the decision. Only the
role's total stream is invariant. Heterogeneous accelerators fall out naturally, since
`mu_v` is per bucket and the bucket key already carries accelerator and GPU count.

The optimizer enforces it in `scaleDownVariantSet`, which is already role-scoped, and
only bounds *how many* replicas may go — which variant gives one up is still the cost
ordering's decision. One consequence worth expecting: when the expensive variant's
replicas are individually large in service-rate terms, the floor can hold them and shed
a cheap one instead. That is correct. Capacity you need cannot be removed merely
because it is the capacity you would rather not pay for.

If any variant holding replicas in a role has no calibrated `mu`, the role's available
side is understated while its arrivals still count, which would hold replicas that are
not needed. Partial knowledge is therefore treated as no knowledge and the constraint is
skipped.

GPU rebalancing overrides the floor. A reclaim happens because another model needs the
GPUs more, and a variant refusing to give one up because its own traffic wants it is
precisely the argument prioritisation exists to settle. The floor is a safety net for a
scale-down nobody asked for, not a veto over one that was — so `reclaimRole` passes
`overrideRateFloor` and the routine optimizer passes `honorRateFloor`.

For P/D, both roles see the same request stream and each gets its own constraint. A
prefill replica completes few or no generations, so its service rate comes from prompts
processed (`vllm:request_prompt_tokens_count`, `sglang:prompt_tokens_histogram_count`)
rather than from the generation-tokens counter.

### Residual risks

- **Mixed time bases.** Demand still uses `TokensInUse`, a 1-minute peak, while capacity uses
  1-minute average TTFT/ITL. The two `W`s do not cancel exactly and the ratio is biased high
  — toward more replicas, so safe for this failure, but systematic. Changing the demand term
  is outside the flag and was deliberately left alone.
- **The band is only 1.25× wide.** With integer `N` and a noisy λ, a one-replica flap is
  still possible. Stabilization is not obviated by this fix.
- **A wrong μ cannot be rescued by any of this**, which is why the instrumentation below
  blocks the next leg.

## Validation

0. **Instrumentation, and it blocks everything else.** Round 1 shipped without emitting
   `k2Source`, μ, λ or the learned ceiling, which is why its result had to be reconstructed
   from replica timelines. Emit them per variant and role, along with replica demand split
   into its occupancy, local-queue and rate components, before any further leg.
1. **Offline** — replay the recorded metrics from the sustained 1000/250 run through
   both estimators and compare `k2` against replica count and queue depth. The
   prediction: only the operating-point capacity holds its replica count through a queue
   drain; the occupancy estimator plateaus high after each one.

   Success criteria, fixed in advance: **zero shed-to-one events while λ ≥ 20 req/s** is
   primary. Average replica count is not, and is expected to rise toward 3–4.
2. **Cluster** — three legs, KEDA `scaleDown` restored to the shipped
   `Percent 100 / 15s` so the drain cap cannot mask the result, controller restarted
   between legs:
   - sustained 1000/250, flag on — expect the two overshoot-correct cycles to
     disappear;
   - sustained 1000/250, flag off — baseline on the same cluster state;
   - **300/300 steady, flag on — regression control.** A more conservative capacity
     estimate is exactly what could turn "correctly holds at one replica" into
     spurious scale-up. This leg must stay flat at 1 with zero errors.
