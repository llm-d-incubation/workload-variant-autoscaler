# Workload-Variant-Autoscaler (WVA)

[![Go Report Card](https://goreportcard.com/badge/github.com/llm-d/llm-d-workload-variant-autoscaler)](https://goreportcard.com/report/github.com/llm-d/llm-d-workload-variant-autoscaler)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)


The Workload Variant Autoscaler (WVA) is a Kubernetes-based global autoscaler for inference model servers serving LLMs. WVA works alongside the standard Kubernetes HPA and external autoscalers like KEDA to drive the scale subresource of inference deployments. The high-level details of the algorithms are documented [here](https://llm-d.ai/docs/architecture/advanced/autoscaling). It determines optimal replica counts for a given request traffic load by considering constraints such as GPU availability, energy budget, and performance budget (latency/throughput).

### What is a Variant?

WVA introduces the concept of **variants** — multiple model servers in an InferencePool that all serve the same base model but differ in hardware configuration (e.g., GPU type), serving configuration (e.g., tensor parallelism, max batch size, quantization), or both.

Use cases include:

- **P/D disaggregation**: prefill is one variant, decode is another — variant = role in a disaggregated pipeline.
- **[batch-gateway](https://github.com/llm-d-incubation/batch-gateway)**: variants distinguish batch vs. interactive workloads sharing the same pool.
- **Autoscaler**: a costed serving configuration the autoscaler chooses among.

## Key Features

- **Intelligent Autoscaling**: Optimizes replica count by observing the current state of the system
- **Cost Optimization**: Minimizes infrastructure costs by picking the correct accelerator variant

## Documentation

See the [architecture and autoscaling design](https://llm-d.ai/docs/architecture/advanced/autoscaling) docs for high-level algorithm details.

See the [docs](docs/README.md) directory for design docs, developer guide, and more.

## How It Works

**Prerequisites:** deploy llm-d infrastructure (model servers) and create an `HPA` or `KEDA` object targeting each deployment.

**WVA then:**

1. Continuously monitors request rates and server performance via Prometheus metrics
2. Capacity model obtains KV cache utilization and queue depth to determine desired replica counts
3. Actuator emits optimization metrics to Prometheus
4. External autoscaler (`HPA`/`KEDA`) reads the metrics and scales the deployment accordingly


## Example

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: llama-8b-autoscaler
  namespace: llm-inference
  annotations:
    llm-d.ai/managed: "true"  # Opt-in to WVA management
    llm-d.ai/variant-cost: "10.0"  # Optional, defaults to "10.0"
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: llama-8b
  # minReplicas: 0  # scale to zero - alpha feature
  maxReplicas: 2
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 60
      policies:
      - type: Pods
        value: 10
        periodSeconds: 15
    scaleDown:
      stabilizationWindowSeconds: 60
      policies:
      - type: Pods
        value: 10
        periodSeconds: 15
  metrics:
  - type: External
    external:
      metric:
        name: wva_desired_replicas
        selector:
          matchLabels:
            variant_name: llama-8b
            exported_namespace: llm-inference
      target:
        type: AverageValue
        averageValue: "1"
```


More examples in [config/samples/hpa/](config/samples/hpa/) and [config/samples/keda/](config/samples/keda/).

## Contributing

We welcome contributions! See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

Join the [llm-d autoscaling community meetings](https://llm-d.ai/slack) to get involved.

## License

Apache 2.0 - see [LICENSE](LICENSE) for details.

## Related Projects

- [llm-d infrastructure](https://github.com/llm-d/llm-d-infra)
- [llm-d main repository](https://github.com/llm-d/llm-d)

## References
- [WVA paper](https://arxiv.org/abs/2603.09730)
- [WVA use case doc](https://docs.google.com/document/d/1ZcMXO0x42qn4X5cu6efgMomYC4pKPwm6r7L79y1AQH4/edit?tab=t.0)
- [Saturation based design discussion](https://docs.google.com/document/d/1iGHqdxRUDpiKwtJFr5tMCKM7RF6fbTfZBL7BTn6UkwA/edit?tab=t.0#heading=h.mdte0lq44ul4)

