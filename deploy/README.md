# Workload-Variant-Autoscaler Deployment Guide

Complete guide for deploying the Workload-Variant-Autoscaler (WVA) on Kubernetes, OpenShift, and Kind clusters.

> **Central Documentation Hub**: This is the main deployment guide containing comprehensive information about deployment methods, Helm chart configuration, and complete configuration reference. Platform-specific guides ([Kubernetes](kubernetes/README.md), [OpenShift](openshift/README.md), [Kind](kind-emulator/README.md)) provide additional platform-specific details and examples.

## Table of Contents

- [Overview](#overview)
- [Prerequisites](#prerequisites)
- [Deployment Methods](#deployment-methods)
  - [Method 1: Automated Deployment Script](#method-1-automated-deployment-script-recommended)
  - [Method 2: Helm Chart](#method-2-helm-chart)
- [Platform-Specific Guides](#platform-specific-guides)
- [Configuration Reference](#configuration-reference)
- [Post-Deployment](#post-deployment)
- [Troubleshooting](#troubleshooting)

## Overview

This guide covers two deployment procedures:

1. **Automated Script**: Complete end-to-end and customizable deployment including WVA, llm-d infrastructure, Prometheus, and HPA
2. **Helm Chart**: Deploy the WVA controller into an existing cluster

## Prerequisites

### Required Tools

All deployment methods require:

- **kubectl** (v1.24+) - Kubernetes CLI
- **helm** (v3.8+) - Package manager for Kubernetes
- **git** - Git CLI

Optional but recommended:

- **yq** (v4+) - YAML processor for configuration

Platform-specific requirements:

- **OpenShift**: `oc` CLI (v4.12+)
- **Kind**: `kind` CLI for local testing

### Cluster Requirements

**Minimum cluster specifications**:

- Kubernetes 1.24+ or OpenShift 4.12+
- Metrics server or Prometheus available
- For non-emulated LLM workloads: GPU availability

**Cluster access**:

- Cluster admin privileges (for full deployment script)
- Or namespace admin + ability to create ClusterRole/ClusterRoleBinding (for Helm-only)

### Required Tokens

- **HuggingFace Token** (for llm-d deployment): after getting access to a model, set a token on [HuggingFace](https://huggingface.co/settings/tokens)
  - Required for: Full deployment script with llm-d
  - Not required for: Helm chart only deployment

## Deployment Methods

### Method 1: Automated Deployment Script (Recommended)

The deployment script provides a complete, automated setup including:

- WVA controller with RBAC configuration
- Prometheus stack (or connects to existing)
- llm-d infrastructure (Gateway, Scheduler, vLLM)
- Prometheus Adapter for external metrics
- ServiceMonitors for metric collection
- VariantAutoscaling custom resources
- HPA configuration
- Automatic GPU detection
- Environment-specific optimizations

#### Quick Start with Make

```bash
# Set required environment variable
export HF_TOKEN="hf_xxxxxxxxxxxxx"

# Deploy to Kubernetes
make deploy-wva-on-k8s

# Deploy to OpenShift
make deploy-wva-on-openshift

# Deploy to Kind (with emulated GPUs)
make deploy-wva-emulated-on-kind
```

#### Manual Script Execution

```bash
# Navigate to deploy directory
cd deploy

# Set environment variables
export HF_TOKEN="hf_xxxxxxxxxxxxx"
export ENVIRONMENT="kubernetes"  # or "openshift", "kind-emulator"

# Run deployment script
bash install.sh
```

#### Script Configuration Options

The script accepts both command-line flags and environment variables:

**Command-line flags**:

```bash
bash install.sh [OPTIONS]

Options:
  -i, --wva-image IMAGE    WVA container image (default: ghcr.io/llm-d/llm-d-workload-variant-autoscaler:latest)
  -m, --model MODEL        Model ID (default: unsloth/Meta-Llama-3.1-8B)
  -a, --accelerator TYPE   GPU type: A100, H100, L40S (auto-detected by default)
  -u, --undeploy          Undeploy all components
  -e, --environment ENV    Environment: kubernetes, openshift, kind-emulator
  -h, --help              Show help
```

**Environment variables** (see [Configuration Reference](#configuration-reference) for complete list):

```bash
# Core Configuration
export HF_TOKEN="hf_xxx"                    # HuggingFace token (required for llm-d)
export MODEL_ID="unsloth/Meta-Llama-3.1-8B" # Model to deploy
export ACCELERATOR_TYPE="A100"              # GPU type (auto-detected)
export WVA_IMAGE_TAG="latest"               # WVA image version

# Deployment Flags
export DEPLOY_PROMETHEUS=true               # Deploy Prometheus stack
export DEPLOY_WVA=true                      # Deploy WVA controller
export DEPLOY_LLM_D=true                    # Deploy llm-d infrastructure
export DEPLOY_PROMETHEUS_ADAPTER=true       # Deploy Prometheus Adapter
export DEPLOY_VA=true                       # Deploy VariantAutoscaling CR
export DEPLOY_HPA=true                      # Deploy HPA

# HPA Configuration
export HPA_STABILIZATION_SECONDS=240        # HPA stabilization window (default: 240s)

# Gateway Configuration
export GATEWAY_PROVIDER="istio"             # Gateway provider: istio, kgateway
export INSTALL_GATEWAY_CTRLPLANE="true"     # Install gateway control plane
export BENCHMARK_MODE="true"                # Use benchmark configuration (for Istio only)

# SLO Targets
export SLO_TPOT=10                         # Time per output token (ms) SLO
export SLO_TTFT=1000                       # Time to first token (ms) SLO
```

#### Interactive Gateway Configuration

When running the script, it will prompt you about Gateway control plane installation:

```bash
Gateway Control Plane Configuration
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

The Gateway control plane (istio) is required to serve requests.
You can either:
  1. Install the Gateway control plane (recommended for new clusters or emulated clusters)
  2. Use an existing Gateway control plane in your cluster (recommended for production clusters)

Do you want to install the Gateway control plane? (y/n):
```

To skip this prompt and automate the decision:

```bash
# Install gateway control plane automatically
export INSTALL_GATEWAY_CTRLPLANE="true"
bash install.sh

# Use existing gateway (don't install)
export INSTALL_GATEWAY_CTRLPLANE="false"
bash install.sh
```

#### Script Deployment Examples

##### Example 1: Full deployment with defaults

```bash
export HF_TOKEN="hf_xxxxx"
make deploy-wva-on-k8s
```

##### Example 2: Custom model and namespace

```bash
export HF_TOKEN="hf_xxxxx"
export MODEL_ID="meta-llama/Llama-2-7b-hf"
export SLO_TPOT=5
export SLO_TTFT=500
make deploy-wva-on-k8s
```

##### Example 3: Deploy only WVA (llm-d already running)

```bash
export DEPLOY_WVA=true
export DEPLOY_LLM_D=false
export DEPLOY_PROMETHEUS=true  # Prometheus is needed for metrics - disable if it is already installed in your cluster
export DEPLOY_PROMETHEUS_ADAPTER=false
export DEPLOY_HPA=false
make deploy-wva-on-k8s
```

##### Example 4: Fast HPA scaling for development/testing

```bash
export HF_TOKEN="hf_xxxxx"
export HPA_STABILIZATION_SECONDS=30  # Fast scaling for dev/test (default: 240)
make deploy-wva-on-k8s
```

##### Example 5: E2E testing with very fast scaling

```bash
export HF_TOKEN="hf_xxxxx"
export HPA_STABILIZATION_SECONDS=0   # Immediate scaling for e2e tests
export VLLM_MAX_NUM_SEQS=8          # Low batch size for easy saturation
export E2E_TESTS_ENABLED=true
make deploy-wva-on-k8s
```

##### Example 6: Parameter estimation with specific batch size

```bash
export HF_TOKEN="hf_xxxxx"
export VLLM_MAX_NUM_SEQS=64         # Match desired max batch size
export MODEL_ID="unsloth/Meta-Llama-3.1-8B"
make deploy-wva-on-k8s
```

##### Example 7: Infra-only mode for e2e testing

Deploy only the llm-d infrastructure and WVA controller without creating VariantAutoscaling or HPA resources. This is useful for e2e testing where tests dynamically create their own VA/HPA resources.

```bash
# Using command-line flag
export HF_TOKEN="hf_xxxxx"
./deploy/install.sh --infra-only

# Using environment variable
export HF_TOKEN="hf_xxxxx"
export INFRA_ONLY=true
make deploy-wva-emulated-on-kind

# Verify: Only WVA controller + llm-d infrastructure should exist
kubectl get variantautoscaling --all-namespaces  # Should be empty
kubectl get hpa --all-namespaces | grep -v kube-system  # Should be empty (except system HPAs)
```

**What gets deployed in infra-only mode:**
- ✅ Prometheus stack (metrics collection)
- ✅ WVA controller
- ✅ llm-d infrastructure (Gateway, CRDs, RBAC, EPP)
- ✅ Prometheus Adapter (external metrics API)
- ❌ VariantAutoscaling CRs (tests create these)
- ❌ HPA resources (tests create these)
- ❌ Model services (tests create these)
```

### Method 2: Helm Chart

The WVA can be deployed as a standalone using Helm, assuming you have:

- Existing Prometheus server
- Existing vLLM deployment
- ServiceMonitors configured
- Prometheus Adapter (optional, for HPA)

This method is particularly useful when there is one (or more) existing llm-d infrastructure deployed

#### Helm Chart Quick Start

```bash
# Add WVA Helm repository (if published)
# helm repo add wva https://llm-d.github.io/workload-variant-autoscaler
# helm repo update

# Or install from local chart
cd charts/workload-variant-autoscaler

# Install with default values
helm install workload-variant-autoscaler . \
  --namespace workload-variant-autoscaler-system \
  --create-namespace

# Or install with custom values
helm install workload-variant-autoscaler . \
  --namespace workload-variant-autoscaler-system \
  --create-namespace \
  --values my-values.yaml
```

#### Helm Chart Configuration

The Helm chart has several configurable parameters. Here's a comprehensive example based on the default values:

**Create `my-values.yaml`**:

```yaml
# WVA Controller Configuration
wva:
  enabled: true

  # Image configuration
  image:
    repository: ghcr.io/llm-d/llm-d-workload-variant-autoscaler
    tag: latest
  imagePullPolicy: Always

  # Metrics configuration
  metrics:
    enabled: true
    port: 8443      # Secure metrics port
    secure: true    # Enable secure metrics endpoint

  # Reconciliation interval
  reconcileInterval: 60s

  # Prometheus configuration
  prometheus:
    monitoringNamespace: workload-variant-autoscaler-monitoring
    baseURL: "https://prometheus-k8s.monitoring.svc.cluster.local:9090"
    
    # TLS configuration
    tls:
      # CA certificate path inside container
      caCertPath: "/etc/ssl/certs/prometheus-ca.crt"
      # Set to true to skip TLS verification (not recommended for production)
      insecureSkipVerify: false
    
    # Provide CA certificate directly
    # caCert: |
    #   -----BEGIN CERTIFICATE-----
    #   YOUR_CA_CERTIFICATE_HERE
    #   -----END CERTIFICATE-----

  # Logging configuration
  logging:
    level: info  # debug, info, warn, error

# llm-d Infrastructure Configuration
llmd:
  namespace: llm-d-inference-scheduler
  modelName: ms-inference-scheduling-llm-d-modelservice
  modelID: "unsloth/Meta-Llama-3.1-8B"

# VariantAutoscaling Configuration
va:
  enabled: true           # Create VariantAutoscaling CR
  accelerator: H100       # GPU type: A100, H100, L40S, etc.
  sloTpot: 10            # Time per output token SLO (ms)
  sloTtft: 1000          # Time to first token SLO (ms)

# HPA Configuration
hpa:
  enabled: true           # Create HPA resource
  maxReplicas: 10        # Maximum number of replicas
  targetAverageValue: "1" # Target value for external metric
  
  # Scaling behavior configuration
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 240  # Wait 240s before scaling up (production default)
      selectPolicy: Max                # Use maximum scale from policies
      policies:
        - type: Pods
          value: 10                    # Scale up by max 10 pods
          periodSeconds: 150           # Per 150 second period
    scaleDown:
      stabilizationWindowSeconds: 240  # Wait 240s before scaling down (production default)
      selectPolicy: Max
      policies:
        - type: Pods
          value: 10                    # Scale down by max 10 pods
          periodSeconds: 150           # Per 150 second period

# vLLM Service Configuration
vllmService:
  enabled: true           # Create Service for vLLM
  nodePort: 30000        # NodePort for external access
  interval: 15s          # ServiceMonitor scrape interval
  scheme: http           # http or https
```

**Install with custom values**:

```bash
helm install workload-variant-autoscaler ./charts/workload-variant-autoscaler \
  --namespace workload-variant-autoscaler-system \
  --create-namespace \
  --values my-values.yaml
```

#### Minimal Helm Installation

For a minimal installation (just the controller, no VariantAutoscaling or HPA):

```bash
# Create minimal values file
cat > minimal-values.yaml <<EOF
wva:
  enabled: true
  image:
    tag: latest
  imagePullPolicy: Always
  
  prometheus:
    baseURL: "https://my-prometheus.monitoring.svc.cluster.local:9090"
    monitoringNamespace: monitoring
    tls:
      insecureSkipVerify: true  # Only for dev/testing
  
  logging:
    level: info

# Disable auto-creation of resources
va:
  enabled: false

hpa:
  enabled: false

vllmService:
  enabled: false
EOF

# Install
helm install workload-variant-autoscaler ./charts/workload-variant-autoscaler \
  -n workload-variant-autoscaler-system \
  --create-namespace \
  -f minimal-values.yaml
```

#### Helm Chart with External Prometheus

If you have an existing Prometheus (e.g., kube-prometheus-stack):

```yaml
# prometheus-values.yaml
wva:
  prometheus:
    baseURL: "https://kube-prometheus-stack-prometheus.monitoring.svc.cluster.local:9090"
    monitoringNamespace: monitoring
    tls:
      insecureSkipVerify: false  # Use proper TLS verification
      caCertPath: "/etc/ssl/certs/prometheus-ca.crt"
    # Provide CA cert via file
    # caCert: |
    #   -----BEGIN CERTIFICATE-----
    #   ...
    #   -----END CERTIFICATE-----
  
  metrics:
    enabled: true
    port: 8443
    secure: true

# Configure llm-d details
llmd:
  namespace: my-llm-namespace
  modelName: my-vllm-deployment
  modelID: "meta-llama/Llama-2-7b-hf"

# Create VariantAutoscaling
va:
  enabled: true
  accelerator: A100
  sloTpot: 10
  sloTtft: 1000

# Create HPA
hpa:
  enabled: true
  maxReplicas: 10
```

```bash
helm install workload-variant-autoscaler ./charts/workload-variant-autoscaler \
  -n workload-variant-autoscaler-system \
  --create-namespace \
  -f prometheus-values.yaml
```

#### Installing with CA Certificate File

If you need to provide a CA certificate for Prometheus TLS:

```bash
# Create a CA certificate file
kubectl get secret prometheus-web-tls \
  -n monitoring \
  -o jsonpath='{.data.ca\.crt}' | base64 -d > /tmp/prometheus-ca.crt

# Install with CA certificate
helm install workload-variant-autoscaler ./charts/workload-variant-autoscaler \
  -n workload-variant-autoscaler-system \
  --create-namespace \
  --set-file wva.prometheus.caCert=/tmp/prometheus-ca.crt \
  --set wva.prometheus.baseURL="https://prometheus-k8s.monitoring.svc:9090" \
  --set wva.prometheus.tls.insecureSkipVerify=false
```

#### Creating VariantAutoscaling Manually

If you don't create VariantAutoscaling via Helm, create it manually:

```bash
cat <<EOF | kubectl apply -f -
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: my-vllm-deployment-decode
  namespace: llm-d-inference-scheduler
  labels:
    inference.optimization/acceleratorName: A100
spec:
  # Model identifier
  modelID: "unsloth/Meta-Llama-3.1-8B"
EOF
```

#### Creating HPA Manually

If using HPA with external metrics:

```bash
cat <<EOF | kubectl apply -f -
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: vllm-deployment-hpa
  namespace: llm-d-inference-scheduler
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: my-vllm-deployment-decode
  maxReplicas: 10
  # minReplicas: 0  # Scale to zero is an alpha feature
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 0  # Tune based on your needs
      policies:
      - type: Pods
        value: 10
        periodSeconds: 15
    scaleDown:
      stabilizationWindowSeconds: 0  # Tune based on your needs
      policies:
      - type: Pods
        value: 10
        periodSeconds: 15
  metrics:
  - type: External
    external:
      metric:
        name: wva_desired_replicas
        selector:
          matchLabels:
            variant_name: my-vllm-deployment-decode
      target:
        type: AverageValue
        averageValue: "1"
EOF
```

#### Helm Uninstall

```bash
# Uninstall the release
helm uninstall workload-variant-autoscaler -n workload-variant-autoscaler-system
```

## Platform-Specific Guides

For platform-specific instructions and considerations:

- **[Kubernetes Guide](kubernetes/README.md)**: Detailed Kubernetes-specific instructions including kube-prometheus-stack setup, GPU operator installation, and ServiceMonitor configuration
- **[OpenShift Guide](openshift/README.md)**: OpenShift-specific instructions including User Workload Monitoring (Thanos), Routes, Security Context Constraints (SCC), and GPU operator on OpenShift
- **[Kind Guide (Local Testing)](kind-emulator/README.md)**: Local development and testing with Kind clusters and emulated GPUs

Each guide includes platform-specific examples, troubleshooting, and quick start commands. All guides use the same [Configuration Reference](#configuration-reference) documented below.

## Configuration Reference

### Environment Variables (Script)

#### Required

| Variable | Description | Required For |
|----------|-------------|--------------|
| `HF_TOKEN` | HuggingFace token | llm-d deployment |

#### Core Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `ENVIRONMENT` | Deployment environment | `kubernetes` |
| `WELL_LIT_PATH_NAME` | llm-d path name | `inference-scheduling` |
| `MODEL_ID` | Model to deploy | `unsloth/Meta-Llama-3.1-8B` |
| `ACCELERATOR_TYPE` | GPU type | Auto-detected |
| `SLO_TPOT` | Time per output token SLO (ms) | `10` |
| `SLO_TTFT` | Time to first token SLO (ms) | `1000` |

#### Image Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `WVA_IMAGE_REPO` | WVA image repository | `ghcr.io/llm-d/llm-d-workload-variant-autoscaler` |
| `WVA_IMAGE_TAG` | WVA image tag | `latest` |
| `WVA_IMAGE_PULL_POLICY` | Image pull policy | `Always` |

#### Namespace Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `WVA_NS` | WVA controller namespace | `workload-variant-autoscaler-system` |
| `MONITORING_NAMESPACE` | Prometheus namespace | `workload-variant-autoscaler-monitoring` |
| `LLMD_NS` | llm-d namespace | `llm-d-inference-scheduler` |

#### Gateway Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `GATEWAY_PROVIDER` | Gateway implementation | `istio` |
| `INSTALL_GATEWAY_CTRLPLANE` | Install gateway control plane | `false` (prompts user) |
| `BENCHMARK_MODE` | Use benchmark Istio config | `true` |

#### Deployment Flags

| Variable | Description | Default |
|----------|-------------|---------|
| `DEPLOY_PROMETHEUS` | Deploy Prometheus stack | `true` |
| `DEPLOY_WVA` | Deploy WVA controller | `true` |
| `DEPLOY_LLM_D` | Deploy llm-d infrastructure | `true` |
| `DEPLOY_PROMETHEUS_ADAPTER` | Deploy Prometheus Adapter | `true` |
| `DEPLOY_VA` | Deploy VariantAutoscaling CR | `true` |
| `DEPLOY_HPA` | Deploy HPA | `true` |
| `INFRA_ONLY` | Deploy only infrastructure (skip VA/HPA) | `false` |
| `SKIP_CHECKS` | Skip prerequisite checks | `false` |

#### HPA Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `HPA_STABILIZATION_SECONDS` | HPA stabilization window in seconds | `240` |

**Best Practices:**
- **Production**: 120-300 seconds (prevents flapping, ensures stability)
- **Development**: 30-60 seconds (faster iteration)
- **E2E Tests**: 0-30 seconds (rapid validation)

**Examples:**
```bash
# Production deployment (default)
HPA_STABILIZATION_SECONDS=240 ./deploy/install.sh

# Development deployment
HPA_STABILIZATION_SECONDS=60 ./deploy/install.sh

# E2E testing
HPA_STABILIZATION_SECONDS=30 ./deploy/install.sh
```

#### Advanced Configuration

| Variable | Description | Default |
|----------|-------------|---------|
| `SKIP_TLS_VERIFY` | Skip TLS verification | Auto-detected |
| `WVA_LOG_LEVEL` | WVA logging level | `info` |
| `VLLM_SVC_ENABLED` | Enable vLLM Service | `true` |
| `VLLM_SVC_NODEPORT` | vLLM NodePort | `30000` |
| `LLM_D_RELEASE` | llm-d version | `v0.3.0` |
| `VLLM_MAX_NUM_SEQS` | vLLM max concurrent sequences per replica | (unset - uses vLLM default) |

**vLLM Performance Tuning:**

The `VLLM_MAX_NUM_SEQS` variable controls the maximum number of concurrent sequences (batch size) that each vLLM replica can handle. Lower values make the server easier to saturate, which is useful for testing autoscaling behavior.

**Use cases:**
- **E2E Testing**: Set to low values (e.g., `8` or `16`) to quickly trigger saturation and test autoscaling
- **Parameter Estimation**: Match this to your desired maximum batch size (see [Parameter Estimation Guide](../docs/tutorials/parameter-estimation.md))
- **Production**: Leave unset to use vLLM's default based on available GPU memory

**Example:**
```bash
# E2E testing with easy saturation
export VLLM_MAX_NUM_SEQS=8
make deploy-wva-on-k8s

# Parameter estimation with batch size 64
export VLLM_MAX_NUM_SEQS=64
make deploy-wva-on-k8s
```

### Helm Values Reference

See the [values.yaml](../charts/workload-variant-autoscaler/values.yaml) file for complete Helm chart configuration options.

## Post-Deployment

### Verification

**Check all components**:

```bash
# WVA controller
kubectl get pods -n workload-variant-autoscaler-system
kubectl logs -n workload-variant-autoscaler-system -l app.kubernetes.io/name=workload-variant-autoscaler

# VariantAutoscaling resources
kubectl get variantautoscaling -A
kubectl describe variantautoscaling <name> -n <namespace>

# HPA (if deployed)
kubectl get hpa -A

# External metrics (if Prometheus Adapter deployed)
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1" | jq
```

**Check metrics flow**:

```bash
# 1. Verify vLLM is exposing metrics
kubectl port-forward -n <llm-namespace> <vllm-pod> 8000:8000
curl http://localhost:8000/metrics | grep vllm:

# 2. Verify Prometheus is scraping
kubectl port-forward -n <monitoring-namespace> svc/prometheus-k8s 9090:9090
# Visit http://localhost:9090 and query: vllm:num_requests_running

# 3. Verify WVA is collecting metrics
kubectl logs -n workload-variant-autoscaler-system -l app.kubernetes.io/name=workload-variant-autoscaler | grep "Collected metrics"

# 4. Verify external metrics API (if using HPA)
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/<namespace>/wva_desired_replicas" | jq
```

### Monitoring WVA

**View logs with filtering**:

```bash
# All logs
kubectl logs -n workload-variant-autoscaler-system -l app.kubernetes.io/name=workload-variant-autoscaler -f

# Filter for optimization decisions
kubectl logs -n workload-variant-autoscaler-system -l app.kubernetes.io/name=workload-variant-autoscaler | \
  grep "OptimizationComplete"

# Filter for metrics issues
kubectl logs -n workload-variant-autoscaler-system -l app.kubernetes.io/name=workload-variant-autoscaler | \
  grep "MetricsMissing\|MetricsStale"

# JSON parsing (if using JSON logging)
kubectl logs -n workload-variant-autoscaler-system -l app.kubernetes.io/name=workload-variant-autoscaler | \
  jq 'select(.level=="ERROR")'
```

**Check VariantAutoscaling status**:

```bash
# List with custom columns
kubectl get variantautoscaling -A -o custom-columns=\
NAME:.metadata.name,\
NAMESPACE:.metadata.namespace,\
MODEL:.spec.model,\
CURRENT:.status.currentReplicas,\
DESIRED:.status.desiredReplicas,\
METRICS:.status.conditions[?(@.type=="MetricsAvailable")].status

# Detailed status
kubectl describe variantautoscaling <name> -n <namespace>
```

### Testing Autoscaling

#### Quick Testing with Low Batch Size

For rapid testing of autoscaling behavior, configure vLLM with a low `max-num-seqs` value to make the server easy to saturate:

```bash
# Deploy with testing-friendly configuration
export VLLM_MAX_NUM_SEQS=8              # Low batch size triggers saturation quickly
export HPA_STABILIZATION_SECONDS=30     # Fast scaling for testing
make deploy-wva-on-k8s  # or -on-openshift, -emulated-on-kind
```

This configuration helps you:
- Quickly verify autoscaling behavior without heavy load
- Test WVA's saturation detection
- Validate HPA integration
- Debug scaling issues faster

**Generate load**:

```bash
# Using guidellm (if available)
guidellm bench \
  --url http://<vllm-service>:<port>/v1 \
  --model <model-id> \
  --rate 50 \
  --duration 300

# Using simple loop
for i in {1..100}; do
  curl -X POST http://<vllm-service>:<port>/v1/completions \
    -H "Content-Type: application/json" \
    -d '{"model":"<model-id>","prompt":"Hello","max_tokens":100}' &
done
```

**Watch autoscaling**:

```bash
# Watch VariantAutoscaling
watch kubectl get variantautoscaling -n <namespace>

# Watch HPA
watch kubectl get hpa -n <namespace>

# Watch pods
watch kubectl get pods -n <namespace>

# Watch external metrics
watch 'kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/<namespace>/wva_desired_replicas" | jq'
```

## Troubleshooting

### Common Issues

#### 1. WVA Pod Not Starting

**Symptoms**:

```bash
kubectl get pods -n workload-variant-autoscaler-system
NAME                                           READY   STATUS    RESTARTS
workload-variant-autoscaler-controller-xxx     0/1     Pending   0
```

**Diagnosis**:

```bash
kubectl describe pod <pod-name> -n workload-variant-autoscaler-system
kubectl logs <pod-name> -n workload-variant-autoscaler-system
```

**Common causes**:

- Insufficient resources: Check resource requests in values
- Image pull errors: validate image repository and tag
- TLS configuration: Verify TLS configuration is correct
- Prometheus configuration: Verify that the WVA can reach Prometheus

**Solutions**:

```bash
# Check image in the pod description
kubectl describe pods -n workload-variant-autoscaler-system pod-name

# Look for TLS- or Prometheus-related error logs
kubectl logs <pod-name> -n workload-variant-autoscaler-system
```

#### 2. Metrics Not Available

**Symptoms**:

- VariantAutoscaling shows `MetricsAvailable: False`
- WVA logs show "Metrics unavailable" warnings

**Diagnosis**:

```bash
# Check WVA logs for metrics errors
kubectl logs -n workload-variant-autoscaler-system -l app.kubernetes.io/name=workload-variant-autoscaler | \
  grep -i "metrics"

# Check if vLLM is exposing metrics
kubectl port-forward -n <namespace> <vllm-pod> 8000:8000
curl -s http://localhost:8000/metrics

# Check ServiceMonitor
kubectl get servicemonitor -A
kubectl describe servicemonitor <name> -n <namespace>

# Check Prometheus targets
kubectl port-forward -n <monitoring-namespace> svc/prometheus-k8s 9090:9090
# Visit http://localhost:9090/targets
```

**Common causes**:

- ServiceMonitor not created or in wrong namespace
- Service selector doesn't match vLLM pods
- Prometheus not scraping the namespace
- vLLM not exposing metrics on expected port

**Solutions**:

```bash
# Verify ServiceMonitor selector matches Service
kubectl get svc -n <namespace> --show-labels
kubectl get servicemonitor -n <monitoring-namespace> -o yaml | grep -A 5 selector

# Check Prometheus configuration
kubectl get prometheus -A -o yaml | grep serviceMonitorNamespaceSelector

# Manually test metrics endpoint
kubectl exec -it <vllm-pod> -n <namespace> -- curl localhost:8000/metrics
```

#### 3. HPA Not Scaling

**Symptoms**:

```bash
kubectl get hpa -n <namespace>
NAME       REFERENCE          TARGETS           MINPODS   MAXPODS   REPLICAS
my-hpa     Deployment/vllm    <unknown>/1(avg)   1         10        1
```

**Diagnosis**:

```bash
# Check HPA status
kubectl describe hpa <name> -n <namespace>

# Check external metrics API on the specified namespace
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/<your-namespace>/wva_desired_replicas" | jq

# Check Prometheus Adapter logs
kubectl logs -n <monitoring-namespace> deployment/prometheus-adapter

# Check if WVA is emitting the metric
kubectl logs -n workload-variant-autoscaler-system -l app.kubernetes.io/name=workload-variant-autoscaler | \
  grep "wva_desired_replicas"
```

**Common causes**:

- Prometheus Adapter not deployed
- External metrics API not registered
- Metric selector doesn't match emitted labels
- WVA not emitting metrics (due to metrics unavailability)

**Solutions**:

```bash
# Verify external metrics API
kubectl api-resources | grep external.metrics

# Check Prometheus Adapter configuration
kubectl get configmap prometheus-adapter -n <monitoring-namespace> -o yaml

# Verify metric exists in Prometheus
kubectl port-forward -n <monitoring-namespace> svc/prometheus-k8s 9090:9090
# Query: wva_desired_replicas{variant_name="<name>"}
```

### Getting Help

If you encounter issues not covered here:

1. **Check logs**: WVA, Prometheus, Prometheus Adapter, vLLM
2. **Verify configuration**: VariantAutoscaling spec, ServiceMonitor, HPA
3. **Test components individually**: Metrics exposure, Prometheus scraping, external metrics API
4. **Review documentation**: Platform-specific READMEs
5. **Open an issue**: Include logs, configuration, and environment details

### Useful Commands Cheatsheet

```bash
# === WVA Controller ===
kubectl get pods -n workload-variant-autoscaler-system
kubectl logs -n workload-variant-autoscaler-system -l app.kubernetes.io/name=workload-variant-autoscaler -f
kubectl describe deployment workload-variant-autoscaler-controller-manager -n workload-variant-autoscaler-system

# === VariantAutoscaling Resources ===
kubectl get variantautoscaling -A
kubectl describe variantautoscaling <name> -n <namespace>
kubectl get variantautoscaling <name> -n <namespace> -o yaml

# === Metrics and Monitoring ===
kubectl get servicemonitor -A
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1" | jq
kubectl port-forward -n <monitoring-namespace> svc/prometheus-k8s 9090:9090

# === HPA ===
kubectl get hpa -A
kubectl describe hpa <name> -n <namespace>
kubectl get hpa <name> -n <namespace> -o yaml

# === Prometheus Adapter ===
kubectl get pods -n <monitoring-namespace> | grep prometheus-adapter
kubectl logs -n <monitoring-namespace> deployment/prometheus-adapter

# === vLLM / Application ===
kubectl get pods -n <app-namespace>
kubectl logs -n <app-namespace> <vllm-pod>
kubectl port-forward -n <app-namespace> <vllm-pod> 8000:8000

# === Configuration ===
kubectl get configmap -n workload-variant-autoscaler-system
kubectl get configmap service-classes -n workload-variant-autoscaler-system -o yaml
kubectl get configmap model-accelerator-data -n workload-variant-autoscaler-system -o yaml
```

## Additional Resources

- **Main Project**: [README.md](../README.md)
- **Kubernetes Guide**: [kubernetes/README.md](kubernetes/README.md)
- **OpenShift Guide**: [openshift/README.md](openshift/README.md)
- **Helm Chart**: [charts/workload-variant-autoscaler](../charts/workload-variant-autoscaler/)
- **API Reference**: [api/v1alpha1](../api/v1alpha1/)
- **Architecture**: [docs/design/modeling-optimization.md](../docs/design/modeling-optimization.md)
