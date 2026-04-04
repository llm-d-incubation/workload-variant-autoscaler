# WVA with EPP Saturation Analyzer

Scripts for deploying WVA using the **EPP saturation analyzer** — an autoscaling
mode that consumes the pool-level saturation signal emitted by the
gateway-api-inference-extension (EPP) latency detector, instead of computing
saturation from per-pod vLLM metrics.

## When to use this

Use the EPP saturation analyzer when:

- Your EPP has the **latency detector plugin** enabled and emits
  `inference_extension_latency_detector_pool_saturation` to Prometheus
- You want **predictive** autoscaling (driven by predicted latency vs SLO)
  rather than reactive autoscaling (driven by observed KV cache / queue depth)
- Your pool has a **single model** (current simplification — no per-model
  saturation breakdown from the EPP signal)

## How the saturation signal works

The EPP latency detector runs a background probe every `probeInterval`:

1. Builds a synthetic prediction request from current traffic profile
2. Calls the latency prediction sidecar per endpoint
3. Computes `endpointSaturation = predictedLatency / SLO`
4. Averages across endpoints → `poolSaturation`

Semantics:
- `< 1.0` = headroom available
- `>= 1.0` = at or over SLO
- Score is unbounded (can be > 1.0 when heavily overloaded)

## The two scripts

### `deploy-epp-saturation.sh`

Main deployment script. Builds the WVA image, installs the Helm chart with
`analyzerName: epp-saturation`, and optionally creates a `VariantAutoscaling`
CR for your model.

**What it does:**
1. Validates `PROMETHEUS_URL` starts with `https://` (WVA enforces HTTPS)
2. Applies/upgrades the `VariantAutoscaling` CRD from `charts/.../crds/`
3. Builds and pushes the WVA container image (`BUILD_IMAGE=true` by default)
4. Installs the Helm chart with:
   - `analyzerName=epp-saturation` + `scaleUpThreshold` / `scaleDownBoundary`
   - Controller enabled
   - Sample VA/vllmService disabled (to avoid namespace-not-found errors)
   - HPA optional (disabled by `OBSERVE_ONLY=true`)
5. Creates a `VariantAutoscaling` CR if `MODEL_ID`, `SCALE_TARGET_NAME`, and
   `MODEL_NAMESPACE` are set

**Key environment variables:**

| Variable | Default | Purpose |
|---|---|---|
| `IMG` | (kaushikmitra registry) | WVA image to build/deploy |
| `BUILD_IMAGE` | `true` | Skip image build with `false` (use existing) |
| `PROMETHEUS_URL` | `https://kube-prometheus-stack-prometheus...:9090` | HTTPS URL of Prometheus with EPP metrics |
| `SCALE_UP_THRESHOLD` | `0.85` | Saturation above which WVA recommends scale-up |
| `SCALE_DOWN_BOUNDARY` | `0.50` | Saturation below which WVA recommends scale-down |
| `OBSERVE_ONLY` | `false` | `true` → disable HPA, WVA just emits recommendations |
| `HPA_MIN_REPLICAS` / `HPA_MAX_REPLICAS` | `1` / `20` | HPA bounds (ignored in observe mode) |
| `MODEL_ID` | (none) | Model identifier for the VA CR |
| `SCALE_TARGET_NAME` | (none) | Name of Deployment/LWS to scale |
| `MODEL_NAMESPACE` | (none) | Namespace where the model runs |
| `DRY_RUN` | `false` | Print actions without executing |

### `deploy-prometheus-tls-proxy.sh`

Helper that deploys an **nginx TLS-terminating proxy** in front of an HTTP-only
Prometheus. WVA enforces HTTPS for `PROMETHEUS_BASE_URL` — if your Prometheus
only serves HTTP (e.g., the standalone `prometheus-community/prometheus` chart),
use this to add a TLS front.

**What it creates:**
- A self-signed TLS cert (if not already present)
- ConfigMap with nginx proxy config
- Deployment running `nginx:1.25-alpine`
- Service exposing port `9443`

After running it, set `PROMETHEUS_URL=https://<service>.<ns>.svc.cluster.local:9443`
in `deploy-epp-saturation.sh`.

## End-to-end deployment on GKE

### Prerequisites

- GKE cluster with: EPP deployed + vLLM (or simulator) model pods + Prometheus
- EPP has the latency detector plugin configured and `SaturationDetector`
  bound to it in the plugins config
- Prometheus scrape config for EPP's `/metrics` endpoint
- Verify the metric exists:
  ```bash
  kubectl port-forward -n <prom-ns> svc/<prom-svc> 19090:<port>
  curl 'http://localhost:19090/api/v1/query?query=inference_extension_latency_detector_pool_saturation'
  ```

### Flow

```bash
# 1. (If Prometheus is HTTP-only) Deploy TLS proxy
NAMESPACE=wva-monitoring \
BACKEND_HOST=prometheus-server.wva-monitoring.svc.cluster.local \
BACKEND_PORT=80 \
  ./deploy/deploy-prometheus-tls-proxy.sh

# 2. Deploy WVA in observe-only mode (no actual scaling)
IMG=us-docker.pkg.dev/my-project/my-repo/wva:epp-saturation \
PROMETHEUS_URL=https://prometheus-tls.wva-monitoring.svc.cluster.local:9443 \
OBSERVE_ONLY=true \
MODEL_ID="Qwen/Qwen3-32B" \
SCALE_TARGET_NAME=my-vllm-deployment \
MODEL_NAMESPACE=default \
HPA_MIN_REPLICAS=1 \
HPA_MAX_REPLICAS=50 \
  ./deploy/deploy-epp-saturation.sh

# 3. Watch recommendations live
kubectl logs -n workload-variant-autoscaler-system -l control-plane=controller-manager -f | \
  grep -E 'EPP pool saturation|analysis result|action.*target'

# 4. Check VA status for the recommended replica count
kubectl get variantautoscaling <name>-va -n <ns> -o yaml
#   .status.desiredOptimizedAlloc.numReplicas is WVA's recommendation
```

### Switching to active scaling

Once you've validated the recommendations look correct, switch from
observe-only to active scaling by redeploying with `OBSERVE_ONLY=false` (or
unset). This enables the chart's HPA which consumes the `wva_desired_replicas`
metric (via prometheus-adapter or KEDA — see the main
[deploy README](README.md) for adapter setup).

## Observability

Three key metrics:

| Metric | Where | Meaning |
|---|---|---|
| `inference_extension_latency_detector_pool_saturation` | EPP → Prometheus | Raw saturation signal (predictedLatency / SLO) |
| `wva_desired_replicas` | WVA → Prometheus | WVA's recommended replica count |
| `wva_current_replicas` | WVA → Prometheus | Current actual replica count |

The WVA-emitted metrics are on `workload-variant-autoscaler-metrics:8443/metrics`
(HTTPS, bearer token auth). Add a Prometheus scrape config pointing there to
make them queryable.

## Troubleshooting

**"No saturation metrics available - pods may not be ready or metrics not yet scraped"**
- Happens if WVA can't query the EPP saturation metric from Prometheus
- Check the EPP target in Prometheus is `up` and the metric is present
- Check WVA can reach Prometheus at `PROMETHEUS_URL`

**"TLS configuration validation failed - HTTPS is required"**
- `PROMETHEUS_URL` must start with `https://`
- Deploy the TLS proxy if your Prometheus is HTTP-only

**VA status shows `MetricsAvailable: False` even with EPP metric present**
- Make sure the saturation scaling ConfigMap has `analyzerName: epp-saturation`
- Check WVA logs for the analyzer branch being selected:
  `grep "Processing model (EPP saturation)"` — should appear each cycle

**Scale-up threshold is too aggressive / not aggressive enough**
- Raise `SCALE_UP_THRESHOLD` closer to `1.0` (scale later, at/above SLO)
- Lower it closer to `0.70` (scale earlier, more headroom)
- Similarly adjust `SCALE_DOWN_BOUNDARY` (must be less than scale-up threshold)

**Scaling flaps / oscillates**
- Widen the hysteresis band: increase `SCALE_UP_THRESHOLD` and/or decrease
  `SCALE_DOWN_BOUNDARY`
- Tune HPA behavior (`stabilizationWindowSeconds`) to smooth reactions
