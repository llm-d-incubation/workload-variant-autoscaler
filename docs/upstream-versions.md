# Upstream Dependency Version Tracking

> This file is the source of truth for the [upstream dependency monitor](../.github/workflows/upstream-monitor.md) workflow.
> Add your project's key upstream dependencies below. The monitor runs daily and creates GitHub issues when breaking changes are detected.

## Dependencies

<!-- Add your tracked dependencies using the format below. Remove this comment when populated. -->

| Dependency | Current Pin | Pin Type | File Location | Upstream Repo |
|-----------|-------------|----------|---------------|---------------|
| **llm-d-inference-sim** | `v0.7.1` | image tag | `test/e2e/fixtures/model_service_builder.go` line 84, `test/utils/resources/llmdsim.go` lines 43,102 | llm-d/llm-d-inference-sim |
