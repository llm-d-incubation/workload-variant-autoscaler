package controller

import (
	"github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// BuildAllocationFromMetrics assembles an Allocation struct from raw optimizer metrics
// and Kubernetes resources. This delegates to utils.BuildAllocationFromMetrics.
func BuildAllocationFromMetrics(
	metrics interfaces.OptimizerMetrics,
	va *v1alpha1.VariantAutoscaling,
	scaleTarget scaletarget.ScaleTargetAccessor,
	acceleratorCost float64,
) (interfaces.Allocation, error) {
	return utils.BuildAllocationFromMetrics(metrics, va, scaleTarget, acceleratorCost)
}
