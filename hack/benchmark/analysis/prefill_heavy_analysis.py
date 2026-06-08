#!/usr/bin/env python3
"""
Prefill Heavy scenario analysis — WVA Default(v1) vs Static 2 Replicas
for Qwen3-32B at 600s, with bar charts and comparison.

Usage:
    python3 hack/benchmark/analysis/prefill_heavy_analysis.py
    python3 hack/benchmark/analysis/prefill_heavy_analysis.py --output /path/to/output.png
"""

import argparse
import sys

try:
    import matplotlib.pyplot as plt
    import matplotlib.ticker as ticker
    import numpy as np
except ImportError:
    print("Missing dependencies. Install with: pip install matplotlib numpy")
    sys.exit(1)


# ── Raw data from docs/benchmark.md ──

CONFIGS = ["WVA Default(v1)", "Static 2 Replicas"]
COLORS = ["#3b82f6", "#f59e0b"]

# Per-run data (3 runs each)
WVA_RUNS = {
    "p99_ttft_ms": [98_810, 97_811, 98_638],
    "p99_itl_ms": [55.06, 54.4, 54.98],
    "avg_replicas": [1.68, 1.77, 1.73],
    "max_replicas": [3, 3, 3],
    "kv_cache_pct": [65.1, 69.2, 64.5],
    "queue_depth": [236.8, 252.4, 220.4],
    "error_count": [4186, 4193, 4173],
    "pod_startup_s": [115, 106, 109],
}

STATIC_RUNS = {
    "p99_ttft_ms": [553_930, 552_625, 559_358],
    "p99_itl_ms": [58.2, 57.5, 58.8],
    "avg_replicas": [2.00, 2.00, 2.00],
    "max_replicas": [2, 2, 2],
    "kv_cache_pct": [50.6, 52.6, 50.8],
    "queue_depth": [0, 0, 0],
    "error_count": [0, 0, 0],
    "pod_startup_s": [94, 94, 92],
}

# Averages
AVG = {}
for key in WVA_RUNS:
    AVG[key] = [
        sum(WVA_RUNS[key]) / len(WVA_RUNS[key]),
        sum(STATIC_RUNS[key]) / len(STATIC_RUNS[key]),
    ]


def plot_analysis(output_path: str):
    fig, axes = plt.subplots(3, 2, figsize=(14, 16))
    fig.suptitle(
        "Prefill Heavy — WVA Default(v1) vs Static 2 Replicas\n"
        "Qwen3-32B · 4000 prompt / 1000 output tokens · 20 RPS · 600s",
        fontsize=14, fontweight="bold", y=0.98,
    )

    x = np.arange(len(CONFIGS))
    width = 0.5
    run_x = np.arange(3)
    run_width = 0.35

    # ── 1. P99 TTFT (bar per run + average line) ──
    ax = axes[0, 0]
    for i, (runs, color, label) in enumerate([
        (WVA_RUNS["p99_ttft_ms"], COLORS[0], "WVA"),
        (STATIC_RUNS["p99_ttft_ms"], COLORS[1], "Static"),
    ]):
        vals_s = [v / 1000 for v in runs]
        bars = ax.bar(run_x + i * run_width, vals_s, run_width, color=color, alpha=0.7, label=label)
        avg_s = sum(vals_s) / len(vals_s)
        ax.axhline(y=avg_s, color=color, linestyle="--", linewidth=1.5, alpha=0.8)
        for bar, v in zip(bars, vals_s):
            ax.text(bar.get_x() + bar.get_width() / 2, bar.get_height() + 5,
                    f"{v:.0f}s", ha="center", va="bottom", fontsize=8)
    ax.set_title("P99 TTFT per Run", fontweight="bold")
    ax.set_ylabel("Seconds")
    ax.set_xticks(run_x + run_width / 2)
    ax.set_xticklabels(["Run 1", "Run 2", "Run 3"])
    ax.legend()
    ax.grid(axis="y", alpha=0.3)

    # ── 2. P99 TTFT comparison (averages) ──
    ax = axes[0, 1]
    ttft_avg = [v / 1000 for v in AVG["p99_ttft_ms"]]
    bars = ax.bar(x, ttft_avg, width, color=COLORS)
    for bar, v in zip(bars, ttft_avg):
        ax.text(bar.get_x() + bar.get_width() / 2, bar.get_height() + 5,
                f"{v:.1f}s", ha="center", va="bottom", fontsize=11, fontweight="bold")
    ratio = ttft_avg[1] / ttft_avg[0]
    ax.set_title(f"Avg P99 TTFT — Static is {ratio:.1f}x worse", fontweight="bold")
    ax.set_ylabel("Seconds")
    ax.set_xticks(x)
    ax.set_xticklabels(CONFIGS)
    ax.grid(axis="y", alpha=0.3)

    # ── 3. Error count per run ──
    ax = axes[1, 0]
    for i, (runs, color, label) in enumerate([
        (WVA_RUNS["error_count"], COLORS[0], "WVA"),
        (STATIC_RUNS["error_count"], COLORS[1], "Static"),
    ]):
        bars = ax.bar(run_x + i * run_width, runs, run_width, color=color, alpha=0.7, label=label)
        for bar, v in zip(bars, runs):
            if v > 0:
                ax.text(bar.get_x() + bar.get_width() / 2, bar.get_height() + 30,
                        f"{v:,}", ha="center", va="bottom", fontsize=8)
    ax.set_title("Error Count per Run", fontweight="bold")
    ax.set_ylabel("Errors")
    ax.set_xticks(run_x + run_width / 2)
    ax.set_xticklabels(["Run 1", "Run 2", "Run 3"])
    ax.legend()
    ax.grid(axis="y", alpha=0.3)

    # ── 4. KV Cache + Replicas ──
    ax = axes[1, 1]
    kv_vals = AVG["kv_cache_pct"]
    rep_vals = AVG["avg_replicas"]
    bars1 = ax.bar(x - 0.15, kv_vals, 0.3, color=COLORS, alpha=0.6)
    ax.set_ylabel("KV Cache (%)")
    ax.set_ylim(0, 100)
    for bar, v in zip(bars1, kv_vals):
        ax.text(bar.get_x() + bar.get_width() / 2, bar.get_height() + 1,
                f"{v:.1f}%", ha="center", va="bottom", fontsize=9)

    ax2 = ax.twinx()
    ax2.bar(x + 0.15, rep_vals, 0.3, color=COLORS, alpha=0.3, edgecolor=COLORS, linewidth=2)
    ax2.set_ylabel("Avg Replicas")
    ax2.set_ylim(0, 5)
    for xi, v in zip(x + 0.15, rep_vals):
        ax2.text(xi, v + 0.05, f"{v:.2f}", ha="center", va="bottom", fontsize=9, fontweight="bold")

    ax.set_title("KV Cache (bars) & Avg Replicas (outlined)", fontweight="bold")
    ax.set_xticks(x)
    ax.set_xticklabels(CONFIGS)
    ax.grid(axis="y", alpha=0.3)

    # ── 5. Queue Depth per run ──
    ax = axes[2, 0]
    for i, (runs, color, label) in enumerate([
        (WVA_RUNS["queue_depth"], COLORS[0], "WVA"),
        (STATIC_RUNS["queue_depth"], COLORS[1], "Static"),
    ]):
        bars = ax.bar(run_x + i * run_width, runs, run_width, color=color, alpha=0.7, label=label)
        for bar, v in zip(bars, runs):
            if v > 0:
                ax.text(bar.get_x() + bar.get_width() / 2, bar.get_height() + 2,
                        f"{v:.0f}", ha="center", va="bottom", fontsize=8)
    ax.set_title("EPP Queue Depth per Run", fontweight="bold")
    ax.set_ylabel("Queue Depth")
    ax.set_xticks(run_x + run_width / 2)
    ax.set_xticklabels(["Run 1", "Run 2", "Run 3"])
    ax.legend()
    ax.grid(axis="y", alpha=0.3)
    ax.annotate("Static has no queue\ndepth metric (N/A)",
                xy=(1.17, 0), fontsize=9, color=COLORS[1], fontstyle="italic",
                ha="center", va="bottom")

    # ── 6. Pod Startup per run ──
    ax = axes[2, 1]
    for i, (runs, color, label) in enumerate([
        (WVA_RUNS["pod_startup_s"], COLORS[0], "WVA"),
        (STATIC_RUNS["pod_startup_s"], COLORS[1], "Static"),
    ]):
        bars = ax.bar(run_x + i * run_width, runs, run_width, color=color, alpha=0.7, label=label)
        for bar, v in zip(bars, runs):
            ax.text(bar.get_x() + bar.get_width() / 2, bar.get_height() + 1,
                    f"{v}s", ha="center", va="bottom", fontsize=8)
    ax.set_title("Pod Startup Time per Run", fontweight="bold")
    ax.set_ylabel("Seconds")
    ax.set_xticks(run_x + run_width / 2)
    ax.set_xticklabels(["Run 1", "Run 2", "Run 3"])
    ax.legend()
    ax.grid(axis="y", alpha=0.3)

    plt.tight_layout(rect=[0, 0, 1, 0.96])
    plt.savefig(output_path, dpi=150, bbox_inches="tight")
    print(f"Saved plot to {output_path}")
    plt.close()


def print_analysis():
    print("=" * 80)
    print("PREFILL HEAVY — WVA Default(v1) vs Static 2 Replicas")
    print("Qwen3-32B · 4000 prompt / 1000 output tokens · 20 RPS · 600s")
    print("=" * 80)

    print(f"\n{'Metric':<28} {'WVA Default(v1)':<20} {'Static 2R':<20} {'Delta':<20}")
    print("-" * 88)

    metrics = [
        ("P99 TTFT",
         f"{AVG['p99_ttft_ms'][0]/1000:.1f}s",
         f"{AVG['p99_ttft_ms'][1]/1000:.1f}s",
         f"Static {AVG['p99_ttft_ms'][1]/AVG['p99_ttft_ms'][0]:.1f}x worse"),
        ("P99 ITL (ms/tok)",
         f"{AVG['p99_itl_ms'][0]:.1f}",
         f"{AVG['p99_itl_ms'][1]:.1f}",
         f"{AVG['p99_itl_ms'][1] - AVG['p99_itl_ms'][0]:+.1f} ms"),
        ("Avg replicas",
         f"{AVG['avg_replicas'][0]:.2f}",
         f"{AVG['avg_replicas'][1]:.2f}",
         f"WVA uses {AVG['avg_replicas'][0] - AVG['avg_replicas'][1]:+.2f}"),
        ("Max replicas",
         f"{AVG['max_replicas'][0]:.0f}",
         f"{AVG['max_replicas'][1]:.0f}",
         ""),
        ("Avg KV cache",
         f"{AVG['kv_cache_pct'][0]:.1f}%",
         f"{AVG['kv_cache_pct'][1]:.1f}%",
         f"{AVG['kv_cache_pct'][0] - AVG['kv_cache_pct'][1]:+.1f}pp"),
        ("Avg queue depth",
         f"{AVG['queue_depth'][0]:.1f}",
         "N/A",
         "Static has no queue metric"),
        ("Avg errors",
         f"{AVG['error_count'][0]:,.0f}",
         f"{AVG['error_count'][1]:,.0f}",
         f"WVA has {AVG['error_count'][0]:,.0f} errors"),
        ("Avg pod startup",
         f"{AVG['pod_startup_s'][0]:.0f}s",
         f"{AVG['pod_startup_s'][1]:.0f}s",
         f"WVA {AVG['pod_startup_s'][0] - AVG['pod_startup_s'][1]:+.0f}s slower"),
        ("Cost (GPU-hr)",
         f"{AVG['avg_replicas'][0]:.2f}",
         f"{AVG['avg_replicas'][1]:.2f}",
         f"WVA {(AVG['avg_replicas'][0]/AVG['avg_replicas'][1]-1)*100:+.0f}% cheaper"),
    ]
    for label, wva, static, delta in metrics:
        print(f"{label:<28} {wva:<20} {static:<20} {delta:<20}")

    print(f"\n{'─' * 80}")
    print("INDIVIDUAL RUNS")
    print(f"{'─' * 80}")
    print(f"\n{'Run':<8} {'WVA TTFT':<14} {'Static TTFT':<14} {'WVA Errors':<14} {'Static Errors':<14}")
    print("-" * 64)
    for i in range(3):
        print(f"Run {i+1:<4} "
              f"{WVA_RUNS['p99_ttft_ms'][i]/1000:.1f}s{'':<8} "
              f"{STATIC_RUNS['p99_ttft_ms'][i]/1000:.1f}s{'':<7} "
              f"{WVA_RUNS['error_count'][i]:<14,} "
              f"{STATIC_RUNS['error_count'][i]:<14,}")

    print(f"\n{'─' * 80}")
    print("ANALYSIS")
    print(f"{'─' * 80}")

    ttft_ratio = AVG["p99_ttft_ms"][1] / AVG["p99_ttft_ms"][0]
    print(f"""
1. LATENCY vs ERRORS TRADEOFF
   WVA P99 TTFT ({AVG['p99_ttft_ms'][0]/1000:.1f}s) is {ttft_ratio:.1f}x better than Static ({AVG['p99_ttft_ms'][1]/1000:.1f}s).
   But WVA achieves this by shedding {AVG['error_count'][0]:,.0f} requests as errors.
   Static has 0 errors — every request completes, just very slowly.
   → WVA's lower TTFT is real but comes at the cost of dropped requests.

2. UNDER-PROVISIONING
   At 20 RPS with single-replica capacity of ~2-3 RPS, this workload needs ~7-10 replicas.
   WVA runs at 1.73 avg replicas (max 3) — a gap of ~4-7 replicas.
   Queue depth averages 236 — requests wait behind hundreds of others.
   → Neither strategy has enough capacity. The workload overwhelms both.

3. POD STARTUP IMPACT
   WVA pod startup: {AVG['pod_startup_s'][0]:.0f}s. Static: {AVG['pod_startup_s'][1]:.0f}s.
   WVA pods take {AVG['pod_startup_s'][0] - AVG['pod_startup_s'][1]:.0f}s longer (model loading for new replicas).
   During those {AVG['pod_startup_s'][0]:.0f}s, ~{int(AVG['pod_startup_s'][0] * 20)} requests arrive.
   → Pod startup is 18% of the 600s benchmark — the first scale-up barely helps.

4. KV CACHE UTILIZATION
   WVA: {AVG['kv_cache_pct'][0]:.1f}% KV cache. Static: {AVG['kv_cache_pct'][1]:.1f}%.
   WVA's higher KV cache indicates its replicas are working harder (fewer replicas, more load each).
   Static's lower KV cache with 2 constant replicas suggests requests are mostly queuing, not computing.
   → KV cache alone doesn't tell the full story without queue depth context.

5. COST EFFICIENCY
   WVA: {AVG['avg_replicas'][0]:.2f} GPU-hr. Static: {AVG['avg_replicas'][1]:.2f} GPU-hr.
   WVA is {(1 - AVG['avg_replicas'][0]/AVG['avg_replicas'][1])*100:.0f}% cheaper but drops {AVG['error_count'][0]:,.0f} requests.
   Cost per successful request is actually worse for WVA when accounting for errors.
   → Pure cost comparison is misleading without error-adjusted metrics.
""")


def main():
    parser = argparse.ArgumentParser(
        description="Prefill Heavy analysis: WVA vs Static (32B, 600s)"
    )
    parser.add_argument(
        "--output", default="prefill_heavy_wva_vs_static.png",
        help="Output path for the chart image (default: prefill_heavy_wva_vs_static.png)"
    )
    parser.add_argument(
        "--no-plot", action="store_true",
        help="Skip generating the plot (text analysis only)"
    )
    args = parser.parse_args()

    print_analysis()

    if not args.no_plot:
        plot_analysis(args.output)


if __name__ == "__main__":
    main()
