# KServe Integration with the Workload-Variant-Autoscaler (WVA)

## Overview

WVA creates and manages autoscaling sub-resources (HPA and KEDA ScaledObjects) for inference
workloads as part of its Helm chart deployment. KServe also creates and manages HPA objects as
part of its `LLMInferenceService` reconciliation for the same target Deployment.

**Two controllers owning autoscaling objects for the same Deployment leads to undefined and
conflicting scaling behavior.** This document explains how to safely integrate WVA with KServe
today, and the longer-term migration path.

---

## Integration Phases

### Phase 1 — Current (KServe owns HPA/KEDA)

In this phase, KServe is the authoritative owner of HPA and KEDA ScaledObjects. WVA is
deployed in **metrics-only mode**: it computes the desired replica count and exposes it via the
`wva_desired_replicas` Prometheus metric, but does **not** create any HPA or ScaledObject in the
cluster. KServe's HPA can consume `wva_desired_replicas` via the Prometheus Adapter external
metrics API.

**Helm configuration:**

```yaml
hpa:
  enabled: false   # WVA will NOT create an HPA — KServe owns it

keda:
  enabled: false   # WVA will NOT create a ScaledObject — KServe owns it (default)
```

Or via `helm install` / `helm upgrade`:

```bash
helm upgrade --install workload-variant-autoscaler ./charts/workload-variant-autoscaler \
  --set hpa.enabled=false \
  --set keda.enabled=false
```

**What WVA still does in this mode:**
- Watches `VariantAutoscaling` CRs and target Deployments.
- Runs the saturation/queueing-model optimization engine.
- Emits `wva_desired_replicas`, `wva_current_replicas`, and `wva_desired_ratio` metrics.
- Updates `VariantAutoscaling` status with the optimized allocation.

**What KServe does:**
- Creates and manages the HPA object (or ScaledObject) for the Deployment.
- Configures the HPA to consume `wva_desired_replicas` from the external metrics API.

---

### Phase 2 — Future (WVA owns HPA/KEDA end-to-end)

In the long term, KServe aims to offload autoscaling sub-resource management entirely to WVA.
Once KServe stops managing HPA/ScaledObject objects on its side, operators simply re-enable
WVA's HPA (or KEDA) creation — **no API changes required**.

```yaml
hpa:
  enabled: true    # WVA now owns the HPA
```

Or with KEDA:

```yaml
hpa:
  enabled: false   # Do NOT enable both simultaneously
keda:
  enabled: true    # WVA now owns the ScaledObject
  prometheusServerAddress: "https://prometheus.svc:9090"
```

> [!IMPORTANT]
> Never set both `hpa.enabled=true` and `keda.enabled=true` for the same target Deployment.
> The Helm chart will reject this combination with a validation error at render time.

---

## Configuration Reference

| Value | Default | Description |
|---|---|---|
| `hpa.enabled` | `true` | Set `false` to prevent WVA from creating an HPA. |
| `keda.enabled` | `false` | Set `true` to have WVA create a KEDA ScaledObject. |
| `keda.prometheusServerAddress` | `""` | Required when `keda.enabled=true`. |
| `keda.minReplicaCount` | `0` | Minimum replicas (0 = scale-to-zero). |
| `keda.maxReplicaCount` | `10` | Maximum replicas. |
| `keda.pollingInterval` | `15` | Metric polling interval (seconds). |
| `keda.cooldownPeriod` | `30` | Idle period before scaling to zero (seconds). |
| `keda.threshold` | `"1"` | Target metric value per replica. |
| `keda.activationThreshold` | `"0"` | Scale from zero when metric exceeds this. |
| `keda.unsafeSsl` | `"false"` | Skip TLS verification (dev only). |

---

## Conflict Prevention

The Helm chart enforces mutual exclusion at render time:

```
hpa.enabled=true  + keda.enabled=true  → helm fail (rejected)
hpa.enabled=false + keda.enabled=false → OK (external platform owns both)
hpa.enabled=true  + keda.enabled=false → OK (WVA owns HPA)
hpa.enabled=false + keda.enabled=true  → OK (WVA owns ScaledObject via KEDA)
```

---

## Further Reading

- [HPA Integration Guide](./hpa-integration.md)
- [KEDA Integration Guide](./keda-integration.md)
