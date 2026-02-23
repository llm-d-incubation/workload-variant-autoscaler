# Workload-Variant-Autoscaler (WVA)

[![Go Report Card](https://goreportcard.com/badge/github.com/llm-d/llm-d-workload-variant-autoscaler)](https://goreportcard.com/report/github.com/llm-d/llm-d-workload-variant-autoscaler)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)


The Workload Variant Autoscaler (WVA) is a Kubernetes-based global autoscaler for inference model servers serving LLMs. WVA works alongside standard Kubernetes HPA autoscaler and external autoscalers like KEDA to scale the object supporting scale subresource. The high-level details of the algorithm are [here](https://github.com/llm-d/llm-d-workload-variant-autoscaler/blob/main/docs/saturation-scaling-config.md ). It determines optimal replica counts for given request traffic loads for inference servers by considering constraints such as GPU count (cluster resources), energy-budget and performance-budget (latency/throughput).
<!--
<![Architecture](docs/design/diagrams/inferno-WVA-design.png)>
-->
## Key Features

- **Intelligent Autoscaling**: Optimizes replica count by observing the current state of the system
- **Cost Optimization**: Minimizes infrastructure costs by picking the correct accelerator variant
<!-- 
- **Performance Modeling**: Uses queueing theory (M/M/1/k, M/G/1 models) for accurate latency and throughput prediction
- **Multi-Model Support**: Manages multiple models with different service classes and priorities -->

## Documentation

### User Guide
- [Installation Guide](docs/user-guide/installation.md)
- [Configuration](docs/user-guide/configuration.md)
- [CRD Reference](docs/user-guide/crd-reference.md)
- [Multi-Controller Isolation](docs/user-guide/multi-controller-isolation.md)

<!-- 

### Tutorials
- [Quick Start Demo](docs/tutorials/demo.md)
- [Parameter Estimation](docs/tutorials/parameter-estimation.md)
- [vLLM Server Setup](docs/tutorials/vllm-samples.md)
-->
### Integrations
- [HPA Integration](docs/integrations/hpa-integration.md)
- [KEDA Integration](docs/integrations/keda-integration.md)
- [Prometheus Metrics](docs/integrations/prometheus.md)

<!-- 

### Design & Architecture
- [Architecture Overview](docs/design/modeling-optimization.md)
- [Architecture Diagrams](docs/design/diagrams/) - Visual architecture and workflow diagrams
-->
<!-- 
### Developer Guide
- [Development Setup](docs/developer-guide/development.md)
- [Contributing](CONTRIBUTING.md)
-->

<!--

## Architecture

WVA consists of several key components:

- **Reconciler**: Kubernetes controller that manages VariantAutoscaling resources
- **Collector**: Gathers cluster state and vLLM server metrics
-->
<!-- 
- **Model Analyzer**: Performs per-model analysis using queueing theory
- **Optimizer**: Makes global scaling decisions across models
-->
<!-- 
- **Optimizer**: Capacity model provides saturation based scaling based on threshold
- **Actuator**: Emits metrics to Prometheus and updates deployment replicas
-->

<!-- 
For detailed architecture information, see the [design documentation](docs/design/modeling-optimization.md).
-->
## How It Works

1. Platform admin deploys llm-d infrastructure (including model servers) and waits for servers to warm up and start serving requests
2. Platform admin creates a `VariantAutoscaling` CR for the running deployment
3. WVA continuously monitors request rates and server performance via Prometheus metrics
<!-- 
4. Model Analyzer estimates latency and throughput using queueing models
5. Optimizer solves for minimal cost allocation meeting all SLOs
-->
4. Capacity model obtains KV cache utilization and queue depth of inference servers with slack capacity to determine replicas
5. Actuator emits optimization metrics to Prometheus and updates VariantAutoscaling status
6. External autoscaler (HPA/KEDA) reads the metrics and scales the deployment accordingly

**Important Notes**:
<!-- 
- Create the VariantAutoscaling CR **only after** your deployment is warmed up to avoid immediate scale-down
-->
- WVA handles the creation order gracefully - you can create the VA before or after the deployment
- If a deployment is deleted, the VA status is immediately updated to reflect the missing deployment
- When the deployment is recreated, the VA automatically resumes operation
- Configure HPA stabilization window (recommend 120s+) for gradual scaling behavior
- WVA updates the VA status with current and desired allocations every reconciliation cycle

## Example

```yaml
apiVersion: llmd.ai/v1alpha1
kind: VariantAutoscaling
metadata:
  name: llama-8b-autoscaler
  namespace: llm-inference
spec:
  scaleTargetRef:
    kind: Deployment
    name: llama-8b
  modelID: "meta/llama-3.1-8b"
  variantCost: "10.0"  # Optional, defaults to "10.0"
```

More examples in [config/samples/](config/samples/).

## Upgrading

### CRD Updates

**Important:** Helm does not automatically update CRDs during `helm upgrade`. When upgrading WVA to a new version with CRD changes, you must manually apply the updated CRDs first:

```bash
# Apply the latest CRDs before upgrading
kubectl apply -f charts/workload-variant-autoscaler/crds/

# Then upgrade the Helm release
helm upgrade workload-variant-autoscaler ./charts/workload-variant-autoscaler \
  --namespace workload-variant-autoscaler-system \
  [your-values...]
```

### Breaking Changes

#### v0.5.0 (upcoming)
- **VariantAutoscaling CRD**: Added `scaleTargetRef` field as **required**. v0.4.1 VariantAutoscaling resources without `scaleTargetRef` must be updated before upgrading:
  - **Impact on Scale-to-Zero**: VAs without `scaleTargetRef` will not scale to zero properly, even with HPAScaleToZero enabled and HPA `minReplicas: 0`, because the HPA cannot reference the target deployment.
  - **Migration**: Update existing VAs to include `scaleTargetRef`:
    ```yaml
    spec:
      scaleTargetRef:
        kind: Deployment
        name: <your-deployment-name>
    ```
  - **Validation**: After CRD update, VAs without `scaleTargetRef` will fail validation.

### Verifying CRD Version

To check if your cluster has the latest CRD schema:

```bash
# Check the CRD fields
kubectl get crd variantautoscalings.llmd.ai -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties}' | jq 'keys'
```

## Contributing

We welcome contributions! See the llm-d Contributing Guide for guidelines.

Join the [llm-d autoscaling community meetings](https://llm-d.ai/slack) to get involved.

## License

Apache 2.0 - see [LICENSE](LICENSE) for details.

## Related Projects

- [llm-d infrastructure](https://github.com/llm-d/llm-d-infra)
- [llm-d main repository](https://github.com/llm-d/llm-d)

## References

- [Saturation based design discussion](https://docs.google.com/document/d/1iGHqdxRUDpiKwtJFr5tMCKM7RF6fbTfZBL7BTn6UkwA/edit?tab=t.0#heading=h.mdte0lq44ul4)

---

For detailed documentation, visit the [docs](docs/) directory.
