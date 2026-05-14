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
**Model:** Qwen/Qwen3-32B
**Workload:** 4000 prompt tokens, 1000 output tokens, 20 RPS, 600s duration
**Saturation Engine:** Default(v1), Tuned(v1)

| Metric | WVA v0.6.0 Default(v1) Run 1 | WVA v0.6.0 Default(v1) Run 2 | WVA v0.6.0 Default(v1) Run 3 | Avg | WVA v0.6.0 Tuned(v1) (prefill) |
|--------|------------------------------|------------------------------|------------------------------|-----|--------------------------------|
| P99 TTFT (ms) | 98,810 | 97,811 | 98,638 | 98,420 | _TBD_ |
| P99 ITL (ms/token) | 55.06 | 54.4 | 54.98 | 54.8 | _TBD_ |
| Avg replicas | 1.68 | 1.77 | 1.73 | 1.73 | _TBD_ |
| Max replicas | 3 | 3 | 3 | 3 | _TBD_ |
| Avg KV cache utilization | 65.1% | 69.2% | 64.5% | 66.3% | _TBD_ |
| Avg queue depth (EPP) | 236.8 | 252.4 | 220.4 | 236.5 | _TBD_ |
| Error count | 4,186 | 4,193 | 4,173 | 4,184 | _TBD_ |
| Avg pod startup (s) | 115 | 106 | 109 | 110 | _TBD_ |
| Cost (avg replicas × GPU/hr) | _TBD_ | 1.77 | 1.73 | 1.73 | _TBD_ |

## Decode Heavy Scenario

**llm-d Release:** v0.6.0
**Model:** Qwen/Qwen3-32B
**Workload:** 1000 prompt tokens, 4000 output tokens, 20 RPS, 600s duration
**Saturation Engine:** Default(v1), Tuned(v1)

| Metric | WVA v0.6.0 Default(v1) Run 1 | WVA v0.6.0 Default(v1) Run 2 | WVA v0.6.0 Default(v1) Run 3 | Avg | WVA v0.6.0 Tuned(v1) (decode) |
|--------|------------------------------|------------------------------|------------------------------|-----|-------------------------------|
| P99 TTFT (ms) | 85,612 | 85,397 | 63,144 | 78,051 | _TBD_ |
| P99 ITL (ms/token) | 47.09 | 47.05 | 47.26 | 47.13 | _TBD_ |
| Avg replicas | 1.73 | 1.82 | 1.96 | 1.84 | _TBD_ |
| Max replicas | 3 | 3 | 3 | 3 | _TBD_ |
| Avg KV cache utilization | 88.8% | 78.2% | 70.7% | 79.2% | _TBD_ |
| Avg queue depth (EPP) | 111.8 | 111.5 | 103.1 | 108.8 | _TBD_ |
| Error count | 3,506 | 3,551 | 3,632 | 3,563 | _TBD_ |
| Avg pod startup (s) | 119 | 103 | 106 | 109 | _TBD_ |
| Cost (avg replicas × GPU/hr) | _TBD_ | 1.82 | 1.96 | 1.89 | _TBD_ |

## Bursty Scenario

**llm-d Release:** v0.6.0
**Model:** Qwen/Qwen3-32B
**Workload:** ~1000 prompt tokens, ~1000 output tokens, multi-stage bursty RPS (15→2→10→15→5→2), 900s total duration
**Saturation Engine:** Default(v1)

| Metric | Run 1 | Run 2 | Run 3 | Avg |
|--------|-------|-------|-------|-----|
| P99 TTFT (ms) | 264,266 | 257,501 | 265,557 | 262,441 |
| P99 ITL (ms/token) | 196.1 | 210.3 | 182.4 | 196.3 |
| Avg replicas | 2.46 | 2.29 | 2.55 | 2.43 |
| Max replicas | 4 | 4 | 4 | 4 |
| Avg KV cache utilization | 31.5% | 50.9% | 52.9% | 45.1% |
| Avg queue depth (EPP) | 15.3 | 113.0 | 32.3 | 53.5 |
| Error count | 6,230 | 6,021 | 6,079 | 6,110 |
| Avg pod startup (s) | 109 | 101 | 100 | 103 |
| Cost (avg replicas × GPU/hr) | 2.46 | 2.29 | 2.55 | 2.43 |

## Symmetrical Scenario

**llm-d Release:** v0.6.0
**Model:** Qwen/Qwen3-32B
**Workload:** 1000 prompt tokens, 1000 output tokens, 20 RPS, 600s duration
**Saturation Engine:** Default(v1)

| Metric | WVA v0.6.0 Default(v1) Run 1 | WVA v0.6.0 Default(v1) Run 2 | WVA v0.6.0 Default(v1) Run 3 | Avg |
|--------|------------------------------|------------------------------|------------------------------|-----|
| P99 TTFT (ms) | 101,083 | 99,542 | 99,937 | 100,187 |
| P99 ITL (ms/token) | 67.61 | 67.0 | 67.25 | 67.29 |
| Avg replicas | 1.70 | 1.75 | 1.64 | 1.70 |
| Max replicas | 3 | 3 | 3 | 3 |
| Avg KV cache utilization | 66.7% | 70.2% | 73.7% | 70.2% |
| Avg queue depth (EPP) | 135.1 | 176.7 | 188.6 | 166.8 |
| Error count | 3,773 | 3,710 | 3,705 | 3,729 |
| Avg pod startup (s) | 97 | 107 | 105 | 103 |
| Cost (avg replicas × GPU/hr) | _TBD_ | 1.75 | 1.64 | 1.70 |
