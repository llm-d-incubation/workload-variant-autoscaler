# Installation Guide

This guide covers installing Workload-Variant-Autoscaler (WVA) on your Kubernetes cluster.

## Prerequisites

- Kubernetes v1.32.0 or later with administrator access or namespace-level permissions
- Helm 3.x
- kubectl configured to access your cluster

## Installation Methods

### Option 1: Kustomize Installation (Recommended)

Using kustomize for more control:

```bash
# Install CRDs
make install

# Deploy the controller
make deploy IMG=quay.io/llm-d/llm-d-workload-variant-autoscaler:latest
```

### Option 2: Helm Installation (Deprecated)

See the [Helm Installation](../../charts/workload-variant-autoscaler/README.md) for detailed instructions.

**Verify the installation:**
```bash
kubectl get pods -n workload-variant-autoscaler-system
```

### Option 3: Local Development and Testing

See the [comprehensive deployment guide](../../deploy/README.md) for detailed instructions.

## Integrating with HPA/KEDA

WVA can work with existing autoscalers:

**For HPA integration:**
See [HPA Integration Guide](hpa-integration.md)

**For KEDA integration:**
See [KEDA Integration Guide](keda-integration.md)

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

**Kustomize:**
```bash
make undeploy
make uninstall  # Remove CRDs
```

**Helm:**
```bash
helm uninstall workload-variant-autoscaler -n workload-variant-autoscaler-system
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
- [Troubleshooting Guide](troubleshooting.md)
- [Developer Guide](../developer-guide/development.md)

## Next Steps

- [Configure your first VariantAutoscaling resource](configuration.md)
- [Set up integration with HPA](hpa-integration.md)
