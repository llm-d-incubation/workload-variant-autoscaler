# Installation Guide

This guide covers installing Workload-Variant-Autoscaler (WVA) on your Kubernetes cluster.

## Prerequisites

- Kubernetes v1.32.0 or later
- Helm 3.x
- kubectl configured to access your cluster
- Cluster admin privileges

## Installation Methods

### Option 1: Helm Installation
See the [Helm Installation](../../charts/workload-variant-autoscaler/README.md) for detailed instructions.

**Verify the installation:**
```bash
kubectl get pods -n workload-variant-autoscaler-system
```

### Option 2: Kustomize Installation

Using kustomize for more control:

```bash
# Install CRDs
make install

# Deploy the controller
make deploy IMG=quay.io/llm-d/workload-variant-autoscaler:latest
```

### Option 3: Local Development (Kind Emulator):
See the [Kind Emulator](../../deploy/kind-emulator/README.md) for detailed instructions.


## Configuration

### Helm Values

Key configuration options:

```yaml
# custom-values.yaml
image:
  repository: quay.io/llm-d/workload-variant-autoscaler
  tag: latest
  pullPolicy: IfNotPresent

resources:
  limits:
    cpu: 500m
    memory: 512Mi
  requests:
    cpu: 100m
    memory: 128Mi

# Enable Prometheus monitoring
prometheus:
  enabled: true
  servicemonitor:
    enabled: true

# Optional: Multi-controller isolation
# Set a unique identifier for this controller instance
# Useful for parallel testing or multi-tenant environments
# See docs/user-guide/multi-controller-isolation.md
wva:
  controllerInstance: ""  # Leave empty for single controller
```

### ConfigMaps

WVA uses ConfigMaps for cluster configuration:

- **Service Classes**: SLO definitions for different service tiers

See [Configuration Guide](configuration.md) for details.

## Integrating with HPA/KEDA

WVA can work with existing autoscalers:

**For HPA integration:**
See [HPA Integration Guide](../integrations/hpa-integration.md)

**For KEDA integration:**
See [KEDA Integration Guide](../integrations/keda-integration.md)

## Verifying Installation

1. **Check controller is running:**
   ```bash
   kubectl get deployment -n workload-variant-autoscaler-system
   ```

2. **Verify CRDs are installed:**
   ```bash
   kubectl get crd variantautoscalings.llmd.ai
   ```

3. **Check controller logs:**
   ```bash
   kubectl logs -n workload-variant-autoscaler-system \
     deployment/workload-variant-autoscaler-controller-manager
   ```

## Uninstallation

**Helm:**
```bash
helm uninstall workload-variant-autoscaler -n workload-variant-autoscaler-system
```

**Kustomize:**
```bash
make undeploy
make uninstall  # Remove CRDs
```

## Troubleshooting

### Common Issues

**Controller not starting:**
- Check if CRDs are installed: `kubectl get crd`
- Verify RBAC permissions
- Check controller logs for errors

**Metrics not appearing:**
- Ensure Prometheus ServiceMonitor is created
- Verify Prometheus has proper RBAC to scrape metrics
- Check network policies aren't blocking metrics endpoint

**See Also:**
- [Configuration Guide](configuration.md)
- [Troubleshooting Guide](troubleshooting.md) (coming soon)
- [Developer Guide](../developer-guide/development.md)

## Next Steps

- [Configure your first VariantAutoscaling resource](configuration.md)
- [Follow the Quick Start Demo](../tutorials/demo.md)
- [Set up integration with HPA](../integrations/hpa-integration.md)

