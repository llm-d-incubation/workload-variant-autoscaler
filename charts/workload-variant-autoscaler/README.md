# workload-variant-autoscaler

![Version: 0.4.1](https://img.shields.io/badge/Version-0.4.1-informational?style=flat-square) ![Type: application](https://img.shields.io/badge/Type-application-informational?style=flat-square) ![AppVersion: v0.4.1](https://img.shields.io/badge/AppVersion-v0.4.1-informational?style=flat-square)

Helm chart for Workload-Variant-Autoscaler (WVA) - GPU-aware autoscaler for LLM inference workloads

## Installation (OpenShift)
Helm is the recommended installation method. Before running, be sure to delete all previous helm installations for `workload-variant-autoscaler` and `prometheus-adapter`. To list all helm charts installed in the cluster run `helm ls -A`.

### Step 1: Setup Variables, Secret, Helm repo
```
export OWNER="llm-d"
export WVA_PROJECT="llm-d-workload-variant-autoscaler"
export WVA_RELEASE="v0.5.0"
export WVA_NS="workload-variant-autoscaler-system"
export MON_NS="openshift-user-workload-monitoring"

kubectl get secret thanos-querier-tls -n openshift-monitoring -o jsonpath='{.data.tls\.crt}' | base64 -d > /tmp/prometheus-ca.crt

git clone -b $WVA_RELEASE -- https://github.com/$OWNER/$WVA_PROJECT.git $WVA_PROJECT
cd $WVA_PROJECT
export WVA_PROJECT=$PWD
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update
```

### Step 2: Update prometheus-adapter To Export WVA Metrics
**Important:** The following helm upgrade command updates the global `prometheus-adapter` 
configmap. If this is a shared cluster then you might want to get the current
settings, manually append the values in `config/samples/prometheus-adapter-values-ocp.yaml`
then run helm upgrade with the appended values. Here's an example how to get the current
values: `kubectl get configmap prometheus-adapter -n $MON_NS -o yaml`

```
helm upgrade -i prometheus-adapter prometheus-community/prometheus-adapter \
  -n $MON_NS \
  -f config/samples/prometheus-adapter-values-ocp.yaml
```

### Step 3: Install WVA Controller Into a Namespace
```
kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: $WVA_NS
  labels:
    app.kubernetes.io/name: workload-variant-autoscaler
    control-plane: controller-manager
    openshift.io/user-monitoring: "true"
EOF

cd $WVA_PROJECT/charts
helm upgrade -i workload-variant-autoscaler ./workload-variant-autoscaler \
  -n $WVA_NS \
  --set-file wva.prometheus.caCert=/tmp/prometheus-ca.crt \
  --set controller.enabled=true \
  --set va.enabled=false \
  --set hpa.enabled=false \
  --set vllmService.enabled=false
```

### Step 4: Add Models as Scale Targets To WVA Controller
After a WVA controller has been installed,
you can add one or more models running in LLMD namespaces as scale targets to the WVA controller. As an example, the following command adds model name `my-model-a` with model ID `meta-llama/Llama-3.1-8` running in `team-a` LLMD namespace. This command creates the corresponding VA, HPA resources in `team-a` namespace.
```
helm install -i wva-model-a ./workload-variant-autoscaler \
  -n $WVA_NS \
  --set controller.enabled=false \
  --set va.enabled=true \
  --set hpa.enabled=true \
  --set llmd.namespace=team-a \
  --set llmd.modelName=my-model-a \
  --set llmd.modelID="meta-llama/Llama-3.1-8"
```
Here is an example to add another model to the same WVA controller:
```
helm install -i wva-model-b ./workload-variant-autoscaler \
  -n $WVA_NS \
  --set controller.enabled=false \
  --set va.enabled=true \
  --set hpa.enabled=true \
  --set llmd.namespace=team-a \
  --set llmd.modelName=my-model-b \
  --set llmd.modelID="Qwen/Qwen3-0.6B"
```


## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| hpa.behavior.scaleDown.policies | list | `[{"periodSeconds":150,"type":"Pods","value":10}]` | Scale-down policies |
| hpa.behavior.scaleDown.selectPolicy | string | `"Max"` | Scale-down policy selection |
| hpa.behavior.scaleDown.stabilizationWindowSeconds | int | `240` | Scale-down stabilization window |
| hpa.behavior.scaleUp.policies | list | `[{"periodSeconds":150,"type":"Pods","value":10}]` | Scale-up policies |
| hpa.behavior.scaleUp.selectPolicy | string | `"Max"` | Scale-up policy selection |
| hpa.behavior.scaleUp.stabilizationWindowSeconds | int | `240` | Scale-up stabilization window |
| hpa.enabled | bool | `true` |  |
| hpa.maxReplicas | int | `10` |  |
| hpa.targetAverageValue | string | `"1"` |  |
| llmd.modelID | string | `"unsloth/Meta-Llama-3.1-8B"` |  |
| llmd.modelName | string | `"ms-inference-scheduling-llm-d-modelservice"` |  |
| llmd.namespace | string | `"llm-d-autoscaler"` |  |
| va.accelerator | string | `"H100"` |  |
| va.enabled | bool | `true` |  |
| va.sloTpot | int | `10` |  |
| va.sloTtft | int | `1000` |  |
| vllmService.enabled | bool | `true` |  |
| vllmService.interval | string | `"15s"` |  |
| vllmService.nodePort | int | `30000` |  |
| vllmService.scheme | string | `"http"` |  |
| wva.configMap.immutable | bool | `false` | If true, makes the main controller ConfigMap (`{release-name}-variantautoscaling-config`) immutable (cannot be updated after creation). Provides security benefits by preventing accidental or malicious configuration changes, but disables runtime config updates. Note: This only affects the main ConfigMap; other ConfigMaps (saturation scaling, scale-to-zero) are not affected. See [Configuration Guide](../../docs/user-guide/configuration.md) |
| wva.controllerInstance | string | `""` | Controller instance label for multi-controller isolation. When set, adds `controller_instance` label to all metrics and filters VariantAutoscaling resources by matching label. Use for parallel testing or multi-tenant environments. See [Multi-Controller Isolation](../../docs/user-guide/multi-controller-isolation.md) |
| wva.enabled | bool | `true` |  |
| wva.image.repository | string | `"ghcr.io/llm-d/llm-d-workload-variant-autoscaler"` |  |
| wva.image.tag | string | `"latest"` |  |
| wva.imagePullPolicy | string | `"Always"` |  |
| wva.metrics.enabled | bool | `true` |  |
| wva.metrics.port | int | `8443` |  |
| wva.metrics.secure | bool | `true` |  |
| wva.prometheus.baseURL | string | `"https://thanos-querier.openshift-monitoring.svc.cluster.local:9091"` |  |
| wva.prometheus.monitoringNamespace | string | `"openshift-user-workload-monitoring"` |  |
| wva.prometheus.tls.caCertPath | string | `"/etc/ssl/certs/prometheus-ca.crt"` |  |
| wva.prometheus.tls.insecureSkipVerify | bool | `true` |  |
| wva.reconcileInterval | string | `"60s"` |  |
| wva.scaleToZero | bool | `false` |  |

----------------------------------------------
Autogenerated from chart metadata using [helm-docs v1.14.2](https://github.com/norwoodj/helm-docs/releases/v1.14.2)

## Configuration Files

### Production vs Development Values

The Helm chart provides different configuration files for different environments:

#### Production Values (`values.yaml`)
- **TLS Verification**: Enabled (`insecureSkipVerify: false`)
- **Logging Level**: Production (`LOG_LEVEL: info`)
- **Security**: Strict security settings for production use
- **Saturation-based Scaling**: Conservative thresholds for production stability

#### Development Values (`values-dev.yaml`)
- **TLS Verification**: Relaxed (`insecureSkipVerify: true`) for easier development
- **Logging Level**: Debug (`LOG_LEVEL: debug`) for detailed development logging
- **Security**: Relaxed settings for development and testing
- **Saturation Scaling**: Aggressive thresholds for faster iteration

### Saturation Scaling Configuration

The chart includes saturation-based scaling thresholds that determine when replicas are saturated and when to scale up:

**Global Defaults** (applied to all models):
```yaml
wva:
  capacityScaling:
    default:
      kvCacheThreshold: 0.80      # Replica saturated if KV cache ≥ 80%
      queueLengthThreshold: 5     # Replica saturated if queue ≥ 5 requests
      kvSpareTrigger: 0.1         # Scale-up if spare KV capacity < 10%
      queueSpareTrigger: 3        # Scale-up if spare queue < 3
```

**Per-Model Overrides** (customize specific models):
```yaml
wva:
  capacityScaling:
    overrides:
      llm-d:
        modelID: "Qwen/Qwen3-0.6B"
        namespace: "llm-d-autoscaler"
        kvCacheThreshold: 0.70      # Lower threshold for production
        kvSpareTrigger: 0.35        # Avg spare KV <10% → scale-up
```

See `docs/saturation-scaling-config.md` for detailed configuration documentation.

### HPA Behavior Configuration

The chart provides full control over HPA scaling behavior through the `hpa.behavior` section. This allows you to configure stabilization windows and scaling policies without post-deployment patching.

**Default Configuration:**
```yaml
hpa:
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 240  # Wait 240s before scaling up
      selectPolicy: Max
      policies:
        - type: Pods
          value: 10
          periodSeconds: 150
    scaleDown:
      stabilizationWindowSeconds: 240  # Wait 240s before scaling down
      selectPolicy: Max
      policies:
        - type: Pods
          value: 10
          periodSeconds: 150
```

**Configuration via Helm:**
```bash
# Production: Conservative scaling (240s stabilization)
helm install workload-variant-autoscaler ./workload-variant-autoscaler \
  --set hpa.behavior.scaleUp.stabilizationWindowSeconds=240 \
  --set hpa.behavior.scaleDown.stabilizationWindowSeconds=240

# E2E Testing: Fast scaling (30s stabilization)
helm install workload-variant-autoscaler ./workload-variant-autoscaler \
  --set hpa.behavior.scaleUp.stabilizationWindowSeconds=30 \
  --set hpa.behavior.scaleDown.stabilizationWindowSeconds=30
```

**Configuration via install.sh:**
```bash
# Set stabilization window via environment variable
HPA_STABILIZATION_SECONDS=120 ./deploy/install.sh

# Production default (240s)
./deploy/install.sh
```

**Key Parameters:**
- **stabilizationWindowSeconds**: Time to wait before applying scaling decisions (prevents flapping)
- **selectPolicy**: How to choose from multiple policies (`Max`, `Min`, `Disabled`)
- **policies**: List of scaling policies defining rate limits

**Best Practices:**
- **Production**: Use 120-300 seconds for stability
- **Development**: Use 30-60 seconds for faster iteration
- **E2E Tests**: Use 30 seconds for rapid validation

See [HPA Integration Guide](../../docs/integrations/hpa-integration.md) for detailed information.

### Usage Examples

#### Production Deployment
```bash
# Use production values (secure by default)
helm install workload-variant-autoscaler ./workload-variant-autoscaler \
  -n workload-variant-autoscaler-system \
  --values values.yaml
```

#### Development Deployment
```bash
# Use development values (relaxed security)
helm install workload-variant-autoscaler ./workload-variant-autoscaler \
  -n workload-variant-autoscaler-system \
  --values values-dev.yaml
```

#### Custom Configuration
```bash
# Override specific values
helm install workload-variant-autoscaler ./workload-variant-autoscaler \
  -n workload-variant-autoscaler-system \
  --values values.yaml \
  --set wva.prometheus.tls.insecureSkipVerify=true \
  --set wva.image.tag=v0.0.1-dev
```

#### Immutable ConfigMap (Security Hardening)
```bash
# Enable immutable ConfigMap for enhanced security
# This prevents accidental or malicious configuration changes
# Note: Disables runtime config updates (requires ConfigMap recreation for changes)
helm install workload-variant-autoscaler ./workload-variant-autoscaler \
  -n workload-variant-autoscaler-system \
  --values values.yaml \
  --set wva.configMap.immutable=true
```

**Security Benefits:**
- Prevents accidental configuration changes
- Protects against malicious modifications
- Ensures configuration integrity
- Reduces attack surface

**Trade-offs:**
- Runtime config updates are disabled
- Configuration changes require:
  1. Deleting the ConfigMap
  2. Updating Helm values and upgrading
  3. Restarting the controller pod

### Multi-Controller Isolation

When running multiple WVA controllers in the same cluster (e.g., for parallel e2e tests or multi-tenant environments), use the `controllerInstance` configuration to prevent metrics conflicts between controllers. See [Multi-Controller Isolation](../../docs/user-guide/multi-controller-isolation.md) for detailed configuration.

#### E2E Testing Example

For parallel e2e tests, each test run can use a unique controller instance:
```bash
# Each PR/run uses its namespace as the controller instance
CONTROLLER_INSTANCE="llm-d-autoscaler-pr-123" ./deploy/install.sh
```

This ensures that:
- Metrics from PR-123's controller have `controller_instance="llm-d-autoscaler-pr-123"`
- HPA for PR-123 only considers metrics with that label
- Stale controllers from other PRs don't affect the HPA decisions

#### Backwards Compatibility

When `wva.controllerInstance` is not set (empty string):
- No `controller_instance` label is added to metrics
- HPA selector does not filter by `controller_instance`
- Behavior is identical to previous versions

### CLEANUP
```
export MON_NS="openshift-user-workload-monitoring"
export WVA_NS="workload-variant-autoscaler-system"

helm delete prometheus-adapter -n $MON_NS
helm delete workload-variant-autoscaler -n $WVA_NS
kubectl delete ns $WVA_NS
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
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/$NAMESPACE/wva_desired_replicas" | jq
```
