# Benchmark Specs

This directory contains [llm-d-benchmark](https://github.com/llm-d/llm-d-benchmark) specifications and scenarios for benchmarking this repository's autoscaler.

## Prerequisites

### Install the `llm-d-benchmark` CLI

Clone `llm-d-benchmark` and install the CLI:

```bash
# Default: clone as a sibling of this repo
git clone https://github.com/llm-d/llm-d-benchmark.git ../llm-d-benchmark
cd ../llm-d-benchmark && ./install.sh
```

Then activate the virtual environment so `llmdbenchmark` is on your PATH:

```bash
source ../llm-d-benchmark/.venv/bin/activate
```

If you cloned `llm-d-benchmark` somewhere other than the default sibling location, activate its venv accordingly:

```bash
source /path/to/llm-d-benchmark/.venv/bin/activate
```

### Prepare a Kubernetes cluster

#### Local cluster (Kind)

1. Create the cluster:

   ```bash
   make create-kind-cluster
   ```

2. Install Prometheus (required for autoscaler metrics):

   ```bash
   helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
   helm upgrade --install prometheus prometheus-community/kube-prometheus-stack \
     --namespace monitoring --create-namespace \
     --set prometheus.prometheusSpec.serviceMonitorSelectorNilUsesHelmValues=false \
     --set-json 'prometheus.prometheusSpec.serviceMonitorSelector={}' \
     --set prometheus.prometheusSpec.podMonitorSelectorNilUsesHelmValues=false \
     --set-json 'prometheus.prometheusSpec.podMonitorSelector={}'
   ```

   This exposes Prometheus at `http://prometheus-operated.monitoring.svc.cluster.local:9090`, which is what the benchmark scenarios are pre-configured to use. If you install into a different namespace or use a different release name, update `prometheus.baseUrl` in the relevant scenario file.

3. Install KEDA 2.20 or later (required for autoscaler triggers):

   ```bash
   helm repo add kedacore https://kedacore.github.io/charts
   helm install keda kedacore/keda -n keda --create-namespace
   ```


## Directory layout

```
benchmark/
├── config/
│   ├── specification/        # Jinja2 spec templates (.yaml.j2) — point llmdbenchmark at these
│   ├── scenarios/            # Scenario YAMLs consumed by each spec
│   └── templates/
│       └── values/
│           └── defaults.yaml # Default values merged into every scenario
```

## Running a benchmark

### Running the CLI directly

After activating the venv (see Prerequisites), run `llmdbenchmark` from the repo root:

```bash
# Standup
llmdbenchmark \
  --spec benchmark/config/specification/simulator/pd-disaggregation-sim.yaml.j2 \
  standup -p <namespace>

# Run
llmdbenchmark \
  --spec benchmark/config/specification/simulator/pd-disaggregation-sim.yaml.j2 \
  run -p <namespace> -l inference-perf -w guide_pd-disaggregation_1.yaml

# Teardown
llmdbenchmark \
  --spec benchmark/config/specification/simulator/pd-disaggregation-sim.yaml.j2 \
  teardown -p <namespace>
```

Use `--dry-run` / `-n` to preview what would be applied without touching the cluster.
