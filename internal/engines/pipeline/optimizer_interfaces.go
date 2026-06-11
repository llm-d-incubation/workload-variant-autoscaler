package pipeline

import (
	"context"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
)

// NamedAnalyzerResult pairs an analyzer's name with its result and mutable
// working counters for the optimizer's allocation loop.
// It is the per-entry type of ModelScalingRequest.AnalyzerResults and is
// only used inside the engine→optimizer contract; it is not a general-purpose
// interfaces type.
//
// Remaining and Spare are initialised from Result.RequiredCapacity and
// Result.SpareCapacity by the engine (model scope) and decremented in place by
// applyAllocation as the optimizer allocates replicas.
// For disaggregated (P/D) models, the optimizer calls initRoleState
// to populate RoleSpare per role and initialize picker-local demand.
// The original Result values are never mutated.
type NamedAnalyzerResult struct {
	Name      string
	Result    *interfaces.AnalyzerResult
	Score     float64            // per-analyzer weight from AnalyzerScoreConfig; used for fair-share priority
	Remaining float64            // mutable remaining required capacity; P-scope for disaggregated, model-scope otherwise
	Spare     float64            // mutable remaining spare capacity; model-scope (non-disaggregated only)
	RoleSpare map[string]float64 // per-role mutable spare; set by initRoleState; nil for non-disaggregated
}

// ModelScalingRequest bundles the analyzer result with variant state for one model.
// The optimizer receives a slice of these — one per model — and produces decisions.
type ModelScalingRequest struct {
	ModelID         string
	Namespace       string
	AnalyzerResults []NamedAnalyzerResult // per-analyzer slice; saturation entry is always first
	VariantStates   []interfaces.VariantReplicaState
	Priority        float64 // Model priority (default 1.0)
	Disaggregated   bool    // true when model has prefill+decode variants
}

// ScalingOptimizer makes final scaling decisions for all models.
//
// Implementations:
//   - CostAwareOptimizer: processes each model independently, minimizes cost (unlimited mode)
//   - GreedyByScoreOptimizer: fair-shares GPUs across models (limited mode)
type ScalingOptimizer interface {
	// Name returns optimizer identifier for logging/metrics.
	Name() string

	// Optimize produces VariantDecisions from analyzer results and optional constraints.
	// constraints may be nil in unlimited mode.
	Optimize(ctx context.Context, requests []ModelScalingRequest, constraints []*ResourceConstraints) []interfaces.VariantDecision
}
