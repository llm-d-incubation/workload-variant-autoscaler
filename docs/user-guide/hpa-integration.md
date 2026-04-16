# HPA Integration with the workload-variant-autoscaler

This guide shows how to integrate Kubernetes HorizontalPodAutoscaler (HPA) with the WVA using the existing deployment environment.

## Overview

After deploying the workload-variant-autoscaler following the provided guides, this guide allows the integration of the following components:

1. **WVA controller**: processes VariantAutoscaling objects and emits the `wva_current_replicas`, the `wva_desired_replicas` and the `wva_desired_ratio` metrics

2. **Prometheus**: scrapes these metrics from the workload-variant-autoscaler `/metrics` endpoint using TLS

3. **Prometheus Adapter**: exposes the metrics to Kubernetes external metrics API

4. **HPA** example configuration: reads the value for the `wva_desired_replicas` metrics and adjusts Deployment replicas accordingly, using an `AverageValue` target

## Prerequisites

- workload-variant-autoscaler deployed (follow [the README guide](../README.md) for the steps to deploy it)
- Prometheus stack already running in `workload-variant-autoscaler-monitoring` namespace
- All components must be fully ready before proceeding: 2-3 minutes may be needed after the deployment

## Quick Setup

> **Note**: The required RBAC permissions for Prometheus to access WVA's secure HTTPS metrics endpoint are automatically deployed via `config/rbac/prometheus_metrics_auth_role_binding.yaml`.

### 1. Create Prometheus CA ConfigMap

Prometheus is deployed with TLS (HTTPS) for security. The Prometheus Adapter needs to connect to Prometheus at https://kube-prometheus-stack-prometheus.workload-variant-autoscaler-monitoring.svc.cluster.local.
But Prometheus uses self-signed certificates (not trusted by default). We will use a CA configmap for TLS Certificate Verification:

```sh
# Extract the TLS certificate from the prometheus-tls secret
kubectl get secret prometheus-tls -n workload-variant-autoscaler-monitoring -o jsonpath='{.data.tls\.crt}' | base64 -d > /tmp/prometheus-ca.crt

# Create ConfigMap with the certificate
kubectl create configmap prometheus-ca --from-file=ca.crt=/tmp/prometheus-ca.crt -n workload-variant-autoscaler-monitoring
```

### 2. Deploy the Prometheus Adapter

Note: a `yaml` example snippet for the Prometheus Adapter configuration with TLS can be found [at the end of this doc](#prometheus-adapter-values-configsamplesprometheus-adapter-valuesyaml).

```sh
# Add Prometheus community helm repo - already there if you deployed workload-variant-autoscaler using the scripts
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update

# Deploy Prometheus Adapter with workload-variant-autoscaler metrics configuration
helm install prometheus-adapter prometheus-community/prometheus-adapter \
  -n workload-variant-autoscaler-monitoring \
  -f config/samples/prometheus-adapter-values.yaml
```

### 3. Wait for Prometheus to discover and fetch metrics emitted by the workload-variant-autoscaler (30-60 seconds)

### 4. Create the VariantAutoscaling resource

```sh
# Apply the VariantAutoscaling resource if not already there (ensure target Deployment exists, e.g. from kind-emulator)
kubectl apply -f config/samples/hpa/va.yaml
```

### 5. Deploy the HPA resource

Note: a `yaml` example snippet for HPA can be found [at the end of this doc](#hpa-configuration-example-configsampleshpa-integrationyaml).

```sh
# Deploy HPA for your deployments
kubectl apply -f config/samples/hpa/hpa.yaml
```

### 6. Verify the integration

- Wait for all components to be ready (1-2 minutes total)

- Check the status of HPA (should show actual target values, not `<unknown>/1`):

```sh
kubectl get hpa -n llm-d-sim
NAME                     REFERENCE                       TARGETS     MINPODS   MAXPODS   REPLICAS   AGE
sample-deployment-hpa   Deployment/sample-deployment   1/1 (avg)   1         10        1          3m14s
```

- Check the VariantAutoscaling resource:

```sh
kubectl get variantautoscaling -n llm-d-sim
NAME                 MODEL             ACCELERATOR   CURRENTREPLICAS   OPTIMIZED   AGE
sample-deployment   default/default   A100          1                 1           39m
```

- Check if the external metrics are available:

```sh
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1" | jq

{
  "kind": "APIResourceList",
  "apiVersion": "v1",
  "groupVersion": "external.metrics.k8s.io/v1beta1",
  "resources": [
    {
      "name": "wva_desired_replicas",
      "singularName": "",
      "namespaced": true,
      "kind": "ExternalMetricValueList",
      "verbs": [
        "get"
      ]
    }
  ]
}
```

- Get the latest value for the `wva_desired_replicas` metric:

```sh
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/llm-d-sim/wva_desired_replicas?labelSelector=variant_name%3Dsample-deployment" | jq
{
  "kind": "ExternalMetricValueList",
  "apiVersion": "external.metrics.k8s.io/v1beta1",
  "metadata": {},
  "items": [
    {
      "metricName": "wva_desired_replicas",
      "metricLabels": {
        "__name__": "wva_desired_replicas",
        "accelerator_type": "A100",
        "endpoint": "https",
        "exported_namespace": "llm-d-sim",
        "instance": "10.244.2.6:8443",
        "job": "workload-variant-autoscaler-controller-manager-metrics-service",
        "namespace": "workload-variant-autoscaler-system",
        "pod": "workload-variant-autoscaler-controller-manager-99c9d77cb-ppjm8",
        "service": "workload-variant-autoscaler-controller-manager-metrics-service",
        "variant_name": "sample-deployment"
      },
      "timestamp": "2025-08-22T19:08:26Z",
      "value": "1"
    }
  ]
}
```

## Example: scale-up scenario

1. Port-forward the Service/Gateway (depending on whether you deployed the workload-variant-autoscaler with `llm-d` or not):

```sh
# If you deployed workload-variant-autoscaler with llm-d:
kubectl port-forward -n llm-d-sim svc/infra-sim-inference-gateway 8000:80

# If you deployed workload-variant-autoscaler without llm-d, port-forward your model service (e.g. the Deployment's Service) on 8000:80.
```

2. Launch the load generator (burst script; requires only `curl`). From repo root, with the vLLM service port-forwarded to localhost:8000:

```sh
export TARGET_URL="http://localhost:8000/v1/chat/completions"
export MODEL_ID="unsloth/Meta-Llama-3.1-8B"
export TOTAL_REQUESTS=200
export BATCH_SIZE=20
./hack/burst_load_generator.sh
```

3. After a few minutes, you can see the scale out:

```sh
kubectl get hpa -n llm-d-sim
NAME                   REFERENCE                     TARGETS     MINPODS   MAXPODS   REPLICAS   AGE
sample-deployment-hpa   Deployment/sample-deployment   1/1 (avg)   1         10        2          20m

kubectl get variantautoscaling -n llm-d-sim
NAME               MODEL             ACCELERATOR   CURRENTREPLICAS   OPTIMIZED   AGE
sample-deployment   default/default   A100          1                 2           20m

kubectl get deployments.apps -n llm-d-sim
NAME               READY   UP-TO-DATE   AVAILABLE   AGE
sample-deployment   2/2     2            2           21m
```

It can be verified that the workload-variant-autoscaler is optimizing and emitting metrics:

```sh
kubectl logs -n workload-variant-autoscaler-system deploy/workload-variant-autoscaler-controller-manager

###
2025-08-22T18:47:50.131850600Z {"level":"DEBUG","ts":"2025-08-22T18:47:50.131Z","msg":"System data prepared for optimization: - { accelerators: [  {   name: G2,   type: Intel-Gaudi-2-96GB,   multiplicity: 1,   memSize: 0,   memBW: 0,   power: {    idle: 0,    full: 0,    midPower: 0,    midUtil: 0   },   cost: 23  },  {   name: MI300X,   type: AMD-MI300X-192GB,   multiplicity: 1,   memSize: 0,   memBW: 0,   power: {    idle: 0,    full: 0,    midPower: 0,    midUtil: 0   },   cost: 65  },  {   name: A100,   type: NVIDIA-A100-PCIE-80GB,   multiplicity: 1,   memSize: 0,   memBW: 0,   power: {    idle: 0,    full: 0,    midPower: 0,    midUtil: 0   },   cost: 40  } ]}"}
2025-08-22T18:47:50.131912017Z {"level":"DEBUG","ts":"2025-08-22T18:47:50.131Z","msg":"System data prepared for optimization: - { serviceClasses: [  {   name: Premium,   model: default/default,   priority: 1,   slo-itl: 24,   slo-ttw: 500,   slo-tps: 0  },  {   name: Premium,   model: llama0-70b,   priority: 1,   slo-itl: 80,   slo-ttw: 500,   slo-tps: 0  },  {   name: Freemium,   model: granite-13b,   priority: 10,   slo-itl: 200,   slo-ttw: 2000,   slo-tps: 0  },  {   name: Freemium,   model: llama0-7b,   priority: 10,   slo-itl: 150,   slo-ttw: 1500,   slo-tps: 0  } ]}"}
2025-08-22T18:47:50.131943892Z {"level":"DEBUG","ts":"2025-08-22T18:47:50.131Z","msg":"System data prepared for optimization: - { models: [  {   name: default/default,   acc: A100,   accCount: 1,   alpha: 20.58,   beta: 0.41,   maxBatchSize: 4,   atTokens: 0  } ]}"}
2025-08-22T18:47:50.131989100Z {"level":"DEBUG","ts":"2025-08-22T18:47:50.131Z","msg":"System data prepared for optimization: - { optimizer: {  unlimited: true,  saturationPolicy: None }}"}
2025-08-22T18:47:50.132035975Z {"level":"DEBUG","ts":"2025-08-22T18:47:50.132Z","msg":"System data prepared for optimization: - { servers: [  {   name: sample-deployment:llm-d-sim,   class: Premium,   model: default/default,   keepAccelerator: true,   minNumReplicas: 1,   maxBatchSize: 4,   currentAlloc: {    accelerator: A100,    numReplicas: 2,    maxBatch: 256,    cost: 80,    itlAverage: 20,    waitAverage: 0,    load: {     arrivalRate: 40,     avgLength: 178,     arrivalCOV: 0,     serviceCOV: 0    }   },   desiredAlloc: {    accelerator: ,    numReplicas: 0,    maxBatch: 0,    cost: 0,    itlAverage: 0,    waitAverage: 0,    load: {     arrivalRate: 0,     avgLength: 0,     arrivalCOV: 0,     serviceCOV: 0    }   }  } ]}"}
2025-08-22T18:47:50.132082392Z {"level":"DEBUG","ts":"2025-08-22T18:47:50.132Z","msg":"Optimization solution - system: Solution: \ns=sample-deployment:llm-d-sim; c=Premium; m=default/default; rate=40; tk=178; sol=1, sat=false, alloc={acc=A100; num=2; maxBatch=4; cost=80, val=0, servTime=21.49347, waitTime=69.7666, rho=0.71789724, maxRPM=25.31145}; slo-itl=24, slo-ttw=500, slo-tps=0 \nAllocationByType: \nname=NVIDIA-A100-PCIE-80GB, count=2, limit=2, cost=80 \ntotalCost=80 \n"}
2025-08-22T18:47:50.132135142Z {"level":"DEBUG","ts":"2025-08-22T18:47:50.132Z","msg":"Optimization completed successfully, emitting optimization metrics"}
2025-08-22T18:47:50.132148142Z {"level":"DEBUG","ts":"2025-08-22T18:47:50.132Z","msg":"Optimized allocation map - numKeys: 1, updateList_count: 1"}
2025-08-22T18:47:50.132165642Z {"level":"DEBUG","ts":"2025-08-22T18:47:50.132Z","msg":"Optimized allocation entry - key: sample-deployment, value: {2025-08-22 18:47:50.1321171 +0000 UTC m=+1620.775291857 A100 2}"}
2025-08-22T18:47:50.132178183Z {"level":"DEBUG","ts":"2025-08-22T18:47:50.132Z","msg":"Optimization metrics emitted, starting to process variants - variant_count: 1"}
2025-08-22T18:47:50.132288225Z {"level":"DEBUG","ts":"2025-08-22T18:47:50.132Z","msg":"Processing variant - index: 0, variantAutoscaling-name: sample-deployment, namespace: llm-d-sim, has_optimized_alloc: true"}
2025-08-22T18:47:50.132290017Z {"level":"DEBUG","ts":"2025-08-22T18:47:50.132Z","msg":"EmitReplicaMetrics completed successfullyvariant sample-deployment"}
2025-08-22T18:47:50.132291350Z {"level":"INFO","ts":"2025-08-22T18:47:50.132Z","msg":"Emitted optimization signals for external autoscaler consumptionvariant sample-deploymentnamespacellm-d-sim"}
2025-08-22T18:47:50.132292725Z {"level":"DEBUG","ts":"2025-08-22T18:47:50.132Z","msg":"Successfully emitted optimization signals for external autoscalersvariant sample-deployment"}
2025-08-22T18:47:50.141451683Z {"level":"DEBUG","ts":"2025-08-22T18:47:50.141Z","msg":"Completed variant processing loop"}
2025-08-22T18:47:50.141458767Z {"level":"INFO","ts":"2025-08-22T18:47:50.141Z","msg":"Reconciliation completed - variants_processed: 1, optimization_successful: true"}
```

## Feature: Scale to Zero

The WVA can leverage on HPA's *alpha* feature for scale to zero functionality, enabling complete resource optimization by scaling deployments down to zero replicas when no load is detected.

To enable `HPAScaleToZero`, you need to enable the corresponding feature flags in the Kind cluster configuration:

1. Find the control-plane node:

```sh
docker ps --filter "name=kind"
CONTAINER ID   IMAGE                  COMMAND                  CREATED             STATUS             PORTS                       NAMES
92af0e9bb762   kindest/node:v1.32.0   "/usr/local/bin/entr…"   About an hour ago   Up About an hour   127.0.0.1:63647->6443/tcp   kind-inferno-gpu-cluster-control-plane
b10130b20176   kindest/node:v1.32.0   "/usr/local/bin/entr…"   About an hour ago   Up About an hour                               kind-inferno-gpu-cluster-worker2
4f649e2ceb92   kindest/node:v1.32.0   "/usr/local/bin/entr…"   About an hour ago   Up About an hour                               kind-inferno-gpu-cluster-worker
```

2. Open a shell into that container:

```sh
docker exec -it kind-inferno-gpu-cluster-control-plane bash
```

3. Apply the feature flag to the `api-server` manifest:

**Note**: these changes may take some time to be applied.

```sh
sed -i 's#- kube-apiserver#- kube-apiserver\n    - --feature-gates=HPAScaleToZero=true#g' /etc/kubernetes/manifests/kube-apiserver.yaml
### Wait for some time
```

4. Verify that the feature is enabled on the `api-server`:

```sh
kubectl -n kube-system get pod -l component=kube-apiserver -o yaml | grep -A2 feature-gates

      - --feature-gates=HPAScaleToZero=true
      - --advertise-address=172.18.0.3
      - --allow-privileged=true
```

5. Apply the feature flag to the `controller-manager` manifest:

**Note**: these changes may take some time to be applied.

```sh
sed -i 's#- kube-controller-manager#- kube-controller-manager\n    - --feature-gates=HPAScaleToZero=true#g' /etc/kubernetes/manifests/kube-controller-manager.yaml
### Wait for some time
```

6. Verify that the feature is enabled on the `api-server`:

```sh
kubectl -n kube-system get pod -l component=kube-controller-manager -o yaml | grep -A2 feature-gates

      - --feature-gates=HPAScaleToZero=true
      - --allocate-node-cidrs=true
      - --authentication-kubeconfig=/etc/kubernetes/controller-manager.conf
```

7. Specify the `minReplicas: 0` field in the `yaml` snippet for HPA and apply it following the integration steps

### Automatic Setup with Kind Cluster Script

When using the `deploy/kind-emulator/setup.sh` script, the HPAScaleToZero feature gate is **enabled by default**. You can disable it if needed:

```sh
# Default: HPAScaleToZero enabled
./deploy/kind-emulator/setup.sh

# Disable HPAScaleToZero feature gate
ENABLE_SCALE_TO_ZERO=false ./deploy/kind-emulator/setup.sh
```

### OpenShift Cluster Configuration

On OpenShift, the HPAScaleToZero feature gate must be enabled at the cluster level by a cluster administrator. This requires modifying the FeatureGate custom resource:

> **Warning**: Modifying FeatureGate on OpenShift may impact cluster stability as HPAScaleToZero is a TechPreview/alpha feature. Consult your cluster administrator before making these changes.

1. Check current FeatureGate configuration:

```sh
oc get featuregate cluster -o yaml
```

2. Enable HPAScaleToZero feature gate:

```sh
oc patch featuregate cluster --type=merge -p '{
  "spec": {
    "featureSet": "CustomNoUpgrade",
    "customNoUpgrade": {
      "enabled": ["HPAScaleToZero"]
    }
  }
}'
```

3. Wait for the cluster to apply the changes (this may take several minutes as nodes are updated):

```sh
oc get clusterversion -w
```

4. Verify the feature gate is enabled:

```sh
oc get featuregate cluster -o jsonpath='{.spec.customNoUpgrade.enabled}'
```

5. Deploy HPA with `minReplicas: 0`:

```sh
# Using install script
HPA_MIN_REPLICAS=0 ./deploy/install.sh -e openshift

# Or via Helm
helm upgrade workload-variant-autoscaler ./charts/workload-variant-autoscaler \
  --set hpa.minReplicas=0
```

### Note on possible timing issues

For this discussion, please refer to the [community doc](https://docs.google.com/document/d/15z1u2HIH7qoxT-nxj4BnZ_TyqHPqIn0FcCPTnIMn7bs/edit?tab=t.0).

## Configuration Files

### HPA Behavior Configuration

HPA behavior (stabilization windows and scaling policies) can be configured at deployment time through Helm values, eliminating the need for post-deployment patching.

#### Via Helm Chart

The WVA Helm chart includes a fully configurable `hpa.behavior` section:

**Production Configuration (Default):**
```yaml
hpa:
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 240  # Wait 4 minutes before scaling up
      selectPolicy: Max
      policies:
        - type: Pods
          value: 10
          periodSeconds: 150
    scaleDown:
      stabilizationWindowSeconds: 240  # Wait 4 minutes before scaling down
      selectPolicy: Max
      policies:
        - type: Pods
          value: 10
          periodSeconds: 150
```

**Deployment Examples:**

```bash
# Production: Conservative scaling (240s stabilization)
helm install workload-variant-autoscaler ./charts/workload-variant-autoscaler \
  --set hpa.behavior.scaleUp.stabilizationWindowSeconds=240 \
  --set hpa.behavior.scaleDown.stabilizationWindowSeconds=240

# Development: Fast scaling (30s stabilization)
helm install workload-variant-autoscaler ./charts/workload-variant-autoscaler \
  --set hpa.behavior.scaleUp.stabilizationWindowSeconds=30 \
  --set hpa.behavior.scaleDown.stabilizationWindowSeconds=30

# E2E Testing: Very fast scaling (0s stabilization)
helm install workload-variant-autoscaler ./charts/workload-variant-autoscaler \
  --set hpa.behavior.scaleUp.stabilizationWindowSeconds=0 \
  --set hpa.behavior.scaleDown.stabilizationWindowSeconds=0
```

#### Via Deployment Script

The `deploy/install.sh` script supports the `HPA_STABILIZATION_SECONDS` environment variable:

```bash
# Custom stabilization window
HPA_STABILIZATION_SECONDS=120 ./deploy/install.sh

# Production default (240 seconds)
./deploy/install.sh

# E2E testing (30 seconds)
HPA_STABILIZATION_SECONDS=30 ./deploy/install.sh
```

#### Via values.yaml

For persistent configuration, edit `values.yaml`:

```yaml
hpa:
  enabled: true
  maxReplicas: 10
  targetAverageValue: "1"
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 180  # Your custom value
      selectPolicy: Max
      policies:
        - type: Pods
          value: 10
          periodSeconds: 150
    scaleDown:
      stabilizationWindowSeconds: 180  # Your custom value
      selectPolicy: Max
      policies:
        - type: Pods
          value: 10
          periodSeconds: 150
```

#### Best Practices

| Environment | Stabilization Window | Rationale |
|-------------|---------------------|-----------|
| **Production** | 120-300s | Prevents flapping, ensures stability |
| **Staging** | 60-120s | Balance between stability and responsiveness |
| **Development** | 30-60s | Faster iteration, acceptable for dev/test |
| **E2E Tests** | 0-30s | Rapid validation, acceptable for automated tests |

**Key Parameters:**

- **stabilizationWindowSeconds**: Time HPA waits before applying scaling decisions
  - Higher values = more stable (prevents flapping)
  - Lower values = more responsive (faster scaling)

- **selectPolicy**: How to choose from multiple policies
  - `Max`: Maximum scale allowed by any policy (most aggressive)
  - `Min`: Minimum scale allowed by any policy (most conservative)
  - `Disabled`: Disable scaling in that direction

- **policies**: List of rate-limiting policies
  - `type: Pods`: Absolute number of pods per period
  - `type: Percent`: Percentage of current pods per period
  - `value`: Amount to scale
  - `periodSeconds`: Time window for the policy

**Important:** The example below shows testing configuration with `stabilizationWindowSeconds: 0`. For production deployments, use the Helm chart's configurable behavior settings as described above.

### Prometheus Adapter Values (`config/samples/prometheus-adapter-values.yaml`)

```yaml
prometheus:
  url: https://kube-prometheus-stack-prometheus.workload-variant-autoscaler-monitoring.svc.cluster.local
  port: 9090

rules:
  external:
  - seriesQuery: 'wva_desired_replicas{variant_name!="",exported_namespace!=""}'
    resources:
      overrides:
        exported_namespace: {resource: "namespace"}
        variant_name: {resource: "deployment"}
    name:
      matches: "^wva_desired_replicas"
      as: "wva_desired_replicas"
    metricsQuery: 'wva_desired_replicas{<<.LabelMatchers>>}'

replicas: 2
logLevel: 4

tls:
  enable: false # Inbound TLS (Client → Adapter)

extraVolumes:
  - name: prometheus-ca
    configMap:
      name: prometheus-ca

extraVolumeMounts:
  - name: prometheus-ca
    mountPath: /etc/prometheus-ca
    readOnly: true

extraArguments:
  - --prometheus-ca-file=/etc/prometheus-ca/ca.crt
```

### HPA Configuration Example (`config/samples/hpa/hpa.yaml`)

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: sample-deployment-hpa
  namespace: llm-d-sim
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: sample-deployment
  minReplicas: 0  # HPAScaleToZero - alpha feature
  maxReplicas: 10
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 0
      policies:
      - type: Pods
        value: 10
        periodSeconds: 15
    scaleDown:
      stabilizationWindowSeconds: 0
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
            variant_name: sample-deployment
      target:
        type: AverageValue
        averageValue: "1"
```
