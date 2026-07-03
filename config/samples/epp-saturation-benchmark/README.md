# EPP-saturation benchmark — deployment bundle

The exact objects behind the results in
[docs/developer-guide/epp-saturation-benchmark.md](../../../docs/developer-guide/epp-saturation-benchmark.md).
Example names assume a serving stack in `llm-d-optimized-baseline` with a
decode Deployment `optimized-baseline-nvidia-gpu-vllm-decode` and an EPP
service `optimized-baseline-epp` — adjust to yours.

## Prerequisites

- A running llm-d serving stack: vLLM decode Deployment + EPP with the
  predicted-latency producer plugin and latency-predictor sidecars — the exact
  EPP plugin configuration the benchmark ran is in
  `epp-plugins-configmap.yaml`. Requests must carry the SLO headers
  (`x-llm-d-slo-ttft-ms` / `x-llm-d-slo-tpot-ms`) for the EPP to emit
  per-request latency and violation metrics.
- Prometheus reachable by WVA (the repo's
  [`deploy/deploy-prometheus-tls-proxy.sh`](../../../deploy/deploy-prometheus-tls-proxy.sh)
  sets up prometheus + the TLS proxy WVA reads through).

## Deploy order

1. **Metrics plumbing**
   - Grant the EPP ServiceAccount auth-delegator so scrapes authenticate:
     `kubectl apply -f epp-metrics-clusterrolebinding.yaml` (edit the subject).
   - Merge the two scrape jobs from `prometheus-scrape-configmap.yaml` into the
     prometheus-server ConfigMap and restart Prometheus. The `wva-metrics` job
     must stay a static-target job with the `namespace → exported_namespace`
     relabel (see the file's comments for why).
2. **WVA** — run
   [`deploy/deploy-epp-saturation.sh`](../../../deploy/deploy-epp-saturation.sh)
   (installs the CRD, deploys WVA with `analyzerName: epp-saturation`). The
   signal-side defaults (P90 signal, `scaleUpThreshold` 0.55,
   `scaleDownBoundary` 0.40, `smoothingAlpha` 0.6, `saturationCap` 2.0) are
   built in; override them per model in the saturation-scaling ConfigMap only
   if your SLO sits close to your base latency.
3. **prometheus-adapter** — expose `wva_desired_replicas` on
   `external.metrics.k8s.io`:
   `helm upgrade -i prometheus-adapter prometheus-community/prometheus-adapter
   -n wva-monitoring -f ../prometheus-adapter-values.yaml
   --set prometheus.url=http://prometheus-server.wva-monitoring.svc --set prometheus.port=80`
4. **Autoscaling objects** —
   `kubectl apply -f decode-variantautoscaling.yaml -f decode-horizontalpodautoscaler.yaml`
   (the HPA carries the benchmarked scale policies).
5. **Warmup fixes** (strongly recommended; ~2m15s → ~100s pod-ready):
   `kubectl -n <ns> patch deploy <decode> --type=strategic
   --patch-file decode-warmup-deployment-patch.yaml`
6. **Verify the chain** before loading:
   `kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/<ns>/wva_desired_replicas"`
   should return a value, and the HPA should show `ScalingActive=True`.

## Run the benchmark

```sh
kubectl apply -f inference-perf-configmap.yaml -f inference-perf-job.yaml
```

Scoring protocol (counter snapshots, denominator from the job's report,
dual-window cost) and the warm-start / predictor burn-in requirements are in
the benchmark doc's "Reproducing" section.
