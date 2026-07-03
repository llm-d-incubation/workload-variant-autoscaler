# WVA with EPP Saturation Analyzer

Guide for the **EPP saturation analyzer** — an autoscaling mode that consumes
the pool-level saturation signal emitted by the gateway-api-inference-extension
(EPP) latency detector, instead of computing saturation from per-pod vLLM
metrics.

For GKE-specific cluster setup (Prometheus, TLS, GMP), see
[README-gke.md](README-gke.md). For the general deployment guide (all
platforms), see [README.md](README.md).

## When to use this

Use the EPP saturation analyzer when:

- Your EPP has the **predicted-latency producer plugin** enabled and emits the
  predicted/actual latency histograms
  (`llm_d_epp_request_predicted_ttft_seconds`, `llm_d_epp_request_ttft_seconds`,
  `llm_d_epp_request_ntpot_seconds`) to Prometheus — WVA derives the saturation
  signal from these vs the configured SLOs (`ttftSLOMs`/`tpotSLOMs`); requests
  must carry `x-llm-d-slo-*-ms` headers
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

## How WVA uses the signal

With `N` current replicas and saturation `S`, WVA uses a normalized capacity
model: `supply = 1.0` (pool's full SLO budget), `demand = S`, and each replica
contributes `1/N` of the supply. This makes the replica recommendation
proportional to pool size:

```
desired_replicas =
  min(ceil(S × N / scaleUpThreshold),   maxReplicas)  if S > scaleUpThreshold
  N                                                    if scaleDownBoundary ≤ S ≤ scaleUpThreshold
  max(ceil(S × N / scaleDownBoundary), minReplicas)   if S < scaleDownBoundary
```

The hysteresis band between `scaleDownBoundary` (default `0.40`) and
`scaleUpThreshold` (default `0.55`) absorbs minor signal noise. The defaults
are calibrated to the P90 signal's healthy band (see
docs/developer-guide/epp-saturation-benchmark.md); raise them if your SLO sits
close to your base latency.

## The scripts

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
| `SCALE_UP_THRESHOLD` | (analyzer default `0.55`) | Override: saturation above which WVA recommends scale-up |
| `SCALE_DOWN_BOUNDARY` | (analyzer default `0.40`) | Override: saturation below which WVA recommends scale-down |
| `OBSERVE_ONLY` | `false` | `true` → disable HPA, WVA just emits recommendations |
| `HPA_MIN_REPLICAS` / `HPA_MAX_REPLICAS` | `1` / `20` | HPA bounds (ignored in observe mode) |
| `MODEL_ID` | (none) | Model identifier for the VA CR |
| `SCALE_TARGET_NAME` | (none) | Name of Deployment/LWS to scale |
| `MODEL_NAMESPACE` | (none) | Namespace where the model runs |
| `DRY_RUN` | `false` | Print actions without executing |

### `deploy-prometheus-tls-proxy.sh`

Helper for HTTP-only Prometheus backends. See [README-gke.md](README-gke.md)
for usage.

## Quickstart (assumes cluster is already set up)

Follow [README-gke.md](README-gke.md) first to get Prometheus + scrape configs
in place, then:

```bash
# Deploy WVA in observe-only mode (no actual scaling)
IMG=us-docker.pkg.dev/<PROJECT>/<REPO>/wva:epp-saturation \
PROMETHEUS_URL=https://prometheus-tls.wva-monitoring.svc.cluster.local:9443 \
OBSERVE_ONLY=true \
MODEL_ID="Qwen/Qwen3-32B" \
SCALE_TARGET_NAME=my-vllm-deployment \
MODEL_NAMESPACE=default \
HPA_MIN_REPLICAS=1 \
HPA_MAX_REPLICAS=50 \
  ./deploy/deploy-epp-saturation.sh

# Watch recommendations live
kubectl logs -n workload-variant-autoscaler-system -l control-plane=controller-manager -f | \
  grep -E 'EPP pool saturation|analysis result|action.*target'

# Check VA status for the recommended replica count
kubectl get variantautoscaling <name>-va -n <ns> -o yaml
#   .status.desiredOptimizedAlloc.numReplicas is WVA's recommendation
```

## Switching to active scaling

Once you've validated the recommendations look correct, switch from
observe-only to active scaling by redeploying with `OBSERVE_ONLY=false` (or
unset). This enables the chart's HPA which consumes the `wva_desired_replicas`
metric (via prometheus-adapter or KEDA — see the main
[deploy README](README.md) for adapter setup).

## Observability

Three key metrics:

| Metric | Where | Meaning |
|---|---|---|
| `llm_d_epp_request_predicted_ttft_seconds` (+ actual TTFT/TPOT histograms) | EPP → Prometheus | Latency signals WVA derives saturation from (P90 vs SLO) |
| `wva_epp_saturation_raw` / `wva_epp_saturation_smoothed` | WVA → Prometheus | The derived saturation signal before/after clamping+EMA |
| `wva_desired_replicas` | WVA → Prometheus | WVA's recommended replica count |
| `wva_current_replicas` | WVA → Prometheus | Current actual replica count |

The WVA-emitted metrics are on `workload-variant-autoscaler-metrics:8443/metrics`
(HTTPS, bearer token auth). Add a Prometheus scrape config pointing there to
make them queryable (see [README-gke.md](README-gke.md)).

## Troubleshooting

**"No saturation metrics available - pods may not be ready or metrics not yet scraped"**
- Happens if WVA can't query the EPP saturation metric from Prometheus
- Check the EPP target in Prometheus is `up` and the metric is present
- Check WVA can reach Prometheus at `PROMETHEUS_URL`

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
