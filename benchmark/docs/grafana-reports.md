# Grafana reports for benchmark runs

`benchmark/hack/benchmark_report.py` turns a raw `llmdbenchmark` results
directory into a standalone `report.html` per run, with links to the
relevant Grafana panels (vLLM KV-cache utilization and queue size) for the
exact time window of that run.

Prometheus doesn't retain data forever, so a plain dashboard link goes stale.
To keep the link useful after retention expires, the tool captures a Grafana
**snapshot** — a frozen, self-contained copy of the panel data — immediately
after the run, while Prometheus still has it.

This assumes a local Grafana at `http://localhost:3000` that you install and
point at the benchmark cluster's Prometheus yourself; the tool only
provisions the datasource + dashboard inside it and drives its snapshot API.

## One-time setup

Run everything through `benchmark/hack/benchmark_report.sh` (not
`benchmark_report.py` directly) — it creates and maintains its own venv at
`benchmark/hack/.venv/` with `requirements.txt` installed, so it works even
on systems (e.g. Homebrew Python) where plain `pip install` is blocked. No
manual venv/pip step needed.

Install Grafana locally — either Homebrew:

```bash
brew install grafana
brew services start grafana
```

or Docker:

```bash
docker run -d --name grafana -p 3000:3000 grafana/grafana-oss
```

Either way it comes up at `http://localhost:3000` (default login `admin`/`admin`).

Port-forward the benchmark cluster's Prometheus to `localhost:9090`. The
service name depends on which monitoring stack the cluster has — check
`keda.prometheus.baseUrl` in the scenario you ran, e.g.:

```bash
# kube-prometheus-stack (the Helm install used by benchmark/README.md's
# "Prepare a Kubernetes cluster" section)
kubectl port-forward -n monitoring svc/prometheus-operated 9090:9090

# or, if the cluster names the release differently:
kubectl port-forward -n <ns> svc/kube-prometheus-stack-prometheus 9090:9090
```

Provision the Prometheus datasource and the benchmark dashboard into your
local Grafana:

```bash
benchmark/hack/benchmark_report.sh configure
```

This is idempotent — re-run it any time you're unsure Grafana is set up
correctly, or after editing `benchmark/config/grafana/dashboard.json`.

## After a benchmark run

With the Prometheus port-forward still running (so the snapshot step can
read the data before it's gone), run:

```bash
benchmark/hack/benchmark_report.sh all <run_dir>
```

where `<run_dir>` is the top-level `llmdbenchmark` run directory, e.g.
`$(BENCHMARK_WORKSPACE)/<user>-<timestamp>/`. This:

1. re-checks Grafana is configured (skip with `--skip-configure`),
2. captures a Grafana snapshot for each experiment under `<run_dir>/results/`,
   writing `grafana_snapshot.yaml` next to that experiment's other result
   files,
3. renders `<run_dir>/report.html` with a section per experiment: run
   metadata, the lifecycle metrics from `summary_lifecycle_metrics.json`,
   the workload config, and both Grafana links (permanent snapshot + live
   time-boxed dashboard).

Each step can also be run on its own — `configure`, `snapshot <experiment_dir>`,
`report <run_dir>` — see `benchmark/hack/benchmark_report.sh --help`.

If Grafana isn't running, `snapshot` fails for that experiment but `report`
still succeeds; the affected experiment's section just says no snapshot was
captured, with the command to capture one later (as long as Prometheus still
has the data).

## Extending the dashboard

`benchmark/config/grafana/dashboard.json` starts minimal — KV-cache
utilization and queue size — scoped by a `namespace` dashboard variable so
the same dashboard works across benchmark namespaces. Add panels the same
way (replica counts, scaling activity, latency) and re-run `configure` to
push the update; `snapshot` picks up whatever panels/targets exist on the
dashboard at capture time.
