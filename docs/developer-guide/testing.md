# Testing Guide

Comprehensive guide for testing the Workload-Variant-Autoscaler (WVA).

## Overview

WVA has a multi-layered testing strategy:

1. **Unit Tests** - Fast, isolated tests for individual packages and functions
2. **Integration Tests** - Tests for component interactions within the controller
3. **E2E Tests** - Environment-agnostic end-to-end tests (Kind emulated or OpenShift), with smoke and full tiers

## Unit Tests

### Running Unit Tests

```bash
# Run all unit tests
make test

# Run with coverage report
go test -cover ./...

# Run specific package
go test ./pkg/solver/...
go test ./pkg/analyzer/...

# Run with verbose output
go test -v ./internal/controller/...

# Generate HTML coverage report
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out -o coverage.html
```

### Unit Test Structure

Unit tests are co-located with the code they test:

```
internal/
├── controller/
│   ├── variantautoscaling_controller.go
│   └── variantautoscaling_controller_test.go
├── saturation/
│   ├── analyzer.go
│   └── analyzer_test.go
└── collector/
    ├── collector.go
    └── collector_test.go

pkg/
└── solver/
    ├── optimizer.go
    ├── optimizer_test.go
    ├── solver.go
    └── solver_test.go
```

### Writing Unit Tests

Example unit test structure:

```go
package solver_test

import (
    "testing"
    . "github.com/onsi/ginkgo/v2"
    . "github.com/onsi/gomega"
)

func TestSolver(t *testing.T) {
    RegisterFailHandler(Fail)
    RunSpecs(t, "Solver Suite")
}

var _ = Describe("Solver", func() {
    Context("when optimizing single variant", func() {
        It("should calculate optimal replicas", func() {
            // Test implementation
            Expect(result).To(Equal(expected))
        })
    })
})
```

### Unit Test Best Practices

- **Use table-driven tests** for testing multiple scenarios
- **Mock external dependencies** (Kubernetes API, Prometheus, etc.)
- **Test edge cases** (zero values, negative numbers, nil pointers, etc.)
- **Keep tests fast** - unit tests should run in milliseconds
- **Use descriptive test names** - clearly state what is being tested
- **Follow AAA pattern** - Arrange, Act, Assert

## Integration Tests

Integration tests validate component interactions within the controller using envtest.

### Running Integration Tests

```bash
# Run integration tests (included in make test)
make test

# Run only controller integration tests
go test ./internal/controller/... -v
```

### envtest Setup

Integration tests use controller-runtime's envtest, which provides a real Kubernetes API server for testing:

```go
var _ = BeforeSuite(func() {
    testEnv = &envtest.Environment{
        CRDDirectoryPaths: []string{
            filepath.Join("..", "..", "config", "crd", "bases"),
        },
    }

    cfg, err := testEnv.Start()
    Expect(err).NotTo(HaveOccurred())

    k8sClient, err = client.New(cfg, client.Options{Scheme: scheme})
    Expect(err).NotTo(HaveOccurred())
})

var _ = AfterSuite(func() {
    Expect(testEnv.Stop()).To(Succeed())
})
```

## End-to-End Tests

WVA provides a **single consolidated E2E suite** that runs on multiple environments (Kind with emulated GPUs, or OpenShift/kubernetes with real infrastructure). Tests are environment-agnostic and parameterized via environment variables; they create VA, HPA, and model services dynamically as part of the test workflow.

- **Location**: `test/e2e/`
- **Environments**: Kind (emulated), OpenShift, or generic Kubernetes
- **Tiers**: Smoke (~5–10 min) for PRs; full suite (~15–25 min) for comprehensive validation

### Scope

E2E is intended to be a **deterministic correctness signal**: resource wiring, reconciliation, and stable invariants (e.g., CRs reconcile, status conditions are set, scalers are created and point at the right targets/metrics). Traffic generation and performance/benchmarking scenarios should live outside `test/e2e/`.

### Infra-Only Setup (Required Before Running Tests)

Tests expect **only** the WVA controller and llm-d infrastructure to be deployed; they create VariantAutoscaling resources, HPAs, and model services themselves. Use the install script in **infra-only** mode:

```bash
# From repository root: deploy only WVA + llm-d infrastructure (no VA/HPA/model services)
cd deploy
export ENVIRONMENT="kind-emulator"   # or "openshift", "kubernetes"
export INFRA_ONLY=true
./install.sh
# Or: ./install.sh --infra-only
```

This deploys:
- WVA controller
- llm-d infrastructure (Gateway, CRDs, RBAC, EPP)
- Prometheus stack and Prometheus Adapter (or KEDA when `SCALER_BACKEND=keda`)
- **No** VariantAutoscaling, HPA, or model services (tests create these)

When `E2E_TESTS_ENABLED=true` (or `ENABLE_SCALE_TO_ZERO=true`), the deploy script also enables **GIE queuing** so scale-from-zero tests can run: it patches the EPP with `ENABLE_EXPERIMENTAL_FLOW_CONTROL_LAYER=true` and applies an **InferenceObjective** (`e2e-default`) that references the default InferencePool. This ensures the metric `inference_extension_flow_control_queue_size` is populated when requests hit the gateway.

**Install script tuning (optional, same variables as `deploy/install.sh`):**

- **`SKIP_HELM_REPO_UPDATE`**: When set to **`true`**, `helm repo update` is skipped during installs (faster, less network churn). Default runs `helm repo update` to refresh repo indexes.
- **`E2E_DEPLOY_WAIT_TIMEOUT`**: For infra-only e2e deploys (`INFRA_ONLY=true` with `E2E_TESTS_ENABLED=true`), caps the `kubectl wait` for the EPP and inference-gateway deployments (default **`120s`**). Raise it if image pulls rollouts routinely exceed that window.

Alternatively, use the Makefile to deploy infra and run tests in one go:

```bash
# Kind: create cluster, deploy infra, run smoke tests
make test-e2e-smoke-with-setup

# Kind: deploy infra only (if cluster already exists), then run full suite
make deploy-e2e-infra
make test-e2e-full
```

See the [E2E Test Suite README](../../test/e2e/README.md) for full configuration options and examples.

### Quick Start

```bash
# Smoke tests (recommended for every PR)
make test-e2e-smoke

# Full suite (on-demand)
make test-e2e-full

# OpenShift: point at cluster and run
export KUBECONFIG=/path/to/openshift/kubeconfig
export ENVIRONMENT=openshift
make test-e2e-smoke
# or make test-e2e-full

# Run a specific test by name
FOCUS="Basic VA lifecycle" make test-e2e-smoke
```

### What the Suite Validates

- **Smoke (label `smoke`)**: Infrastructure readiness, basic VA lifecycle, target condition validation
- **Full (label `full`)**: Smoke plus additional deterministic correctness checks (scale-from-zero, limiter, pod scraping, etc.)

### Configuration

Key environment variables (see [E2E Test Suite README](../../test/e2e/README.md) for the full list):

| Variable | Default | Description |
|----------|---------|-------------|
| `ENVIRONMENT` | `kind-emulator` | `kind-emulator`, `openshift`, or `kubernetes` |
| `USE_SIMULATOR` | `true` | Emulated GPUs (true) or real vLLM (false) |
| `SCALE_TO_ZERO_ENABLED` | `false` | Enable scale-to-zero tests (Kind supports both enabled and disabled) |
| `SCALER_BACKEND` | `prometheus-adapter` | `prometheus-adapter` or `keda` (KEDA only for kind-emulator) |
| `POD_READY_TIMEOUT` / `SCALE_UP_TIMEOUT` | `300` / `600` | Model ready vs longest scale/job waits (seconds) |
| `E2E_EVENTUALLY_STANDARD`, etc. | see README | Optional `Eventually` timeouts and poll intervals (`E2E_EVENTUALLY_*`, `E2E_EVENTUALLY_POLL*`) |
| `RESTART_PROMETHEUS_ADAPTER` | `auto` | kind-emulator: `auto` probes adapter + API before restarting pods; `true`/`false` force always/never |

Deploy-time knobs (passed through when you run `./deploy/install.sh` or `make deploy-e2e-infra`): `SKIP_HELM_REPO_UPDATE`, `E2E_DEPLOY_WAIT_TIMEOUT` — see **Install script tuning** above.

For running multiple test runs in parallel, use [multi-controller isolation](../user-guide/multi-controller-isolation.md) (`CONTROLLER_INSTANCE`).

## Test Comparison Matrix

| Aspect | Unit Tests | Integration Tests | E2E Consolidated (Kind emulated) | E2E Consolidated (OpenShift) |
|--------|-----------|-------------------|----------------------------------|------------------------------|
| **Speed** | Fast (<1min) | Fast (1-3min) | Smoke 5-10min / Full 15-25min | Smoke 5-10min / Full 15-25min |
| **Isolation** | Complete | Partial | Complete (Kind) | Shared cluster |
| **GPU Required** | No | No | No (emulated) | Yes (real) |
| **Infrastructure** | None | envtest | Kind + infra-only deploy | OpenShift + infra-only deploy |
| **Realism** | Low | Medium | High (emulated) | Production-like |
| **CI-Friendly** | Yes | Yes | Yes | Requires cluster |
| **Local Dev** | Yes | Yes | Yes | Cluster access needed |

## Continuous Integration

### GitHub Actions Workflows

WVA uses GitHub Actions for automated testing:

#### PR Checks Workflow

**File**: `.github/workflows/ci-pr-checks.yaml`

Runs on every pull request:
- Linting (golangci-lint)
- Unit tests
- Build verification
- Code coverage reporting

#### E2E Tests Workflow

E2E workflows run the **consolidated suite** (`test/e2e/`):
- **Smoke** (`make test-e2e-smoke`): Fast validation on Kind (or OpenShift when `ENVIRONMENT=openshift`)
- **Full** (`make test-e2e-full`): Full suite; typically run with infra deployed via `deploy-e2e-infra` or equivalent

Infrastructure is deployed in **infra-only** mode (WVA + llm-d only); tests create VA, HPA, and model services dynamically.

#### OpenShift E2E Tests Workflow

**File**: `.github/workflows/ci-e2e-openshift.yaml`

Runs OpenShift E2E tests on dedicated cluster:
- Triggered manually or on specific labels
- Deploys PR-specific namespaces
- Runs multi-model tests
- On failure: automatically scales down GPU workloads while preserving debugging resources (VA, HPA, logs)
- Smart resource management frees GPUs for other PRs without manual intervention

#### Triggering E2E via PR Comments

You can trigger E2E runs by commenting on a PR:

| Comment | Workflow | Who can use | Effect |
|--------|----------|-------------|--------|
| **`/ok-to-test`** | `ci-pr-checks.yaml` + `ci-e2e-openshift.yaml` | Users with write access | Runs the **full** Kind E2E suite **and** the OpenShift E2E (GPU) run on this PR. On fork PRs, this is required before OpenShift E2E can run. |
| **`/retest`** | `ci-e2e-openshift.yaml` | Users with write access | **OpenShift E2E only:** Re-run the OpenShift E2E workflow (e.g. after a failure, flake, or new commits). Same workflow as `/ok-to-test`, different trigger intent. |

**When to use:**

- **`/ok-to-test`**: Comment this when you want the full E2E suite to run on your PR. It triggers both the full Kind E2E (instead of smoke only) and the OpenShift E2E. By default, PRs only run smoke E2E on Kind.
- **`/retest`**: Use to re-run only the OpenShift E2E workflow (e.g. after a failure or new commits).
- **Fork PRs**: If you opened a PR from a fork, OpenShift E2E will not run until a maintainer or admin comments **`/ok-to-test`**. Branch protection should require the **e2e-openshift** status check so merge stays blocked until that run passes (the gate check is intentionally green on fork PRs to avoid a false failure that cannot be updated from upstream).

### Running CI Tests Locally

#### Simulate PR Checks

```bash
# Run linter
make lint

# Run unit tests
make test

# Build binary
make build

# Build Docker image
make docker-build
```

#### Simulate E2E CI

```bash
# Deploy infra (infra-only), then run smoke or full suite
make deploy-e2e-infra
make test-e2e-smoke
# or: make test-e2e-full

# One-shot: create cluster, deploy infra, run smoke tests
make test-e2e-smoke-with-setup
```

## Testing Best Practices

### General Guidelines

1. **Write tests first** (TDD approach) - helps design better APIs
2. **Test behavior, not implementation** - tests should survive refactoring
3. **Keep tests independent** - tests should not depend on each other
4. **Use meaningful assertions** - prefer specific matchers over generic equality
5. **Clean up resources** - always clean up in AfterEach/AfterAll blocks
6. **Document complex tests** - add comments explaining non-obvious test logic

### Ginkgo/Gomega Patterns

#### Use Descriptive Test Names

```go
// ✅ Good
It("should recommend scale-up when KV cache exceeds 70% threshold", func() {
    // ...
})

// ❌ Bad
It("should work", func() {
    // ...
})
```

#### Use Eventually for Async Operations

```go
// ✅ Good - waits for condition to become true
Eventually(func(g Gomega) {
    va := &v1alpha1.VariantAutoscaling{}
    err := k8sClient.Get(ctx, key, va)
    g.Expect(err).NotTo(HaveOccurred())
    g.Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 2))
}, timeout, interval).Should(Succeed())

// ❌ Bad - may fail due to timing
va := &v1alpha1.VariantAutoscaling{}
k8sClient.Get(ctx, key, va)
Expect(va.Status.DesiredOptimizedAlloc.NumReplicas).To(BeNumerically(">=", 2))
```

#### Use Consistently for Stable State

```go
// Verify replicas remain stable for 30 seconds
Consistently(func(g Gomega) {
    deploy := &appsv1.Deployment{}
    err := k8sClient.Get(ctx, key, deploy)
    g.Expect(err).NotTo(HaveOccurred())
    g.Expect(*deploy.Spec.Replicas).To(Equal(int32(2)))
}, 30*time.Second, 5*time.Second).Should(Succeed())
```

#### Use Ordered for Sequential Tests

```go
var _ = Describe("Scale-up workflow", Ordered, func() {
    // These tests run in order and share state
    It("should create resources", func() { /* ... */ })
    It("should detect saturation", func() { /* ... */ })
    It("should scale up", func() { /* ... */ })
})
```

### Test Organization

#### Use Contexts for Grouping

```go
var _ = Describe("Optimizer", func() {
    Context("with single variant", func() {
        It("should optimize for cost", func() { /* ... */ })
        It("should meet SLO requirements", func() { /* ... */ })
    })

    Context("with multiple variants", func() {
        It("should prefer cheaper variant", func() { /* ... */ })
        It("should distribute load evenly", func() { /* ... */ })
    })
})
```

#### Use BeforeEach/AfterEach for Setup/Teardown

```go
var _ = Describe("Controller", func() {
    var (
        namespace string
        cleanup   func()
    )

    BeforeEach(func() {
        namespace = "test-" + randomString()
        // Setup test resources
    })

    AfterEach(func() {
        // Clean up test resources
        if cleanup != nil {
            cleanup()
        }
    })

    It("should reconcile resources", func() {
        // Test implementation
    })
})
```

## Debugging Tests

### Debugging Unit Tests

```bash
# Run with verbose output
go test -v ./pkg/solver/...

# Run specific test
go test -v ./pkg/solver/... -run TestSolver/should_optimize

# Enable Ginkgo trace
go test -v ./pkg/analyzer/... -ginkgo.trace

# Run with debugger (delve)
dlv test ./internal/controller/... -- -ginkgo.v
```

### Debugging E2E Tests

#### View Test Logs

```bash
# Consolidated E2E suite (smoke or full)
go test ./test/e2e/ -v -ginkgo.v -ginkgo.label-filter="smoke"
go test ./test/e2e/ -v -ginkgo.v -ginkgo.label-filter="full && !flaky" -timeout 35m
```

#### Access Test Cluster

```bash
# For Kind E2E tests (default cluster name: kind-wva-gpu-cluster or from CLUSTER_NAME)
export KUBECONFIG=~/.kube/config   # or path from kind get kubeconfig
kubectl get pods -A
kubectl logs -n workload-variant-autoscaler-system deployment/workload-variant-autoscaler-controller-manager

# For OpenShift E2E tests
oc get pods -A
oc logs -n workload-variant-autoscaler-system deployment/workload-variant-autoscaler-controller-manager
```

#### Keep Cluster Alive After Failure

```bash
# Run tests; on failure, cluster is kept by default (DELETE_CLUSTER=false)
make test-e2e-smoke-with-setup
# Inspect: kubectl get all -A
# To delete cluster after: DELETE_CLUSTER=true make test-e2e-smoke-with-setup
# Or manually: kind delete cluster --name <CLUSTER_NAME>
```

### Common Test Failures

#### Test Times Out

**Symptoms**: Test hangs or exceeds timeout

**Possible causes**:
- Controller stuck in reconciliation loop
- HPA not reading metrics
- Prometheus not scraping metrics
- Resource quotas preventing pod creation

**Debugging steps**:
```bash
kubectl get events -A --sort-by='.lastTimestamp'
kubectl describe va -n <namespace>
kubectl logs -n workload-variant-autoscaler-system deployment/workload-variant-autoscaler-controller-manager
```

#### Metrics Not Available

**Symptoms**: External metrics API returns empty or error

**Possible causes**:
- Prometheus adapter not running
- Metrics not being scraped
- Incorrect metric labels or selectors

**Debugging steps**:
```bash
# Check external metrics API
kubectl get --raw "/apis/external.metrics.k8s.io/v1beta1/namespaces/<namespace>/wva_desired_replicas" | jq

# Check Prometheus
kubectl port-forward -n workload-variant-autoscaler-monitoring svc/prometheus-operated 9090:9090
# Query: wva_desired_replicas{variant_name="..."}
```

#### Deployment Not Scaling

**Symptoms**: HPA shows desired replicas but deployment doesn't scale

**Possible causes**:
- Resource constraints (CPU/memory/GPU)
- Node capacity exceeded
- PDB preventing scale-up
- Deployment controller issues

**Debugging steps**:
```bash
kubectl describe hpa -n <namespace>
kubectl describe deploy -n <namespace>
kubectl get events -n <namespace> --sort-by='.lastTimestamp'
kubectl top nodes
```

## Performance / Benchmarking

Performance and benchmarking scenarios (traffic generation, throughput/latency measurement, scale-up latency, etc.) are intentionally **out of scope** for `test/e2e/` so that e2e remains deterministic. Use the project’s dedicated benchmarking tooling/workflows instead.

## Test Coverage Goals

Current coverage targets:
- **Unit tests**: 70%+ code coverage
- **Integration tests**: All controller operations
- **E2E tests**: Critical user workflows

### Checking Coverage

```bash
# Generate coverage report
go test -coverprofile=coverage.out ./...

# View summary
go tool cover -func=coverage.out

# Generate HTML report
go tool cover -html=coverage.out -o coverage.html

# View in browser
open coverage.html  # macOS
xdg-open coverage.html  # Linux
```

## Contributing Tests

When contributing, please ensure:

1. ✅ **All new code has unit tests** - aim for 70%+ coverage
2. ✅ **Critical paths have integration tests** - especially controller logic
3. ✅ **New features have E2E tests** - validate end-to-end behavior
4. ✅ **Tests are documented** - explain what is being tested and why
5. ✅ **Tests follow naming conventions** - use descriptive names
6. ✅ **Tests clean up resources** - no resource leaks in tests
7. ✅ **Tests pass locally before pushing** - run `make test` and `make test-e2e-smoke` (or `make test-e2e-full`)

## Related Documentation

- [Development Guide](development.md) - Development environment setup
- [E2E Test Suite README](../../test/e2e/README.md) - Consolidated E2E tests (Kind, OpenShift, infra-only setup)
- [Contributing Guide](../../CONTRIBUTING.md) - Contribution guidelines
