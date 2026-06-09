# Workload-Variant-Autoscaler Deployment Guide

> **Note:** This guide is developer/operator-oriented and covers deploying WVA from source. If you are an end user looking to install WVA as part of llm-d, see the [llm-d workload-autoscaling guide](https://github.com/llm-d/llm-d/blob/main/guides/workload-autoscaling/README.wva.md) instead.

Complete guide for deploying the Workload-Variant-Autoscaler (WVA) on Kubernetes, OpenShift, and Kind clusters.

> **Central Documentation Hub**: This is the main deployment guide containing comprehensive information about deployment methods and configuration reference. Platform-specific guides ([Kubernetes](kubernetes/README.md), [OpenShift](openshift/README.md), [Kind](kind-emulator/README.md)) provide additional platform-specific details and examples.

## Table of Contents

- [Overview](#overview)
- [Prerequisites](#prerequisites)
- [Deployment Methods](#deployment-methods)
  - [Method 1: Automated Deployment Script](#method-1-automated-deployment-script-recommended)
  - [Method 2: Kustomize (direct controller install)](#method-2-kustomize-direct-controller-install)
  - [Legacy: Helm Chart (Deprecated)](#legacy-helm-chart-deprecated)
- [Platform-Specific Guides](#platform-specific-guides)
- [Configuration Reference](#configuration-reference)
- [Post-Deployment](#post-deployment)
- [Troubleshooting](#troubleshooting)

## Overview

This guide covers two deployment procedures:

1. **Automated Script**: Complete end-to-end and customizable deployment including WVA, llm-d infrastructure, Prometheus, and HPA
2. **Kustomize**: Install the WVA controller directly into an existing cluster

## Prerequisites

### Required Tools

All deployment methods require:

- **kubectl** (v1.24+) - Kubernetes CLI
- **kustomize** (v5+) - for direct controller installs
- **helm** (v3.8+) - for upstream dependencies (LWS, KEDA, Prometheus stack)
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
- Or namespace admin + ability to create ClusterRole/ClusterRoleBinding (for Kustomize direct install)

### Workload Requirements

WVA needs two things to associate Prometheus metrics with a `VariantAutoscaling` resource: the pod label must be present, and the Prometheus monitor must propagate it into metrics.

**1. `llm-d.ai/variant` label on your scale target**

Add the `llm-d.ai/variant` label to the **pod template** of your Deployment or LeaderWorkerSet. Set the value to the name of the corresponding `VariantAutoscaling` resource. WVA does not add this label automatically.

```yaml
# Deployment — add to spec.template.metadata.labels
spec:
  template:
    metadata:
      labels:
        llm-d.ai/variant: <VariantAutoscaling-name>
```

```yaml
# LeaderWorkerSet — add to both leaderTemplate and workerTemplate
spec:
  leaderWorkerTemplate:
    leaderTemplate:
      metadata:
        labels:
          llm-d.ai/variant: <VariantAutoscaling-name>
    workerTemplate:
      metadata:
        labels:
          llm-d.ai/variant: <VariantAutoscaling-name>
```

**2. Relabeling rule on your ServiceMonitor or PodMonitor**

The `llm-d.ai/variant` pod label must be propagated into Prometheus metric series as `llm_d_ai_variant`. Add the following target relabeling rule to the `ServiceMonitor` or `PodMonitor` that scrapes your model-server pods:

```yaml
# ServiceMonitor — add to spec.endpoints[].relabelings
spec:
  endpoints:
  - relabelings:
    - sourceLabels: [__meta_kubernetes_pod_label_llm_d_ai_variant]
      targetLabel: llm_d_ai_variant
      action: replace
```

```yaml
# PodMonitor — add to spec.podMetricsEndpoints[].relabelings
spec:
  podMetricsEndpoints:
  - relabelings:
    - sourceLabels: [__meta_kubernetes_pod_label_llm_d_ai_variant]
      targetLabel: llm_d_ai_variant
      action: replace
```

> **Important**: This rule must be under `relabelings` (target relabeling), **not** `metricRelabelings`. The `__meta_kubernetes_pod_label_*` labels are only available during target relabeling.

If either requirement is missing, WVA will not make scaling decisions for the affected variant. See [Controller Behavior](../docs/design/controller-behavior.md#prerequisites) for more details.

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
CREATE_CLUSTER=true make deploy-e2e-infra
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

**Command-line flags** (`deploy/install.sh`):

```bash
bash install.sh [OPTIONS]

Options:
  -i, --wva-image IMAGE    WVA container image (default: ghcr.io/llm-d/llm-d-workload-variant-autoscaler:latest)
  -u, --undeploy           Undeploy WVA, monitoring, and scaler (not llm-d)
  -e, --environment ENV    kubernetes | openshift | kind-emulator
  -h, --help               Show help
```

**llm-d stack** (gateway, EPP, ModelService): deploy using the [llm-d guides](https://github.com/llm-d/llm-d/tree/main/guides/optimized-baseline) directly. For EPP-only setup (GAIE standalone chart + tokenreview RBAC), use `deploy/install-epp.sh` after `install.sh`.

**Environment variables** (see [Configuration Reference](#configuration-reference)):

```bash
# Always set for install.sh
export ENVIRONMENT="kubernetes"   # or openshift, kind-emulator
export LLMD_NS="llm-d-optimized-baseline"  # namespace WVA watches

# When DEPLOY_WVA=true (default)
export WVA_IMAGE_REPO="ghcr.io/llm-d/llm-d-workload-variant-autoscaler"
export WVA_IMAGE_TAG="latest"

# Optional
export DEPLOY_WVA=false                     # Monitoring + scaler only
export DEPLOY_PROMETHEUS=false
export DEPLOY_OPERATIONAL_DASHBOARD=true    # Deploy Grafana and operational dashboard
# export DEPLOY_LWS=true           # Install LeaderWorkerSet (needed for full e2e suite; default false)
```

#### Script deployment examples

**VariantAutoscaling** and **HPA** resources are not created by `install.sh`; create them directly with `kubectl apply` or let tests/operators manage them.

##### Example 1: Base WVA infra + EPP

```bash
./deploy/install.sh -e kubernetes
# EPP (GAIE standalone chart + RBAC):
LLM_D_RELEASE=v0.7.0 GAIE_VERSION=v1.5.0 LLMD_NS=llm-d-optimized-baseline \
  ./deploy/install-epp.sh
# Model server: follow llm-d/llm-d guides/optimized-baseline
```

##### Example 2: E2E-style stack (same as `make deploy-e2e-infra`)

```bash
make deploy-e2e-infra ENVIRONMENT=kind-emulator IMG=localhost/llm-d-workload-variant-autoscaler:dev
```

##### Example 3: WVA + monitoring only (no llm-d)

```bash
export DEPLOY_WVA=true
export DEPLOY_PROMETHEUS=true
export DEPLOY_OPERATIONAL_DASHBOARD=true
export DEPLOY_PROMETHEUS_ADAPTER=true
./deploy/install.sh -e kubernetes
```

##### Example 4: Install with LeaderWorkerSet (for full e2e suite)

```bash
export DEPLOY_LWS=true
./deploy/install.sh -e kubernetes
```

### Method 2: Kustomize (direct controller install)

Install the WVA controller directly into an existing cluster using Kustomize. This is the recommended method when you already have Prometheus and want to manage the controller install without the full automated script.

#### Kubernetes

```bash
# Set the controller image
cd config/base/manager
kustomize edit set image controller=ghcr.io/llm-d/llm-d-workload-variant-autoscaler:v0.7.0

# Apply
kubectl apply -k ../../overlays/cluster-scoped/kubernetes
```

#### OpenShift

```bash
cd config/base/manager
kustomize edit set image controller=ghcr.io/llm-d/llm-d-workload-variant-autoscaler:v0.7.0

kubectl apply -k ../../overlays/namespace-scoped/openshift
```

#### Undeploy

```bash
kubectl delete -k config/overlays/cluster-scoped/kubernetes    # or config/overlays/namespace-scoped/openshift
```

### Legacy: Helm Chart (Deprecated)

> **This Helm chart is deprecated.** Use [Kustomize](#method-2-kustomize-direct-controller-install) instead.
> The chart will be removed in the next minor release.

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
  namespace: llm-d-optimized-baseline
  modelName: optimized-baseline-nvidia-gpu-vllm-decode
  modelID: "unsloth/Meta-Llama-3.1-8B"

# VariantAutoscaling Configuration
va:
  enabled: true           # Create VariantAutoscaling CR
  # accelerator: Optional. If not specified, it will be auto-discovered
  # from target deployment. If specified, it will be used as fall-back value if it can't 
  # be discovered.
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
  # accelerator: Optional. If not specified, it will be auto-discovered
  # from target deployment. If specified, it will be used as fall-back value if it can't 
  # be discovered.
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
  namespace: llm-d-optimized-baseline
  labels:
    inference.optimization/acceleratorName: A100
spec:
  # Model identifier
  modelID: "unsloth/Meta-Llama-3.1-8B"
EOF
```

> **Required**: Before creating the VA, ensure the `llm-d.ai/variant` label is set on your Deployment or LeaderWorkerSet pod template with value `my-vllm-deployment-decode` (matching the VA name above). See [Workload Requirements](#workload-requirements).

#### Creating HPA Manually

If using HPA with external metrics:

```bash
cat <<EOF | kubectl apply -f -
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: vllm-deployment-hpa
  namespace: llm-d-optimized-baseline
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
            exported_namespace: $NAMESPACE
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
| `WVA_BASE_NAME` | Controller base name | `optimized-baseline` |

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
| `LLMD_NS` | llm-d namespace | `llm-d-optimized-baseline` |

#### Deployment Flags (`install.sh`)

| Variable | Description | Default |
|----------|-------------|---------|
| `DEPLOY_PROMETHEUS` | Deploy Prometheus stack | `true` |
| `DEPLOY_OPERATIONAL_DASHBOARD` | Deploy Grafana and operational dashboard | `true` |
| `DEPLOY_WVA` | Deploy WVA controller | `true` |
| `DEPLOY_PROMETHEUS_ADAPTER` | Deploy Prometheus Adapter (when `SCALER_BACKEND=prometheus-adapter`) | `true` |
| `DEPLOY_LWS` | Deploy LeaderWorkerSet (needed only for full e2e suite; skip for smoke, benchmarks, or pre-installed clusters) | `false` |
| `SKIP_CHECKS` | Skip prerequisite checks | `false` |
| `SCALER_BACKEND` | `prometheus-adapter`, `keda`, or `none` | `prometheus-adapter` |

VariantAutoscaling, HPA stabilization, and vLLM ModelService tuning are not controlled by `install.sh`; manage them via `kubectl apply` directly (see the [llm-d guides](https://github.com/llm-d/llm-d/tree/main/guides/optimized-baseline) for reference manifests).

#### Advanced (`install.sh`)

| Variable | Description | Default |
|----------|-------------|---------|
| `SKIP_TLS_VERIFY` | Skip TLS verification | Auto-detected |
| `WVA_LOG_LEVEL` | WVA logging level | `info` |
| `VLLM_SVC_ENABLED` | Enable vLLM Service in chart | `true` |
| `VLLM_SVC_NODEPORT` | vLLM NodePort | `30000` |
| `LWS_NAMESPACE` | Namespace for LeaderWorkerSet installation | `lws-system` |
| `LWS_CHART_VERSION` | LeaderWorkerSet Helm chart version | `0.8.0` |

#### Optional: capacity thresholds after `make deploy-e2e-infra`

If `KV_SPARE_TRIGGER` and/or `QUEUE_SPARE_TRIGGER` are set in the environment, the Makefile patches the `wva-saturation-scaling-config` ConfigMap after install.

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
# Deploy WVA infra, then tune vLLM max-num-seqs in the llm-d ModelService manifest
export VLLM_MAX_NUM_SEQS=8
make deploy-wva-on-k8s   # runs install.sh (WVA + monitoring + scaler + LWS)
# Apply llm-d model serving manifests separately via kubectl apply or the llm-d guides
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
kubectl describe deployment controller-manager -n workload-variant-autoscaler-system

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
- **Kustomize overlays**: [config/overlays/cluster-scoped/kubernetes](../config/overlays/cluster-scoped/kubernetes/), [config/overlays/namespace-scoped/openshift](../config/overlays/namespace-scoped/openshift/)
- **Helm Chart (deprecated)**: [charts/workload-variant-autoscaler](../charts/workload-variant-autoscaler/)
- **API Reference**: [api/v1alpha1](../api/v1alpha1/)
- **Architecture**: [docs/design/modeling-optimization.md](../docs/design/modeling-optimization.md)
