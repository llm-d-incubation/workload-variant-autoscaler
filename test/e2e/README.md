# E2E Test Suite

Environment-agnostic end-to-end tests for Workload-Variant-Autoscaler.

## Overview

This test suite is designed to run on **any Kubernetes cluster** (Kind, OpenShift, etc.) with **any EPP configuration**. Tests are parameterized via environment variables and dynamically create their own resources during execution.

### Key Principles

1. **Environment-Agnostic**: Same tests run on Kind (emulated GPUs) or real Kubernetes environments with GPUs
2. **Infrastructure Separation**: Tests require "infra-only" deployment (WVA controller + llm-d infrastructure)
3. **Dynamic Resource Management**: Each test creates VA, HPA, and model services as part of the test workflow
4. **Tiered Testing**: Smoke tests for quick validation, full suite for comprehensive coverage

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
FOCUS="Basic VA lifecycle" make test-e2e
```

### Environment Configuration

Set environment variables to customize test behavior:

```bash
# Environment
export ENVIRONMENT=kind                    # kind, openshift, etc
export LLMD_NAMESPACE=llm-d-sim           # llm-d infrastructure namespace
export WVA_NAMESPACE=workload-variant-autoscaler-system

# Infrastructure mode
export USE_SIMULATOR=true                  # true=emulated GPUs, false=real vLLM
export SCALE_TO_ZERO_ENABLED=false        # HPAScaleToZero feature gate

# Model configuration
export MODEL_ID=unsloth/Meta-Llama-3.1-8B
export ACCELERATOR_TYPE=nvidia.com/gpu
export MAX_NUM_SEQS=5                     # Lower = easier to saturate

# Load generation
export LOAD_STRATEGY=synthetic            # synthetic or sharegpt
export REQUEST_RATE=8
export NUM_PROMPTS=1000
export INPUT_TOKENS=100
export OUTPUT_TOKENS=50

# Timeouts (seconds)
export POD_READY_TIMEOUT=300              # 5 minutes
export SCALE_UP_TIMEOUT=600               # 10 minutes
```

### Example: Run on Kind with Emulated GPUs

```bash
export ENVIRONMENT=kind
export USE_SIMULATOR=true
export SCALE_TO_ZERO_ENABLED=false
make test-e2e-smoke
```

### Example: Run on OpenShift with Real GPUs

```bash
export ENVIRONMENT=openshift
export USE_SIMULATOR=false
export LOAD_STRATEGY=sharegpt
export REQUEST_RATE=20
export NUM_PROMPTS=3000
make test-e2e-full
```

### Example: Run with Scale-to-Zero Enabled

```bash
export ENVIRONMENT=kind
export USE_SIMULATOR=true
export SCALE_TO_ZERO_ENABLED=true  # Requires HPAScaleToZero feature gate
make test-e2e-full
```

## Test Tiers

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

3. **Target Condition Validation** (~1 min)
   - Verify TargetResolved=True when deployment exists
   - Verify TargetResolved=False when deployment doesn't exist

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
1. **Saturation Mode - Single VA** (~8 min)
   - Create InferencePool, model service, VA, HPA dynamically
   - Full scale-up cycle (1 → 3 replicas)
   - Saturation detection (KV cache threshold + queue length)
   - HPA integration and stabilization
   - Scale-down when load decreases

2. **Saturation Mode - Multiple VAs** (~10 min)
   - Create two InferencePools with different accelerators
   - Create two VAs with different cost configurations
   - Verify cost-based scaling (prefer cheaper accelerator)
   - Verify independent scaling per VA

3. **Scale-From-Zero** (~7 min)
   - Create HPA with minReplicas=0
   - Verify deployment scales to 0 when idle
   - Generate first request, verify scale-up from 0 → 1
   - Verify request queuing during cold start

4. **GPU Limiter** (~8 min)
   - Create two VAs with different accelerator constraints
   - Verify limiter prevents scheduling on mismatched GPUs
   - Verify correct accelerator selection based on VA spec

5. **Scale-to-Zero** (Label: `full`, `flaky`) (~7 min)
   - Create HPA with scale-to-zero enabled
   - Generate load, verify scale-up
   - Stop load, verify scale-down to 0 after idle period
   - *Note: Currently disabled due to flakiness*

6. **Scale-to-Zero Disabled** (~5 min)
   - Verify minimum replicas are maintained when scale-to-zero is disabled via ConfigMap
   - Tests ConfigMap-based feature disable

7. **PodScrapingSource** (~3 min)
   - Verify metrics collection from EPP pods
   - Tests PodScrapingSource discovery and scraping
   - Note: Direct scraping tests skipped on Kind (use in-cluster tests)

**Run Command:**
```bash
make test-e2e-full
# Or with Ginkgo directly
ginkgo -v --label-filter="full && !flaky" ./test/e2e/
```

### Tier 3: Real Hardware Validation

**Purpose:** Production-realistic validation with actual GPUs
**Duration:** 30-45 minutes
**Trigger:** Manual trigger for pre-release validation

**Configuration:**
- Environment: OpenShift with real GPUs (A100, H100, MI300X)
- UseSimulator: false
- LoadStrategy: sharegpt (realistic traffic patterns)

**Tests:** Same as Tier 2, but with different configuration

**Unique Value:**
- Real vLLM cold-start latency (5-10 seconds vs instant)
- Actual GPU memory pressure and KV cache behavior
- Production-like workload patterns from ShareGPT dataset
- Validates integration with real Prometheus metrics from vLLM

**Run Command:**
```bash
ENVIRONMENT=openshift \
USE_SIMULATOR=false \
LOAD_STRATEGY=sharegpt \
REQUEST_RATE=20 \
NUM_PROMPTS=3000 \
make test-e2e-full
```

## Test Structure

### Directory Layout

```
test/e2e/
├── config.go              # Environment configuration system
├── suite_test.go          # Environment-agnostic BeforeSuite/AfterSuite
├── smoke_test.go          # Smoke tests (Tier 1)
├── saturation_test.go     # Saturation detection and scale-up tests
├── scale_from_zero_test.go # Scale-from-zero tests
├── scale_to_zero_test.go  # Scale-to-zero tests (including disabled scenario)
├── limiter_test.go        # GPU limiter tests
├── target_condition_test.go # TargetResolved condition tests
├── pod_scraping_test.go   # PodScrapingSource metrics collection tests
├── fixtures/              # Resource builders for dynamic creation
│   ├── infra_builder.go   # InferencePool, ModelService factories
│   ├── va_builder.go      # VariantAutoscaling factories
│   ├── hpa_builder.go     # HPA factories
│   └── workload_builder.go # Load job factories
└── README.md              # This file
```

### Test Lifecycle

Each test follows this pattern:

1. **BeforeAll**: Dynamically create test resources
   - InferencePool
   - Model service (vLLM or simulator)
   - VariantAutoscaling
   - HPA
   - Load jobs (if needed)

2. **Test Execution**: Verify behavior
   - Wait for resource readiness
   - Generate load
   - Verify scaling behavior
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
| `Environment` | `ENVIRONMENT` | `kind` | Cluster type: kind, openshift, kubernetes |
| `UseSimulator` | `USE_SIMULATOR` | `true` | Use emulated GPUs (true) or real vLLM (false) |
| `ScaleToZeroEnabled` | `SCALE_TO_ZERO_ENABLED` | `false` | Enable HPAScaleToZero feature gate |
| `ModelID` | `MODEL_ID` | `unsloth/Meta-Llama-3.1-8B` | Model ID for deployments |
| `MaxNumSeqs` | `MAX_NUM_SEQS` | `5` | vLLM batch size (lower = easier to saturate) |
| `LoadStrategy` | `LOAD_STRATEGY` | `synthetic` | Load generation: synthetic or sharegpt |
| `RequestRate` | `REQUEST_RATE` | `8` | Requests per second |
| `NumPrompts` | `NUM_PROMPTS` | `1000` | Total number of requests |

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

### Scale-Up Tests Fail

**Possible Causes:**
1. **Metrics not available:** Check Prometheus is scraping EPP metrics
2. **HPA not reconciling:** Verify external metrics API is working
3. **Load insufficient:** Lower `MAX_NUM_SEQS` to make saturation easier

**Debug Commands:**
```bash
# Check VA status
kubectl get variantautoscaling -n llm-d-sim -o yaml

# Check HPA status
kubectl get hpa -n llm-d-sim -o yaml

# Check external metrics
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
