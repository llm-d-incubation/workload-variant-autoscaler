# Workload-Variant-Autoscaler Documentation

Welcome to the WVA documentation! This directory contains comprehensive guides for users, developers, and operators.

## Documentation Structure

### User Guide

Getting started and using WVA:

- **[Installation Guide](user-guide/installation.md)** - Installing WVA on your cluster
- **[Configuration](user-guide/configuration.md)** - Configuring WVA for your workloads
- **[CRD Reference](user-guide/crd-reference.md)** - Complete API reference for VariantAutoscaling
- **[Multi-Controller Isolation](user-guide/multi-controller-isolation.md)** - Running multiple WVA controller instances
- **[LeaderWorkerSet Support](user-guide/LeaderWorkerSet-support.md)** - Supporting LeaderWorkerSets as scale targets

### Integrations

Integration with other systems:

- **[HPA Integration](user-guide/hpa-integration.md)** - Using WVA with Horizontal Pod Autoscaler
- **[KEDA Integration](user-guide/keda-integration.md)** - Using WVA with KEDA
- **[Prometheus Integration](integrations/prometheus.md)** - Custom metrics and monitoring

### Design & Architecture

Understanding how WVA works:

- **[Modeling & Optimization](design/modeling-optimization.md)** - Queue theory models and optimization algorithms
- **[Controller Behavior](design/controller-behavior.md)** - Event handling and reconciliation behavior
- **[Architecture Diagrams](design/diagrams/)** - System architecture and workflows

### Developer Guide

Contributing to WVA:

- **[Development Setup](developer-guide/development.md)** - Setting up your dev environment
- **[Testing](developer-guide/testing.md)** - Running tests and CI workflows
- **[Debugging](developer-guide/debugging.md)** - Debugging techniques and tools
- **[Contributing](../CONTRIBUTING.md)** - How to contribute to the project

## Quick Links

- [Main README](../README.md)
- [Kubernetes Deployment](../deploy/kubernetes/README.md)
- [OpenShift Deployment](../deploy/openshift/README.md)
- [Local Development with Kind Emulator](../deploy/kind-emulator/README.md)

## Additional Resources

- [Community Proposal](https://docs.google.com/document/d/1n6SAhloQaoSyF2k3EveIOerT-f97HuWXTLFm07xcvqk/edit)
- [llm-d Infrastructure](https://github.com/llm-d/llm-d-infra)
- [API Proposal](https://docs.google.com/document/d/1j2KRAT68_FYxq1iVzG0xVL-DHQhGVUZBqiM22Hd_0hc/edit)

## Need Help?

- Check the [Troubleshooting Guide](user-guide/troubleshooting.md)
- Open a [GitHub Issue](https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues)
- Join community meetings

---

**Note:** Documentation is continuously being improved. If you find errors or have suggestions, please open an issue or submit a PR!
