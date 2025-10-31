# Deploying the Workload-Variant-Autoscaler and llm-d on OpenShift

This guide shows how to deploy the Workload-Variant-Autoscaler (integrated with the HorizontalPodAutoscaler) on OpenShift (OCP), allowing vLLM servers to scale accordingly to the observed load.

**Note**: the deployment was tested on an OCP cluster with *admin* privileges.

## Overview

After deploying the Workload-Variant-Autoscaler following the provided guides, this guide allows the integration of the following components:

1. **Inferno Controller** processes VariantAutoscaling objects and emits the `inferno_desired_replicas` metrics

2. **Prometheus and Thanos** scrape these metrics from the Workload-Variant-Autoscaler `/metrics` endpoint using TLS, using the existing monitoring infrastructure present on OCP

3. **Prometheus Adapter** exposes the metrics to Kubernetes external metrics API

4. **HPA** reads the value for the `inferno_desired_replicas` metrics and adjusts Deployment replicas accordingly

5. **llm-d infrastructure**: a Gateway and the Inference Scheduler are deployed to forward requests to the pods running vLLM instances.

## Prerequisites

- Prometheus stack already present on the OCP cluster
- All components must be fully ready before proceeding: 2-3 minutes may be needed after the deployment

### 0. Deploy the Workload-Variant-Autoscaler and llm-d

First, export the following environment variables:

```sh
export WVA_PROJECT=$PWD
export BASE_NAME="inference-scheduling"
export NAMESPACE="llm-d-$BASE_NAME"
export MONITORING_NAMESPACE="openshift-user-workload-monitoring" 
```

*Note*: the `MONITORING_NAMESPACE` environment variable may need to change depending on your OCP setup.

Then, create the namespace in which the `llm-d` infrastructure will be deployed:

```bash
kubectl create namespace $NAMESPACE
```

#### Deploying the Workload-Variant-Autoscaler

Before running the Make target to deploy Workload-Variant-Autoscaler, the `PROMETHEUS_BASE_URL` in the `config/manager/configmap.yaml` must be changed into a valid URL, to be able to connect to Thanos:

```yaml
# ...
  PROMETHEUS_BASE_URL: "https://thanos-querier.openshift-monitoring.svc.cluster.local:9091"
```

**Note**: the Prometheus/Thanos URL may change depending on your OCP setup. You may want to run the following command to find the appropriate Service:

```bash
kubectl get svc -n openshift-monitoring

kubectl get svc -n openshift-user-workload-monitoring
```

And then use the appropriate URL associated with the `thanos-querier` Service.

After that, you can deploy the Workload-Variant-Autoscaler using the basic `Make` target:

```sh
make deploy IMG=quay.io/infernoautoscaler/inferno-controller:0.0.1-multi-arch
# make deploy IMG=ghcr.io/llm-d/workload-variant-autoscaler:latest
```

Then, you need to deploy the required ConfigMaps for the accelerator costs and the service classes. An example of this configuration can be found in the following command.

```sh
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: ConfigMap
# This configMap defines the set of accelerators available
# to the autoscaler to assign to variants
#
# For each accelerator, need to specify a (unique) name and some properties:
# - device is the name of the device (card) corresponding to this accelerator,
#   it should be the same as the device specified in the node object
# - cost is the cents/hour cost of this accelerator
#
metadata:
  name: service-classes-config
  namespace: workload-variant-autoscaler-system
data:
  premium.yaml: |
    name: Premium
    priority: 1
    data:
      - model: default/default
        slo-tpot: 24
        slo-ttft: 500
      - model: llama0-70b
        slo-tpot: 80
        slo-ttft: 500
      - model: unsloth/Meta-Llama-3.1-8B
        slo-tpot: 9
        slo-ttft: 1000
  freemium.yaml: |
    name: Freemium
    priority: 10
    data:
      - model: granite-13b
        slo-tpot: 200
        slo-ttft: 2000
      - model: llama0-7b
        slo-tpot: 150
        slo-ttft: 1500
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: accelerator-unit-costs
  namespace: workload-variant-autoscaler-system
data:
  A100: |
    {
    "device": "NVIDIA-A100-PCIE-80GB",
    "cost": "40.00"
    }
  MI300X: |
    {
    "device": "AMD-MI300X-192GB",
    "cost": "65.00"
    }
  G2: |
    {
    "device": "Intel-Gaudi-2-96GB",
    "cost": "23.00"
    }
  H100: |
    {
    "device": "NVIDIA-H100-80GB-HBM3",
    "cost": "100.0"
    }
  L40S: |
    {
    "device": "NVIDIA-L40S",
    "cost": "32.00"
    }
EOF

```

And then, create the required ServiceMonitor for the Workload-Variant-Autoscaler, to be deployed in the `MONITORING_NAMESPACE` namespace.
An example of this configuration can be found in the following command.

```sh
cat <<EOF | kubectl apply -f -
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  labels:
    app.kubernetes.io/managed-by: kustomize
    app.kubernetes.io/name: workload-variant-autoscaler
    control-plane: controller-manager
  name: workload-variant-autoscaler-controller-manager-metrics-monitor
  namespace: $MONITORING_NAMESPACE
spec:
  endpoints:
  - bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
    interval: 10s
    path: /metrics
    port: https
    scheme: https
    tlsConfig:
      insecureSkipVerify: true
  namespaceSelector:
    matchNames:
    - workload-variant-autoscaler-system
  selector:
    matchLabels:
      app.kubernetes.io/name: workload-variant-autoscaler
      control-plane: controller-manager
EOF
```

#### Note on RBAC issues

If you experience `403: Forbidden` or `401: Unauthorized` errors for the WVA when it tries to connect to Prometheus, you might consider adding the following ClusterRole to the ServiceAccount:

```bash
oc adm policy add-cluster-role-to-user cluster-monitoring-view -z workload-variant-autoscaler-controller-manager -n workload-variant-autoscaler-system
```

#### Deploying the llm-d infrastructure

*Note*:

- This guide will follow the **Well-Lit Path** steps for the recommended out of the box scheduling configuration for most vLLM deployments using **llm-d**, [available here](https://github.com/llm-d-incubation/llm-d-infra/tree/main/quickstart/examples/inference-scheduling).

- If you want to deploy other examples using **llm-d**, please refer to the [llm-d infrastructure repo](https://github.com/llm-d-incubation/llm-d-infra/tree/main/quickstart/examples).

1. First, create a secret containing your HuggingFace token. **Make sure to insert a *valid* HF token** in the environment variable.

```sh
export HF_TOKEN="<your-hf-token>"
kubectl create secret generic llm-d-hf-token \
    --from-literal="HF_TOKEN=${HF_TOKEN}" \
    --namespace "${NAMESPACE}" \
    --dry-run=client -o yaml | kubectl apply -f -
```

2. Then, clone the llm-d infrastructure repository:

```sh
export OWNER="llm-d-incubation" 
export PROJECT="llm-d-infra"
export RELEASE="v1.3.1"
git clone -b $RELEASE -- https://github.com/$OWNER/$PROJECT.git $PROJECT
```

3. Install the required dependencies, including the Gateway API and Inference Extension CRDs:

```sh
cd $WVA_PROJECT/$PROJECT/quickstart
bash dependencies/install-deps.sh
bash gateway-control-plane-providers/install-gateway-provider-dependencies.sh
```

4. Install the Gateway provider CRDs:

```sh
 # switching to kgateway v2.0.3
yq eval '.releases[].version = "v2.0.3"' -i "gateway-control-plane-providers/kgateway.helmfile.yaml"
helmfile apply -f "gateway-control-plane-providers/kgateway.helmfile.yaml"
```

*Note*: this setup was tested using `kgateway`. Currently, there are issues with `kgateway v2.0.4` used with **llm-d**, therefore we need to switch to `v2.0.3`.

5. Install the llm-d core components:

*Note*: we want to run experiments using the `unsloth/Meta-Llama-3.1-8B` model. Therefore, we will change the model deployed by `llm-d` into it.

*Note*: for testing purposes and to avoid deploying a LoadBalancer Service, we want to change the deployed service type to `NodePort`.

```sh
export EXAMPLES_DIR="$WVA_PROJECT/$PROJECT/quickstart/examples/$BASE_NAME"

yq eval '.gateway.service.type = "NodePort"' -i $WVA_$PROJECT/$PROJECT/charts/llm-d-infra/values.yaml

cd $EXAMPLES_DIR
sed -i '' "s/llm-d-inference-scheduler/$NAMESPACE/g" helmfile.yaml.gotmpl
yq eval '(.. | select(. == "Qwen/Qwen3-0.6B")) = "unsloth/Meta-Llama-3.1-8B" | (.. | select(. == "hf://Qwen/Qwen3-0.6B")) = "hf://unsloth/Meta-Llama-3.1-8B"' -i ms-$BASE_NAME/values.yaml
helmfile apply -e kgateway
```

6. Finally, deploy the Service and ServiceMonitor, needed by Prometheus to scrape metrics from the vLLM Deployment that will be installed by **llm-d**. An example of this configuration can be found in the following command (*Note*: you may need to switch the `nodePort` if it is already being used).

```bash
cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Service
metadata:
  name: vllm-service
  namespace: $NAMESPACE
  labels:
    llm-d.ai/model: ms-inference-scheduling-llm-d-modelservice
spec:
  selector:
    llm-d.ai/model: ms-inference-scheduling-llm-d-modelservice
  ports:
    - name: vllm
      port: 8200
      protocol: TCP
      targetPort: 8200
      nodePort: 30000
  type: NodePort
---
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: vllm-servicemonitor
  namespace: $MONITORING_NAMESPACE
  labels:
    llm-d.ai/model: ms-inference-scheduling-llm-d-modelservice
spec:
  selector:
    matchLabels:
      llm-d.ai/model: ms-inference-scheduling-llm-d-modelservice
  endpoints:
  - port: vllm
    path: /metrics
    interval: 15s
  namespaceSelector:
    any: true
EOF
```

7. **Note** this step is required **only** up to `llm-d-infra v1.3.1`, as later versions will provide a fix for this bug.

Because of a known bug on the `prefix-cache-scorer` used by the `inference-scheduler`, we are disabling it for this example, by applying an edited version of the EPP ConfigMap. An example configuration for the `ConfigMap` can be found in the following command.

```bash
cat <<EOF | kubectl apply -f -
apiVersion: v1
data:
  default-plugins.yaml: |
    apiVersion: inference.networking.x-k8s.io/v1alpha1
    kind: EndpointPickerConfig
    plugins:
    - type: low-queue-filter
      parameters:
        threshold: 128
    - type: lora-affinity-filter
      parameters:
        threshold: 0.999
    - type: least-queue-filter
    - type: least-kv-cache-filter
    - type: decision-tree-filter
      name: low-latency-filter
      parameters:
        current:
          pluginRef: low-queue-filter
        nextOnSuccess:
          decisionTree:
            current:
              pluginRef: lora-affinity-filter
            nextOnSuccessOrFailure:
              decisionTree:
                current:
                  pluginRef: least-queue-filter
                nextOnSuccessOrFailure:
                  decisionTree:
                    current:
                      pluginRef: least-kv-cache-filter
        nextOnFailure:
          decisionTree:
            current:
              pluginRef: least-queue-filter
            nextOnSuccessOrFailure:
              decisionTree:
                current:
                  pluginRef: lora-affinity-filter
                nextOnSuccessOrFailure:
                  decisionTree:
                    current:
                      pluginRef: least-kv-cache-filter
    - type: random-picker
      parameters:
        maxNumOfEndpoints: 1
    - type: single-profile-handler
    schedulingProfiles:
    - name: default
      plugins:
      - pluginRef: low-latency-filter
      - pluginRef: random-picker
  plugins-v2.yaml: |
    apiVersion: inference.networking.x-k8s.io/v1alpha1
    kind: EndpointPickerConfig
    plugins:
    - type: queue-scorer
    - type: kv-cache-scorer
    # - type: prefix-cache-scorer
      parameters:
        hashBlockSize: 64
        maxPrefixBlocksToMatch: 256
        lruCapacityPerServer: 31250
    - type: max-score-picker
      parameters:
        maxNumOfEndpoints: 1
    - type: single-profile-handler
    schedulingProfiles:
    - name: default
      plugins:
      - pluginRef: queue-scorer
        weight: 1
      - pluginRef: kv-cache-scorer
        weight: 1
      # - pluginRef: prefix-cache-scorer
      #   weight: 1
      - pluginRef: max-score-picker
kind: ConfigMap
metadata:
  annotations:
    meta.helm.sh/release-name: gaie-inference-scheduling
    meta.helm.sh/release-namespace: $NAMESPACE
  labels:
    app.kubernetes.io/managed-by: Helm
  name: gaie-inference-scheduling-epp
  namespace: $NAMESPACE
EOF
```

### 1. Prometheus: create Thanos CA ConfigMap

Prometheus and Thanos are deployed on OCPs with TLS (HTTPS) for security. The Prometheus Adapter needs to connect to Thanos at https://thanos-querier.openshift-monitoring.svc.cluster.local.

**Note**: the Prometheus/Thanos URL and secret may change depending on your OCP setup.

We will use a CA configmap for TLS Certificate Verification:

```sh
# Extract the TLS certificate from the thanos-querier-tls secret
kubectl get secret thanos-querier-tls -n openshift-monitoring -o jsonpath='{.data.tls\.crt}' | base64 -d > /tmp/prometheus-ca.crt

# Create ConfigMap with the certificate
kubectl create configmap prometheus-ca --from-file=ca.crt=/tmp/prometheus-ca.crt -n $MONITORING_NAMESPACE
```

### 2. Deploy the Prometheus Adapter

Note: a `yaml` example snippet for the Prometheus Adapter configuration with TLS for OCP can be found [at the end of this README](#prometheus-adapter-values-configsamplesprometheus-adapter-values-ocpyaml).

```sh
# Add Prometheus community helm repo - already there if you deployed Workload-Variant-Autoscaler using the scripts
cd $WVA_PROJECT
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update

# Deploy Prometheus Adapter with Workload-Variant-Autoscaler metrics configuration
helm upgrade -i prometheus-adapter prometheus-community/prometheus-adapter \
  -n $MONITORING_NAMESPACE \
  -f config/samples/prometheus-adapter-values-ocp.yaml
```

#### Note on RBAC issues

If you experience `403: Forbidden` or `401: Unauthorized` errors for the Prometheus-Adapter when it tries to connect to Prometheus, you might consider adding the following ClusterRole to the ServiceAccount:

```bash
oc adm policy add-cluster-role-to-user cluster-monitoring-view -z prometheus-adapter -n openshift-user-workload-monitoring
```

### 3. Create the VariantAutoscaling resource

An example of VariantAutoscaling resource can be found in the following command.

**Note**: this example considers a VariantAutoscaling with using `H100` GPUs. **This should be *changed* depending on what GPUs you have available on your cluster**.  

```sh
cat <<EOF | kubectl apply -f -
apiVersion: llmd.ai/v1alpha1
# Optimizing a variant, create only when the model is deployed and serving traffic
# this is for the collector the collect existing (previous) running metrics of the variant.
kind: VariantAutoscaling
metadata:
  # Unique name of the variant
  name: ms-inference-scheduling-llm-d-modelservice-decode 
  namespace: $NAMESPACE
  labels:
    inference.optimization/acceleratorName: H100
# This is essentially static input to the optimizer
spec:
  # OpenAI API compatible name of the model
  modelID: "unsloth/Meta-Llama-3.1-8B"
  # Add SLOs in configmap, add reference to this per model data
  # to avoid duplication and Move to ISOs when available
  sloClassRef:
    # Configmap name to load in the same namespace as optimizer object
    # we start with static (non-changing) ConfigMaps (for ease of implementation only)
    name: premium-slo
    # Key (modelID) present inside configmap
    key: opt-125m
  # Static profiled benchmarked data for a variant running on different accelerators
  modelProfile:
    accelerators:
      - acc: "H100"
        accCount: 1
        perfParms: 
          decodeParms:
            # Decode parameters for ITL equation: itl = alpha + beta * maxBatchSize
            alpha: "6.958"
            beta: "0.042"
          # Prefill parameters for TTFT equation: ttft = gamma + delta * tokens * maxBatchSize  
          prefillParms:
            gamma: "5.2"
            delta: "0.1"
        maxBatchSize: 512
      - acc: "L40S"
        accCount: 1
        perfParms: 
          decodeParms:
            alpha: "22.619"
            beta: "0.181"
          prefillParms:
            gamma: "226.19"
            delta: "0.018"
        maxBatchSize: 512
EOF
```

### 4. Wait for Prometheus to fetch metrics from the Workload-Variant-Autoscaler

You can verify that metrics are being emitted and fetched by querying for the following (*Note*: it may take 1-2mins for the metrics to be available):

```bash
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/$NAMESPACE/inferno_desired_replicas" | jq

{
  "kind": "ExternalMetricValueList",
  "apiVersion": "external.metrics.k8s.io/v1beta1",
  "metadata": {},
  "items": [
    {
      "metricName": "inferno_desired_replicas",
      "metricLabels": {
        "__name__": "inferno_desired_replicas",
        "accelerator_type": "H100",
        "endpoint": "https",
        "exported_namespace": "llm-d-inference-scheduling",
        "instance": "10.130.3.58:8443",
        "job": "workload-variant-autoscaler-controller-manager-metrics-service",
        "managed_cluster": "dc670625-c0d1-48d6-bcc3-b932aaceecb4",
        "namespace": "workload-variant-autoscaler-system",
        "pod": "workload-variant-autoscaler-controller-manager-685966979-8lnnr",
        "prometheus": "openshift-monitoring/k8s",
        "service": "workload-variant-autoscaler-controller-manager-metrics-service",
        "variant_name": "ms-inference-scheduling-llm-d-modelservice-decode"
      },
      "timestamp": "2025-09-04T18:30:31Z",
      "value": "1"
    }
  ]
}
```

### 5. Deploy the HPA resource

Note: an example configuration for HPA can be found in the following command:

```sh
cat <<EOF | kubectl apply -f -
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: vllm-deployment-hpa
  namespace: $NAMESPACE
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: ms-inference-scheduling-llm-d-modelservice-decode
  # minReplicas: 0  # scale to zero - alpha feature
  maxReplicas: 10
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 0 # tune this value 
      policies:
      - type: Pods
        value: 10
        periodSeconds: 15
    scaleDown:
      stabilizationWindowSeconds: 0 # tune this value
      policies:
      - type: Pods
        value: 10
        periodSeconds: 15
  metrics:
  - type: External
    external:
      metric:
        name: inferno_desired_replicas
        selector:
          matchLabels:
            variant_name: ms-inference-scheduling-llm-d-modelservice-decode
      target:
        type: AverageValue
        averageValue: "1"
EOF
```

### 6. Verify the integration

- Wait for all components to be ready (1-2 minutes total)

- Check the status of HPA (should show actual target values, not `<unknown>/1`):

```sh
kubectl get hpa -n $NAMESPACE
NAME                  REFERENCE                                                      TARGETS     MINPODS   MAXPODS   REPLICAS   AGE
vllm-deployment-hpa   Deployment/ms-inference-scheduling-llm-d-modelservice-decode   1/1 (avg)   1         10        1          147m
```

- Check the VariantAutoscaling resource:

```sh
kubectl get variantautoscaling -n $NAMESPACE
NAME                                                MODEL                       ACCELERATOR   CURRENTREPLICAS   OPTIMIZED   AGE
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          1                 1           3h41m
```

### Adding probe definition to vLLM deployment for clean startup

As of today, the `llm-d-infra` deployed example (`inference scheduling`) uses a ModelService that does **not** include probe definition for the vLLM deployment. This could cause newly spawned Pods to be prematurely marked as `Ready` by Kubernetes, and consequently be scored and picked by the Inference Scheduler to serve incoming requests, although the vLLM server still has not finished its startup configuration.

The side effects of this behavior are:

- Requests of any criticality level forwarded to the new Pod(s) are dropped.

- A consequent decrease in the incoming request rate is observed by the Workload Variant Autoscaler.

The latter could even bring the Autoscaler to wrongly decrease the desired number of replicas, causing effective SLO violation. Because of that, there is the need to have a clean startup configuration using readiness and startup probes: a `yaml` example snippet for the patch can be found [at the end of this README](#vllm-deployment-probes-patch-example-configsamplesprobes-patchyaml).

```bash
# After creating the file `config/samples/probes-patch.yaml`
# Apply probe patch for the existing vLLM deployment
kubectl patch deployment ms-inference-scheduling-llm-d-modelservice-decode -n $NAMESPACE --patch-file config/samples/probes-patch.yaml
```

## Running benchmarks

We use **GuideLLM** as a load generator for the vLLM servers deployed by the `llm-d` infrastructure, and scaled by the Workload-Variant-Autoscaler.

We can generate traffic by using Kubernetes `Job`s, which will launch GuideLLM to generate traffic for the servers. A sample `yaml` snippet for a Job launching GuideLLM against the Inference Gateway deployed by `llm-d` can be found in the following commands.

*Note*: depending on the deployed model, there may be the need to deploy multiple GuideLLM `Job`s to see a scale-up recommendation from the Workload-Variant-Autoscaler.

## Example: scale-up scenario

1. Launch the GuideLLM `Job` to send load to the vLLM servers via the following command:

```bash
cat <<EOF | kubectl apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: guidellm-job-1
  namespace: $NAMESPACE
spec:
  template:
    spec:
      containers:
      - name: guidellm-benchmark-container
        image: quay.io/vishakharamani/guidellm:latest
        imagePullPolicy: IfNotPresent
        env:
        - name: HF_HOME
          value: "/tmp"
        command: ["/usr/local/bin/guidellm"]
        args:
        - "benchmark"
        - "--target"
        - "http://infra-inference-scheduling-inference-gateway:80"
        - "--rate-type"
        - "constant"
        - "--rate"
        - "8" # req/sec
        - "--max-seconds"
        - "1800"
        - "--model"
        - "unsloth/Meta-Llama-3.1-8B"
        - "--data"
        - "prompt_tokens=128,output_tokens=512"
        - "--output-path"
        - "/tmp/benchmarks.json" 
      restartPolicy: Never
  backoffLimit: 4
EOF
```

2. After a while, you will see a scale out happening:

```sh
kubectl get hpa -n $NAMESPACE -w                                                             
NAME                  REFERENCE                                                      TARGETS     MINPODS   MAXPODS   REPLICAS   AGE
vllm-deployment-hpa   Deployment/ms-inference-scheduling-llm-d-modelservice-decode   1/1 (avg)   1         10        1          51m
vllm-deployment-hpa   Deployment/ms-inference-scheduling-llm-d-modelservice-decode   2/1 (avg)   1         10        1          54m
vllm-deployment-hpa   Deployment/ms-inference-scheduling-llm-d-modelservice-decode   1/1 (avg)   1         10        2          54m

kubectl get va -n $NAMESPACE -w
NAME                                                MODEL                       ACCELERATOR   CURRENTREPLICAS   OPTIMIZED   AGE
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          1                 1           54m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          1                 2           55m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          1                 2           55m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          1                 2           55m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          2                 2           56m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          2                 2           56m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          2                 2           56m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          2                 2           57m

kubectl get deployment -n $NAMESPACE  -w
NAME                                                READY   UP-TO-DATE   AVAILABLE   AGE
ms-inference-scheduling-llm-d-modelservice-decode   1/1     1            1           55m
ms-inference-scheduling-llm-d-modelservice-decode   1/2     1            1           57m
ms-inference-scheduling-llm-d-modelservice-decode   1/2     1            1           57m
ms-inference-scheduling-llm-d-modelservice-decode   1/2     1            1           57m
ms-inference-scheduling-llm-d-modelservice-decode   1/2     2            1           57m
ms-inference-scheduling-llm-d-modelservice-decode   2/2     2            2           57m
```

3. Once the load has stopped, the vLLM deployment will be scaled in to 1 replica:

```bash
kubectl get hpa -n $NAMESPACE -w
NAME                  REFERENCE                                                      TARGETS     MINPODS   MAXPODS   REPLICAS   AGE
vllm-deployment-hpa   Deployment/ms-inference-scheduling-llm-d-modelservice-decode   1/1 (avg)   1         10        1          51m
vllm-deployment-hpa   Deployment/ms-inference-scheduling-llm-d-modelservice-decode   2/1 (avg)   1         10        1          54m
vllm-deployment-hpa   Deployment/ms-inference-scheduling-llm-d-modelservice-decode   1/1 (avg)   1         10        2          54m
vllm-deployment-hpa   Deployment/ms-inference-scheduling-llm-d-modelservice-decode   500m/1 (avg)   1         10        2          58m
vllm-deployment-hpa   Deployment/ms-inference-scheduling-llm-d-modelservice-decode   1/1 (avg)      1         10        1          58m

kubectl get va -n $NAMESPACE -w
NAME                                                MODEL                       ACCELERATOR   CURRENTREPLICAS   OPTIMIZED   AGE
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          1                 1           53m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          1                 1           54m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          1                 1           54m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          1                 1           54m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          1                 2           55m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          1                 2           55m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          1                 2           55m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          2                 2           56m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          2                 2           56m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          2                 2           56m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          2                 2           57m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          2                 2           57m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          2                 2           57m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          2                 2           58m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          2                 2           58m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          2                 2           58m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          2                 1           59m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          2                 1           59m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          2                 1           59m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          1                 1           60m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          1                 1           60m
ms-inference-scheduling-llm-d-modelservice-decode   unsloth/Meta-Llama-3.1-8B   H100          1                 1           60m

kubectl get deployment -n $NAMESPACE -w
NAME                                                READY   UP-TO-DATE   AVAILABLE   AGE
ms-inference-scheduling-llm-d-modelservice-decode   1/1     1            1           55m
ms-inference-scheduling-llm-d-modelservice-decode   1/2     1            1           57m
ms-inference-scheduling-llm-d-modelservice-decode   1/2     1            1           57m
ms-inference-scheduling-llm-d-modelservice-decode   1/2     1            1           57m
ms-inference-scheduling-llm-d-modelservice-decode   1/2     2            1           57m
ms-inference-scheduling-llm-d-modelservice-decode   2/2     2            2           57m
ms-inference-scheduling-llm-d-modelservice-decode   2/1     2            2           61m
ms-inference-scheduling-llm-d-modelservice-decode   2/1     2            2           61m
ms-inference-scheduling-llm-d-modelservice-decode   1/1     1            1           61m
```

## Undeploy the infrastructure

### Undeploying the `llm-d` infrastructure components

To undeploy the `llm-d` components, uninstall the Helm Charts deployed previously:

```bash
export BASE_NAME="inference-scheduling"
export NAMESPACE="llm-d-$BASE_NAME"
export MONITORING_NAMESPACE="openshift-user-workload-monitoring"
```

```bash
# Uninstall the deployed Helm Charts
helm uninstall infra-$BASE_NAME -n ${NAMESPACE}
helm uninstall gaie-$BASE_NAME -n ${NAMESPACE}
helm uninstall ms-$BASE_NAME -n ${NAMESPACE}
```

### Undeploying the Workload-Variant-Autoscaler

To undeploy the Workload-Variant-Autoscaler, use the following `Make` target:

```bash
make undeploy
```

and then remove all other deployed additional resources:

```bash
kubectl delete -n $NAMESPACE secret llm-d-hf-token --ignore-not-found

kubectl delete -n $NAMESPACE svc vllm-service --ignore-not-found
kubectl delete -n $MONITORING_NAMESPACE servicemonitor workload-variant-autoscaler-controller-manager-metrics-monitor --ignore-not-found
kubectl delete -n $MONITORING_NAMESPACE servicemonitor vllm-service --ignore-not-found
kubectl delete -n $MONITORING_NAMESPACE configmap prometheus-ca --ignore-not-found

kubectl delete -n $NAMESPACE hpa vllm-deployment-hpa --ignore-not-found

helm uninstall prometheus-adapter -n ${MONITORING_NAMESPACE} --ignore-not-found
```

## Configuration Files

### Prometheus Adapter Values (`config/samples/prometheus-adapter-values-ocp.yaml`)

```yaml
prometheus:
  url: https://thanos-querier.openshift-monitoring.svc.cluster.local
  port: 9091

rules:
  external:
  - seriesQuery: 'inferno_desired_replicas{variant_name!="",exported_namespace!=""}'
    resources:
      overrides:
        exported_namespace: {resource: "namespace"}
        variant_name: {resource: "deployment"}  
    name:
      matches: "^inferno_desired_replicas"
      as: "inferno_desired_replicas"
    metricsQuery: 'inferno_desired_replicas{<<.LabelMatchers>>}'

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
  - --prometheus-token-file=/var/run/secrets/kubernetes.io/serviceaccount/token


# k8s 1.21 needs fsGroup to be set for non root deployments
# ref: https://github.com/kubernetes/kubernetes/issues/70679
podSecurityContext:
  fsGroup: null    # this may need to change, depending on the allowed IDs for the OCP project

# SecurityContext of the container
# ref. https://kubernetes.io/docs/tasks/configure-pod-container/security-context
securityContext:
  allowPrivilegeEscalation: false
  capabilities:
    drop: ["ALL"]
  readOnlyRootFilesystem: true
  runAsNonRoot: true
  runAsUser: null   # this may need to change, depending on the allowed IDs for the OCP project
  seccompProfile:
    type: RuntimeDefault
```

### vLLM Deployment Probes Patch Example (`config/samples/probes-patch.yaml`)

```yaml
spec:
  template:
    spec:
      containers:
      - name: vllm
        readinessProbe:
            httpGet:
              path: /health
              port: 8200
              scheme: HTTP
            # vLLM's health check is simple, so we can more aggressively probe it.  Readiness
            # check endpoints should always be suitable for aggressive probing, but may be
            # slightly more expensive than readiness probes.
            periodSeconds: 1
            successThreshold: 1
            # vLLM has a very simple health implementation, which means that any failure is
            # likely significant,
            failureThreshold: 1
            timeoutSeconds: 1
          # We set a startup probe so that we don't begin directing traffic or checking
          # liveness to this instance until the model is loaded.
        startupProbe:
            # Failure threshold is when we believe startup will not happen at all, and is set
            # to the maximum possible time we believe loading a model will take. In our
            # default configuration we are downloading a model from HuggingFace, which may
            # take a long time, then the model must load into the accelerator. We choose
            # 10 minutes as a reasonable maximum startup time before giving up and attempting
            # to restart the pod.
            #
            # IMPORTANT: If the core model takes more than 10 minutes to load, pods will crash
            # loop forever. Be sure to set this appropriately.
            failureThreshold: 600
            # Set delay to start low so that if the base model changes to something smaller
            # or an optimization is deployed, we don't wait unneccesarily.
            initialDelaySeconds: 30
            # As a startup probe, this stops running and so we can more aggressively probe
            # even a moderately complex startup - this is a very important workload.
            periodSeconds: 1
            httpGet:
              # vLLM does not start the OpenAI server (and hence make /health available)
              # until models are loaded. This may not be true for all model servers.
              path: /health
              port: 8200
              scheme: HTTP
```

**Note**: the HPA `StabilizationWindow` is a configuration parameter that aims to smooth *flapping* behaviors, where the desired number of replicas is continuously changing, causing the startup and termination of vLLM servers that may take a while to be ready, and would waste resources too.
This behavior could happen in rapid load rate changes, and therefore this parameter should be tuned accordingly.

For instance, if you aim to test your setup sending load for 1 minute, then expect no load for the following minute before resuming it, to optimize resources you should use:

```yaml
scaleUp:
      stabilizationWindowSeconds: 0
# ...
 scaleDown:
      stabilizationWindowSeconds: 120
```

This way, HPA does not scale out the number of replicas just to bring it up again when load resumes, avoiding waste of resources and preventing a second vLLM instance to be started up.
