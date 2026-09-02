#!/usr/bin/env python3
"""
Provision a local Grafana instance for benchmark observability, snapshot the
relevant dashboard for a run before Prometheus expires its data, and render
a standalone HTML summary report per run.

Usage:
    # One-time (or whenever unsure Grafana is set up correctly):
    python3 benchmark/hack/benchmark_report.py configure

    # After a benchmark run finishes (while Prometheus still has the data):
    python3 benchmark/hack/benchmark_report.py all <run_dir>

    # Individual steps:
    python3 benchmark/hack/benchmark_report.py snapshot <experiment_dir>
    python3 benchmark/hack/benchmark_report.py report <run_dir>

<run_dir> is the top-level llmdbenchmark run directory, e.g.
`<workspace>/<user>-<timestamp>/`. <experiment_dir> is one experiment under
its `results/` subdirectory, e.g. `<run_dir>/results/inference-perf-*_1`.
"""

import argparse
import base64
import datetime
import glob
import json
import os
import re
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

import yaml

DASHBOARD_JSON_PATH = os.path.join(
    os.path.dirname(os.path.abspath(__file__)), "..", "config", "grafana", "dashboard.json"
)
DASHBOARD_UID = "llm-d-autoscaling-benchmark-run"
DEFAULT_GRAFANA_URL = "http://localhost:3000"
DEFAULT_GRAFANA_USER = "admin"
DEFAULT_GRAFANA_PASSWORD = "admin"
DEFAULT_PROMETHEUS_URL = "http://localhost:9090"
SNAPSHOT_WINDOW_BUFFER_SECONDS = 60


def _http(method, url, user=None, password=None, json_body=None, params=None):
    """Issue an HTTP request, returning (status_code, decoded_json_or_None)."""
    if params:
        url = f"{url}?{urllib.parse.urlencode(params)}"
    data = json.dumps(json_body).encode() if json_body is not None else None
    headers = {"Content-Type": "application/json", "Accept": "application/json"}
    req = urllib.request.Request(url, data=data, headers=headers, method=method)
    if user is not None:
        creds = base64.b64encode(f"{user}:{password}".encode()).decode()
        req.add_header("Authorization", f"Basic {creds}")
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            body = resp.read()
            return resp.status, (json.loads(body) if body else None)
    except urllib.error.HTTPError as e:
        body = e.read()
        try:
            return e.code, json.loads(body)
        except (ValueError, UnicodeDecodeError):
            return e.code, {"error": body.decode(errors="replace")}
    except urllib.error.URLError as e:
        raise ConnectionError(f"cannot reach {url}: {e.reason}") from e


def _load_yaml(path):
    if not os.path.isfile(path):
        return {}
    with open(path) as f:
        return yaml.safe_load(f) or {}


def _load_json(path):
    if not os.path.isfile(path):
        return None
    with open(path) as f:
        return json.load(f)


# --------------------------------------------------------------------------
# configure
# --------------------------------------------------------------------------

def cmd_configure(args):
    grafana_url = args.grafana_url.rstrip("/")
    user, password = args.grafana_user, args.grafana_password

    try:
        status, health = _http("GET", f"{grafana_url}/api/health")
    except ConnectionError as e:
        print(f"ERROR: {e}", file=sys.stderr)
        return 1
    if status != 200:
        print(f"ERROR: Grafana health check failed ({status}): {health}", file=sys.stderr)
        return 1
    print(f"Grafana reachable at {grafana_url} (version {health.get('version', '?')})")

    ds_payload = {
        "name": "Prometheus",
        "type": "prometheus",
        "access": "proxy",
        "url": args.prometheus_url,
        "isDefault": True,
    }
    status, existing = _http(
        "GET", f"{grafana_url}/api/datasources/name/Prometheus", user=user, password=password
    )
    if status == 200:
        if existing.get("url") != args.prometheus_url:
            status, result = _http(
                "PUT",
                f"{grafana_url}/api/datasources/{existing['id']}",
                user=user,
                password=password,
                json_body=ds_payload,
            )
            if status not in (200, 202):
                print(f"ERROR: failed to update Prometheus datasource: {result}", file=sys.stderr)
                return 1
            print(f"Updated Prometheus datasource URL to {args.prometheus_url}")
        else:
            print("Prometheus datasource already configured correctly")
    else:
        status, result = _http(
            "POST", f"{grafana_url}/api/datasources", user=user, password=password, json_body=ds_payload
        )
        if status not in (200, 201):
            print(f"ERROR: failed to create Prometheus datasource: {result}", file=sys.stderr)
            return 1
        print(f"Created Prometheus datasource pointing at {args.prometheus_url}")

    with open(DASHBOARD_JSON_PATH) as f:
        dashboard = json.load(f)
    dashboard["id"] = None
    status, result = _http(
        "POST",
        f"{grafana_url}/api/dashboards/db",
        user=user,
        password=password,
        json_body={"dashboard": dashboard, "overwrite": True, "folderId": 0},
    )
    if status != 200:
        print(f"ERROR: failed to provision dashboard: {result}", file=sys.stderr)
        return 1
    print(f"Dashboard provisioned: {grafana_url}{result.get('url', '')}")
    return 0


# --------------------------------------------------------------------------
# snapshot
# --------------------------------------------------------------------------

def _run_window(experiment_dir):
    """Return (namespace, from_epoch_s, to_epoch_s) for an experiment's run window."""
    meta = _load_yaml(os.path.join(experiment_dir, "run_metadata.yaml"))
    if not meta.get("harness_start") or not meta.get("harness_stop"):
        raise ValueError(f"no harness_start/harness_stop in {experiment_dir}/run_metadata.yaml")
    start = yaml.safe_load(f'x: {meta["harness_start"]}')["x"]
    stop = yaml.safe_load(f'x: {meta["harness_stop"]}')["x"]
    start_s = start.timestamp() - SNAPSHOT_WINDOW_BUFFER_SECONDS
    stop_s = stop.timestamp() + SNAPSHOT_WINDOW_BUFFER_SECONDS
    return meta.get("namespace", ".*"), start_s, stop_s, meta.get("experiment_id", os.path.basename(experiment_dir))


# Grafana's App Platform (k8s-style) API namespaces dashboards per org; the
# default org (id 1, the only org in a fresh local instance) is "default".
GRAFANA_K8S_NAMESPACE = "default"


def _prometheus_datasource_uid(grafana_url, user, password):
    status, ds = _http(
        "GET", f"{grafana_url}/api/datasources/name/Prometheus", user=user, password=password
    )
    if status != 200:
        raise RuntimeError(f"Prometheus datasource not found ({status}): {ds}")
    return ds["uid"]


def _query_range(grafana_url, ds_uid, user, password, expr, namespace, start_s, stop_s, step):
    expr = expr.replace("$namespace", namespace)
    status, result = _http(
        "GET",
        f"{grafana_url}/api/datasources/proxy/uid/{ds_uid}/api/v1/query_range",
        user=user,
        password=password,
        params={"query": expr, "start": start_s, "end": stop_s, "step": step},
    )
    if status != 200 or result.get("status") != "success":
        print(f"WARNING: query_range failed for {expr!r}: {result}", file=sys.stderr)
        return []
    return result["data"]["result"]


def _interpolate_legend(legend_format, labels):
    if not legend_format:
        return labels.get("__name__", "value")
    return re.sub(r"\{\{\s*(\w+)\s*\}\}", lambda m: labels.get(m.group(1), ""), legend_format)


def _iso(epoch_s):
    return (
        datetime.datetime.fromtimestamp(epoch_s, datetime.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3]
        + "Z"
    )


def _prometheus_series_to_frame(refid, expr, step_s, labels, times_s, values):
    """Build a Grafana DataFrameJSON frame (the shape /api/ds/query and v2
    dashboard snapshots use) for one Prometheus series."""
    value_field_name = labels.get("__name__", "Value")
    legend = _interpolate_legend(labels.get("__legend__", ""), labels) or value_field_name
    return {
        "schema": {
            "refId": refid,
            "fields": [
                {
                    "name": "Time",
                    "type": "time",
                    "typeInfo": {"frame": "time.Time"},
                    "config": {"interval": step_s * 1000},
                },
                {
                    "name": value_field_name,
                    "type": "number",
                    "typeInfo": {"frame": "float64"},
                    "labels": {k: v for k, v in labels.items() if k != "__legend__"},
                    "config": {"displayNameFromDS": legend},
                },
            ],
            "meta": {
                "custom": {"calculatedMinStep": step_s * 1000, "resultType": "matrix"},
                "executedQueryString": f"Expr: {expr}\nStep: {step_s}s",
                "preferredVisualisationType": "graph",
                "type": "timeseries-multi",
                "typeVersion": [0, 1],
            },
        },
        "data": {"values": [times_s, values]},
    }


def cmd_snapshot(args):
    grafana_url = args.grafana_url.rstrip("/")
    user, password = args.grafana_user, args.grafana_password

    try:
        namespace, start_s, stop_s, experiment_id = _run_window(args.experiment_dir)
        ds_uid = _prometheus_datasource_uid(grafana_url, user, password)
        status, dashboard_resp = _http(
            "GET",
            f"{grafana_url}/apis/dashboard.grafana.app/v2/namespaces/{GRAFANA_K8S_NAMESPACE}"
            f"/dashboards/{args.dashboard_uid}",
            user=user,
            password=password,
        )
        if status != 200:
            raise RuntimeError(f"dashboard {args.dashboard_uid} not found ({status}): {dashboard_resp}")
    except (ConnectionError, RuntimeError, ValueError) as e:
        print(f"ERROR: {e}", file=sys.stderr)
        return 1

    # Grafana's dashboard snapshots embed frozen query results using the v2
    # ("Scenes") dashboard schema (elements/layout), not the legacy
    # panels/gridPos schema returned by /api/dashboards/uid/<uid> -- the
    # legacy shape is silently accepted by POST /api/snapshots but the
    # modern snapshot viewer fails to load it ("Snapshot not found").
    spec = dashboard_resp["spec"]
    step_s = max(15, int((stop_s - start_s) / 500))
    for element in spec.get("elements", {}).values():
        for query in element.get("spec", {}).get("data", {}).get("spec", {}).get("queries", []):
            query_spec = query["spec"]
            refid = query_spec["refId"]
            orig = query_spec["query"]["spec"]
            expr = orig["expr"].replace("$namespace", namespace)
            legend_format = orig.get("legendFormat", "")

            series = _query_range(grafana_url, ds_uid, user, password, expr, namespace, start_s, stop_s, step_s)
            frames = []
            for s in series:
                labels = dict(s.get("metric", {}), __legend__=legend_format)
                times = [int(float(t) * 1000) for t, _ in s["values"]]
                values = [float(v) for _, v in s["values"]]
                frames.append(_prometheus_series_to_frame(refid, expr, step_s, labels, times, values))

            query_spec["query"] = {
                "datasource": {"name": "grafana"},
                "group": "grafana",
                "kind": "DataQuery",
                "spec": {"queryType": "snapshot", "snapshot": frames},
            }

    spec["timeSettings"]["from"] = _iso(start_s)
    spec["timeSettings"]["to"] = _iso(stop_s)

    dashboard = dict(spec)
    dashboard["uid"] = args.dashboard_uid
    dashboard["id"] = None
    dashboard["schemaVersion"] = None
    dashboard["version"] = None
    dashboard["snapshot"] = {"originalUrl": f"/d/{args.dashboard_uid}"}

    status, snap_result = _http(
        "POST",
        f"{grafana_url}/api/snapshots",
        user=user,
        password=password,
        json_body={"dashboard": dashboard, "name": experiment_id, "expires": 0},
    )
    if status != 200:
        print(f"ERROR: failed to create snapshot: {snap_result}", file=sys.stderr)
        return 1

    live_url = (
        f"{grafana_url}/d/{args.dashboard_uid}"
        f"?orgId=1&from={int(start_s * 1000)}&to={int(stop_s * 1000)}"
        f"&var-namespace={urllib.parse.quote(namespace)}"
    )
    out = {
        "experiment_id": experiment_id,
        "namespace": namespace,
        "captured_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "snapshot_url": snap_result.get("url"),
        "snapshot_delete_url": snap_result.get("deleteUrl"),
        "live_dashboard_url": live_url,
    }
    out_path = os.path.join(args.experiment_dir, "grafana_snapshot.yaml")
    with open(out_path, "w") as f:
        yaml.safe_dump(out, f, sort_keys=False)
    print(f"Snapshot captured: {out['snapshot_url']}")
    print(f"Wrote {out_path}")
    return 0


# --------------------------------------------------------------------------
# report
# --------------------------------------------------------------------------

def _flatten(value, prefix=""):
    """Yield (dotted.path, value) for scalar leaves of a nested dict/list."""
    if isinstance(value, dict):
        for k, v in value.items():
            yield from _flatten(v, f"{prefix}.{k}" if prefix else str(k))
    elif isinstance(value, list):
        if value and all(isinstance(v, (int, float, str, type(None))) for v in value):
            yield prefix, ", ".join(str(v) for v in value)
        elif value:
            yield prefix, f"[{len(value)} items]"
    else:
        yield prefix, value


def _html_escape(s):
    return (
        str(s)
        .replace("&", "&amp;")
        .replace("<", "&lt;")
        .replace(">", "&gt;")
        .replace('"', "&quot;")
    )


def _kpi_table_html(rows):
    body = "\n".join(
        f"<tr><td>{_html_escape(k)}</td><td>{_html_escape(v)}</td></tr>" for k, v in rows
    )
    return f"<table class='kpi'><tbody>{body}</tbody></table>"


def _render_experiment(experiment_dir):
    meta = _load_yaml(os.path.join(experiment_dir, "run_metadata.yaml"))
    traffic = _load_yaml(os.path.join(experiment_dir, "traffic_complete.yaml"))
    config_raw = ""
    config_path = os.path.join(experiment_dir, "config.yaml")
    if os.path.isfile(config_path):
        with open(config_path) as f:
            config_raw = f.read()

    metrics = _load_json(os.path.join(experiment_dir, "summary_lifecycle_metrics.json")) or _load_json(
        os.path.join(experiment_dir, "stage_0_lifecycle_metrics.json")
    )

    experiment_id = meta.get("experiment_id", os.path.basename(experiment_dir))
    meta_rows = [
        ("experiment_id", experiment_id),
        ("harness", meta.get("harness_name")),
        ("model", meta.get("model")),
        ("namespace", meta.get("namespace")),
        ("start", meta.get("harness_start")),
        ("stop", meta.get("harness_stop")),
        ("duration", meta.get("harness_delta")),
        ("exit_code", meta.get("harness_rc")),
        ("traffic_completed_at", traffic.get("completed_at")),
    ]
    meta_rows = [(k, v) for k, v in meta_rows if v is not None]

    metric_rows = list(_flatten(metrics)) if metrics else []

    snapshot = _load_yaml(os.path.join(experiment_dir, "grafana_snapshot.yaml"))
    if snapshot:
        grafana_html = (
            "<p><strong>Grafana snapshot</strong> (survives Prometheus retention): "
            f"<a href='{_html_escape(snapshot['snapshot_url'])}'>{_html_escape(snapshot['snapshot_url'])}</a>"
            f" &mdash; captured {_html_escape(snapshot.get('captured_at', '?'))}</p>"
            "<p><strong>Live dashboard</strong> (time-boxed to this run, requires Prometheus to "
            f"still hold the data): <a href='{_html_escape(snapshot['live_dashboard_url'])}'>open</a></p>"
        )
    else:
        grafana_html = (
            "<p class='muted'>No Grafana snapshot captured for this run. "
            f"Run <code>python3 benchmark/hack/benchmark_report.py snapshot {_html_escape(experiment_dir)}</code> "
            "while Prometheus still has the data.</p>"
        )

    return f"""
<section class="experiment">
  <h2>{_html_escape(experiment_id)}</h2>
  <h3>Run</h3>
  {_kpi_table_html(meta_rows)}
  <h3>Metrics</h3>
  {_kpi_table_html(metric_rows) if metric_rows else "<p class='muted'>No lifecycle metrics found.</p>"}
  <h3>Grafana</h3>
  {grafana_html}
  <details>
    <summary>Workload config</summary>
    <pre>{_html_escape(config_raw)}</pre>
  </details>
</section>
"""


_HTML_STYLE = """
body { font-family: -apple-system, Helvetica, Arial, sans-serif; margin: 2rem; color: #1a1a1a; }
h1 { border-bottom: 2px solid #ddd; padding-bottom: .5rem; }
section.experiment { margin-bottom: 3rem; padding: 1rem 1.5rem; border: 1px solid #ddd; border-radius: 8px; }
table.kpi { border-collapse: collapse; margin: .5rem 0 1rem; }
table.kpi td { padding: .25rem .75rem; border-bottom: 1px solid #eee; }
table.kpi td:first-child { font-weight: 600; color: #444; }
.muted { color: #888; }
pre { background: #f6f6f6; padding: 1rem; overflow-x: auto; font-size: .85em; }
code { background: #f0f0f0; padding: .1em .3em; border-radius: 3px; }
"""


def cmd_report(args):
    experiment_dirs = sorted(
        d
        for d in glob.glob(os.path.join(args.run_dir, "results", "*"))
        if os.path.isfile(os.path.join(d, "run_metadata.yaml"))
    )
    if not experiment_dirs:
        print(f"ERROR: no experiments with run_metadata.yaml found under {args.run_dir}/results", file=sys.stderr)
        return 1

    sections = "\n".join(_render_experiment(d) for d in experiment_dirs)
    html = f"""<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Benchmark report: {_html_escape(os.path.basename(os.path.normpath(args.run_dir)))}</title>
<style>{_HTML_STYLE}</style>
</head>
<body>
<h1>Benchmark report: {_html_escape(os.path.basename(os.path.normpath(args.run_dir)))}</h1>
{sections}
</body>
</html>
"""
    out_path = os.path.join(args.run_dir, "report.html")
    with open(out_path, "w") as f:
        f.write(html)
    print(f"Wrote {out_path}")
    return 0


# --------------------------------------------------------------------------
# all
# --------------------------------------------------------------------------

def cmd_all(args):
    if not args.skip_configure:
        if cmd_configure(args) != 0:
            print("WARNING: configure step failed; continuing without it", file=sys.stderr)

    experiment_dirs = sorted(
        d
        for d in glob.glob(os.path.join(args.run_dir, "results", "*"))
        if os.path.isfile(os.path.join(d, "run_metadata.yaml"))
    )
    for d in experiment_dirs:
        snapshot_args = argparse.Namespace(
            experiment_dir=d,
            grafana_url=args.grafana_url,
            grafana_user=args.grafana_user,
            grafana_password=args.grafana_password,
            dashboard_uid=args.dashboard_uid,
        )
        if cmd_snapshot(snapshot_args) != 0:
            print(f"WARNING: snapshot failed for {d}; report will note it as missing", file=sys.stderr)

    return cmd_report(argparse.Namespace(run_dir=args.run_dir))


# --------------------------------------------------------------------------
# CLI
# --------------------------------------------------------------------------

def _add_grafana_auth_args(parser):
    parser.add_argument("--grafana-url", default=DEFAULT_GRAFANA_URL)
    parser.add_argument("--grafana-user", default=DEFAULT_GRAFANA_USER)
    parser.add_argument("--grafana-password", default=DEFAULT_GRAFANA_PASSWORD)


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)

    p_configure = sub.add_parser("configure", help="Provision the Prometheus datasource + benchmark dashboard")
    _add_grafana_auth_args(p_configure)
    p_configure.add_argument("--prometheus-url", default=DEFAULT_PROMETHEUS_URL)
    p_configure.set_defaults(func=cmd_configure)

    p_snapshot = sub.add_parser("snapshot", help="Capture a Grafana snapshot for one experiment")
    p_snapshot.add_argument("experiment_dir")
    _add_grafana_auth_args(p_snapshot)
    p_snapshot.add_argument("--dashboard-uid", default=DASHBOARD_UID)
    p_snapshot.set_defaults(func=cmd_snapshot)

    p_report = sub.add_parser("report", help="Render report.html for a run")
    p_report.add_argument("run_dir")
    p_report.set_defaults(func=cmd_report)

    p_all = sub.add_parser("all", help="configure + snapshot (per experiment) + report")
    p_all.add_argument("run_dir")
    _add_grafana_auth_args(p_all)
    p_all.add_argument("--prometheus-url", default=DEFAULT_PROMETHEUS_URL)
    p_all.add_argument("--dashboard-uid", default=DASHBOARD_UID)
    p_all.add_argument("--skip-configure", action="store_true")
    p_all.set_defaults(func=cmd_all)

    args = parser.parse_args()
    sys.exit(args.func(args))


if __name__ == "__main__":
    main()
