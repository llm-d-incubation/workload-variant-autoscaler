# KEDA Integration with the Workload-Variant-Autoscaler

This document describes how to integrate the Kubernetes Event-driven Autoscaler (KEDA) with the Workload-Variant-Autoscaler (WVA) sing the existing deployment environment.

## Overview

After deploying the Workload-Variant-Autoscaler following the provided guides, this guide allows the integration of the following components:

1. **WVA Controller**: processes VariantAutoscaling objects and emits the `inferno_current_replicas`, the `inferno_desired_replicas` and the `inferno_desired_ratio` metrics

2. **Prometheus**: scrapes these metrics from the Workload-Variant-Autoscaler `/metrics` endpoint using TLS

3. **KEDA** example configuration:

- Deploys a `ScaledObject` and configures the underlying HPA for the Deployment.

- Reads the values for the `inferno_desired_replicas` metrics and adjusts Deployment replicas accordingly, using an `AverageValue` target.

- Natively supports scale to zero.

## Prerequisites

- workload-variant-autoscaler deployed (follow [the README guide](../README.md) for the steps to deploy it)
- Prometheus stack already running in `workload-variant-autoscaler-monitoring` namespace
- All components must be fully ready before proceeding: 2-3 minutes may be needed after the deployment

## Quick Setup

1. Install KEDA:

```bash
# Adds the KEDA core Helm repository
helm repo add kedacore https://kedacore.github.io/charts
helm repo update

# Installs KEDA in keda-system namespace
helm install keda kedacore/keda \
  --namespace keda-system \
  --create-namespace \
  --set prometheus.metricServer.enabled=true \
  --set prometheus.operator.enabled=true
```

2. Apply the sample KEDA ScaledObject configuration. A basic `yaml` example snippet for the ScaledObject targeting the vLLM-emulator deployment can be found [at the end of this doc](#keda-scaleobject-configuration-example-configsampleskeda-scaled-object-vllmeyaml).

```bash
# After creating config/samples/scaledobject-vllme.yaml
kubectl apply -f config/samples/scaledobject-vllme.yaml
```

3. Verify the installation:

```bash
# Check KEDA resources
kubectl get scaledobjects -n llm-d-sim

kubectl get scaledobjects.keda.sh -n llm-d-sim                              
NAME                      SCALETARGETKIND      SCALETARGETNAME    MIN   MAX   READY   ACTIVE   FALLBACK   PAUSED    TRIGGERS     AUTHENTICATIONS   AGE
vllme-deployment-scaler   apps/v1.Deployment   vllme-deployment         10    True    False    False      Unknown   prometheus                     33m
```

```bash
## Check the KEDA Operator logs
kubectl logs -n keda-system deployment/keda-operator

# Shows scaling events
kubectl get events -n llm-d-sim --field-selector type=Normal
```

**Note**: using the sample configuration, KEDA will scale down the Deployment to 0, until it fetches metrics that go beyond the threshold value.

4. Apply the VariantAutoscaling resource for the Deployment, so that the workload-variant-autoscaler starts emitting metrics:

```bash
# For the default vllme-deployment
kubectl apply -f deploy/examples/vllm-emulator/vllme-setup/vllme-variantautoscaling.yaml
```

5. Verify the HorizontalPodAutoscaler deployed by the ScaledObject and used by KEDA. After a while, you should see that it is correcly receiving metrics:

```bash
kubectl get hpa -n llm-d-sim

NAME                            REFERENCE                     TARGETS     MINPODS   MAXPODS   REPLICAS   AGE
wva-keda-hpa-vllme-deployment   Deployment/vllme-deployment   1/1 (avg)   1         10        1          40s
```

6. Verify that metrics are being emitted:

```bash
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1" | jq

{
  "kind": "APIResourceList",
  "apiVersion": "v1",
  "groupVersion": "external.metrics.k8s.io/v1beta1",
  "resources": [
    {
      "name": "externalmetrics",
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

```bash
# Check specific metric for the vLLM-e ScaledObject
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/llm-d-sim/s0-prometheus?labelSelector=scaledobject.keda.sh%2Fname%3Dvllme-deployment-scaler" | jq

{
  "kind": "ExternalMetricValueList",
  "apiVersion": "external.metrics.k8s.io/v1beta1",
  "metadata": {},
  "items": [
    {
      "metricName": "s0-prometheus",
      "metricLabels": null,
      "timestamp": "2025-08-29T13:31:00Z",
      "value": "1"
    }
  ]
}
```

**Note**: KEDA creates its own external metric names based on the trigger configuration. The original Prometheus metric `inferno_desired_replicas` is therefore used to emit  the `s0-prometheus` metric in KEDA's external metrics API, where:

- `s0` = Scaler index (first scaler = 0)
- `prometheus` = Scaler type

To query KEDA external metrics, you must use:

- The KEDA-generated metric name (`s0-prometheus`)
- The proper label selector (`scaledobject.keda.sh/name=<scaledobject-name>`)

### 7. Scaling Events Monitoring

```bash
# Watch all scaling events in the namespace
kubectl get events -n llm-d-sim --field-selector type=Normal -w

### ...
37m         Normal   KEDAScalersStarted           scaledobject/vllme-deployment-scaler                      Scaler prometheus is built
37m         Normal   KEDAScalersStarted           scaledobject/vllme-deployment-scaler                      Started scalers watch
37m         Normal   ScaledObjectReady            scaledobject/vllme-deployment-scaler                      ScaledObject is ready for scaling
36m         Normal   KEDAScaleTargetDeactivated   scaledobject/vllme-deployment-scaler                      Deactivated apps/v1.Deployment llm-d-sim/vllme-deployment from 1 to 0
3m37s       Normal   KEDAScaleTargetActivated     scaledobject/vllme-deployment-scaler                      Scaled apps/v1.Deployment llm-d-sim/vllme-deployment from 0 to 1, triggered by inferno-desired-replicas
3m37s       Normal   ScalingReplicaSet            deployment/vllme-deployment                               Scaled up replica set vllme-deployment-64f7cd79f5 from 0 to 1
```

```bash
# Monitor deployment scaling events specifically  
kubectl get events -n llm-d-sim --field-selector involvedObject.name=vllme-deployment
```

```bash
# Check KEDA-specific events
kubectl get events -n llm-d-sim | grep -i keda

# ...
38m         Normal    KEDAScalersStarted           scaledobject/vllme-deployment-scaler                      Scaler prometheus is built
38m         Normal    KEDAScalersStarted           scaledobject/vllme-deployment-scaler                      Started scalers watch
38m         Normal    KEDAScaleTargetDeactivated   scaledobject/vllme-deployment-scaler                      Deactivated apps/v1.Deployment llm-d-sim/vllme-deployment from 1 to 0
4m55s       Normal    KEDAScaleTargetActivated     scaledobject/vllme-deployment-scaler                      Scaled apps/v1.Deployment llm-d-sim/vllme-deployment from 0 to 1, triggered by inferno-desired-replicas
```

## Example: scale-up scenario

1. Port-forward the Gateway:

```sh
# If you deployed workload-variant-autoscaler with llm-d:
kubectl port-forward -n llm-d-sim svc/infra-sim-inference-gateway 8000:80 
```

2. Launch the load generator via the following command:

```sh
cd tools/vllm-emulator
pip install -r requirements.txt
python loadgen.py --model default/default  --rate '[[1200, 40]]' --url http://localhost:8000/v1 --content 50
```

3. After a few minutes, you can see the scale out:

```sh
kubectl get hpa -n llm-d-sim

NAME                            REFERENCE                     TARGETS     MINPODS   MAXPODS   REPLICAS   AGE
wva-keda-hpa-vllme-deployment   Deployment/vllme-deployment   1/1 (avg)   1         10        1          114s
wva-keda-hpa-vllme-deployment   Deployment/vllme-deployment   2/1 (avg)   1         10        1          6m1s
wva-keda-hpa-vllme-deployment   Deployment/vllme-deployment   1/1 (avg)   1         10        2          6m16s

kubectl get va -n llm-d-sim 
NAME               MODEL             ACCELERATOR   CURRENTREPLICAS   OPTIMIZED   AGE
vllme-deployment   default/default   A100          1                 2           11m

kubectl get deployments.apps -n llm-d-sim
NAME               READY   UP-TO-DATE   AVAILABLE   AGE
vllme-deployment   2/2     2            2           12m
```

It can be verified that the workload-variant-autoscaler is optimizing and emitting metrics:

```sh
kubectl logs -n workload-variant-autoscaler-system deploy/workload-variant-autoscaler-controller-manager

###
2025-09-12T17:03:42.153155510Z {"level":"DEBUG","ts":"2025-09-12T17:03:42.152Z","msg":"Found inventory: nodeName - kind-inferno-gpu-cluster-control-plane , model - NVIDIA-A100-PCIE-80GB , count - 2 , mem - 81920"}
2025-09-12T17:03:42.153174593Z {"level":"DEBUG","ts":"2025-09-12T17:03:42.153Z","msg":"Found inventory: nodeName - kind-inferno-gpu-cluster-worker , model - AMD-MI300X-192G , count - 2 , mem - 196608"}
2025-09-12T17:03:42.153176093Z {"level":"DEBUG","ts":"2025-09-12T17:03:42.153Z","msg":"Found inventory: nodeName - kind-inferno-gpu-cluster-worker2 , model - Intel-Gaudi-2-96GB , count - 2 , mem - 98304"}
2025-09-12T17:03:42.153219760Z {"level":"INFO","ts":"2025-09-12T17:03:42.153Z","msg":"Found SLO for model - model: default/default, class: Premium, slo-tpot: 24, slo-ttft: 500"}
2025-09-12T17:03:42.155720801Z {"level":"DEBUG","ts":"2025-09-12T17:03:42.155Z","msg":"System data prepared for optimization: - { count: [  {   type: AMD-MI300X-192G,   count: 2  },  {   type: Intel-Gaudi-2-96GB,   count: 2  },  {   type: NVIDIA-A100-PCIE-80GB,   count: 2  } ]}"}
2025-09-12T17:03:42.155726718Z {"level":"DEBUG","ts":"2025-09-12T17:03:42.155Z","msg":"System data prepared for optimization: - { accelerators: [  {   name: A100,   type: NVIDIA-A100-PCIE-80GB,   multiplicity: 1,   memSize: 0,   memBW: 0,   power: {    idle: 0,    full: 0,    midPower: 0,    midUtil: 0   },   cost: 40  },  {   name: G2,   type: Intel-Gaudi-2-96GB,   multiplicity: 1,   memSize: 0,   memBW: 0,   power: {    idle: 0,    full: 0,    midPower: 0,    midUtil: 0   },   cost: 23  },  {   name: MI300X,   type: AMD-MI300X-192GB,   multiplicity: 1,   memSize: 0,   memBW: 0,   power: {    idle: 0,    full: 0,    midPower: 0,    midUtil: 0   },   cost: 65  } ]}"}
2025-09-12T17:03:42.155729968Z {"level":"DEBUG","ts":"2025-09-12T17:03:42.155Z","msg":"System data prepared for optimization: - { serviceClasses: [  {   name: Premium,   priority: 1,   modelTargets: [    {     model: default/default,     slo-itl: 24,     slo-ttw: 500,     slo-tps: 0    },    {     model: meta/llama0-70b,     slo-itl: 80,     slo-ttw: 500,     slo-tps: 0    }   ]  },  {   name: Freemium,   priority: 10,   modelTargets: [    {     model: ibm/granite-13b,     slo-itl: 200,     slo-ttw: 2000,     slo-tps: 0    },    {     model: meta/llama0-7b,     slo-itl: 150,     slo-ttw: 1500,     slo-tps: 0    }   ]  } ]}"}
2025-09-12T17:03:42.159912718Z {"level":"DEBUG","ts":"2025-09-12T17:03:42.155Z","msg":"System data prepared for optimization: - { models: [  {   name: default/default,   acc: A100,   accCount: 1,   alpha: 20.58,   beta: 0.41,   maxBatchSize: 4,   atTokens: 0  } ]}"}
2025-09-12T17:03:42.159921135Z {"level":"DEBUG","ts":"2025-09-12T17:03:42.155Z","msg":"System data prepared for optimization: - { optimizer: {  unlimited: true,  delayedBestEffort: false,  saturationPolicy: None }}"}
2025-09-12T17:03:42.159924135Z {"level":"DEBUG","ts":"2025-09-12T17:03:42.155Z","msg":"System data prepared for optimization: - { servers: [  {   name: vllme-deployment:llm-d-sim,   class: Premium,   model: default/default,   keepAccelerator: true,   minNumReplicas: 1,   maxBatchSize: 4,   currentAlloc: {    accelerator: A100,    numReplicas: 2,    maxBatch: 256,    cost: 80,    itlAverage: 20,    waitAverage: 0,    load: {     arrivalRate: 41.32,     avgLength: 178,     arrivalCOV: 0,     serviceCOV: 0    }   },   desiredAlloc: {    accelerator: ,    numReplicas: 0,    maxBatch: 0,    cost: 0,    itlAverage: 0,    waitAverage: 0,    load: {     arrivalRate: 0,     avgLength: 0,     arrivalCOV: 0,     serviceCOV: 0    }   }  } ]}"}
2025-09-12T17:03:42.159926301Z {"level":"DEBUG","ts":"2025-09-12T17:03:42.155Z","msg":"Optimization solution - system: Solution: \ns=vllme-deployment:llm-d-sim; c=Premium; m=default/default; rate=41.32; tk=178; sol=1, sat=false, alloc={acc=A100; num=2; maxBatch=4; cost=80, val=0, servTime=21.509005, waitTime=78.63574, rho=0.72974753, maxRPM=25.31145}; slo-itl=24, slo-ttw=500, slo-tps=0 \nAllocationByType: \nname=NVIDIA-A100-PCIE-80GB, count=2, limit=2, cost=80 \ntotalCost=80 \n"}
2025-09-12T17:03:42.159927260Z {"level":"DEBUG","ts":"2025-09-12T17:03:42.155Z","msg":"Optimization completed successfully, emitting optimization metrics"}
2025-09-12T17:03:42.159928093Z {"level":"DEBUG","ts":"2025-09-12T17:03:42.155Z","msg":"Optimized allocation map - numKeys: 1, updateList_count: 1"}
2025-09-12T17:03:42.159928926Z {"level":"DEBUG","ts":"2025-09-12T17:03:42.155Z","msg":"Optimized allocation entry - key: vllme-deployment, value: {2025-09-12 17:03:42.155767718 +0000 UTC m=+2434.958473746 A100 2}"}
2025-09-12T17:03:42.159930093Z {"level":"DEBUG","ts":"2025-09-12T17:03:42.155Z","msg":"Optimization metrics emitted, starting to process variants - variant_count: 1"}
2025-09-12T17:03:42.159930885Z {"level":"DEBUG","ts":"2025-09-12T17:03:42.155Z","msg":"Processing variant - index: 0, variantAutoscaling-name: vllme-deployment, namespace: llm-d-sim, has_optimized_alloc: true"}
2025-09-12T17:03:42.159932093Z {"level":"DEBUG","ts":"2025-09-12T17:03:42.155Z","msg":"EmitReplicaMetrics completed for variantAutoscaling-name: vllme-deployment, current-replicas: 2, desired-replicas: 2, accelerator: A100"}
2025-09-12T17:03:42.159933010Z {"level":"DEBUG","ts":"2025-09-12T17:03:42.155Z","msg":"Successfully emitted optimization signals for external autoscalers - variant: vllme-deployment"}
2025-09-12T17:03:42.172232926Z {"level":"DEBUG","ts":"2025-09-12T17:03:42.172Z","msg":"Completed variant processing loop"}
2025-09-12T17:03:42.172298176Z {"level":"INFO","ts":"2025-09-12T17:03:42.172Z","msg":"Reconciliation completed - variants_processed: 1, optimization_successful: true"}
```

### KEDA ScaleObject Configuration Example (`config/samples/keda-scaled-object-vllme.yaml`)

```yaml
apiVersion: keda.sh/v1alpha1
kind: ScaledObject
metadata:
  name: vllme-deployment-scaler
  namespace: llm-d-sim
  labels:
    app: vllme-deployment
    scaler: keda-workload-variant-autoscaler
spec:
  # Target deployment to scale
  scaleTargetRef:
    apiVersion: apps/v1                  # default
    kind: Deployment                     # default
    name: vllme-deployment
  
  # Scaling configuration
  pollingInterval: 5                     # Check metrics every 5 seconds
  cooldownPeriod: 30                     # Wait 30 seconds before scaling to 0
  initialCooldownPeriod: 30              # Wait 30 seconds after creation before cooldown
  # minReplicaCount: 1                   # Minimum replicas (defaults to 0)
  maxReplicaCount: 10                    # Maximum replicas

  # Fallback configuration in case Prometheus metrics are unavailable
  fallback:
    failureThreshold: 3                  # Fail after 3 consecutive failures
    replicas: 2                          # Fallback to 2 replicas
    behavior: "currentReplicasIfHigher"  # If the current number of replicas is higher than fallback.replicas, this value will be used as fallback replicas.
                                         # If the current number of replicas is lower, the value of fallback.replicas will be used.
  
  # Advanced HPA configuration
  advanced:
    restoreToOriginalReplicaCount: false
    horizontalPodAutoscalerConfig:
      name: wva-keda-hpa-vllme-deployment
      behavior:
        scaleDown:
          stabilizationWindowSeconds: 0
          policies:
          - type: Percent
            value: 100                        # Scale down by max 100% at a time
            periodSeconds: 30                 # Check every 30 seconds
          - type: Pods
            value: 5                          # Scale down by max 4 pods at a time
            periodSeconds: 15
        scaleUp:
          stabilizationWindowSeconds: 0      
          policies:
          - type: Percent
            value: 100                        # Scale up by max 100% at a time
            periodSeconds: 30                 # Check every 30 seconds
          - type: Pods
            value: 5                          # Scale up by max 5 pods at a time
            periodSeconds: 15

  # Prometheus trigger using inferno metrics
  triggers:
  - type: prometheus
    name: inferno-desired-replicas
    metadata:
      # Prometheus server address
      serverAddress: https://kube-prometheus-stack-prometheus.workload-variant-autoscaler-monitoring.svc.cluster.local:9090
      
      # Use inferno_desired_replicas as the scaling metric
      # IMPORTANT: Query must include all 4 labels that the controller emits
      query: |
        inferno_desired_replicas{
          variant_name="vllme-deployment",
          exported_namespace="llm-d-sim",
          accelerator_type="A100",
          variant_id="default/default-A100-1"
        }

      # Scaling configuration for inferno_desired_replicas metric (integer values)
      threshold: '1'                       
      activationThreshold: '0'             # Activation: scale out from 0 when the metric is above 0
      metricType: "AverageValue"           

      unsafeSsl: "true"                    # Skip SSL verification for self-signed certificates
```
