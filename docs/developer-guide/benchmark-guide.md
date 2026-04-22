# Running WVA Scaling Benchmarks

Step-by-step guide for deploying and running WVA scaling benchmarks on an OpenShift cluster. This covers both **single-model** and **multi-model** benchmarks, from cluster access to running the tests and interpreting results.

## Prerequisites

### Required Tools

Verify the following tools are installed on your machine:

```bash
oc version --client
oc version --client  # includes kubectl functionality
helm version --short
yq --version
jq --version
go version
```

If any are missing, install via Homebrew: `brew install openshift-cli helm yq jq go`

### Required Access

- OpenShift cluster credentials (API URL + token)
- HuggingFace token with access to the models you want to deploy

---

## Step 1: Log In to the OpenShift Cluster

Get your login token from the OpenShift web console:

1. Open the OpenShift console in your browser
2. Click your username (top right) → **Copy login command**
3. Click **Display Token**
4. Copy the `oc login` command and run it:

```bash
oc login --token=sha256~XXXXXXXXXXXXXXXXXXXX --server=https://api.your-cluster.example.com:6443
```

Verify access and confirm which cluster you're connected to:

```bash
oc whoami
oc whoami --show-console
oc whoami --show-server
```

Check available GPUs on the cluster:

```bash
oc get nodes -o jsonpath='{range .items[?(@.status.allocatable.nvidia\.com/gpu)]}{.metadata.name}{"\t"}{.metadata.labels.nvidia\.com/gpu\.product}{"\n"}{end}'
```

---

## Step 2: Set Up Your Namespace

First, check which namespaces you already have access to:

```bash
oc projects
```

If you have an existing namespace you can use, use that as `<your-namespace>` in the commands below.

If you have cluster-admin access, create a fresh namespace:

```bash
oc new-project <your-namespace>
```

> **Note**: If you get a `Forbidden` error, you don't have permission to create namespaces. Contact the cluster admin to get admin access or have a namespace created for you.

Label the namespace for OpenShift user-workload monitoring (so Prometheus can scrape metrics):

```bash
oc label namespace <your-namespace> openshift.io/user-monitoring=true --overwrite
```

---

## Step 3: Export Your HuggingFace Token

The only environment variable you need to export is the HuggingFace token (required for model downloads):

```bash
export HF_TOKEN="hf_xxxxxxxxxxxxxxxxxxxxx"
```

All other configuration is passed directly to the deploy/test commands in later steps.

---

## Step 4: Clone the Repository

If you haven't already:

```bash
git clone https://github.com/llm-d/llm-d-workload-variant-autoscaler.git
cd llm-d-workload-variant-autoscaler
```

Make sure you're on the correct branch:

```bash
git checkout main
# Or check out a specific PR branch:
# gh pr checkout <pr-number>
```

---

## Step 5a: Run the Single-Model Benchmark

The single-model benchmark tests WVA scaling behavior with one model under different workload patterns. Scenario configurations are defined in `test/benchmark/scenarios/`.

| Scenario | Prompt Tokens | Output Tokens | Rate | What it tests |
|----------|--------------|---------------|------|---------------|
| `prefill_heavy` | 4000 | 1000 | 20 RPS | Prefill (prompt processing) — long input, short output |
| `decode_heavy` | 1000 | 4000 | 20 RPS | Decode (token generation) — short input, long output |

### 1. Deploy Single-Model Infrastructure

```bash
make deploy-e2e-infra \
  ENVIRONMENT=openshift \
  WVA_NS=<your-namespace> LLMD_NS=<your-namespace> \
  E2E_EMULATED_LLMD_NAMESPACE=<your-namespace> \
  NAMESPACE_SCOPED=true SKIP_BUILD=true \
  DECODE_REPLICAS=1 IMG_TAG=v0.6.0 LLM_D_RELEASE=v0.6.0 \
  DEPLOY_PROMETHEUS_ADAPTER=false
```

Wait for all pods to be ready:

```bash
oc get pods -n <your-namespace>
```

Expected output — vLLM decode pod, EPP, gateway, and WVA controller all `Running`:

```
NAME                                                              READY   STATUS    RESTARTS   AGE
gaie-inference-scheduling-epp-...                                 1/1     Running   0          4m
infra-inference-scheduling-inference-gateway-istio-...            1/1     Running   0          4m
ms-inference-scheduling-llm-d-modelservice-decode-...             1/1     Running   0          4m
workload-variant-autoscaler-controller-manager-...                1/1     Running   0          2m
workload-variant-autoscaler-controller-manager-...                1/1     Running   0          2m
```

### 2. Run the Prefill Heavy Benchmark

```bash
make test-benchmark \
  ENVIRONMENT=openshift \
  E2E_EMULATED_LLMD_NAMESPACE=<your-namespace> \
  BENCHMARK_SCENARIO=prefill_heavy
```

### 3. Run the Decode Heavy Benchmark

```bash
make test-benchmark \
  ENVIRONMENT=openshift \
  E2E_EMULATED_LLMD_NAMESPACE=<your-namespace> \
  BENCHMARK_SCENARIO=decode_heavy
```

Each benchmark run takes approximately 15–20 minutes (30s warmup + 600s load generation + monitoring overhead).

### Expected Output

On success, the test prints a results summary and exits with code 0:

```
SUCCESS! -- 1 Passed | 0 Failed | 0 Pending | 6 Skipped
--- PASS: TestBenchmark
PASS
```

The results summary includes:
- TTFT and ITL latency percentiles (p50, p90, p99)
- Avg/max replicas and replica timeline
- KV cache utilization, vLLM queue depth, and EPP queue depth
- Achieved RPS, error count, and incomplete request count

### What the Benchmark Does

1. Finds the Helm-deployed decode deployment in the namespace
2. Creates a VariantAutoscaling (VA) resource (min=1, max=10, cost=10)
3. Creates an HPA with external metric `wva_desired_replicas`
4. Patches EPP ConfigMap with flow control and scorer weights
5. Launches a GuideLLM load generation job with the scenario parameters
6. Monitors replicas, KV cache utilization, and queue depth every 15s
7. Extracts and reports TTFT, ITL, throughput, and error metrics

### 4. Cleanup

```bash
oc delete project <your-namespace>
```

---

## Step 5b: Run the Multi-Model Benchmark

The multi-model benchmark tests WVA scaling across multiple models sharing the same infrastructure.

Replace `<your-namespace>` with your namespace:

```bash
# 1. Undeploy previous run (clean slate)
make undeploy-multi-model-infra \
  ENVIRONMENT=openshift \
  WVA_NS=<your-namespace> LLMD_NS=<your-namespace> \
  MODELS="Qwen/Qwen3-0.6B,unsloth/Meta-Llama-3.1-8B"

# 2. Deploy multi-model infrastructure
make deploy-multi-model-infra \
  ENVIRONMENT=openshift \
  WVA_NS=<your-namespace> LLMD_NS=<your-namespace> \
  NAMESPACE_SCOPED=true SKIP_BUILD=true \
  DECODE_REPLICAS=1 IMG_TAG=v0.6.0 LLM_D_RELEASE=v0.6.0 \
  DEPLOY_PROMETHEUS_ADAPTER=false \
  MODELS="Qwen/Qwen3-0.6B,unsloth/Meta-Llama-3.1-8B"

# 3. Run the benchmark
make test-multi-model-scaling \
  ENVIRONMENT=openshift \
  LLMD_NS=<your-namespace> \
  MODELS="Qwen/Qwen3-0.6B,unsloth/Meta-Llama-3.1-8B"
```

Expected result: `make test-multi-model-scaling` passes with exit code 0.

### Monitor During the Benchmark

In a separate terminal, watch the scaling behavior:

```bash
watch oc get hpa -n <your-namespace>
watch oc get variantautoscaling -n <your-namespace>
```

### Cleanup

```bash
oc delete project <your-namespace>
```

---
