# Analyzer checklists

This document defines the benchmark-based checklists that will enable a analyzer to be graduated. The new analyzer could be expected to work with existing analyzers in turn provide value to WVA by improving latency or cost or both. the **current default analyzer's** results recorded in [`docs/benchmark.md`](../benchmark.md) in general the expectation is that the new analyzer(s) should improve over the reported baselines for specific scenarion(s) that the analyzer targets.

## Reference Workloads

Every candidate analyzer in WVA must be periodically benchmarked against scenario(s) which it plan's to improve. Benchmarking should be done using llmd components by installing Gateway and the llmd request scheduler plugins with flow controller enabled on GPU cluster. below are few sample current scenarios.

| Scenario | Input Tokens | Output Tokens | Request Rate | Duration |
|---|---|---|---|---|
| **Prefill-heavy** | 4000 | 1000 |  depends on analyzer | depends on analyzer |
| **Decode-heavy** | 1000 | 4000 |   depends on analyzer | depends on analyzer |
| **Symmetrical** | 1000 | 1000 |    depends on analyzer | depends on analyzer |

## Reference Environment

Benchmark results are comparable when run under identical conditions, we currently run benchmark on below profile, for comparison it is better to use the same hardware profile:

- **Hardware**: NVIDIA H100 (OpenShift cluster)
- **Model**: The model specified in `docs/benchmark.md` (currently Qwen/Qwen3-32B)
- **Load generator**: GuideLLM or inference-perf 
- **HPA settings**: As documented in the HPA Configuration section of `docs/benchmark.md`
- **SLO targets**: Every run must declare the latency SLOs it is evaluated against —
  a TTFT SLO and a TPOT/ITL SLO (e.g. streaming: TTFT ≤ 3s, TPOT ≤ 100ms). SLO-based
  analyzers consume these targets directly (`saturation = predictedLatency / SLO`), so
  the targets must be fixed and recorded for a run to be meaningful or comparable.
  The targets are set in the WVA saturation-scaling ConfigMap
  (`ttftSLOMs` / `tpotSLOMs` — see
  [`deploy/configmap-saturation-scaling.yaml`](../../deploy/configmap-saturation-scaling.yaml));
  the load generator should score against the same values.


## Measured Metrics

The following metrics are collected for each benchmark run:

| Metric | What It Measures |
|---|---|
| **Combined SLO attainment %** | Fraction of requests meeting **both** the TTFT and TPOT/ITL SLO. For SLO-based analyzers this is the primary pass/fail metric — raw percentiles below are diagnostic context for it. |
| **P9x TTFT** (ms) | Worst-case time to first token — user-perceived responsiveness |
| **P9x ITL** (ms/token) | Worst-case inter-token latency — streaming quality |
| **Error rate** | Fraction of requests that failed (timeouts, 5xx, etc.) |
| **Avg replicas** | Mean replica count over the run — proxy for cost |
| **Max replicas** | Peak replica count reached — headroom usage |
| **Avg KV cache utilization** | Mean KV cache pressure — how close to memory saturation |
| **Avg queue depth** | Mean EPP queue depth — scaling responsiveness indicator |

**Combined SLO attainment %** is computed per request from the load generator's
per-request latencies (e.g. GuideLLM `results.json`): a request passes when
`TTFT ≤ TTFT_SLO` **and** `TPOT ≤ TPOT_SLO`; the metric is the passing fraction over the
run. Because the pool saturation signal is `max(predictedTTFT/TTFT_SLO, predictedTPOT/TPOT_SLO)`,
attainment is the most direct check that the analyzer scaled to defend the binding SLO.
A new SLO-based analyzer is expected to improve combined attainment at equal-or-lower cost
versus the baseline, or hold attainment at lower cost, for the scenario it targets.


## Recording Results

All benchmark results must be added to [`docs/benchmark.md`](../benchmark.md) in a new section following the existing format:

```markdown
<Scenario Name>

**llm-d Release:** <version>
**Model:** <model name>
**Workload:** <input tokens>, <output tokens>, <RPS>, <duration>
**SLO targets:** TTFT ≤ <value>, TPOT/ITL ≤ <value>
**Saturation Engine:** <analyzer name and version>

| Metric | <Analyzer A> | <Analyzer B> |
|--------|-------------|-------------|
| Combined SLO attainment % | ... | ... |
| P9x TTFT (ms) | ... | ... |
| P9x ITL (ms/token) | ... | ... |
| Avg replicas | ... | ... |
| Max replicas | ... | ... |
| Avg KV cache utilization | ... | ... |
| Avg queue depth (EPP) | ... | ... |
| Error count | ... | ... |
| Cost (avg replicas × GPU/hr) | ... | ... |
```

Include the analyzer's configuration parameters (thresholds, tuning knobs) alongside the results so that others can reproduce the run with clear instructions in developer docs.
