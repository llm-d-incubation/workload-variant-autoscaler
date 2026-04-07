# WVA Autoscaler Benchmarking

This directory contains the tools and scripts to automate the end-to-end benchmarking of the Workload Variant Autoscaler (WVA) against standard HPA baselines. It orchestrates deploying the environment, running GuideLLM synthetic workloads, extracting metrics, and generating comparison reports.

## Architecture & Directory Structure

- `run/`: Contains `run_ci_benchmark.sh`, the master orchestration script. It handles teardown, standup, baseline/WVA execution, and metric extraction. Also contains scripts for triplicate runs.
- `scenarios/`: Custom workload profiles (e.g., `prefill_heavy`, `decode_heavy`, `symmetrical`) which are automatically injected during the benchmark run.
- `extract/`: Contains `get_benchmark_report.py` for generating PDF plots and latency statistics from the offline data. (Run automatically by the CI script).
- `dump_epp_fc_metrics/`: Contains scripts to dump raw Prometheus metrics into offline JSON for analysis. (Run automatically by the CI script).

## Setup Instructions

This benchmarking suite acts as a wrapper around the `llm-d-benchmark` repository. 

### 1. Clone the Benchmark Repository
Ensure `llm-d-benchmark` is cloned **inside the `wva-autoscaler` root directory**:
```bash
cd /path/to/wva-autoscaler
git clone https://github.com/llm-d/llm-d-benchmark.git
```

### 2. Export Required HuggingFace Token

The `llm-d-benchmark` deployment layer strictly requires a HuggingFace authentication token to spin up the vLLM modelservice endpoint (even for public/non-gated models).
You **MUST** export your token to your shell environment before initiating the test orchestrator:
```bash
export LLMDBENCH_HF_TOKEN="hf_your_token_here"
```

## Running Benchmarks

Run the main orchestrator script directly from its location in this repository:

```bash
cd run
./run_ci_benchmark.sh -n "my-namespace" -m "Qwen/Qwen3-0.6B" -s "inference-scheduling" -w "symmetrical"
```

### Configuration Flags

| Flag | Default | Description |
|---|---|---|
| `-n` | `default` | The Kubernetes namespace to use for the benchmark. |
| `-m` | `Qwen/Qwen3-0.6B` | The model to deploy and benchmark. |
| `-s` | `inference-scheduling` | The scenario file to use during the standup phase. |
| `-w` | `chatbot_synthetic` | The workload profile to simulate (e.g., `chatbot_synthetic`, `symmetrical`). It will auto-detect matching profiles in `scenarios/`. |
| `-d` | *(none)* | Enable Direct HPA mode (Bypasses WVA scaling logic). |
| `-t` | *(none)* | Apply a custom WVA Threshold ConfigMap path (e.g., `-t ../scenarios/wva_threshold/wva-threshold-config.yaml`). |

### Direct HPA Baseline (-d)

To run a baseline benchmark using the standard Kubernetes Horizontal Pod Autoscaler (HPA) instead of WVA, pass the `-d` flag. This will:
1. Deploy the standard environment.
2. Scale the WVA controller down to 0, completely disabling its scaling logic.
3. Deploy a custom direct HPA targeting the decode model server directly on queue size and running requests metrics.

### Automated Metrics Extraction Phase

After the GuideLLM load generation completes, `run_ci_benchmark.sh` automatically performs **Step 6**. It will:
1. Identify the newly generated GuideLLM results on the remote PVC.
2. Download them locally to `wva-autoscaler/exp_data/`.
3. Execute `dump_all_metrics.py` to drain Prometheus metrics inside the benchmark time-window boundaries.
4. Execute `get_benchmark_report.py` to plot hardware capacity, response patterns (TTFT/ITL), and autoscaling behavior, cleanly packaged into a PDF report.

You do not need to run python extraction scripts manually unless you want to re-generate the plot from cached offline data.

> [!NOTE] 
> **Python Dependencies**: `run_ci_benchmark.sh` automatically creates an isolated local virtual environment (`hack/benchmark/.venv`) and installs the exact library versions from `requirements.txt` to guarantee deterministic extraction. If you intend to run the `dump_all_metrics.py` or `get_benchmark_report.py` scripts standalone, ensure you either activate this virtual environment or install the libraries manually:
> ```bash
> source .venv/bin/activate
> # or manually
> pip install -r requirements.txt
> ```
