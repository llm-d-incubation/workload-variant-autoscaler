# Deploying WVA on GKE

Step-by-step guide for deploying the Workload-Variant-Autoscaler (WVA) on
Google Kubernetes Engine. Covers the GKE-specific plumbing around Prometheus,
TLS, and Google Managed Prometheus (GMP).

For the **EPP saturation analyzer** specifically, see
[README-epp-saturation.md](README-epp-saturation.md).
For the **general deployment guide** (all platforms), see [README.md](README.md).

---

## Prerequisites

- GKE cluster with at least one GPU node pool
- `kubectl` configured for the cluster
- Helm 3 installed
- Docker + write access to a container registry (Artifact Registry recommended)
- Your inference workload (vLLM, llm-d, etc.) already running in the cluster

---

## Step 1: Choose a Prometheus setup

WVA queries Prometheus for metrics. GKE clusters typically have one of:

### Option A: Google Managed Prometheus (GMP)

GMP is enabled by default on GKE Autopilot and available on Standard clusters.
It uses `PodMonitoring` / `ClusterPodMonitoring` CRDs from
`monitoring.googleapis.com/v1`. Check if it's running:

```bash
kubectl get pods -n gmp-system -l app.kubernetes.io/name=collector
```

GMP collectors run as a DaemonSet and store data in Cloud Monitoring. WVA cannot
query GMP directly from within the cluster — you'd need to deploy the
**managed Prometheus frontend** or run a separate in-cluster Prometheus that
scrapes the same targets.

### Option B: Self-managed Prometheus (kube-prometheus-stack or standalone)

Simpler for development. Deploy the standalone Prometheus chart:

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

## Step 2: Expose Prometheus over HTTPS (required by WVA)

**WVA validates that `PROMETHEUS_BASE_URL` uses `https://`** — connections over
plain HTTP are rejected at startup. If your Prometheus is HTTP-only (like the
standalone chart above), deploy the TLS proxy:

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

If you're using kube-prometheus-stack with TLS enabled, skip this step and
point WVA directly at the managed Prometheus service.

---

## Step 3: Configure Prometheus to scrape your workload metrics

WVA needs metrics from your inference workload and (optionally) from the EPP.
Typical metrics to scrape:

- **vLLM pods** (`:8000/metrics` or custom port) — KV cache, queue depth,
  request latency
- **EPP** (`:9090/metrics`) — saturation signals, scheduler metrics
- **WVA itself** (`:8443/metrics`) — `wva_desired_replicas` etc.

### For self-managed Prometheus

Add `extraScrapeConfigs` to your Helm values:

```yaml
extraScrapeConfigs: |
  - job_name: epp-metrics
    scheme: http
    scrape_interval: 5s
    authorization:
      type: Bearer
      credentials_file: /var/run/secrets/kubernetes.io/serviceaccount/token
    static_configs:
      - targets:
          - my-epp.default.svc.cluster.local:9090
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

Then `helm upgrade prometheus prometheus-community/prometheus -n wva-monitoring -f values.yaml`.

Also grant the Prometheus SA permission to access token-authenticated endpoints:

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

### For GMP

Apply a `PodMonitoring` (namespaced) or `ClusterPodMonitoring` (cluster-wide)
resource. See [wva-podmonitoring.yaml](wva-podmonitoring.yaml) for a working
example (includes RBAC for GMP to read the WVA bearer token secret).

```bash
kubectl apply -f deploy/wva-podmonitoring.yaml
```

GMP's `port` field matches the **container port name** (e.g., `https` for port
8443), not the number.

---

## Step 4: (Optional) build and push a custom WVA image

The default chart uses the upstream image
(`ghcr.io/llm-d/llm-d-workload-variant-autoscaler`), so you can skip this step
and go straight to Step 5.

Only build your own image if you need to test local changes:

```bash
# Authenticate to Artifact Registry
gcloud auth configure-docker us-docker.pkg.dev

# Build and push
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

If an older version of the CRD is already installed from a previous deploy,
this will upgrade it. Existing `VariantAutoscaling` resources may become
incompatible with the new schema — check `kubectl get variantautoscaling -A`
and recreate if needed.

---

## Step 6: Deploy WVA via Helm

Uses the default upstream image from the chart's `values.yaml`:

```bash
helm upgrade -i workload-variant-autoscaler \
  ./charts/workload-variant-autoscaler \
  -n workload-variant-autoscaler-system --create-namespace \
  --set wva.prometheus.baseURL=https://prometheus-tls.wva-monitoring.svc.cluster.local:9443 \
  --set wva.prometheus.tls.insecureSkipVerify=true \
  --set wva.namespaceScoped=false \
  --set wva.reconcileInterval=30s \
  --set controller.enabled=true \
  --set va.enabled=false \
  --set vllmService.enabled=false \
  --set hpa.enabled=false
```

Notes:
- Uses the default `ghcr.io/llm-d/llm-d-workload-variant-autoscaler` image
  pinned in the chart's `values.yaml`. Override with `wva.image.repository` /
  `wva.image.tag` if you built a custom image in Step 4.
- `va.enabled=false` disables the chart's sample VariantAutoscaling
  (it assumes a specific namespace that may not exist on your cluster)
- `vllmService.enabled=false` disables the chart's sample vLLM service
- `hpa.enabled=false` runs in observe-only mode (WVA emits recommendations,
  no actual scaling). Enable later with `hpa.enabled=true` + `hpa.minReplicas`,
  `hpa.maxReplicas`.

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

# VA status updated?
kubectl get variantautoscaling -A -o custom-columns=\
NAME:.metadata.name,\
DESIRED:.status.desiredOptimizedAlloc.numReplicas,\
ACCEL:.status.desiredOptimizedAlloc.accelerator,\
METRICS:.status.conditions[?(@.type=='MetricsAvailable')].status
```

---

## Switching to active scaling

When you've validated the recommendations look right, enable the HPA:

```bash
helm upgrade workload-variant-autoscaler ./charts/workload-variant-autoscaler \
  -n workload-variant-autoscaler-system --reuse-values \
  --set hpa.enabled=true \
  --set hpa.minReplicas=1 \
  --set hpa.maxReplicas=20
```

For the HPA to actually scale the Deployment, you also need a metrics adapter
that exposes `wva_desired_replicas` via the external metrics API:
**prometheus-adapter** or **KEDA**. See the main [README.md](README.md) for
adapter setup details.

---

## Troubleshooting (GKE-specific)

**`ImagePullBackOff` from Artifact Registry**
- Make sure nodes have Workload Identity or the right IAM role
  (`roles/artifactregistry.reader`)
- Check `gcloud auth configure-docker` was run in the same region

**Prometheus pod crash-loops after reinstalling kube-prometheus-stack**
- Old ClusterRoles / webhooks may have stale `meta.helm.sh/release-namespace`
  annotations. Either re-annotate them to the new namespace or `helm uninstall`
  completely first and then reinstall.

**GMP collector logs `watch: unknown (get secrets)`**
- The collector ServiceAccount needs permission to read the bearer token
  Secret referenced in PodMonitoring `authorization.credentials.secret`.
  Add a Role + RoleBinding (see
  [wva-podmonitoring.yaml](wva-podmonitoring.yaml) for the pattern).

**WVA logs `TLS configuration validation failed - HTTPS is required`**
- `PROMETHEUS_URL` must start with `https://`. Use the TLS proxy
  (Step 2) if your Prometheus only serves HTTP.

**WVA logs `server returned HTTP status 401 Unauthorized`**
- The EPP or target metrics endpoint requires authentication. Add bearer
  token auth to the scrape config, or disable auth on the target
  (`--metrics-endpoint-auth=false` for EPP).

**`wva_desired_replicas` not appearing in Prometheus**
- Check the `wva-metrics` scrape target is healthy (`up == 1`)
- The WVA metrics endpoint is HTTPS + bearer token — confirm your scrape
  config has `tls_config.insecure_skip_verify: true` and the
  `credentials_file` or equivalent auth
- Only the **leader** WVA pod emits metrics (leader election via lease);
  verify at least one pod holds the lease
