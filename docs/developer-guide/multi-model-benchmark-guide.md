# Multi-Model Benchmark Guide

Step-by-step guide for deploying and running the WVA multi-model scaling benchmark on an OpenShift cluster using the Go-based deployment tool (`deploy/multimodel/`). This covers everything from cluster access to running the benchmark and interpreting results.

## Prerequisites

### Required Tools

Verify the following tools are installed on your machine:

```bash
oc version --client
kubectl version --client
helm version --short
yq --version
jq --version
go version
```

If any are missing, install via Homebrew: `brew install openshift-cli kubectl helm yq jq go`

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
kubectl get nodes -o jsonpath='{range .items[?(@.status.allocatable.nvidia\.com/gpu)]}{.metadata.name}{"\t"}{.metadata.labels.nvidia\.com/gpu\.product}{"\n"}{end}'
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
kubectl create namespace <your-namespace>
```

> **Note**: If you get a `Forbidden` error, you don't have permission to create namespaces. Contact the cluster admin (Marcio Silva) to get admin access or have a namespace created for you.

Label the namespace for OpenShift user-workload monitoring (so Prometheus can scrape metrics):

```bash
kubectl label namespace <your-namespace> openshift.io/user-monitoring=true --overwrite
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

## Step 5: Run the Multi-Model Benchmark

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
watch kubectl get hpa -n <your-namespace>
watch kubectl get variantautoscaling -n <your-namespace>
```

### Delete the Namespace (optional, after you're done)

```bash
kubectl delete namespace <your-namespace>
```

---

