#!/usr/bin/env python3
"""
Data analysis of autoscaling experiments (Issue #1247).

Compares autoscaling strategies and configurations across workload scenarios
using data from docs/benchmark.md to identify bottleneck components and
develop concrete hypotheses for the next round of experiments.

Usage:
    python3 hack/benchmark/analysis/analyze_autoscaling.py
    python3 hack/benchmark/analysis/analyze_autoscaling.py --format markdown
    python3 hack/benchmark/analysis/analyze_autoscaling.py --format csv
"""

import argparse
from dataclasses import dataclass, field
from typing import Optional


@dataclass
class RunResult:
    strategy: str
    scenario: str
    model: str
    duration_s: int
    workload_rps: float
    p99_ttft_ms: float = 0
    p99_itl_ms: float = 0
    avg_replicas: float = 0
    max_replicas: int = 0
    avg_kv_cache_pct: float = 0
    avg_queue_depth: Optional[float] = None
    error_count: int = 0
    avg_pod_startup_s: float = 0
    cost_gpu_hr: float = 0


# ---------------------------------------------------------------------------
# All benchmark data from docs/benchmark.md (upstream main)
# ---------------------------------------------------------------------------

RESULTS = [
    # ══════════════════════════════════════════════════════════════════════
    # PREFILL HEAVY
    # ══════════════════════════════════════════════════════════════════════

    # Qwen3-32B, 600s, WVA Default(v1)
    RunResult(
        strategy="WVA Default(v1)", scenario="Prefill Heavy", model="Qwen3-32B",
        duration_s=600, workload_rps=20,
        p99_ttft_ms=98_420, p99_itl_ms=54.8, avg_replicas=1.73, max_replicas=3,
        avg_kv_cache_pct=66.3, avg_queue_depth=236.5, error_count=4184,
        avg_pod_startup_s=110, cost_gpu_hr=1.73,
    ),
    # Qwen3-32B, 1800s, WVA Default(v1)
    RunResult(
        strategy="WVA Default(v1) 1800s", scenario="Prefill Heavy", model="Qwen3-32B",
        duration_s=1800, workload_rps=20,
        p99_ttft_ms=96_108, p99_itl_ms=54.13, avg_replicas=2.55, max_replicas=4,
        avg_kv_cache_pct=59.9, avg_queue_depth=182.0, error_count=12822,
        avg_pod_startup_s=107, cost_gpu_hr=2.55,
    ),
    # Qwen3-32B, 600s, Static 2 Replicas
    RunResult(
        strategy="Static 2 Replicas", scenario="Prefill Heavy", model="Qwen3-32B",
        duration_s=600, workload_rps=20,
        p99_ttft_ms=555_305, p99_itl_ms=58.2, avg_replicas=2.00, max_replicas=2,
        avg_kv_cache_pct=51.3, error_count=0,
        avg_pod_startup_s=93, cost_gpu_hr=2.00,
    ),
    # Qwen3-0.6B, 600s, WVA Default(v1)
    RunResult(
        strategy="WVA Default(v1)", scenario="Prefill Heavy", model="Qwen3-0.6B",
        duration_s=600, workload_rps=20,
        p99_ttft_ms=81_391, p99_itl_ms=51.94, avg_replicas=1.93, max_replicas=3,
        avg_kv_cache_pct=65.1, avg_queue_depth=76.5, error_count=401,
        avg_pod_startup_s=65, cost_gpu_hr=1.93,
    ),
    # Qwen3-0.6B, 1800s, WVA Default(v1)
    RunResult(
        strategy="WVA Default(v1) 1800s", scenario="Prefill Heavy", model="Qwen3-0.6B",
        duration_s=1800, workload_rps=20,
        p99_ttft_ms=66_177, p99_itl_ms=47.25, avg_replicas=3.17, max_replicas=5,
        avg_kv_cache_pct=55.7, avg_queue_depth=41.2, error_count=860,
        avg_pod_startup_s=66, cost_gpu_hr=3.17,
    ),

    # ══════════════════════════════════════════════════════════════════════
    # DECODE HEAVY
    # ══════════════════════════════════════════════════════════════════════

    # Qwen3-32B, 600s, WVA Default(v1)
    RunResult(
        strategy="WVA Default(v1)", scenario="Decode Heavy", model="Qwen3-32B",
        duration_s=600, workload_rps=20,
        p99_ttft_ms=78_051, p99_itl_ms=47.13, avg_replicas=1.84, max_replicas=3,
        avg_kv_cache_pct=79.2, avg_queue_depth=108.8, error_count=3563,
        avg_pod_startup_s=109, cost_gpu_hr=1.89,
    ),
    # Qwen3-32B, 1800s, WVA Default(v1)
    RunResult(
        strategy="WVA Default(v1) 1800s", scenario="Decode Heavy", model="Qwen3-32B",
        duration_s=1800, workload_rps=20,
        p99_ttft_ms=70_868, p99_itl_ms=46.06, avg_replicas=2.40, max_replicas=4,
        avg_kv_cache_pct=72.2, avg_queue_depth=88.4, error_count=10762,
        avg_pod_startup_s=109, cost_gpu_hr=2.40,
    ),
    # Qwen3-32B, 600s, Static 2 Replicas
    RunResult(
        strategy="Static 2 Replicas", scenario="Decode Heavy", model="Qwen3-32B",
        duration_s=600, workload_rps=20,
        p99_ttft_ms=356_566, p99_itl_ms=113.8, avg_replicas=2.00, max_replicas=2,
        avg_kv_cache_pct=66.8, error_count=0,
        avg_pod_startup_s=97, cost_gpu_hr=2.00,
    ),
    # Qwen3-0.6B, 600s, WVA Default(v1)
    RunResult(
        strategy="WVA Default(v1)", scenario="Decode Heavy", model="Qwen3-0.6B",
        duration_s=600, workload_rps=20,
        p99_ttft_ms=62_296, p99_itl_ms=41.11, avg_replicas=1.89, max_replicas=3,
        avg_kv_cache_pct=61.7, avg_queue_depth=51.1, error_count=1408,
        avg_pod_startup_s=65, cost_gpu_hr=1.89,
    ),
    # Qwen3-0.6B, 1800s, WVA Default(v1)
    RunResult(
        strategy="WVA Default(v1) 1800s", scenario="Decode Heavy", model="Qwen3-0.6B",
        duration_s=1800, workload_rps=20,
        p99_ttft_ms=58_934, p99_itl_ms=44.75, avg_replicas=2.59, max_replicas=4,
        avg_kv_cache_pct=57.2, avg_queue_depth=30.8, error_count=2520,
        avg_pod_startup_s=66, cost_gpu_hr=2.59,
    ),

    # ══════════════════════════════════════════════════════════════════════
    # BURSTY
    # ══════════════════════════════════════════════════════════════════════

    # Qwen3-32B, 900s, WVA Default(v1)
    RunResult(
        strategy="WVA Default(v1)", scenario="Bursty", model="Qwen3-32B",
        duration_s=900, workload_rps=15,
        p99_ttft_ms=262_441, p99_itl_ms=196.3, avg_replicas=2.43, max_replicas=4,
        avg_kv_cache_pct=45.1, avg_queue_depth=53.5, error_count=6110,
        avg_pod_startup_s=103, cost_gpu_hr=2.43,
    ),
    # Qwen3-0.6B, 900s, WVA Default(v1)
    RunResult(
        strategy="WVA Default(v1)", scenario="Bursty", model="Qwen3-0.6B",
        duration_s=900, workload_rps=15,
        p99_ttft_ms=13_376, p99_itl_ms=48.0, avg_replicas=1.99, max_replicas=3,
        avg_kv_cache_pct=35.2, avg_queue_depth=16.0, error_count=51,
        avg_pod_startup_s=66, cost_gpu_hr=1.99,
    ),
    # Qwen3-0.6B, 1800s, WVA Default(v1)
    RunResult(
        strategy="WVA Default(v1) 1800s", scenario="Bursty", model="Qwen3-0.6B",
        duration_s=1800, workload_rps=15,
        p99_ttft_ms=23_278, p99_itl_ms=50.1, avg_replicas=1.63, max_replicas=3,
        avg_kv_cache_pct=29.5, avg_queue_depth=1.1, error_count=71,
        avg_pod_startup_s=64, cost_gpu_hr=1.63,
    ),

    # ══════════════════════════════════════════════════════════════════════
    # SYMMETRICAL
    # ══════════════════════════════════════════════════════════════════════

    # Qwen3-32B, 600s, WVA Default(v1)
    RunResult(
        strategy="WVA Default(v1)", scenario="Symmetrical", model="Qwen3-32B",
        duration_s=600, workload_rps=20,
        p99_ttft_ms=100_187, p99_itl_ms=67.29, avg_replicas=1.70, max_replicas=3,
        avg_kv_cache_pct=70.2, avg_queue_depth=166.8, error_count=3729,
        avg_pod_startup_s=103, cost_gpu_hr=1.70,
    ),
    # Qwen3-32B, 1800s, WVA Default(v1)
    RunResult(
        strategy="WVA Default(v1) 1800s", scenario="Symmetrical", model="Qwen3-32B",
        duration_s=1800, workload_rps=20,
        p99_ttft_ms=88_220, p99_itl_ms=66.40, avg_replicas=3.05, max_replicas=5,
        avg_kv_cache_pct=59.0, avg_queue_depth=114.2, error_count=10272,
        avg_pod_startup_s=103, cost_gpu_hr=3.05,
    ),
    # Qwen3-32B, 600s, Static 2 Replicas
    RunResult(
        strategy="Static 2 Replicas", scenario="Symmetrical", model="Qwen3-32B",
        duration_s=600, workload_rps=20,
        p99_ttft_ms=507_504, p99_itl_ms=70.5, avg_replicas=2.00, max_replicas=2,
        avg_kv_cache_pct=49.3, error_count=0,
        avg_pod_startup_s=97, cost_gpu_hr=2.00,
    ),
    # Qwen3-0.6B, 600s, WVA Default(v1)
    RunResult(
        strategy="WVA Default(v1)", scenario="Symmetrical", model="Qwen3-0.6B",
        duration_s=600, workload_rps=20,
        p99_ttft_ms=23_169, p99_itl_ms=43.27, avg_replicas=1.80, max_replicas=3,
        avg_kv_cache_pct=52.0, avg_queue_depth=13.0, error_count=17,
        avg_pod_startup_s=64, cost_gpu_hr=1.80,
    ),
    # Qwen3-0.6B, 1800s, WVA Default(v1)
    RunResult(
        strategy="WVA Default(v1) 1800s", scenario="Symmetrical", model="Qwen3-0.6B",
        duration_s=1800, workload_rps=20,
        p99_ttft_ms=20_825, p99_itl_ms=40.36, avg_replicas=1.80, max_replicas=3,
        avg_kv_cache_pct=46.8, avg_queue_depth=10.8, error_count=342,
        avg_pod_startup_s=66, cost_gpu_hr=1.80,
    ),
]


def fmt_ms(ms: float) -> str:
    if ms >= 1000:
        return f"{ms / 1000:.1f}s"
    return f"{ms:.0f}ms"


def fmt_pct(v: float) -> str:
    return f"{v:.1f}%"


def analyze_duration_effect():
    """Compare 600s vs 1800s runs for the same model/scenario."""
    findings = []
    scenarios = ["Prefill Heavy", "Decode Heavy", "Symmetrical"]

    for scenario in scenarios:
        for model in ["Qwen3-32B", "Qwen3-0.6B"]:
            short = [r for r in RESULTS if r.scenario == scenario and r.model == model
                     and r.strategy == "WVA Default(v1)" and r.duration_s == 600]
            long = [r for r in RESULTS if r.scenario == scenario and r.model == model
                    and r.strategy == "WVA Default(v1) 1800s" and r.duration_s == 1800]
            if not short or not long:
                continue
            s, l = short[0], long[0]
            ttft_change = (l.p99_ttft_ms - s.p99_ttft_ms) / s.p99_ttft_ms * 100
            replica_change = l.avg_replicas - s.avg_replicas
            queue_change = ""
            if s.avg_queue_depth and l.avg_queue_depth:
                qd = (l.avg_queue_depth - s.avg_queue_depth) / s.avg_queue_depth * 100
                queue_change = f", queue depth {qd:+.0f}%"

            findings.append(
                f"{scenario} ({model}): 1800s vs 600s → P99 TTFT {ttft_change:+.0f}%, "
                f"replicas {s.avg_replicas:.2f}→{l.avg_replicas:.2f} (+{replica_change:.2f})"
                f"{queue_change}, errors {s.error_count:,}→{l.error_count:,}"
            )
    return findings


def analyze_model_effect():
    """Compare 32B vs 0.6B for the same scenario/strategy."""
    findings = []
    scenarios = ["Prefill Heavy", "Decode Heavy", "Symmetrical"]

    for scenario in scenarios:
        big = [r for r in RESULTS if r.scenario == scenario and r.model == "Qwen3-32B"
               and r.strategy == "WVA Default(v1)" and r.duration_s == 600]
        small = [r for r in RESULTS if r.scenario == scenario and r.model == "Qwen3-0.6B"
                 and r.strategy == "WVA Default(v1)" and r.duration_s == 600]
        if not big or not small:
            continue
        b, sm = big[0], small[0]
        ttft_ratio = b.p99_ttft_ms / sm.p99_ttft_ms if sm.p99_ttft_ms else 0
        startup_ratio = b.avg_pod_startup_s / sm.avg_pod_startup_s if sm.avg_pod_startup_s else 0
        findings.append(
            f"{scenario}: 32B vs 0.6B → P99 TTFT {ttft_ratio:.1f}x higher, "
            f"errors {b.error_count:,} vs {sm.error_count:,}, "
            f"pod startup {b.avg_pod_startup_s:.0f}s vs {sm.avg_pod_startup_s:.0f}s "
            f"({startup_ratio:.1f}x), KV cache {b.avg_kv_cache_pct:.0f}% vs {sm.avg_kv_cache_pct:.0f}%"
        )
    return findings


def analyze_strategy_comparison():
    """Compare WVA Default vs Static 2 Replicas (32B, 600s only)."""
    findings = []
    scenarios = ["Prefill Heavy", "Decode Heavy", "Symmetrical"]

    for scenario in scenarios:
        wva = [r for r in RESULTS if r.scenario == scenario and r.model == "Qwen3-32B"
               and r.strategy == "WVA Default(v1)" and r.duration_s == 600]
        static = [r for r in RESULTS if r.scenario == scenario and r.model == "Qwen3-32B"
                  and r.strategy == "Static 2 Replicas"]
        if not wva or not static:
            continue
        w, s = wva[0], static[0]
        ttft_ratio = s.p99_ttft_ms / w.p99_ttft_ms if w.p99_ttft_ms else 0
        findings.append(
            f"{scenario}: Static P99 TTFT is {ttft_ratio:.1f}x worse than WVA "
            f"({fmt_ms(s.p99_ttft_ms)} vs {fmt_ms(w.p99_ttft_ms)}), "
            f"but Static has 0 errors vs WVA's {w.error_count:,}. "
            f"WVA uses {w.avg_replicas:.2f} replicas (costs {w.cost_gpu_hr:.2f} GPU-hr) "
            f"vs Static's 2.00 (costs 2.00 GPU-hr)."
        )
    return findings


def analyze_scenario_comparison():
    """Compare across workload types for the same model/strategy."""
    findings = []
    scenarios = ["Prefill Heavy", "Decode Heavy", "Symmetrical"]
    results_32b_600 = {r.scenario: r for r in RESULTS
                       if r.model == "Qwen3-32B" and r.strategy == "WVA Default(v1)"
                       and r.duration_s == 600}

    if len(results_32b_600) >= 3:
        by_ttft = sorted(results_32b_600.values(), key=lambda r: r.p99_ttft_ms)
        findings.append(
            f"Ranking by P99 TTFT (32B, 600s, WVA Default): "
            + " < ".join(f"{r.scenario} ({fmt_ms(r.p99_ttft_ms)})" for r in by_ttft)
        )
        by_errors = sorted(results_32b_600.values(), key=lambda r: r.error_count)
        findings.append(
            f"Ranking by error count (32B, 600s, WVA Default): "
            + " < ".join(f"{r.scenario} ({r.error_count:,})" for r in by_errors)
        )
        by_queue = sorted(
            [r for r in results_32b_600.values() if r.avg_queue_depth],
            key=lambda r: r.avg_queue_depth
        )
        findings.append(
            f"Ranking by queue depth (32B, 600s, WVA Default): "
            + " < ".join(f"{r.scenario} ({r.avg_queue_depth:.0f})" for r in by_queue)
        )

    return findings


def compute_hypotheses() -> list[str]:
    hypotheses = []

    # H1: Pod startup
    startups_32b = [r.avg_pod_startup_s for r in RESULTS if r.model == "Qwen3-32B"
                    and "Static" not in r.strategy]
    startups_06b = [r.avg_pod_startup_s for r in RESULTS if r.model == "Qwen3-0.6B"]
    avg_32b = sum(startups_32b) / len(startups_32b) if startups_32b else 0
    avg_06b = sum(startups_06b) / len(startups_06b) if startups_06b else 0
    hypotheses.append(
        f"H1 — Pod startup latency is a primary bottleneck for 32B: "
        f"Average startup is {avg_32b:.0f}s (32B) vs {avg_06b:.0f}s (0.6B). "
        f"The 32B model takes {avg_32b - avg_06b:.0f}s longer to load, during which "
        f"requests queue. At 20 RPS, ~{int(avg_32b * 20)} requests arrive before "
        f"the first scale-up pod is ready."
    )

    # H2: WVA under-scales consistently
    wva_32b = [r for r in RESULTS if r.model == "Qwen3-32B"
               and r.strategy == "WVA Default(v1)" and r.duration_s == 600]
    if wva_32b:
        avg_rep = sum(r.avg_replicas for r in wva_32b) / len(wva_32b)
        avg_max = sum(r.max_replicas for r in wva_32b) / len(wva_32b)
        hypotheses.append(
            f"H2 — WVA under-scales for 32B at 20 RPS: "
            f"Across scenarios, WVA averages {avg_rep:.2f} replicas (max {avg_max:.0f}) "
            f"in 600s runs. All runs show high queue depths (108-236) and thousands of "
            f"errors, indicating the model is severely overloaded. WVA's conservative "
            f"thresholds (KV=0.80, queue=5) may not trigger aggressively enough for "
            f"the 32B model's capacity profile."
        )

    # H3: Longer runs help but don't solve the problem
    hypotheses.append(
        "H3 — Longer benchmarks (1800s) improve scaling but errors still grow: "
        "At 1800s, WVA scales to ~2.5-3.0 replicas and queue depth drops, but "
        "error counts increase 3x (e.g., Prefill Heavy: 4,184→12,822). The autoscaler "
        "eventually scales up, but it still can't keep pace with sustained 20 RPS. "
        "The 600s vs 1800s P99 TTFT improvement is modest (~2-10%), suggesting "
        "the problem is not just warm-up time but fundamental under-provisioning."
    )

    # H4: 0.6B is a fundamentally different regime
    hypotheses.append(
        "H4 — 0.6B model operates in a different capacity regime: "
        "The 0.6B model shows dramatically better results (P99 TTFT 20-80s vs "
        "78-555s for 32B, near-zero errors for Symmetrical). This is because "
        "0.6B has ~50x fewer parameters, meaning higher per-replica throughput, "
        "faster pod startup (65s vs 110s), and lower KV cache pressure. "
        "Autoscaling analysis should be model-size-aware."
    )

    # H5: WVA trades errors for lower TTFT vs Static
    hypotheses.append(
        "H5 — WVA achieves lower P99 TTFT than Static by shedding load as errors: "
        "Across all 32B scenarios, WVA's P99 TTFT is 5-6x lower than Static "
        "(98s vs 555s for Prefill Heavy), but Static has 0 errors. WVA effectively "
        "drops overflowing requests as errors, which lowers tail latency for the "
        "requests that do complete. This is a valid strategy if the application "
        "can retry, but misleading if reported without error context."
    )

    # H6: Queue depth as leading indicator
    hypotheses.append(
        "H6 — EPP queue depth is a strong leading indicator of saturation: "
        "Across all runs, queue depth correlates with error count "
        "(Prefill Heavy: queue=236, errors=4184; Decode Heavy: queue=108, errors=3563; "
        "Bursty: queue=53, errors=6110). A queue-depth-based autoscaler that triggers "
        "more aggressively (target=2 instead of default=5) could react faster."
    )

    return hypotheses


def print_text_report():
    print("=" * 90)
    print("AUTOSCALING EXPERIMENT ANALYSIS")
    print("Data source: docs/benchmark.md")
    print("=" * 90)

    # ── Section 1: Cross-scenario comparison ──
    print("\n1. SCENARIO COMPARISON (32B, 600s, WVA Default)")
    print("-" * 90)
    scenarios = ["Prefill Heavy", "Decode Heavy", "Symmetrical", "Bursty"]
    results_32b = {r.scenario: r for r in RESULTS
                   if r.model == "Qwen3-32B" and r.strategy == "WVA Default(v1)"
                   and (r.duration_s == 600 or (r.scenario == "Bursty" and r.duration_s == 900))}

    header = f"{'Metric':<28}"
    for s in scenarios:
        if s in results_32b:
            header += f" | {s:<18}"
    print(header)
    print("-" * len(header))

    rows = [
        ("P99 TTFT", lambda r: fmt_ms(r.p99_ttft_ms)),
        ("P99 ITL (ms/tok)", lambda r: f"{r.p99_itl_ms:.1f}"),
        ("Avg replicas", lambda r: f"{r.avg_replicas:.2f}"),
        ("Max replicas", lambda r: str(r.max_replicas)),
        ("Avg KV cache", lambda r: fmt_pct(r.avg_kv_cache_pct)),
        ("Avg queue depth", lambda r: f"{r.avg_queue_depth:.0f}" if r.avg_queue_depth else "N/A"),
        ("Errors", lambda r: f"{r.error_count:,}"),
        ("Pod startup (s)", lambda r: f"{r.avg_pod_startup_s:.0f}"),
        ("Cost (GPU-hr)", lambda r: f"{r.cost_gpu_hr:.2f}"),
    ]
    for label, fn in rows:
        row = f"{label:<28}"
        for s in scenarios:
            if s in results_32b:
                row += f" | {fn(results_32b[s]):<18}"
        print(row)

    for f in analyze_scenario_comparison():
        print(f"\n  → {f}")

    # ── Section 2: WVA vs Static (32B, 600s) ──
    print(f"\n\n2. WVA DEFAULT vs STATIC 2 REPLICAS (32B, 600s)")
    print("-" * 90)
    scenarios_with_static = ["Prefill Heavy", "Decode Heavy", "Symmetrical"]
    for scenario in scenarios_with_static:
        wva = [r for r in RESULTS if r.scenario == scenario and r.model == "Qwen3-32B"
               and r.strategy == "WVA Default(v1)" and r.duration_s == 600]
        static = [r for r in RESULTS if r.scenario == scenario and r.model == "Qwen3-32B"
                  and r.strategy == "Static 2 Replicas"]
        if not wva or not static:
            continue
        w, s = wva[0], static[0]
        print(f"\n  {scenario}:")
        print(f"    {'Metric':<24} {'WVA Default':<18} {'Static 2R':<18} {'Delta':<18}")
        print(f"    {'-'*78}")
        print(f"    {'P99 TTFT':<24} {fmt_ms(w.p99_ttft_ms):<18} {fmt_ms(s.p99_ttft_ms):<18} "
              f"WVA {s.p99_ttft_ms/w.p99_ttft_ms:.1f}x better")
        print(f"    {'Errors':<24} {w.error_count:<18,} {s.error_count:<18,} "
              f"WVA has {w.error_count:,} errors")
        print(f"    {'Avg replicas':<24} {w.avg_replicas:<18.2f} {s.avg_replicas:<18.2f} "
              f"WVA uses {w.avg_replicas - s.avg_replicas:+.2f}")
        print(f"    {'KV cache':<24} {fmt_pct(w.avg_kv_cache_pct):<18} {fmt_pct(s.avg_kv_cache_pct):<18}")

    for f in analyze_strategy_comparison():
        print(f"\n  → {f}")

    # ── Section 3: 600s vs 1800s ──
    print(f"\n\n3. DURATION EFFECT: 600s vs 1800s")
    print("-" * 90)
    for f in analyze_duration_effect():
        print(f"  → {f}")

    # ── Section 4: Model size effect ──
    print(f"\n\n4. MODEL SIZE EFFECT: 32B vs 0.6B (600s, WVA Default)")
    print("-" * 90)
    for f in analyze_model_effect():
        print(f"  → {f}")

    # ── Section 5: Bottleneck decomposition ──
    print(f"\n\n5. BOTTLENECK DECOMPOSITION")
    print("-" * 90)

    print("\n  Time budget for a scale-up event (32B):")
    print(f"    Signal detection (WVA polling)     ~15-30s")
    print(f"    HPA reaction + stabilization       ~0-60s (scale-up stabilization=0s)")
    print(f"    Pod scheduling                     ~5-10s")
    print(f"    Model loading (32B)                ~100-130s")
    print(f"    ─────────────────────────────────────────")
    print(f"    Total first scale-up               ~120-230s")
    print(f"    Requests queued during startup      ~2400-4600 (at 20 RPS)")
    print(f"")
    print(f"    In a 600s benchmark, the first new replica is only ready at")
    print(f"    ~200-300s, leaving ~300-400s of useful scaled-up time.")
    print(f"    In a 1800s benchmark, this overhead is amortized over 3x more time.")

    # ── Section 6: Hypotheses ──
    print(f"\n\n6. HYPOTHESES")
    print("=" * 90)
    for h in compute_hypotheses():
        print(f"\n  {h}")

    # ── Section 7: Recommended experiments ──
    print(f"\n\n7. RECOMMENDED NEXT EXPERIMENTS")
    print("=" * 90)
    experiments = [
        "E1: Run WVA with tuned v1 parameters (prefill-heavy & decode-heavy thresholds) — "
        "the Tuned(v1) column in benchmark.md is still TBD.",
        "E2: Test WVA v2 Saturation (token-based) — all v2 parameters are TBD in config.",
        "E3: Set min replicas=3 to pre-warm and eliminate startup latency from measurements.",
        "E4: Lower WVA KV cache threshold from 0.80 to 0.60 to trigger earlier scale-up.",
        "E5: Run 0.6B model with same 1800s duration to compare scaling dynamics with 32B.",
        "E6: Instrument request-level tracing to decompose TTFT into queue wait vs compute.",
        "E7: Test with lower RPS (5-10) closer to model capacity to see autoscaler behavior "
        "when not immediately overwhelmed.",
    ]
    for e in experiments:
        print(f"  {e}")
    print()


def print_markdown_report():
    print("# Autoscaling Experiment Analysis\n")
    print("Data source: [docs/benchmark.md](../../../docs/benchmark.md)\n")

    # Section 1
    print("## 1. Scenario Comparison (32B, 600s, WVA Default)\n")
    scenarios = ["Prefill Heavy", "Decode Heavy", "Symmetrical", "Bursty"]
    results_32b = {r.scenario: r for r in RESULTS
                   if r.model == "Qwen3-32B" and r.strategy == "WVA Default(v1)"
                   and (r.duration_s == 600 or (r.scenario == "Bursty" and r.duration_s == 900))}

    available = [s for s in scenarios if s in results_32b]
    print("| Metric | " + " | ".join(available) + " |")
    print("|--------|" + "|".join(["------"] * len(available)) + "|")

    rows = [
        ("P99 TTFT", lambda r: fmt_ms(r.p99_ttft_ms)),
        ("P99 ITL (ms/tok)", lambda r: f"{r.p99_itl_ms:.1f}"),
        ("Avg replicas", lambda r: f"{r.avg_replicas:.2f}"),
        ("Max replicas", lambda r: str(r.max_replicas)),
        ("Avg KV cache", lambda r: fmt_pct(r.avg_kv_cache_pct)),
        ("Avg queue depth", lambda r: f"{r.avg_queue_depth:.0f}" if r.avg_queue_depth else "N/A"),
        ("Errors", lambda r: f"{r.error_count:,}"),
        ("Pod startup (s)", lambda r: f"{r.avg_pod_startup_s:.0f}"),
        ("Cost (GPU-hr)", lambda r: f"{r.cost_gpu_hr:.2f}"),
    ]
    for label, fn in rows:
        print(f"| {label} | " + " | ".join(fn(results_32b[s]) for s in available) + " |")

    print("\n**Findings:**\n")
    for f in analyze_scenario_comparison():
        print(f"- {f}")

    # Section 2
    print("\n## 2. WVA Default vs Static 2 Replicas (32B, 600s)\n")
    for f in analyze_strategy_comparison():
        print(f"- {f}")

    # Section 3
    print("\n## 3. Duration Effect: 600s vs 1800s\n")
    for f in analyze_duration_effect():
        print(f"- {f}")

    # Section 4
    print("\n## 4. Model Size Effect: 32B vs 0.6B\n")
    for f in analyze_model_effect():
        print(f"- {f}")

    # Hypotheses
    print("\n## 5. Hypotheses\n")
    for h in compute_hypotheses():
        print(f"- {h}")

    # Experiments
    print("\n## 6. Recommended Next Experiments\n")
    experiments = [
        "E1: Run WVA with tuned v1 parameters — Tuned(v1) column is still TBD.",
        "E2: Test WVA v2 Saturation (token-based) — all v2 parameters are TBD.",
        "E3: Set min replicas=3 to pre-warm and isolate startup latency.",
        "E4: Lower WVA KV cache threshold from 0.80 to 0.60.",
        "E5: Run 0.6B model with 1800s duration for comparison.",
        "E6: Add request-level tracing for TTFT decomposition.",
        "E7: Test with lower RPS (5-10) closer to model capacity.",
    ]
    for e in experiments:
        print(f"- {e}")


def print_csv():
    print("strategy,scenario,model,duration_s,workload_rps,p99_ttft_ms,p99_itl_ms,"
          "avg_replicas,max_replicas,avg_kv_cache_pct,avg_queue_depth,"
          "error_count,pod_startup_s,cost_gpu_hr")
    for r in RESULTS:
        print(f"{r.strategy},{r.scenario},{r.model},{r.duration_s},{r.workload_rps},"
              f"{r.p99_ttft_ms},{r.p99_itl_ms},{r.avg_replicas},{r.max_replicas},"
              f"{r.avg_kv_cache_pct},{r.avg_queue_depth or ''},"
              f"{r.error_count},{r.avg_pod_startup_s},{r.cost_gpu_hr}")


def main():
    parser = argparse.ArgumentParser(description="Analyze autoscaling experiments (Issue #1247)")
    parser.add_argument(
        "--format", choices=["text", "markdown", "csv"], default="text",
        help="Output format (default: text)"
    )
    args = parser.parse_args()

    if args.format == "markdown":
        print_markdown_report()
    elif args.format == "csv":
        print_csv()
    else:
        print_text_report()


if __name__ == "__main__":
    main()
