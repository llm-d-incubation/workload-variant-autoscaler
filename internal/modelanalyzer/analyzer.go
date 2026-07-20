package modelanalyzer

import (
	"context"

	interfaces "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	llmdOptv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/variant"
	inferno "github.com/llm-d/llm-d-workload-variant-autoscaler/pkg/core"
)

// Performance analyzer of queueing models associated with variant servers
type ModelAnalyzer struct {
	// data about the inferencing system
	// (accelerators, models, service classes, servers, capacities, allocations)
	system *inferno.System
}

// Create a new instance of a model analyzer
func NewModelAnalyzer(system *inferno.System) *ModelAnalyzer {
	return &ModelAnalyzer{system: system}
}

// Analyze a particular variant and generate corresponding allocations that achieve SLOs for all accelerators, used by the optimizer
func (ma *ModelAnalyzer) AnalyzeModel(ctx context.Context,
	va llmdOptv1alpha1.VariantAutoscaling) *interfaces.ModelAnalyzeResponse {

	serverName := utils.FullName(va.Name, va.Namespace)
	if server, exists := ma.system.Servers()[serverName]; exists {
		server.Calculate(ma.system.Accelerators())
		return CreateModelAnalyzeResponseFromAllocations(server.AllAllocations())
	}
	return &interfaces.ModelAnalyzeResponse{}
}
