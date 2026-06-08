#!/usr/bin/env python3
"""
Phase 1: Single-Replica Capacity Baseline — Load Sweep Charts

Generates two publication-ready charts from the 12-stage Qwen3-32B
load sweep (0.5 → 18 RPS, 300s per stage, 1 fixed replica).

Usage:
    python3 hack/benchmark/analysis/load_sweep_plots.py
    python3 hack/benchmark/analysis/load_sweep_plots.py --output-dir /path/to/dir
"""

import argparse
import os
import sys

try:
    import matplotlib.pyplot as plt
    import matplotlib.ticker as ticker
    import numpy as np
except ImportError:
    print("Missing dependencies. Install with: pip install matplotlib numpy")
    sys.exit(1)


# ── Load sweep data from inference-perf results ──
# 12 stages: 0.5 → 18 RPS, 300s each, Qwen3-32B, 1 replica

STAGES = [
    {"stage": 0,  "rps": 0.5,  "achieved_rps": 0.46, "successes": 150,  "failures": 0,    "p50_ttft": 85.5,    "p99_ttft": 124,      "p50_tpot": 27.6,  "p99_tpot": 28.3,  "output_tps": 466.5,  "total_tps": 920.2},
    {"stage": 1,  "rps": 1.0,  "achieved_rps": 0.96, "successes": 290,  "failures": 0,    "p50_ttft": 86.3,    "p99_ttft": 136,      "p50_tpot": 27.7,  "p99_tpot": 28.9,  "output_tps": 912.3,  "total_tps": 1807.7},
    {"stage": 2,  "rps": 2.0,  "achieved_rps": 1.84, "successes": 565,  "failures": 0,    "p50_ttft": 4685,    "p99_ttft": 18_800,   "p50_tpot": 33.3,  "p99_tpot": 45.2,  "output_tps": 1378.0, "total_tps": 2726.0},
    {"stage": 3,  "rps": 3.0,  "achieved_rps": 2.56, "successes": 834,  "failures": 0,    "p50_ttft": 25_400,  "p99_ttft": 66_900,   "p50_tpot": 39.4,  "p99_tpot": 53.5,  "output_tps": 1901.0, "total_tps": 3744.0},
    {"stage": 4,  "rps": 4.0,  "achieved_rps": 2.39, "successes": 1046, "failures": 66,   "p50_ttft": 55_200,  "p99_ttft": 132_000,  "p50_tpot": 40.3,  "p99_tpot": 56.2,  "output_tps": 1816.0, "total_tps": 3591.0},
    {"stage": 5,  "rps": 6.0,  "achieved_rps": 2.49, "successes": 1361, "failures": 459,  "p50_ttft": 59_100,  "p99_ttft": 149_000,  "p50_tpot": 40.3,  "p99_tpot": 54.3,  "output_tps": 1891.0, "total_tps": 3733.0},
    {"stage": 6,  "rps": 8.0,  "achieved_rps": 2.51, "successes": 1665, "failures": 750,  "p50_ttft": 66_100,  "p99_ttft": 156_000,  "p50_tpot": 39.9,  "p99_tpot": 55.3,  "output_tps": 1887.0, "total_tps": 3736.0},
    {"stage": 7,  "rps": 10.0, "achieved_rps": 2.53, "successes": 1974, "failures": 1035, "p50_ttft": 67_400,  "p99_ttft": 154_000,  "p50_tpot": 40.1,  "p99_tpot": 56.7,  "output_tps": 1895.0, "total_tps": 3740.0},
    {"stage": 8,  "rps": 12.0, "achieved_rps": 2.55, "successes": 2285, "failures": 1329, "p50_ttft": 66_300,  "p99_ttft": 153_000,  "p50_tpot": 39.8,  "p99_tpot": 55.5,  "output_tps": 1892.0, "total_tps": 3738.0},
    {"stage": 9,  "rps": 14.0, "achieved_rps": 2.52, "successes": 2588, "failures": 1618, "p50_ttft": 67_800,  "p99_ttft": 155_000,  "p50_tpot": 40.0,  "p99_tpot": 55.8,  "output_tps": 1883.0, "total_tps": 3721.0},
    {"stage": 10, "rps": 16.0, "achieved_rps": 2.54, "successes": 2895, "failures": 1916, "p50_ttft": 67_700,  "p99_ttft": 156_000,  "p50_tpot": 39.9,  "p99_tpot": 56.2,  "output_tps": 1888.0, "total_tps": 3733.0},
    {"stage": 11, "rps": 18.0, "achieved_rps": 2.51, "successes": 3191, "failures": 2218, "p50_ttft": 66_600,  "p99_ttft": 157_000,  "p50_tpot": 40.0,  "p99_tpot": 55.6,  "output_tps": 1878.0, "total_tps": 3712.0},
]

rps     = [s["rps"] for s in STAGES]
achieved = [s["achieved_rps"] for s in STAGES]
p50_ttft = [s["p50_ttft"] for s in STAGES]
p99_ttft = [s["p99_ttft"] for s in STAGES]
p50_tpot = [s["p50_tpot"] for s in STAGES]
p99_tpot = [s["p99_tpot"] for s in STAGES]
output_tps = [s["output_tps"] for s in STAGES]
successes = [s["successes"] for s in STAGES]
failures  = [s["failures"] for s in STAGES]
failure_pct = [f / (s + f) * 100 if (s + f) > 0 else 0 for s, f in zip(successes, failures)]

KNEE_RPS = 2.0
PEAK_RPS = 3.0
SAT_RPS  = 4.0

COLORS = {
    "primary": "#2563eb",
    "secondary": "#7c3aed",
    "success": "#16a34a",
    "danger": "#dc2626",
    "warning": "#ea580c",
    "muted": "#94a3b8",
    "knee": "#f59e0b",
    "peak": "#16a34a",
    "sat": "#dc2626",
}


def add_zone_shading(ax, ymax):
    ax.axvspan(0, KNEE_RPS, alpha=0.06, color=COLORS["success"], zorder=0)
    ax.axvspan(KNEE_RPS, SAT_RPS, alpha=0.06, color=COLORS["knee"], zorder=0)
    ax.axvspan(SAT_RPS, 19, alpha=0.06, color=COLORS["danger"], zorder=0)

    for xval, label, color in [
        (KNEE_RPS, "Knee", COLORS["knee"]),
        (PEAK_RPS, "Peak", COLORS["peak"]),
        (SAT_RPS,  "Saturation", COLORS["sat"]),
    ]:
        ax.axvline(x=xval, color=color, linestyle="--", linewidth=1.5, alpha=0.7)


def plot_throughput(output_path: str):
    """Chart 1: Throughput & failure rate vs. offered load."""
    fig, ax1 = plt.subplots(figsize=(10, 5.5))

    ax1.plot(rps, achieved, "o-", color=COLORS["primary"], linewidth=2.5,
             markersize=7, label="Successful Responses (RPS)", zorder=5)
    ax1.plot(rps, rps, "--", color=COLORS["muted"], linewidth=1, alpha=0.5,
             label="Ideal (1:1)", zorder=3)

    add_zone_shading(ax1, max(rps) * 1.1)

    ax1.set_xlabel("Requests Sent (RPS)", fontsize=12)
    ax1.set_ylabel("Successful Responses (RPS)", fontsize=12, color=COLORS["primary"])
    ax1.tick_params(axis="y", labelcolor=COLORS["primary"])
    ax1.set_xlim(0, 19)
    ax1.set_ylim(0, max(rps) * 1.05)

    ax2 = ax1.twinx()
    ax2.fill_between(rps, failure_pct, alpha=0.15, color=COLORS["danger"], zorder=2)
    ax2.plot(rps, failure_pct, "s-", color=COLORS["danger"], linewidth=1.5,
             markersize=5, alpha=0.8, label="Failure %", zorder=4)
    ax2.set_ylabel("Failure Rate (%)", fontsize=12, color=COLORS["danger"])
    ax2.tick_params(axis="y", labelcolor=COLORS["danger"])
    ax2.set_ylim(0, max(failure_pct) * 1.3 if max(failure_pct) > 0 else 10)

    ax3 = ax1.twinx()
    ax3.spines["right"].set_position(("outward", 60))
    ax3.plot(rps, output_tps, "^-", color=COLORS["secondary"], linewidth=1.5,
             markersize=5, alpha=0.8, label="Output tok/s", zorder=4)
    ax3.set_ylabel("Output Tokens/s", fontsize=12, color=COLORS["secondary"])
    ax3.tick_params(axis="y", labelcolor=COLORS["secondary"])
    ax3.set_ylim(0, max(output_tps) * 1.15)

    y_top = max(rps) * 0.95
    ax1.annotate("Knee\n2.0 RPS", xy=(KNEE_RPS, 1.84), fontsize=9,
                 fontweight="bold", color=COLORS["knee"], ha="center",
                 xytext=(KNEE_RPS, y_top * 0.65),
                 arrowprops=dict(arrowstyle="->", color=COLORS["knee"], lw=1.2))
    ax1.annotate("Peak\n3.0 RPS", xy=(PEAK_RPS, 2.56), fontsize=9,
                 fontweight="bold", color=COLORS["peak"], ha="center",
                 xytext=(PEAK_RPS + 0.8, y_top * 0.85),
                 arrowprops=dict(arrowstyle="->", color=COLORS["peak"], lw=1.2))
    ax1.annotate("Saturation\n4.0 RPS", xy=(SAT_RPS, 2.39), fontsize=9,
                 fontweight="bold", color=COLORS["sat"], ha="center",
                 xytext=(SAT_RPS + 1.2, y_top * 0.55),
                 arrowprops=dict(arrowstyle="->", color=COLORS["sat"], lw=1.2))

    lines1, labels1 = ax1.get_legend_handles_labels()
    lines2, labels2 = ax2.get_legend_handles_labels()
    lines3, labels3 = ax3.get_legend_handles_labels()
    ax1.legend(lines1 + lines2 + lines3, labels1 + labels2 + labels3,
               loc="upper left", fontsize=9, framealpha=0.9)

    ax1.set_title(
        "Qwen3-32B Single-Replica Capacity — Throughput & Failure Rate vs. Requests Sent",
        fontsize=13, fontweight="bold", pad=12,
    )
    ax1.grid(axis="both", alpha=0.2)

    plt.tight_layout()
    plt.savefig(output_path, dpi=150, bbox_inches="tight")
    print(f"Saved: {output_path}")
    plt.close()


def plot_latency(output_path: str):
    """Chart 2: P99 TTFT & P99 TPOT vs. offered load."""
    fig, ax1 = plt.subplots(figsize=(10, 5.5))

    ax1.semilogy(rps, [t / 1000 for t in p99_ttft], "o-", color=COLORS["primary"],
                 linewidth=2.5, markersize=7, label="P99 TTFT", zorder=5)
    ax1.semilogy(rps, [t / 1000 for t in p50_ttft], "o--", color=COLORS["primary"],
                 linewidth=1.5, markersize=5, alpha=0.5, label="P50 TTFT", zorder=4)

    add_zone_shading(ax1, 200)

    ax1.set_xlabel("Requests Sent (RPS)", fontsize=12)
    ax1.set_ylabel("TTFT (seconds, log scale)", fontsize=12, color=COLORS["primary"])
    ax1.tick_params(axis="y", labelcolor=COLORS["primary"])
    ax1.set_xlim(0, 19)

    ax1.axhline(y=1.0, color=COLORS["muted"], linestyle=":", linewidth=1, alpha=0.5)
    ax1.text(18.5, 1.1, "1s SLO reference", fontsize=8, color=COLORS["muted"],
             ha="right", va="bottom")

    ax2 = ax1.twinx()
    ax2.plot(rps, p99_tpot, "s-", color=COLORS["warning"], linewidth=2,
             markersize=6, label="P99 TPOT (ms/tok)", zorder=5)
    ax2.plot(rps, p50_tpot, "s--", color=COLORS["warning"], linewidth=1.5,
             markersize=4, alpha=0.5, label="P50 TPOT (ms/tok)", zorder=4)
    ax2.set_ylabel("TPOT (ms/token)", fontsize=12, color=COLORS["warning"])
    ax2.tick_params(axis="y", labelcolor=COLORS["warning"])
    ax2.set_ylim(20, max(p99_tpot) * 1.3)

    ax1.annotate(
        "138× jump\n136ms → 18.8s",
        xy=(2.0, 18.8), fontsize=10, fontweight="bold", color=COLORS["danger"],
        ha="center",
        xytext=(5.5, 1.5),
        arrowprops=dict(arrowstyle="->", color=COLORS["danger"], lw=1.5),
        bbox=dict(boxstyle="round,pad=0.3", facecolor="white", edgecolor=COLORS["danger"], alpha=0.9),
    )

    ax1.annotate(
        "TPOT only\n2× increase\n(28→56 ms)",
        xy=(18, 55.6), fontsize=9, color=COLORS["warning"],
        ha="right", va="bottom",
        xytext=(16, 0.15),
        arrowprops=dict(arrowstyle="->", color=COLORS["warning"], lw=1.2),
        bbox=dict(boxstyle="round,pad=0.3", facecolor="white", edgecolor=COLORS["warning"], alpha=0.9),
    )

    lines1, labels1 = ax1.get_legend_handles_labels()
    lines2, labels2 = ax2.get_legend_handles_labels()
    ax1.legend(lines1 + lines2, labels1 + labels2,
               loc="center left", fontsize=9, framealpha=0.9)

    ax1.set_title(
        "Qwen3-32B Single-Replica — Latency (TTFT & TPOT) vs. Requests Sent",
        fontsize=13, fontweight="bold", pad=12,
    )
    ax1.grid(axis="both", alpha=0.2)

    plt.tight_layout()
    plt.savefig(output_path, dpi=150, bbox_inches="tight")
    print(f"Saved: {output_path}")
    plt.close()


def main():
    parser = argparse.ArgumentParser(
        description="Generate load sweep charts for Qwen3-32B single-replica capacity"
    )
    parser.add_argument(
        "--output-dir", default=".",
        help="Directory for output PNGs (default: current dir)"
    )
    args = parser.parse_args()

    os.makedirs(args.output_dir, exist_ok=True)
    plot_throughput(os.path.join(args.output_dir, "load_sweep_throughput.png"))
    plot_latency(os.path.join(args.output_dir, "load_sweep_latency.png"))


if __name__ == "__main__":
    main()
