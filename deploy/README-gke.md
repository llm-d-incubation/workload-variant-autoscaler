# Deploying WVA on GKE

Step-by-step guide for deploying the Workload-Variant-Autoscaler (WVA) on
Google Kubernetes Engine. Covers the GKE-specific plumbing around Prometheus
and TLS.

For the **EPP saturation analyzer** specifically, see
[README-epp-saturation.md](README-epp-saturation.md).
For the **general deployment guide** (all platforms), see [README.md](README.md).

> This guide only documents steps that have been verified end-to-end on a
> GKE cluster. Other GKE options (GMP frontend, kube-prometheus-stack,
> active HPA scaling with prometheus-adapter/KEDA) are possible but not
> covered here.

---

## Prerequisites

- GKE cluster with at least one GPU node pool
- `kubectl` configured for the cluster
- Helm 3 installed
- Your inference workload (vLLM, llm-d, etc.) already running in the cluster

---

## Step 1: Deploy a standalone Prometheus

```bash
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update

helm install prometheus prometheus-community/prometheus \
  -n wva-monitoring --create-namespace \
  --set server.persistentVolume.enabled=false \
  --set alertmanager.enabled=false \
  --set kube-state-metrics.enabled=false \
  --set prometheus-node-exporter.enabled=false \
  --set prometheus-pushgateway.enabled=false \
  --wait --timeout 300s
```

This gives you `prometheus-server.wva-monitoring.svc.cluster.local:80` (HTTP).

---

## Step 2: Expose Prometheus over HTTPS

**WVA validates that `PROMETHEUS_BASE_URL` uses `https://`** — connections
over plain HTTP are rejected at startup. Since the standalone chart serves
HTTP only, deploy the TLS proxy:

```bash
NAMESPACE=wva-monitoring \
BACKEND_HOST=prometheus-server.wva-monitoring.svc.cluster.local \
BACKEND_PORT=80 \
  ./deploy/deploy-prometheus-tls-proxy.sh
```

This creates:
- A self-signed TLS cert
- An `nginx:1.25-alpine` Deployment that fronts Prometheus
- A `prometheus-tls` Service on port `9443`

Use `https://prometheus-tls.wva-monitoring.svc.cluster.local:9443` as the
`PROMETHEUS_URL` for WVA.

---

## Step 3: Configure Prometheus to scrape workload + WVA metrics

WVA needs metrics from the EPP (for saturation) and emits its own metrics
that Prometheus should scrape back.

Write the values file:

```yaml
# prom-values.yaml
server:
  persistentVolume:
    enabled: false
alertmanager:
  enabled: false
kube-state-metrics:
  enabled: false
prometheus-node-exporter:
  enabled: false
prometheus-pushgateway:
  enabled: false
extraScrapeConfigs: |
  - job_name: epp-metrics
    scheme: http
    scrape_interval: 5s
    authorization:
      type: Bearer
      credentials_file: /var/run/secrets/kubernetes.io/serviceaccount/token
    static_configs:
      - targets:
          - <your-epp-service>.<ns>.svc.cluster.local:9090
  - job_name: wva-metrics
    scheme: https
    scrape_interval: 5s
    tls_config:
      insecure_skip_verify: true
    authorization:
      type: Bearer
      credentials_file: /var/run/secrets/kubernetes.io/serviceaccount/token
    static_configs:
      - targets:
          - workload-variant-autoscaler-metrics.workload-variant-autoscaler-system.svc.cluster.local:8443
```

```bash
helm upgrade prometheus prometheus-community/prometheus \
  -n wva-monitoring -f prom-values.yaml --wait --timeout 60s
```

Grant the Prometheus SA permission to access token-authenticated endpoints:

```bash
kubectl create clusterrolebinding prometheus-auth-delegator \
  --clusterrole=system:auth-delegator \
  --serviceaccount=wva-monitoring:prometheus-server

kubectl create clusterrole prometheus-metrics-reader \
  --non-resource-url=/metrics --verb=get
kubectl create clusterrolebinding prometheus-metrics-reader \
  --clusterrole=prometheus-metrics-reader \
  --serviceaccount=wva-monitoring:prometheus-server
```

If the EPP has `--metrics-endpoint-auth=true` (the default), the scrape will
fail with `401 Unauthorized` unless the Prometheus SA's bearer token is
accepted. Either:
- Keep auth on and rely on the ClusterRoleBinding above (preferred), or
- Add `--metrics-endpoint-auth=false --secure-serving=false` to the EPP
  deployment's container args (simpler for dev)

Restart the Prometheus deployment to pick up the new config:

```bash
kubectl rollout restart deployment prometheus-server -n wva-monitoring
```

Verify the targets are healthy (port-forward + curl):

```bash
kubectl port-forward -n wva-monitoring svc/prometheus-server 19090:80 &
curl -s 'http://localhost:19090/api/v1/targets?state=active' | \
  python3 -c "import json,sys; \
    [print(f\"{t['labels']['job']:20s} {t['health']}\") for t in json.load(sys.stdin)['data']['activeTargets']]"
```

Both `epp-metrics` and `wva-metrics` jobs should show `up`.

---

## Step 4: (Optional) build and push a custom WVA image

The default chart uses the upstream image
(`ghcr.io/llm-d/llm-d-workload-variant-autoscaler`), so skip to Step 5 unless
you need local changes:

```bash
gcloud auth configure-docker us-docker.pkg.dev
export IMG=us-docker.pkg.dev/<PROJECT>/<REPO>/wva:latest
make docker-build docker-push IMG=$IMG
```

Then override the image in Step 6:
```
--set wva.image.repository=us-docker.pkg.dev/<PROJECT>/<REPO>/wva \
--set wva.image.tag=latest
```

---

## Step 5: Install the VariantAutoscaling CRD

```bash
kubectl apply -f charts/workload-variant-autoscaler/crds/llmd.ai_variantautoscalings.yaml
```

If an older version is already installed, this upgrades it. Existing
`VariantAutoscaling` resources may become incompatible with the new schema
— check `kubectl get variantautoscaling -A` and recreate if needed.

---

## Step 6: Deploy WVA via Helm

```bash
helm upgrade -i workload-variant-autoscaler \
  ./charts/workload-variant-autoscaler \
  -n workload-variant-autoscaler-system --create-namespace \
  --set wva.prometheus.baseURL=https://prometheus-tls.wva-monitoring.svc.cluster.local:9443 \
  --set wva.prometheus.tls.insecureSkipVerify=true \
  --set wva.namespaceScoped=false \
  --set controller.enabled=true \
  --set va.enabled=false \
  --set vllmService.enabled=false \
  --set hpa.enabled=false
```

Notes:
- Uses the default `ghcr.io/llm-d/llm-d-workload-variant-autoscaler` image
  from the chart's `values.yaml`
- `va.enabled=false` disables the chart's sample VariantAutoscaling
  (it assumes a specific namespace that may not exist on your cluster)
- `vllmService.enabled=false` disables the chart's sample vLLM service
- `hpa.enabled=false` runs in **observe-only mode** — WVA emits
  recommendations to the VA status and to Prometheus, but no HPA is
  created, so no actual scaling happens. This is useful for first-time
  validation before enabling the scaling loop.

### Enabling active scaling (after validation)

To wire the recommendations into actual replica changes, enable the HPA
in the chart:

```bash
helm upgrade workload-variant-autoscaler ./charts/workload-variant-autoscaler \
  -n workload-variant-autoscaler-system --reuse-values \
  --set hpa.enabled=true \
  --set hpa.minReplicas=1 \
  --set hpa.maxReplicas=20
```

The chart creates an HPA that reads `wva_desired_replicas` as an
**external metric**. For the external metrics API to be served, you also
need a **metrics adapter**:

- **prometheus-adapter** — translates Prometheus metrics into the external
  metrics API
- **KEDA** — alternative, uses a `ScaledObject` CR

The repo's installer (`deploy/install.sh`) sets this up automatically
including the APIService guard that prevents KEDA from reclaiming the
external metrics endpoint from prometheus-adapter. See
[deploy/lib/scaler_runtime.sh](lib/scaler_runtime.sh) for the
reference flow.

**Active scaling with prometheus-adapter/KEDA has not been validated in
this guide.** Follow the main [deploy README](README.md) or use
`deploy/install.sh` for that path.

---

## Step 7: Create a `VariantAutoscaling` CR for your model

```yaml
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: my-model-va
  namespace: default
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: my-vllm-deployment
  modelID: "Qwen/Qwen3-32B"
  minReplicas: 1
  maxReplicas: 20
  variantCost: "10.0"
```

```bash
kubectl apply -f my-va.yaml
```

---

## Step 8: Verify

```bash
# WVA pods running?
kubectl get pods -n workload-variant-autoscaler-system

# WVA reading metrics and making recommendations?
kubectl logs -n workload-variant-autoscaler-system \
  -l control-plane=controller-manager -f | \
  grep -E 'Processing model|action.*target'

# VA status shows a recommendation?
kubectl get variantautoscaling -A -o custom-columns=\
NAME:.metadata.name,\
DESIRED:.status.desiredOptimizedAlloc.numReplicas,\
ACCEL:.status.desiredOptimizedAlloc.accelerator,\
METRICS:.status.conditions[?(@.type=='MetricsAvailable')].status
```

---

## Troubleshooting

**Stale Helm ownership after a previous `kube-prometheus-stack` install**
Cluster-scoped resources (ClusterRoles, Services, webhooks) can retain
`meta.helm.sh/release-namespace` annotations from the old release and
block a new install in a different namespace. Either re-annotate them to
the new namespace or `helm uninstall` completely first and then reinstall.

**WVA logs `TLS configuration validation failed - HTTPS is required`**
`PROMETHEUS_URL` must start with `https://`. Use the TLS proxy from Step 2
if your Prometheus only serves HTTP.

**Prometheus scrape shows `server returned HTTP status 401 Unauthorized`**
The target's metrics endpoint requires authentication. Either add the
bearer token scrape config + ClusterRoleBindings from Step 3, or disable
auth on the target (e.g., `--metrics-endpoint-auth=false` on EPP).

**Prometheus scrape shows `http: server gave HTTP response to HTTPS client`**
The scrape config uses `scheme: https` but the target serves HTTP. Change
to `scheme: http` in the scrape config.
