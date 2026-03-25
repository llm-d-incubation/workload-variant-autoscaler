# Developer Guide

Guide for developers contributing to Workload-Variant-Autoscaler.

## Development Environment Setup

### Prerequisites

- Go 1.25.0+
- Docker 17.03+
- kubectl 1.32.0+
- Kind (for local testing)
- Make

### Initial Setup

1. **Clone the repository:**

   ```bash
   git clone https://github.com/llm-d/llm-d-workload-variant-autoscaler.git 
   cd llm-d-workload-variant-autoscaler
   ```

2. **Install dependencies:**

   ```bash
   go mod download
   ```

3. **Install development tools:**

   ```bash
   make setup-envtest
   make controller-gen
   make kustomize
   ```

## Project Structure

```bash
workload-variant-autoscaler/
├── api/v1alpha1/          # CRD definitions
├── cmd/                   # Main application entry points
├── config/                # Kubernetes manifests
│   ├── crd/              # CRD manifests
│   ├── rbac/             # RBAC configurations
│   ├── manager/          # Controller deployment
│   └── samples/          # Example resources
├── deploy/                # Deployment scripts
│   ├── kubernetes/       # K8s deployment
│   ├── openshift/        # OpenShift deployment
│   └── kind-emulator/    # Local Kind cluster with GPU emulation
├── docs/                  # Documentation
├── internal/              # Private application code
│   ├── actuator/         # Metric emission & scaling
│   ├── collector/        # Metrics collection
│   ├── config/           # Internal configuration
│   ├── constants/        # Application constants
│   ├── controller/       # Controller implementation
│   ├── datastore/        # Data storage abstractions
│   ├── discovery/        # Resource discovery
│   ├── engines/          # Scaling engines (saturation, scale-from-zero)
│   ├── indexers/         # Kubernetes indexers
│   ├── interfaces/       # Interface definitions
│   ├── logging/          # Logging utilities
│   ├── metrics/          # Metrics definitions
│   ├── modelanalyzer/    # Model analysis
│   ├── saturation/       # Saturation detection logic
│   └── utils/            # Utility functions
├── pkg/                   # Public libraries
│   ├── analyzer/         # Queue theory models
│   ├── solver/           # Optimization algorithms
│   ├── core/             # Core domain models
│   ├── config/           # Configuration structures
│   └── manager/          # Manager utilities
├── test/                  # Tests
│   ├── e2e/                  # E2E tests (consolidated suite: Kind, OpenShift)
│   └── utils/                 # Test utilities
└── charts/                # Helm charts
    └── workload-variant-autoscaler/
```

## Development Workflow

### Running Locally

#### Option 1: Outside the cluster

```bash
# Run the controller on your machine (connects to configured cluster)
make run
```

#### Option 2: In a Kind cluster

The recommended approach is the one-shot command that creates the cluster and deploys everything
in a single step, avoiding a CRD timing race (see [Known Setup Issues](#known-setup-issues)):

```bash
# Recommended: create cluster + deploy WVA + llm-d infrastructure in one step
CREATE_CLUSTER=true make deploy-wva-emulated-on-kind
```

Alternatively, as two separate steps:

```bash
# Step 1: Create a Kind cluster with emulated GPUs
make create-kind-cluster

# Or deploy with the full llm-d infrastructure
make deploy-wva-emulated-on-kind
```

> **Note:** If the two-step approach fails with `no matches for kind "InferencePool"`,
> see [Known Setup Issues](#known-setup-issues).

### Making Changes

1. **Create a feature branch:**

   ```bash
   git checkout -b feature/my-feature
   ```

2. **Make your changes**

3. **Generate code if needed:**

   ```bash
   # After modifying CRDs
   make manifests generate
   ```

4. **Run unit tests:**

   ```bash
   make test
   ```

5. **Run linter:**

   ```bash
   make lint
   ```

## Building and Testing

### Build the Binary

```bash
make build
```

The binary will be in `bin/manager`.

### Build Docker Image

```bash
make docker-build IMG=<your-registry>/wva-controller:tag
```

### Push Docker Image

```bash
make docker-push IMG=<your-registry>/wva-controller:tag
```

### Multi-architecture Build

```bash
PLATFORMS=linux/arm64,linux/amd64 make docker-buildx IMG=<your-registry>/wva-controller:tag
```

## Testing

### Unit Tests

```bash
# Run all unit tests
make test

# Run specific package tests
go test ./internal/controller/...

# With coverage
go test -cover ./...
```

### E2E Tests

WVA has a single consolidated E2E suite (`test/e2e/`) that runs on Kind (emulated) or OpenShift/kubernetes. Deploy infrastructure in infra-only mode first, then run tests.

**Location**: `test/e2e/`

```bash
# Smoke tests (Kind, ~5-10 min)
make test-e2e-smoke

# Full suite (Kind)
make test-e2e-full

# OpenShift: set KUBECONFIG and ENVIRONMENT=openshift first
export ENVIRONMENT=openshift
make test-e2e-smoke
# or make test-e2e-full

# Run specific tests
FOCUS="Basic VA lifecycle" make test-e2e-smoke
```

See [Testing Guide](testing.md) and [E2E Test Suite README](../../test/e2e/README.md) for infra-only setup and configuration. For OpenShift, set `ENVIRONMENT=openshift` and use the same targets.

### Manual Testing

1. **Deploy to Kind cluster:**

   ```bash
   make deploy-wva-emulated-on-kind IMG=<your-image>
   ```

2. **Create test resources:**

   ```bash
   kubectl apply -f config/samples/
   ```

3. **Monitor controller logs:**

   ```bash
   kubectl logs -n workload-variant-autoscaler-system \
     deployment/workload-variant-autoscaler-controller-manager -f
   ```

## Code Generation

### After Modifying CRDs

```bash
# Generate deepcopy, CRD manifests, and RBAC
make manifests generate
```

### Generate CRD Documentation

```bash
make crd-docs
```

Output will be in `docs/user-guide/crd-reference.md`.

## Debugging

### VSCode Launch Configuration

Create `.vscode/launch.json`:

```json
{
  "version": "0.2.0",
  "configurations": [
    {
      "name": "Debug Controller",
      "type": "go",
      "request": "launch",
      "mode": "auto",
      "program": "${workspaceFolder}/cmd/main.go",
      "env": {
        "KUBECONFIG": "${env:HOME}/.kube/config"
      },
      "args": []
    }
  ]
}
```

### Debugging in Cluster

```bash
# Build debug image
go build -gcflags="all=-N -l" -o bin/manager cmd/main.go

# Deploy and attach debugger (e.g., Delve)
```

### Viewing Controller Logs

```bash
kubectl logs -n workload-variant-autoscaler-system \
  -l control-plane=controller-manager --tail=100 -f
```

## Common Development Tasks

### Adding a New Field to CRD

1. Modify `api/v1alpha1/variantautoscaling_types.go`
2. Run `make manifests generate`
3. Update tests
4. Run `make crd-docs`
5. Update user documentation

### Adding a New Metric

1. Define metric in `internal/metrics/metrics.go`
2. Emit metric from appropriate controller location
3. Update Prometheus integration docs
4. Add to Grafana dashboards (if applicable)

### Modifying Optimization Logic

1. Update code in `pkg/solver/` or `pkg/analyzer/`
2. Add/update unit tests
3. Run `make test`
4. Update design documentation if algorithm changes

## Documentation

### Updating Documentation

After code changes, update relevant docs in:

- `docs/user-guide/` - User-facing changes
- `docs/design/` - Architecture/design changes
- `docs/integrations/` - Integration guide updates

**Note**: Documentation updates are partially automated via the [Update Docs workflow](agentic-workflows.md#update-docs). The workflow analyzes code changes and creates draft PRs with documentation updates.

### Testing Documentation

Verify all commands and examples in documentation work:

```bash
# Test installation steps
# Test configuration examples
# Test all code snippets
```

## GitHub Agentic Workflows

The repository uses AI-powered workflows to automate documentation updates, workflow creation, and debugging. These workflows are powered by the `gh-aw` CLI extension.

Key workflows:
- **Update Docs**: Automatically updates documentation on every push to main
- **Create Agentic Workflow**: Interactive workflow designer
- **Debug Agentic Workflow**: Workflow debugging assistant

See [Agentic Workflows Guide](agentic-workflows.md) for detailed information on working with these automation tools.

## Release Process

See the [Release Process](release-process.md) guide for how to cut a release. It covers:

- Pre-release checklist (changelog, optional version bumps, upstream pins)
- Creating the tag and GitHub Release (which triggers image build and Helm chart publish)
- What runs automatically: Docker image push, Helm chart version bump and publish to GHCR, and commit-back of chart files
- Post-release (required): update the llm-d [workload-autoscaling](https://github.com/llm-d/llm-d/tree/main/guides/workload-autoscaling) guide to the new WVA version
- Enabling other team members to perform releases (permissions, secrets, documentation)

## Getting Help

- Check [CONTRIBUTING.md](../../CONTRIBUTING.md)
- Review existing code and tests
- Ask in GitHub Discussions
- Attend community meetings

## Useful Commands

```bash
# Format code
make fmt

# Vet code
make vet

# Run linter
make lint

# Fix linting issues
make lint-fix

# Clean build artifacts
rm -rf bin/ dist/

# Reset Kind cluster
make destroy-kind-cluster
make create-kind-cluster
```

## Next Steps

- Review [Code Style Guidelines](../../CONTRIBUTING.md#coding-guidelines)
- Check out [Good First Issues](https://github.com/llm-d/llm-d-workload-variant-autoscaler/labels/good%20first%20issue)

---

## Known Setup Issues

### InferencePool CRD not found during `make deploy-wva-emulated-on-kind`

**Symptom:**
```
Error: no matches for kind "InferencePool" in version "inference.networking.x-k8s.io/v1alpha2"
ensure CRDs are installed first
```

**Cause:** When running `make create-kind-cluster` and `make deploy-wva-emulated-on-kind` as two
separate commands, there can be a timing race: the Gateway API Inference Extension CRDs are
applied by the deploy script but the Kubernetes API server hasn't finished registering them
before the helmfile tries to deploy the `InferencePool` resource.

**Fix (Option 1 — preferred):** Use the one-shot command, which gives the API server enough
time to register the CRDs during cluster startup:

```bash
CREATE_CLUSTER=true make deploy-wva-emulated-on-kind
```

**Fix (Option 2):** If you already have a running cluster and hit this error, install the CRDs
manually and re-run the deploy (it is idempotent):

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api-inference-extension/releases/download/v1.0.1/manifests.yaml
kubectl wait --for=condition=Established crd/inferencepools.inference.networking.x-k8s.io --timeout=30s
kubectl wait --for=condition=Established crd/inferencepools.inference.networking.k8s.io --timeout=30s
make deploy-wva-emulated-on-kind
```
