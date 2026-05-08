# Benchmark Results

Summary of WVA benchmark runs with configuration details. 

## Environment

| Component | Version / Detail |
|-----------|-----------------|
| **Hardware** | NVIDIA H100 (OpenShift cluster) |
| **Load Generator** | GuideLLM (Poisson profile) |

## EPP Configuration

| Parameter | Default Value | Tuned Value |
|-----------|---------------|-------------|
| Scorer weights | queue=2, kv-cache=2, prefix-cache=3 | TBD |
| Feature gates | flowControl | TBD |

## WVA Configuration

| Parameter | Default | Tuned (prefill heavy) | Tuned (decode heavy) |
|-----------|---------|----------------------|-----------------------|
| **v1 Saturation (spare-based)** | | | |
| KV cache threshold | 0.80 | 0.90 | 0.75 |
| Queue length threshold | 5 | 10 | 3 |
| KV spare trigger | 0.10 | 0.05 | 0.15 |
| Queue spare trigger | 3 | 2 | 5 |
| Enable limiter | false | false | NA |
| Cost factor | 10.0 | 10.0 | 10.0 |
| **v2 Saturation (token-based)** | | | |
| Scale-up threshold | 0.85 | _TBD_ | _TBD_ |
| Scale-down boundary | 0.70 | _TBD_ | _TBD_ |
| Priority | 1.0 | _TBD_ | _TBD_ |
| Analyzer name | saturation | _TBD_ | _TBD_ |
| Analyzer score | 1.0 | _TBD_ | _TBD_ |
| Enable limiter | false | _TBD_ | _TBD_ |
| Cost factor | 10.0 | _TBD_ | _TBD_ |

## HPA Configuration

| Parameter | Value |
|-----------|-------|
| Min replicas | 1 |
| Max replicas | 10 |
| Scale-up stabilization | 0s |
| Scale-up policy | 10 Pods / 150s |
| Scale-down stabilization | 240s |
| Scale-down policy | 10 Pods / 150s |
| Metric source | External (`wva_desired_replicas`) |

## Prefill Heavy Scenario

**llm-d Release:** v0.6.0
**Model:** unsloth/Meta-Llama-3.1-8B
**Workload:** 4000 prompt tokens, 1000 output tokens, 20 RPS, 600s duration
**Saturation Engine:** Default(v1), Tuned(v1)

| Metric | WVA v0.6.0 Default(v1) | WVA v0.6.0 Tuned(v1) (prefill) |
|--------|----------------|------------------------|
| P99 TTFT (ms) | 64,968 | _TBD_ |
| P99 ITL (ms) | 68.45 | _TBD_ |
| Avg replicas | 1.91 | _TBD_ |
| Max replicas | 2 | _TBD_ |
| Avg KV cache utilization | 0.4307 | _TBD_ |
| Avg queue depth (EPP) | 158.36 | _TBD_ |
| Error count | 361 (510 incomplete) | _TBD_ |
| Cost (avg replicas × GPU/hr) | 1.91 | _TBD_ |

## Decode Heavy Scenario

**llm-d Release:** v0.6.0
**Model:** unsloth/Meta-Llama-3.1-8B
**Workload:** 1000 prompt tokens, 4000 output tokens, 20 RPS, 600s duration
**Saturation Engine:** Default(v1), Tuned(v1)

| Metric |WVA v0.6.0 Default(v1) | WVA v0.6.0 Tuned(v1) (decode) |
|--------|----------------|------------------------|
| P99 TTFT (ms) | 41,361 | _TBD_ |
| P99 ITL (ms) | 38.03 | _TBD_ |
| Avg replicas | 1.92 | _TBD_ |
| Max replicas | 2 | _TBD_ |
| Avg KV cache utilization | 0.5155 | _TBD_ |
| Avg queue depth (EPP) | 34.25 | _TBD_ |
| Error count | 192 (511 incomplete) | _TBD_ |
| Cost (avg replicas × GPU/hr) | 1.92 | _TBD_ |

## Symmetrical Scenario

**llm-d Release:** v0.6.0
**Model:** unsloth/Meta-Llama-3.1-8B
**Workload:** 1000 prompt tokens, 1000 output tokens, 20 RPS, 600s duration
**Saturation Engine:** Default(v1)

| Metric | WVA v0.6.0 Default(v1) |
|--------|----------------|
| P99 TTFT (ms) | 2,188 |
| P99 ITL (ms) | 35.09 |
| Avg replicas | 1.92 |
| Max replicas | 2 |
| Avg KV cache utilization | 0.2110 |
| Avg queue depth (EPP) | 4.38 |
| Error count | 0 (343 incomplete) |
| Cost (avg replicas × GPU/hr) | 1.92 |
