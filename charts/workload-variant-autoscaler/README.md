### INSTALL (on OpenShift)
1. Before running, be sure to delete all previous helm installations for workload-variant-scheduler and prometheus-adapter.
2. llm-d must be installed for WVA to do it's magic. If you plan on installing llm-d with these instructions, please be sure to remove any other helm installation of llm-d before proceeding.

#### NOTE: to view which helm charts you already have installed in your cluster, use:
```
helm ls -A
```

```
export OWNER="llm-d-incubation"
export WVA_PROJECT="workload-variant-autoscaler"
export WVA_RELEASE="v0.0.1"
export LLMD_PROJECT="llm-d-infra"
export LLMD_RELEASE="v1.3.1"
export HF_TOKEN="<your_token_here>"
export WVA_NS="workload-variant-autoscaler-system"
export BASE_NAME="inference-scheduling"
export LLMD_NS="llm-d-$BASE_NAME"
export MON_NS="openshift-user-workload-monitoring"

kubectl get secret thanos-querier-tls -n openshift-monitoring -o jsonpath='{.data.tls\.crt}' | base64 -d > /tmp/prometheus-ca.crt

git clone -b $WVA_RELEASE -- https://github.com/$OWNER/$WVA_PROJECT.git $WVA_PROJECT
cd $WVA_PROJECT
export WVA_PROJECT=$PWD
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update

helm upgrade -i prometheus-adapter prometheus-community/prometheus-adapter \
  -n $MON_NS \
  -f config/samples/prometheus-adapter-values-ocp.yaml

kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: $WVA_NS
  labels:
    app.kubernetes.io/name: workload-variant-autoscaler
    control-plane: controller-manager
---
apiVersion: v1
kind: Namespace
metadata:
  name: $LLMD_NS
EOF

cd $WVA_PROJECT/charts
helm upgrade -i workload-variant-autoscaler ./workload-variant-autoscaler \
  -n $WVA_NS \
  --set-file wva.prometheus.caCert=/tmp/prometheus-ca.crt \
  --set va.accelerator=L40S \
  --set llmd.modelID=unsloth/Meta-Llama-3.1-8B \
  --set vllmService.enabled=true \
  --set vllmService.nodePort=30000
```

### INSTALL LLM-D
```
cd $WVA_PROJECT
kubectl create secret generic llm-d-hf-token \
    --from-literal="HF_TOKEN=${HF_TOKEN}" \
    --namespace "${LLMD_NS}" \
    --dry-run=client -o yaml | kubectl apply -f -

git clone -b $LLMD_RELEASE -- https://github.com/$OWNER/$LLMD_PROJECT.git $LLMD_PROJECT

cd $WVA_PROJECT/$LLMD_PROJECT/quickstart
bash dependencies/install-deps.sh
bash gateway-control-plane-providers/install-gateway-provider-dependencies.sh

yq eval '.releases[].version = "v2.0.3"' -i "gateway-control-plane-providers/kgateway.helmfile.yaml"
helmfile apply -f "gateway-control-plane-providers/kgateway.helmfile.yaml"

export EXAMPLES_DIR="$WVA_PROJECT/$LLMD_PROJECT/quickstart/examples/$BASE_NAME"
yq eval '.gateway.service.type = "NodePort"' -i $WVA_PROJECT/$LLMD_PROJECT/charts/llm-d-infra/values.yaml

cd $EXAMPLES_DIR
sed -i '' "s/llm-d-inference-scheduler/$LLMD_NS/g" helmfile.yaml.gotmpl
yq eval '(.. | select(. == "Qwen/Qwen3-0.6B")) = "unsloth/Meta-Llama-3.1-8B" | (.. | select(. == "hf://Qwen/Qwen3-0.6B")) = "hf://unsloth/Meta-Llama-3.1-8B"' -i ms-$BASE_NAME/values.yaml
helmfile apply -e kgateway

export GATEWAY_NAME="infra-inference-scheduling-inference-gateway"
kubectl patch gatewayparameters.gateway.kgateway.dev $GATEWAY_NAME \
  -n $LLMD_NS \
  --type='merge' \
  -p '{"spec":{"kube":{"service":{"type":"NodePort"}}}}'

cd $WVA_PROJECT/..
```

### CLEANUP
```
export MON_NS="openshift-user-workload-monitoring"
export WVA_NS="workload-variant-autoscaler-system"
export LLMD_NS="llm-d-$BASE_NAME"

helm delete prometheus-adapter -n $MON_NS
helm delete workload-variant-autoscaler -n $WVA_NS
kubectl delete ns $WVA_NS $LLMD_NS
```

### VALIDATION / TROUBLESHOOTING
1. Check for 'error' in workload-variant-autoscaler-controller-manager-xxxxx in the workload-variant-autoscaler-system namespace
```
kubectl logs pod workload-variant-autoscaler-controller-manager-xxxxx -n workload-variant-autoscaler-system | grep error
```
2. Check for '404' in prometheus-adapter in the openshift-user-workload-monitoring namespace
```
kubectl logs pod prometheus-adapter-xxxxx -n openshift-user-workload-monitoring | grep 404
```
3. Check, after a few minutes following installation, for metric collection
```
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/$NAMESPACE/inferno_desired_replicas" | jq
```
