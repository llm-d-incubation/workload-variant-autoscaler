# CI Benchmark - Phase 1 & 2 Implementation Guide

This document explains the implementation of Phase 1 and Phase 2 of the automated Workload Variant Autoscaler (WVA) benchmarking system, as introduced in PR #900.

## Overview

The goal of this system is to automatically test the reaction time and stability of the WVA controller under simulated load, and provide a visual report (via Grafana) of its performance.

This is implemented as a Go-based test suite using the Ginkgo framework, running against an emulated Kind (Kubernetes in Docker) cluster.

### Phase 1: The Load Test (Scale-Up Latency)

Phase 1 focuses on measuring the autoscaler's reaction time. We run a scenario called `scale-up-latency` which consists of 4 distinct phases:

1. **Baseline (2 mins):** No load is applied. The test ensures the system is stable at exactly 1 replica.
2. **Spike (5 mins):** The test launches parallel Kubernetes Jobs that use `curl` to blast the model server with a massive burst of requests. The test measures the **Scale-Up Time** (how many seconds it takes for the autoscaler to detect the queue and request more replicas).
3. **Sustained (3 mins):** The load continues at a steady rate. The test queries Prometheus to calculate stability metrics, such as:
   * **Max Replicas:** The peak number of servers provisioned.
   * **Avg Queue Depth:** The average number of requests waiting in the vLLM queue.
   * **Replica Oscillation (σ):** The standard deviation of the replica count, ensuring the autoscaler isn't rapidly scaling up and down (thrashing).
4. **Cooldown (5 mins):** The load generation jobs are deleted. The test measures the **Scale-Down Time** (how long it takes for the autoscaler to realize the queue is empty and scale back down to 1 replica).

**Note on Load Generation:** This test does *not* use `guidellm`. Because we only care about filling the queue to trigger a scaling event (not measuring precise token throughput), we use lightweight `curl` scripts running in parallel pods.

### Phase 2: Grafana Persistence & Visuals

Phase 2 focuses on making the results easy to understand by automatically capturing Grafana dashboards.

Because the Kind cluster is ephemeral (it is destroyed immediately after the test finishes), we must extract the visual data before the cluster is deleted.

1. **Ephemeral Grafana:** During the test setup, an instance of Grafana is deployed alongside Prometheus. It is pre-configured with a custom `benchmark-dashboard.json`.
2. **Image Renderer:** The Grafana deployment includes the `grafana-image-renderer` plugin.
3. **Snapshot Export:** When the test finishes, the Go code uses the Grafana API to:
   * Export the entire dashboard as a raw JSON snapshot (`benchmark-grafana-snapshot.json`). This file can be imported into *any* Grafana instance later to view the interactive graphs.
   * Render all 5 individual panels (Replicas, Queue Depth, KV Cache, Desired Replicas, Saturation) as static `.png` images.
4. **CI Integration:** In GitHub Actions, these JSON and PNG files are uploaded as workflow artifacts. The workflow then posts a PR comment containing the numerical metrics table and embeds the PNG images directly in the comment for immediate visual feedback.

## How to Run Locally

You can run this entire suite locally on your machine. It takes approximately 20-25 minutes.

1. Ensure you have Docker and `kind` installed.
2. Check out the benchmark branch:
   ```bash
   gh pr checkout 900
   ```
3. Clean up any existing clusters:
   ```bash
   make destroy-kind-cluster
   ```
4. Run the benchmark suite (this will create the cluster, deploy the infrastructure, and run the tests):
   ```bash
   make test-benchmark-with-setup CREATE_CLUSTER=true
   ```

### Viewing Local Results

When the test completes, the results are saved to your local `/tmp` directory:

* **Metrics JSON:** `cat /tmp/benchmark-results.json`
* **Grafana PNGs:** `open /tmp/benchmark-panels/`
* **Grafana Snapshot:** `/tmp/benchmark-grafana-snapshot.json`

## What's Next (Phase 3)

Phase 3 will involve taking this exact same Go-based test framework and running it against a real OpenShift cluster with physical GPUs (e.g., CoreWeave) and real AI models, rather than the emulated Kind environment. This will provide true end-to-end latency metrics.
