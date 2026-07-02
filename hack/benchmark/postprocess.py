#!/usr/bin/env python3
"""
Post-process llm-d-benchmark results into a markdown table.

Produces the exact table format used in docs/benchmark.md.

Usage:
    # Single run:
    python hack/benchmark/postprocess.py results/guidellm-*_1

    # Three runs (Run 1 | Run 2 | Run 3 | Avg):
    python hack/benchmark/postprocess.py results/guidellm-*_1 \\
                                          results/guidellm-*_2 \\
                                          results/guidellm-*_3

    # With scenario header:
    python hack/benchmark/postprocess.py --scenario "Prefill Heavy — Qwen/Qwen3-32B (600s)" \\
        results/guidellm-*_1 results/guidellm-*_2 results/guidellm-*_3
"""

import argparse
import json
import os
import sys
from statistics import mean

METRICS = [
    "P99 TTFT (ms)",
    "P99 ITL (ms/token)",
    "Avg replicas",
    "Max replicas",
    "Avg KV cache utilization",
    "Avg queue depth (EPP)",
    "Error count",
    "Avg pod startup (s)",
]


def _parse_prometheus_value(line, metric_name):
    """Extract a float from a Prometheus exposition-format line."""
    if not line.startswith(metric_name):
        return None
    rest = line[len(metric_name):]
    if rest and rest[0] not in ("{", " "):
        return None
    if rest.startswith("{"):
        close = rest.find("}")
        if close < 0:
            return None
        rest = rest[close + 1:]
    try:
        return float(rest.strip())
    except (ValueError, IndexError):
        return None


def _extract_latency(results_dir):
    """P99 TTFT and P99 ITL from results.json."""
    path = os.path.join(results_dir, "results.json")
    if not os.path.isfile(path):
        return None, None

    with open(path) as f:
        data = json.load(f)

    metrics = data["benchmarks"][0]["metrics"]

    def _p99(section_key):
        section = metrics.get(section_key, {}).get("successful", {})
        pcts = section.get("percentiles", {})
        if isinstance(pcts, list):
            pcts = {p["percentile"]: p["value"] for p in pcts}
        return pcts.get("p99") or pcts.get(99) or section.get("max")

    return _p99("time_to_first_token_ms"), _p99("inter_token_latency_ms")


def _extract_error_count(results_dir):
    """Error count from results.json."""
    path = os.path.join(results_dir, "results.json")
    if not os.path.isfile(path):
        return 0
    with open(path) as f:
        data = json.load(f)
    return data["benchmarks"][0]["metrics"]["request_totals"].get("errored", 0)


def _extract_replica_stats(results_dir):
    """Avg and max ready replicas from replica_status_timeseries.json."""
    path = os.path.join(results_dir, "metrics", "processed",
                        "replica_status_timeseries.json")
    if not os.path.isfile(path):
        return None, None

    with open(path) as f:
        data = json.load(f)

    totals = []
    for snap in data["snapshots"]:
        ready = sum(
            (c.get("ready_replicas", 0) or 0) for c in snap["controllers"]
            if "decode" in c.get("name", "")
        )
        totals.append(ready)

    if not totals:
        return None, None
    return mean(totals), max(totals)


def _extract_kv_cache_avg(results_dir):
    """Average KV cache utilization (%) from raw vLLM metrics."""
    raw_dir = os.path.join(results_dir, "metrics", "raw")
    if not os.path.isdir(raw_dir):
        return None

    values = []
    for fname in os.listdir(raw_dir):
        if not fname.endswith(".log") or "router-epp" in fname:
            continue
        if fname == "collection_debug.log":
            continue
        fpath = os.path.join(raw_dir, fname)
        with open(fpath) as f:
            for line in f:
                val = _parse_prometheus_value(line.strip(),
                                              "vllm:kv_cache_usage_perc")
                if val is not None:
                    values.append(val * 100)
                    break

    return mean(values) if values else None


def _extract_queue_depth_avg(results_dir):
    """Average EPP queue depth from raw EPP metrics."""
    raw_dir = os.path.join(results_dir, "metrics", "raw")
    if not os.path.isdir(raw_dir):
        return None

    values = []
    for fname in sorted(os.listdir(raw_dir)):
        if not fname.endswith(".log") or "router-epp" not in fname:
            continue
        fpath = os.path.join(raw_dir, fname)
        with open(fpath) as f:
            for line in f:
                val = _parse_prometheus_value(
                    line.strip(),
                    "inference_extension_flow_control_queue_size")
                if val is not None:
                    values.append(val)
                    break

    return mean(values) if values else None


def _extract_pod_startup_avg(results_dir):
    """Average pod startup time (s) from pod_startup_times.json."""
    path = os.path.join(results_dir, "metrics", "processed",
                        "pod_startup_times.json")
    if not os.path.isfile(path):
        return None

    with open(path) as f:
        data = json.load(f)

    times = [p["startup_seconds"] for p in data.get("pods", [])
             if p.get("startup_seconds") is not None]
    return mean(times) if times else None


def _fmt(metric, value):
    """Format a value to match the benchmark.md number style."""
    if value is None:
        return "?"

    if metric == "P99 TTFT (ms)":
        return f"{value:,.0f}"
    if metric == "P99 ITL (ms/token)":
        return f"{value:.2f}" if (value * 100) % 10 != 0 else f"{value:.1f}"
    if metric == "Avg replicas":
        return f"{value:.2f}"
    if metric == "Max replicas":
        return str(int(value))
    if metric == "Avg KV cache utilization":
        return f"{value:.1f}%"
    if metric == "Avg queue depth (EPP)":
        return f"{value:.1f}"
    if metric == "Error count":
        return f"{int(value):,}"
    if metric == "Avg pod startup (s)":
        return str(round(value))
    return str(value)


def process_one(results_dir):
    """Extract all benchmark.md metrics from one results directory."""
    p99_ttft, p99_itl = _extract_latency(results_dir)
    avg_rep, max_rep = _extract_replica_stats(results_dir)
    kv_avg = _extract_kv_cache_avg(results_dir)
    queue_avg = _extract_queue_depth_avg(results_dir)
    startup_avg = _extract_pod_startup_avg(results_dir)
    error_count = _extract_error_count(results_dir)

    return {
        "P99 TTFT (ms)": p99_ttft,
        "P99 ITL (ms/token)": p99_itl,
        "Avg replicas": avg_rep,
        "Max replicas": max_rep,
        "Avg KV cache utilization": kv_avg,
        "Avg queue depth (EPP)": queue_avg,
        "Error count": error_count,
        "Avg pod startup (s)": startup_avg,
    }


def _compute_avg(runs):
    """Compute average column across multiple runs (raw numeric values)."""
    avg = {}
    for m in METRICS:
        vals = [r[m] for r in runs if r[m] is not None]
        avg[m] = mean(vals) if vals else None
    return avg


def format_table(runs, labels):
    """Render a markdown table matching the benchmark.md style."""
    show_avg = len(runs) > 1
    cols = list(labels)
    data_cols = list(runs)

    if show_avg:
        cols.append("Avg")
        data_cols.append(_compute_avg(runs))

    header = "| Metric | " + " | ".join(cols) + " |"
    sep = "|--------|" + "|".join(["------"] * len(cols)) + "|"

    rows = []
    for m in METRICS:
        cells = [_fmt(m, run[m]) for run in data_cols]
        rows.append(f"| {m} | " + " | ".join(cells) + " |")

    return "\n".join([header, sep] + rows)


def main():
    ap = argparse.ArgumentParser(
        description="Post-process llm-d-benchmark results into a markdown table")
    ap.add_argument("results_dirs", nargs="+",
                    help="One or more benchmark results directories")
    ap.add_argument("--scenario", type=str, default=None,
                    help="Scenario heading (e.g. 'Prefill Heavy — Qwen/Qwen3-32B (600s)')")
    ap.add_argument("--json", action="store_true",
                    help="Output raw JSON instead of markdown")
    args = ap.parse_args()

    runs = []
    labels = []
    for d in args.results_dirs:
        if not os.path.isdir(d):
            print(f"WARNING: {d} is not a directory, skipping", file=sys.stderr)
            continue
        print(f"Processing: {d}", file=sys.stderr)
        runs.append(process_one(d))
        labels.append(f"Run {len(runs)}")

    if not runs:
        print("ERROR: No valid results directories found", file=sys.stderr)
        sys.exit(1)

    if args.json:
        print(json.dumps(runs, indent=2))
        return

    print()
    if args.scenario:
        print(f"### {args.scenario}\n")
    print(format_table(runs, labels))
    print()


if __name__ == "__main__":
    main()
