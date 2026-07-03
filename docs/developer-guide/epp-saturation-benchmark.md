# EPP-saturation analyzer — benchmark (Prefill-Heavy, active scaling)

An end-to-end evaluation of the `epp-saturation` analyzer driving **active**
autoscaling on GKE, against the canonical Prefill-Heavy workload — eleven scored
runs across two clusters, converging on a validated configuration and a set of
measured findings about reactive latency-signal autoscaling. Complements RFC
([#1018](https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues/1018))
and the metrics in [analyzer-checklists.md](analyzer-checklists.md).

> **Status: validated proof-of-concept, not a production sign-off.** One workload
> shape, one model, H100-class GPUs. See [Caveats](#caveats).

## Setup

| | |
|---|---|
| Clusters / GPU | GKE, 3× a3-highgpu (8× H100 80 GB each); benchmark reproduced on a second identical cluster after spot preemption |
| Model | `Qwen/Qwen3-32B` (TP=2, 2 GPU / replica) |
| Serving | llm-d EPP (predicted-latency plugins + online-trained latency-predictor sidecars), standalone routing |
| SLO | TTFT ≤ 3000 ms, TPOT ≤ 100 ms (per-request via `x-llm-d-slo-*-ms` headers) |
| Signal | `saturation = max(P90 predTTFT / TTFT_SLO, P90 predTPOT / TPOT_SLO)`, 1-min window, clamped at `saturationCap`, EMA-smoothed |
| Scaler | WVA `epp-saturation` → `wva_desired_replicas` → prometheus-adapter → external-metric HPA → decode Deployment |
| Bounds | minReplicas 3, maxReplicas 8 (= measured peak demand) |
| Load | inference-perf, Prefill-Heavy **4000 in / 1000 out**, staged ramp **2→4→6→8→10→4→1 rps**, ~42 min, warm-start protocol |

## Headline result

WVA (default configuration, calibrated predictor) vs a peak-sized static pool:

| | WVA | static-at-peak (8) |
|---|---|---|
| **TTFT attainment (≤ 3 s)** | **95.9 %** | 100 % |
| **Combined SLO attainment** | **95.8 %** | 100 % |
| TTFT violations (of 12 480 requests) | **515** | 0 |
| **Avg replicas — full episode** | **5.93** | 8 |
| Avg replicas — 42-min load window | 6.55 | 8 |
| Cost vs static-at-peak | **~26 % fewer** GPU-replica-hours | — |

WVA trades ~4 SLO points for ~26 % lower cost. Client-side per-stage
percentiles confirm the residual violations sit in one shallow transient at the
rate-6 crossing (stage p90 = 5.1 s); the **rate-10 peak stage ran cleaner than
the idle stages** (p90 = 0.59 s) because capacity was in place before it
arrived.

> **Note — earlier defaults:** before the threshold band was aligned to the P90
> signal, the analyzer's defaults (`0.85 / 0.50`, α 0.3, mean-based signal)
> measured **84.7 % at ~5.9** on this profile — the trigger fired past the
> queueing knee and every scale-up paid the full transient. Those runs are
> retained in the experiment frontier below; the aligned values are now the
> code defaults.

## The aligned configuration

Every knob exists to remove one measured term of the scale-up lag:

| knob | value | removes |
|---|---|---|
| signal = **P90** predicted latency (not mean) | built-in | signal blindness below the knee — the mean stays flat at any healthy utilization; the tail rises as queueing variance appears |
| `scaleUpThreshold` | **0.55** (code default) | trigger lag — fires on the P90 pre-rise instead of after the SLO budget is nearly burned (0.85 × SLO sits past the knee) |
| `scaleDownBoundary` | **0.40** (code default) | lull drain — healthy loaded pools read P90 ≈ 0.35–0.45, so they retain replicas through lulls; idle pools (≈ 0.1) still shed |
| `smoothingAlpha` | **0.6** (code default) | ask lag — EMA reaches the clamp in 1–2 cycles (safe because `saturationCap` bounds spikes; that is the clamp's job) |
| `saturationCap` | 2.0 (code default) | EMA poisoning — knee signals reach 10–90× SLO; magnitude above ~2× carries no information and delays recovery |
| HPA scaleUp | `Max(100 %, 4 pods)/60 s` (operator-side) | grant lag — the whole ask lands in one step; pods warm in parallel |
| vLLM compile-cache on node-local hostPath + 5 s startupProbe (operator-side) | deployment | warmup — pod-ready ~100 s instead of ~2 m 15 s |

Measured at the knee: trigger → full capacity in **~2 minutes** (previous
configurations: 4–6 min), and the raw signal peaked at **2.5** versus 9–36 in
every earlier run — the knee was caught at its base.

The signal-side values are the analyzer's **code defaults**; the HPA policy and
the warmup fixes live in operator-owned objects and are documented here as the
reference deployment. The threshold band is calibrated to the P90 signal's
measured healthy range on this workload — a workload whose SLO sits close to
its base latency shifts that band upward and should raise the thresholds
accordingly.

## Predictor calibration is part of the control loop

The latency predictor trains online, and its calibration state materially
changes autoscaling quality — measured directly by running the identical
aligned configuration twice on a fresh cluster:

| | cold predictor (first ~40 min of traffic) | calibrated predictor |
|---|---|---|
| Combined attainment | 85.6 % | **95.8 %** |
| Cost (42-min window) | 6.51 | 6.55 |

Ten attainment points from calibration alone. Mechanism: the signal levels sit
within ±0.05 of the retention boundary, and a miscalibrated predictor puts them
on the wrong side (the pool drained to 3 before the ramp instead of holding 4).
Operationally: **after an EPP/predictor restart, expect degraded scaling
decisions until the predictor has seen representative traffic**, and treat a
persistent calibration shift as a reason to re-check the threshold band.

Historical note: an early benchmark measured ~95 % on what later proved to be a
fresh, *pessimistic* predictor plus an unclamped EMA — an accidental
early-warning signal. That result was initially irreproducible after the
predictor calibrated and the clamp landed. The aligned configuration reproduces
it legitimately: P90 supplies the early warning by statistics instead of by
miscalibration. Predicted-vs-actual TTFT accuracy on the calibrated predictor:
Pearson r = 0.994.

## Full experiment frontier

All runs: same profile, same SLOs, warm-start protocol unless noted.

| configuration | attainment | cost | takeaway |
|---|---|---|---|
| **aligned (P90, 0.55/0.40, α 0.6, burst, fast warmup)** | **95.8 %** | **5.93 (episode)** | headline; knee caught at its base |
| aligned, cold predictor | 85.6 % | 6.51 | calibration is part of the loop |
| previous defaults (mean signal, 0.85/0.50, α 0.3, +1 pod/min) | 84.7 % | 5.89 | best of the pre-P90 configs |
| previous defaults, cold start (3) | 83.8 % | 4.92 | cheapest; pays full knee |
| P90 signal alone (0.85 threshold) | 85.1 % | ~5.9 | early trigger wasted without the rest of the pipeline |
| composition (P90 + α 0.6 + burst, 0.85 threshold) | 81.0 % | 5.77 | pipeline fast, trigger ignored the pre-rise for 6 min |
| burst grants alone (cap 12) | 84.2 % | 7.14 | grants aren't the bottleneck; ask is signal-gated |
| below-knee thresholds on the **mean** signal (0.4/0.25) | 80.2 % | 6.75 | mean has no pre-knee band; knee moved to peak rate |
| clamp + warmup-hold (step load, micro-benchmark) | — | — | hold cuts peak 11→8 on steps but **falls behind ramps** (79 % scored) — shipped default-off |

## Findings

1. **The knee is a step function, and all violations live in one transient.**
   Continuous batching absorbs load with almost no latency movement until
   ρ ≈ 1 (measured per-pod capacity ≈ 1.4–1.5 rps for this workload), then the
   TTFT-vs-load curve goes vertical. Every configuration's violations sat in a
   single window where a load step crossed the drained pool's capacity.
2. **The violation bill is structural:** `(signal lag + ask lag + grant lag +
   warmup) × arrival rate at the crossing stage`. Each term was attacked
   separately and each fix alone was worth ≤ 1–2 min; only the composition,
   triggered early enough, collapsed the window.
3. **A mean-latency signal is a trailing indicator by construction** — the
   queue must exist before the mean rises. The P90 rises with queueing
   variance, before the mean, providing the only config-reachable early
   warning. Leading indicators (queue depth, running-request count) would fire
   earlier still — top follow-up.
4. **Threshold placement must match the signal's dynamic range.** The healthy
   band of the P90 signal is ≈ 0.2–0.45 of a 3 s SLO; scale-up at 0.85 means
   "act after the cliff". The same 0.4-style threshold on the *mean* signal
   fails (finding 3): alignment is signal-specific.
5. **Faster HPA grants alone change nothing** (84.2 % vs 84.7 %): the ask is
   EMA/credit-gated, so grants were never the bottleneck. Symmetrically, extra
   `maxReplicas` headroom is wasted under a trailing signal — capacity beyond
   peak-size cannot arrive in time to matter. Size `maxReplicas` at measured
   peak demand.
6. **HPA scale-down stabilization windows operate on recommendation history**,
   not replica history — an idle-started pool gets no protection from them, and
   lull retention is better expressed in the signal boundary (`scaleDownBoundary`
   vs the healthy-load floor) than in HPA behavior.
7. **Warmup decomposition** (measured): ~65 s process/CUDA/NCCL init + 11 s
   weights + 33 s torch.compile + 8 s graph capture + up-to-30 s probe lag.
   The compile cache is container-ephemeral by default and vLLM writes it under
   `$HOME` — mount a node-local hostPath at `VLLM_CACHE_ROOT` and tighten the
   startupProbe to 5 s: pod-ready ~100 s (first pod per node still pays full
   compile).
8. **Ramp-rate perspective:** this profile's slope (+0.33 rps/min) is 4× slower
   than the pool's reactive capacity growth (~1.45 rps/min once triggered). The
   danger is not the slope but discontinuous steps landing on a drained pool.
   Smooth ramps are easy; retry storms and launch spikes need floors or warm
   pools, not better reactivity.

## Caveats

1. One workload shape / model / GPU class (Prefill-Heavy 4000/1000, Qwen3-32B,
   H100 TP=2); the threshold band (0.55/0.40) is calibrated to this signal's
   measured healthy floor and should be re-derived per workload.
2. The predictor-calibration sensitivity (finding above) means results depend
   on predictor training state; the headline uses a calibrated predictor.
3. Violations scored from EPP-side counters (`llm_d_epp_request_slo_violation_total`);
   denominators cross-checked against the load generator's own report (exact
   match, 12 480). Client-side per-request scoring (`per_request: true`) is a
   follow-up.
4. Scrape loss aliases as idle (empty query → 0 saturation, by design for
   idle pools) — a broken scrape under load scales toward minReplicas with
   `MetricsAvailable=True`. An `up{}` guard is deferred.
5. Multi-variant model groups report `ScalingCapped=False` rather than Unknown
   (cap detection is single-variant only today).

## Reproducing

Raw time series: [`assets/epp-saturation/aligned-run-timeseries.csv`](assets/epp-saturation/aligned-run-timeseries.csv)
(headline run) and [`assets/epp-saturation/previous-defaults-run-timeseries.csv`](assets/epp-saturation/previous-defaults-run-timeseries.csv)
(columns: ts, sat_raw, sat_smoothed, wva_desired, current_replicas, predTTFT_s, actTTFT_s, actTPOT_s, …).

- **Attainment:** snapshot `sum(llm_d_epp_request_slo_violation_total{type=…})`
  and `sum(llm_d_epp_request_total)` at run start and end; violations from the
  counter delta, denominator from the load generator's own report
  (`request_lifecycle` summary). Cross-check with
  `increase(..._bucket{le="3.0"}[window])` — note Prometheus stores `le="3.0"`.
- **Cost:** report both the fixed 42-min-window average (cross-run comparable)
  and `avg_over_time(wva_current_replicas[<episode>])` over the exact job
  start→completion window (absolute). The load generator drains between stages,
  so episodes run ~15–20 min past the nominal profile length.
- **Warm-start protocol:** pin HPA `minReplicas` to the peak count, wait for
  Ready, launch the load, then restore — an active autoscaler immediately
  drains a manually-scaled idle pool, so plain `kubectl scale` does not hold.
- **Predictor burn-in:** after any EPP/predictor restart, run the full profile
  once un-scored before collecting results.

## Future work

- **Leading-indicator blend:** add queue-depth / running-request signals
  (already exported by the EPP and vLLM) alongside the P90 latency signal —
  fires before any latency moves; the measured path to ≥ 98 % on step loads.
- **Sleep-mode warm pool:** vLLM level-1 sleep (weights in CPU RAM, wake in
  seconds) turns warmup ~0; needs a sleep/wake orchestrator and sleep-aware
  readiness so the EPP excludes sleeping pods (`VLLM_SERVER_DEV_MODE` endpoints).
- Client-side per-request SLO scoring; additional workload shapes; multi-variant
  cap semantics; scrape-health guard.
