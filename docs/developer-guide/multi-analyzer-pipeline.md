# Multi-Analyzer Pipeline (developer reference)

> **Status: STUB.** This doc summarises the multi-analyzer pipeline at a
> high level and points to the design doc for detailed architecture,
> alternatives considered, and future direction. Full developer-guide
> content covering all three multi-analyzer PRs (registration, threshold,
> optimizer) will be added post-review.

The Workload Variant Autoscaler's scaling engine runs multiple **analyzers**
in series each cycle. Each analyzer consumes the same per-replica metrics
and produces an `*interfaces.AnalyzerResult` carrying per-variant capacity,
model-level totals, and (for P/D disaggregated models) per-role capacity.
The engine post-step calibrates `RequiredCapacity` / `SpareCapacity` at
every scope using a uniform threshold formula. The optimizer reads a
per-analyzer slice (`[]NamedAnalyzerResult`) and decides scaling actions
over it via shared free functions in `internal/engines/pipeline/`.

## Components

- **Registration** — `internal/engines/saturation/engine.go`:
  `RegisterAnalyzer(name, analyzer) error`. `cmd/main.go` registers external
  analyzers (e.g., throughput) before `StartOptimizeLoop`. Saturation V2 is
  pre-registered. Registry is snapshotted at Start; late registration
  returns an error.
- **Engine post-step** — `internal/engines/saturation/engine_v2.go`:
  `applyUniversalThreshold(*AnalyzerResult, scaleUp, scaleDown)` applies a
  pure formula `RC = max(0, TD/scaleUp − Anticipated)` /
  `SC = max(0, TS − TD/scaleDown)` at model scope and every role.
- **Aggregation helpers** — `internal/engines/aggregation/`:
  `SumTotalSupply`, `SumTotalAnticipatedSupply`, `SumTotalDemand`,
  `AggregateByRole` over `[]VariantCapacity`. Analyzer authors use these to
  populate per-scope `Total*` fields.
- **Optimizer slice flow** — `internal/engines/pipeline/`:
  `NamedAnalyzerResult` slice carries each analyzer's result + working
  scratch state for allocation. `CostAwareOptimizer` and
  `GreedyByScoreOptimizer` consume the slice via shared free functions
  (single-variant + paired + role-iterated helpers).

## Detailed design

For full architecture (per-variant canonical model; linearity invariant;
α coupling for P/D; paired scale-up + role-iterated scale-down;
alternatives considered including the rejected combine-in-engine
algorithm; future direction), see the design doc:

https://github.com/deanlorenz/llm-d-workload-variant-autoscaler/blob/plans/planning/multi-analyzer-design.md

This is the Type-1 design doc for the multi-analyzer mission. It will be
folded into this developer-guide post-review across all three multi-analyzer
PRs (#1225 registration, #1228 threshold, optimizer pending).
