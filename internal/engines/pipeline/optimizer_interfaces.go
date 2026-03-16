package pipeline

import (
	"context"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
)

// ModelScalingRequest bundles the analyzer result with variant state for one model.
// The optimizer receives a slice of these — one per model — and produces decisions.
type ModelScalingRequest struct {
	ModelID       string
	Namespace     string
	Result        *interfaces.AnalyzerResult
	VariantStates []interfaces.VariantReplicaState
	Priority      float64 // Model priority (default 1.0)
	Disaggregated bool    // true when model has prefill+decode variants
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
