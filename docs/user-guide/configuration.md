# Configuration Guide

This guide explains how to configure Workload-Variant-Autoscaler for your workloads.

## VariantAutoscaling Resource

The `VariantAutoscaling` CR is the primary configuration interface for WVA.

### Basic Example

```yaml
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: llama-8b-autoscaler
  namespace: llm-inference
spec:
  # Model identifier (OpenAI API compatible name)
  modelID: "meta/llama-3.1-8b"

  # Unique variant identifier: modelID-accelerator-acceleratorCount
  variantID: "meta/llama-3.1-8b-A100-1"

  # Accelerator configuration
  accelerator: "A100"
  acceleratorCount: 1

  # SLO class reference (points to ConfigMap key)
  sloClassRef:
    name: serviceclass
    key: Premium

  # Variant performance profile
  variantProfile:
    maxBatchSize: 256
    perfParms:
      # Prefill parameters: ttft = gamma + delta * tokens * maxBatchSize
      prefillParms:
        gamma: "5.2"
        delta: "0.1"
      # Decode parameters: itl = alpha + beta * maxBatchSize
      decodeParms:
        alpha: "20.58"
        beta: "0.41"
```

### Complete Reference

For complete field documentation, see the [CRD Reference](crd-reference.md).

## ConfigMaps

WVA uses two ConfigMaps for cluster-wide configuration.

### Accelerator Unit Cost ConfigMap

Defines GPU pricing for cost optimization:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: accelerator-unitcost
  namespace: workload-variant-autoscaler-system
data:
  accelerators: |
    - name: A100
      type: NVIDIA-A100-PCIE-80GB
      cost: 40
      memSize: 81920
    - name: MI300X
      type: AMD-MI300X-192GB
      cost: 65
      memSize: 196608
    - name: H100
      type: NVIDIA-H100-80GB-HBM3
      cost: 80
      memSize: 81920
```

### Service Class ConfigMap

Defines SLO requirements for different service tiers:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: serviceclass
  namespace: workload-variant-autoscaler-system
data:
  serviceClasses: |
    - name: Premium
      model: meta/llama-3.1-8b
      priority: 1
      slo-itl: 24        # Time per output token (ms)
      slo-ttw: 500       # Time to first token (ms)
      
    - name: Standard
      model: meta/llama-3.1-8b
      priority: 5
      slo-itl: 50
      slo-ttw: 1000
      
    - name: Freemium
      model: meta/llama-3.1-8b
      priority: 10
      slo-itl: 100
      slo-ttw: 2000
```

## Configuration Options

### Required Spec Fields

- **modelID**: Identifier for your model (OpenAI API compatible name, e.g., "meta/llama-3.1-8b")
- **variantID**: Unique variant identifier in format `{modelID}-{accelerator}-{acceleratorCount}` (e.g., "meta/llama-3.1-8b-A100-1")
- **accelerator**: GPU type for this variant (e.g., "A100", "MI300X", "H100")
- **acceleratorCount**: Number of accelerators per replica (minimum: 1)
- **sloClassRef**: Reference to SLO configuration in ConfigMap
  - **name**: ConfigMap name (e.g., "serviceclass")
  - **key**: Key within ConfigMap matching your service tier (e.g., "Premium")
- **variantProfile**: Performance characteristics for this variant
  - **maxBatchSize**: Maximum batch size supported (must match vLLM server config)
  - **perfParms**: Performance parameters for latency prediction
    - **prefillParms**: TTFT equation parameters (gamma, delta)
    - **decodeParms**: ITL equation parameters (alpha, beta)

### Advanced Options

See [CRD Reference](crd-reference.md) for advanced configuration options.

## Best Practices

### Choosing Service Classes

- **Premium**: Latency-sensitive applications (chatbots, interactive AI)
- **Standard**: Moderate latency requirements (content generation)
- **Freemium**: Best-effort, cost-optimized (batch processing)

### Batch Size Tuning

Batch size affects throughput and latency performance:
- WVA **mirrors** the vLLM server's configured batch size (e.g., `--max-num-seqs`)
- Do not override `variantProfile.maxBatchSize` in VariantAutoscaling unless you also change the vLLM server configuration
- When tuning batch size, update **both** the vLLM server argument and the WVA VariantAutoscaling `variantProfile.maxBatchSize` field together
- Monitor SLO compliance after any batch size changes

## Monitoring Configuration

WVA exposes metrics for monitoring. See:
- [Prometheus Integration](../integrations/prometheus.md)
- [Custom Metrics](../integrations/prometheus.md#custom-metrics)

## Examples

More configuration examples in:
- [config/samples/](../../config/samples/)
- [Tutorials](../tutorials/)

## Troubleshooting Configuration

### Common Issues

**SLOs not being met:**
- Verify service class configuration matches workload
- Check if accelerator has sufficient capacity
- Review model parameter estimates (alpha, beta values)

**Cost too high:**
- Consider allowing accelerator flexibility (`keepAccelerator: false`)
- Review service class priorities
- Check if min replicas can be reduced

## Next Steps

- [Run the Quick Start Demo](../tutorials/demo.md)
- [Integrate with HPA](../integrations/hpa-integration.md)
- [Set up Prometheus monitoring](../integrations/prometheus.md)

