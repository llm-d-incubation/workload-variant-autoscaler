# E2E Test Suite

Environment-agnostic end-to-end tests for Workload-Variant-Autoscaler.

## Overview

This test suite is designed to run on **any Kubernetes cluster** (Kind, OpenShift, etc.) with **any EPP configuration**. Tests are parameterized via environment variables and dynamically create their own resources during execution.

### Scope (deterministic correctness)

These e2e tests are intended to be **deterministic functionality checks**: resource wiring, reconciliation, and stable invariants (e.g., CRs reconcile, status conditions are set, scalers are created and point at the right targets/metrics).

If a test needs **high traffic**, long “wait and see” timing, or performance assertions (scale-up latency, throughput, replica stability under sustained load), it does **not** belong in this e2e suite. Keep that work in a separate benchmarking/perf workflow so e2e remains a reliable correctness signal.

### Key Principles

1. **Environment-Agnostic**: Same tests run on Kind (emulated GPUs) or real Kubernetes environments with GPUs
2. **Infrastructure Separation**: Tests require "infra-only" deployment (WVA controller + llm-d infrastructure)
3. **Dynamic Resource Management**: Each test creates VA, HPA, and model services as part of the test workflow
4. **Tiered Testing**: Smoke tests for quick validation, full suite for comprehensive coverage
5. **Serialize If Needed**: Since the scope is **deterministic correctness**, if there are tests that should be run serially then make them as such, and make sure the environment is clean in each `BeforeAll`. Running tests such as for Deployment, LWS with 1 leader+1 worker, LWS with 1 leader+0 worker in parallel pointing to the same model can have issues with conflicting resources and can be hard to track.

## Prerequisites

### Infrastructure Setup

Before running tests, deploy the infrastructure in "infra-only" mode:

```bash
# Deploy only WVA controller + llm-d infrastructure (no VA/HPA resources)
cd deploy
export ENVIRONMENT="kind-emulator"  # or "openshift", "kubernetes"
export INFRA_ONLY=true
./install.sh
```

This deploys:
- ✅ WVA controller
- ✅ llm-d infrastructure (Gateway, CRDs, RBAC, EPP)
- ✅ Prometheus stack (metrics collection)
- ✅ Prometheus Adapter (external metrics API)
- ❌ **NO** VariantAutoscaling resources (tests create these)
- ❌ **NO** HPA resources (tests create these)
- ❌ **NO** Model services (tests create these)

### Verify Infrastructure

```bash
# WVA controller should be running
kubectl get pods -n workload-variant-autoscaler-system

# No VA resources should exist
kubectl get variantautoscaling --all-namespaces  # Should be empty

# No test HPA resources should exist
kubectl get hpa --all-namespaces | grep -v kube-system  # Should be empty
```

## Running Tests

### Quick Start

```bash
# Smoke tests (5-10 minutes) - Run on every PR
make test-e2e-smoke

# Full suite (15-25 minutes) - Run on-demand
make test-e2e-full

# Run specific test
FOCUS="Basic VA lifecycle" make test-e2e-smoke
```

### Environment Configuration

Set environment variables to customize test behavior:

```bash
# Environment (use kind-emulator for Makefile + deploy/install.sh emulated Kind flow)
export ENVIRONMENT=kind-emulator           # or openshift
export LLMD_NAMESPACE=llm-d-sim           # llm-d infrastructure namespace
export WVA_NAMESPACE=workload-variant-autoscaler-system

# Infrastructure mode
export USE_SIMULATOR=true                  # true=emulated GPUs, false=real vLLM
export SCALE_TO_ZERO_ENABLED=false        # HPAScaleToZero feature gate

# Scaler backend: prometheus-adapter (HPA with wva_desired_replicas) or keda (ScaledObjects)
# Only one backend per run; with keda, do not deploy Prometheus Adapter for external metrics.
export SCALER_BACKEND=prometheus-adapter  # or keda

# Model configuration
export MODEL_ID=unsloth/Meta-Llama-3.1-8B
export ACCELERATOR_TYPE=nvidia.com/gpu
export MAX_NUM_SEQS=5                     # Lower = easier to saturate

# Timeouts (seconds)
export POD_READY_TIMEOUT=300              # 5 minutes — model deployment ready
export SCALE_UP_TIMEOUT=600               # 10 minutes — long steps (e.g. scale-from-zero job completion)

# Gomega Eventually tuning (optional; defaults match former hard-coded waits)
export E2E_EVENTUALLY_SHORT=30            # quick checks / delete verification
export E2E_EVENTUALLY_MEDIUM=60         # ~1m single steps
export E2E_EVENTUALLY_STANDARD=120        # default for most reconcile waits (BeforeSuite sets Gomega default)
export E2E_EVENTUALLY_LONG=180          # MetricsAvailable-type waits
export E2E_EVENTUALLY_EXTENDED=300       # multi-minute engine / HPA steps (~5m)
export E2E_EVENTUALLY_POLL=5              # default polling interval (seconds)
export E2E_EVENTUALLY_POLL_QUICK=2
export E2E_EVENTUALLY_POLL_SLOW=10
export E2E_EVENTUALLY_POLL_VERY_SLOW=15

# kind-emulator + prometheus-adapter: BeforeSuite probes adapter readiness + `external.metrics.k8s.io/v1beta1` discovery
# before optionally restarting pods.
# auto (default if unset): restart only if the probe fails within E2E_PROM_ADAPTER_PROBE_SEC (default 90).
# true: always delete adapter pods (legacy). false: never restart.
export RESTART_PROMETHEUS_ADAPTER=auto   # or true / false
export E2E_PROM_ADAPTER_PROBE_SEC=90
```

### Optional: faster `deploy/install.sh` for e2e

`deploy/install.sh` runs **`helm repo update`** by default. To skip (faster but requires existing repo indexes), set **`SKIP_HELM_REPO_UPDATE=true`**.

For infra-only e2e deploys, **`E2E_DEPLOY_WAIT_TIMEOUT`** (default **`120s`**) bounds how long `install.sh` waits for the EPP deployment and inference-gateway deployment to become Available after llm-d Helm apply. Increase if your cluster is slow to pull images.

### Example: Run on Kind with Emulated GPUs

```bash
export ENVIRONMENT=kind-emulator
export USE_SIMULATOR=true
export SCALE_TO_ZERO_ENABLED=false
make test-e2e-smoke
```

### Example: Run on OpenShift with Real GPUs

```bash
export ENVIRONMENT=openshift
export USE_SIMULATOR=false
make test-e2e-full
```

### Example: Run with Scale-to-Zero Enabled

```bash
export ENVIRONMENT=kind-emulator
export USE_SIMULATOR=true
export SCALE_TO_ZERO_ENABLED=true  # Requires HPAScaleToZero feature gate (or use SCALER_BACKEND=keda)
make test-e2e-full
```

The scale-from-zero spec submits traffic via a small **curl** Job; see **Trigger job tunables** under Tier 2 (full suite) for `numRequests`, timeouts, and the gateway URL shape.

### Example: Run with KEDA as Scaler Backend

When using KEDA, set `SCALER_BACKEND=keda` and **`ENVIRONMENT=kind-emulator`**; the deploy script will install KEDA and skip Prometheus Adapter. **KEDA is only supported for the kind-emulator (emulated) environment;** for OpenShift use Prometheus Adapter or the platform CMA.

> **Note:** We do not install the OpenShift Custom Metrics Autoscaler (CMA) operator in e2e. We install **upstream KEDA** (e.g. via Helm) to **imitate** CMA behavior—same ScaledObject-driven flow and external metrics API usage. E2E with `SCALER_BACKEND=keda` is a stand-in for validating WVA with an OpenShift CMA–style scaler.

```bash
# Deploy e2e infrastructure with KEDA, then run smoke tests
make deploy-e2e-infra SCALER_BACKEND=keda
make test-e2e-smoke SCALER_BACKEND=keda

# Or deploy + run in one go (smoke or full)
make deploy-e2e-infra SCALER_BACKEND=keda && make test-e2e-full SCALER_BACKEND=keda
```

To undeploy after using KEDA: `SCALER_BACKEND=keda make undeploy-wva-emulated-on-kind`.

### Run smoke with full setup (Kind + KEDA) and save output

Single command that creates the Kind cluster, deploys e2e infra with KEDA, and runs smoke tests. You can run this from any terminal; use `tee` to save output for later reference.

```bash
ENVIRONMENT=kind-emulator \
USE_SIMULATOR=true \
SCALE_TO_ZERO_ENABLED=false \
CREATE_CLUSTER=true \
INSTALL_GATEWAY_CTRLPLANE=true \
E2E_TESTS_ENABLED=true \
IMG=ghcr.io/llm-d/llm-d-workload-variant-autoscaler:0.0.1-test \
DELETE_CLUSTER=false \
SCALER_BACKEND=keda \
make test-e2e-smoke-with-setup 2>&1 | tee test/e2e/e2e-smoke-keda-with-setup.log
```

## Test Tiers

**Ginkgo filters:** `make test-e2e-smoke` runs **`-ginkgo.label-filter="smoke"`** (only specs with the `smoke` label). `make test-e2e-full` runs **`-ginkgo.label-filter="full && !flaky"`** (non-flaky specs with the `full` label). Many specs carry **both** `smoke` and `full`, so the **full run is a superset of smoke**: it re-executes those shared specs and adds full-only scenarios (e.g. scale-from-zero, limiter). Running full does not replace smoke in CI—they answer different “how much to run” questions.

### Tier 1: Smoke Tests (Label: `smoke`)

**Purpose:** Fast validation on every PR to catch 80% of issues
**Duration:** 5-10 minutes
**Trigger:** Automatic on every PR

**Tests:**
1. **Infrastructure Readiness** (~2 min)
   - Verify WVA controller is running
   - Verify llm-d infrastructure deployed
   - Verify Prometheus is scraping metrics
   - Verify external metrics API is available

2. **Basic VA Lifecycle** (~3-5 min)
   - Dynamically create InferencePool + model service
   - Dynamically create VariantAutoscaling resource
   - Verify controller reconciles successfully
   - Check VA status conditions (TargetResolved=true)
   - Verify external metrics API returns values

3. **Error handling (smoke)** (~few min)
   - Deployment delete/recreate while VA exists; **TargetResolved** returns True after recovery
   - Metrics unavailability handling (MetricsAvailable condition)

**Run Command:**
```bash
make test-e2e-smoke
# Or with Ginkgo directly
ginkgo -v --label-filter="smoke" ./test/e2e/
```

### Tier 2: Full E2E Suite (Label: `full`)

**Purpose:** Comprehensive validation before merge
**Duration:** 15-25 minutes
**Trigger:** On-demand via `/test-full` slash command

**Tests:**
1. **Scale-From-Zero** (~7 min)
   - Requires EPP flow control (`E2E_TESTS_ENABLED=true` or `ENABLE_SCALE_TO_ZERO=true` patches EPP). The scale-from-zero spec applies **InferenceObjective** `e2e-default` via `test/e2e/fixtures` when the CRD exists (install.sh no longer applies it for e2e).
   - Create HPA (or KEDA ScaledObject) with minReplicas=0
   - Verify deployment scales to 0 when idle
   - Generate first request, verify scale-up from 0 → 1
   - Verify request queuing during cold start

   **Trigger job tunables** (`createScaleFromZeroTriggerJob` in [`scale_from_zero_test.go`](scale_from_zero_test.go)): these are **constants in code today** (not environment variables). Adjust them in that helper if your cluster is slow or the gateway times out.

   | Parameter | Current value | Role |
   |-----------|---------------|------|
   | `numRequests` | `10` | Loop count: sequential POSTs to the gateway so the scale-from-zero engine can observe queued work. |
   | Inter-request delay | `sleep 2` (seconds) | Pause between POSTs; keeps pressure on the flow-control path without a single burst. |
   | Per-request HTTP timeout | `curl --max-time 180` | Seconds to wait for each completion response (cold start can be slow). |
   | Job `backoffLimit` | `3` | Kubernetes Job retries if the pod exits non-zero. |
   | Gateway URL | `http://<discovered-service>:80/v1/completions` | Service name is the first Service in the llm-d namespace whose name contains **`inference-gateway`**. Uses **text completions**, not `/v1/chat/completions`. |
   | Request body | JSON | `"model"`: same as test **`MODEL_ID`** (`cfg.ModelID`); `"prompt"`: fixed test string; `"max_tokens"`: `50`. |
   | Job container image | `quay.io/curl/curl:8.11.1` | Must remain a non–Docker Hub image per e2e policy. |
   | Pod resources | 100m–200m CPU, 128Mi–256Mi memory | `curl` sidecar workload. |
   | Success criterion | At least one HTTP **200** | Script exits `0` if `SUCCESS > 0` after all attempts (allows some failures while the model still scales). |

2. **GPU Limiter** (~8 min)
   - Create two VAs with different accelerator constraints
   - Verify limiter prevents scheduling on mismatched GPUs
   - Verify correct accelerator selection based on VA spec

3. **PodScrapingSource** (~3 min)
   - Verify metrics collection from EPP pods
   - Tests PodScrapingSource discovery and scraping
   - Note: Direct scraping tests skipped on Kind (use in-cluster tests)

4. **Saturation analyzer path and status propagation** (~2-6 min)
   - Toggle saturation config `analyzerName` between `"saturation"` (V2) and unset (V1)
   - Verify controller processing path transitions for a dedicated test model
   - Verify stable status contract: `DesiredOptimizedAlloc` is populated and `MetricsAvailable=True`
   - Run a bounded V1 threshold-crossing request job (no sustained load)
   - Bounded deterministic assertions only (no benchmark/load criteria)

   **Threshold-crossing tunables** ([`createSaturationThresholdTriggerJob`](saturation_analyzer_path_test.go); shell in [`fixtures/saturation_threshold_trigger.sh`](fixtures/saturation_threshold_trigger.sh), embedded with `//go:embed`):

   | Parameter | Current value | Role |
   |-----------|---------------|------|
   | `numRequests` | `6` | Exact, bounded completion requests for the V1 threshold scenario. |
   | `max_tokens` | `400` | Keeps each request active long enough for metrics scrape/analyzer evaluation. |
   | Service preflight retries | `24` | Retry budget before sending traffic (`/v1/models` probe loop). |
   | Service preflight delay | `5s` | Delay between `/v1/models` probe attempts. |
   | Per-request HTTP timeout | `curl --max-time 240` | Bounds request runtime while tolerating cold starts. |
   | Job `backoffLimit` | `1` | One retry max to reduce hidden variability. |
   | Target URL | `http://<model-service>:8000/v1/completions` | Direct model service path (not gateway) to keep trigger deterministic. |
   | Endpoint readiness gate | service Endpoints ready `> 0` | Test waits for Kubernetes endpoints before creating the trigger job. |
   | Job container image | `quay.io/curl/curl:8.11.1` | Non–Docker Hub image per e2e policy. |

**Run Command:**
```bash
make test-e2e-full
# Or with Ginkgo directly
ginkgo -v --label-filter="full && !flaky" ./test/e2e/
```

### Tier 3: Real hardware validation (same suite, different cluster)

**Purpose:** Run the **same** correctness e2e specs (**Tier 1** / **Tier 2** filters) on a cluster with **real GPUs** and **real vLLM** (or your production-like model server), not the Kind simulator.

**Duration:** Often longer than emulated runs (image pulls, model load, cold start).

**Trigger:** Manual or release gates (e.g. OpenShift workflow). Not a separate test binary—only **configuration and environment** change.

**Configuration (typical):**
- **`ENVIRONMENT=openshift`** (or **`kubernetes`** with a GPU-capable cluster aligned with `deploy/install.sh`)
- **`USE_SIMULATOR=false`** so tests use the **real vLLM** path in fixtures (`model_service_builder`)
- **`MODEL_ID`**, **`ACCELERATOR_TYPE`**, and namespaces set to match your pool and registry
- **Increase timeouts if needed:** `POD_READY_TIMEOUT`, `E2E_EVENTUALLY_*`, `SCALE_UP_TIMEOUT` (see [Environment Configuration](#environment-configuration))

This tier does **not** reintroduce benchmark-style load: there are **no** `LOAD_STRATEGY` / `REQUEST_RATE` / `NUM_PROMPTS` knobs in this suite. Heavy or dataset-driven traffic belongs in a **benchmarking** workflow, not here.

**Unique value:**
- Real model cold-start and GPU behavior vs instant simulator
- Prometheus metrics and scraping against a live vLLM stack
- Validates wiring and reconciliation under production-like constraints (subject to the same Ginkgo labels as Tier 2)

**Run command (example):**
```bash
ENVIRONMENT=openshift \
USE_SIMULATOR=false \
MODEL_ID=<your-model-id> \
ACCELERATOR_TYPE=<valid-label-value> \
make test-e2e-full
```

For a quicker pass: `make test-e2e-smoke` with the same exports.

## Test Structure

### Directory Layout

```
test/e2e/
├── config.go              # Environment configuration system
├── suite_test.go          # Environment-agnostic BeforeSuite/AfterSuite
├── smoke_test.go          # Smoke tests (Tier 1)
├── scale_from_zero_test.go # Scale-from-zero tests
├── limiter_test.go        # GPU limiter tests
├── pod_scraping_test.go   # PodScrapingSource metrics collection tests
├── fixtures/              # Resource builders for dynamic creation
│   ├── infra_builder.go   # InferencePool, ModelService factories
│   ├── va_builder.go      # VariantAutoscaling factories
│   ├── model_service_builder.go
│   ├── hpa_builder.go     # HPA factories
│   ├── scaled_object_builder.go
└── README.md              # This file
```

### Test Lifecycle

Each test follows this pattern:

1. **BeforeAll**: Dynamically create test resources
   - InferencePool
   - Model service (vLLM or simulator)
   - VariantAutoscaling
   - HPA

2. **Test Execution**: Verify behavior
   - Wait for resource readiness
   - Check metrics and status

3. **AfterAll**: Clean up test resources
   - Delete VA, HPA, deployments, jobs
   - Wait for resources to be deleted

**Key Principle:** Each test creates and cleans up its own resources. No shared state between tests.

## Configuration Reference

See [config.go](config.go:1) for the complete list of configuration options.

### Key Configuration Fields

| Field | Environment Variable | Default | Description |
|-------|---------------------|---------|-------------|
| `Environment` | `ENVIRONMENT` | `kind-emulator` | `kind-emulator` (emulated Kind), `openshift`, or `kubernetes` |
| `UseSimulator` | `USE_SIMULATOR` | `true` | Use emulated GPUs (true) or real vLLM (false) |
| `ScaleToZeroEnabled` | `SCALE_TO_ZERO_ENABLED` | `false` | Enable HPAScaleToZero feature gate |
| `ModelID` | `MODEL_ID` | `unsloth/Meta-Llama-3.1-8B` | Model ID for deployments |
| `MaxNumSeqs` | `MAX_NUM_SEQS` | `5` | vLLM batch size (lower = easier to saturate) |
| `EventuallyStandardSec` | `E2E_EVENTUALLY_STANDARD` | `120` | Default `Eventually` timeout (see bash block above for full set) |
| `ScaleUpTimeout` | `SCALE_UP_TIMEOUT` | `600` | Longest scale / job waits |
| (suite) | `RESTART_PROMETHEUS_ADAPTER` | `auto` | adapter pod restart policy on kind-emulator (`auto` probes adapter pod Ready + `external.metrics.k8s.io/v1beta1` discovery; restart only on probe failure) |

Bounded **minimal traffic** (e.g. scale-from-zero trigger job) is documented per spec in code; sustained load belongs in benchmarking, not this suite.

## Troubleshooting

### Tests Fail with "WVA controller not found"

**Solution:** Ensure infra-only deployment was successful:
```bash
kubectl get pods -n workload-variant-autoscaler-system
```

### Tests Timeout Waiting for Model Service

**Solution:** Increase `POD_READY_TIMEOUT`:
```bash
export POD_READY_TIMEOUT=600  # 10 minutes
```

### HPA, external metrics, or scale-from-zero

Use this when smoke/full tests fail on **VA reconciliation**, **HPA / desired replicas**, **`wva_desired_replicas`**, or **scale-from-zero** (queue not visible, job times out).

**Things to verify:**
1. **Prometheus** is scraping model/EPP targets; **`MetricsAvailable`** on the VA in `kubectl describe`.
2. **`external.metrics.k8s.io`** works when using **`SCALER_BACKEND=prometheus-adapter`**; on kind-emulator, the default `auto` mode already probes adapter pod Ready + `external.metrics.k8s.io/v1beta1` discovery. If the API is still empty after install, set **`RESTART_PROMETHEUS_ADAPTER=true`** to force a restart.
3. **Scale-from-zero:** infra deployed with **`E2E_TESTS_ENABLED=true`** (or **`ENABLE_SCALE_TO_ZERO=true`**) so EPP flow control is on; raise **`E2E_EVENTUALLY_*`** / **`SCALE_UP_TIMEOUT`** if cold start is slow; see **Tier 2** trigger job tunables.

**Debug commands** (adjust `-n` to your llm-d namespace, e.g. `LLMD_NAMESPACE`):
```bash
kubectl get variantautoscaling -n llm-d-sim -o yaml
kubectl get hpa -n llm-d-sim -o yaml
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/llm-d-sim/wva_desired_replicas"
```

### Tests Leave Orphaned Resources

**Solution:** Run AfterSuite cleanup manually:
```bash
# Delete test VAs
kubectl delete variantautoscaling -n llm-d-sim -l test-resource=true

# Delete test HPAs
kubectl delete hpa -n llm-d-sim -l test-resource=true

# Delete test deployments
kubectl delete deployment -n llm-d-sim -l test-resource=true

# Delete test jobs
kubectl delete job -n llm-d-sim -l test-resource=true
```

## Contributing

### Adding New Tests

1. Create a new test file in `test/e2e/`
2. Use fixtures from `test/e2e/fixtures/` to create resources
3. Add appropriate Ginkgo labels (`smoke`, `full`, `flaky`)
4. Ensure BeforeAll/AfterAll cleanup is implemented
5. Update this README with test description

### Example Test Template

```go
var _ = Describe("My New Test", Label("full"), Ordered, func() {
    var (
        poolName = "my-test-pool"
        vaName   = "my-test-va"
    )

    BeforeAll(func() {
        // Create test resources using fixtures
        err := fixtures.CreateInferencePool(ctx, crClient, cfg.LLMDNamespace, poolName, 8000)
        Expect(err).NotTo(HaveOccurred())
        // ... create more resources
    })

    AfterAll(func() {
        // Clean up test resources
        _ = crClient.Delete(ctx, &v1alpha1.VariantAutoscaling{
            ObjectMeta: metav1.ObjectMeta{Name: vaName, Namespace: cfg.LLMDNamespace},
        })
    })

    It("should do something", func() {
        // Test implementation
    })
})
```

## See Also

- [Developer Testing Guide](../../docs/developer-guide/testing.md)
- [Deployment Guide](../../deploy/README.md)
- [INFRA_ONLY Mode Documentation](../../deploy/README.md#example-7-infra-only-mode-for-e2e-testing)
