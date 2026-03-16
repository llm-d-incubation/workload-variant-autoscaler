package utils

import (
	"fmt"
	"strconv"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	appsv1 "k8s.io/api/apps/v1"
)

// BuildAllocationFromMetrics assembles an Allocation struct from raw optimizer metrics
// and Kubernetes resources. This is responsible for:
// - Converting raw metrics (seconds -> milliseconds, formatting strings)
// - Extracting K8s information (replicas, accelerator, cost calculation)
// - Assembling the final Allocation struct
//
// This function is placed in utils to avoid import cycles between collector and controller packages.
func BuildAllocationFromMetrics(
	metrics interfaces.OptimizerMetrics,
	va *v1alpha1.VariantAutoscaling,
	deployment appsv1.Deployment,
	acceleratorCost float64,
) (interfaces.Allocation, error) {
	// Extract K8s information
	// Number of replicas
	numReplicas := int(*deployment.Spec.Replicas)

	// Accelerator type - extract from deployment nodeSelector/nodeAffinity or VA labels
	acc := GetAcceleratorNameFromDeployment(va, &deployment)
	if acc == "" {
		return interfaces.Allocation{},
			fmt.Errorf("accelerator name not found in deployment nodeSelector/nodeAffinity or VA label %q for: %s", AcceleratorNameLabel, va.Name)
	}

	// Calculate variant cost
	// VariantCost removed from Status as it is duplicated from Spec (per-replica cost)
	// or derived (total cost).

	// Max batch size - TODO: collect value from server
	maxBatch := 256

	// Convert metrics and format values to meet CRD validation regex '^\\d+(\\.\\d+)?$'
	// Convert seconds to milliseconds for TTFT and ITL
	ttftMilliseconds := metrics.TTFTSeconds * 1000
	itlMilliseconds := metrics.ITLSeconds * 1000

	ttftAverageStr := strconv.FormatFloat(ttftMilliseconds, 'f', 2, 64)
	itlAverageStr := strconv.FormatFloat(itlMilliseconds, 'f', 2, 64)
	arrivalRateStr := strconv.FormatFloat(metrics.ArrivalRate, 'f', 2, 64)
	avgInputTokensStr := strconv.FormatFloat(metrics.AvgInputTokens, 'f', 2, 64)
	avgOutputTokensStr := strconv.FormatFloat(metrics.AvgOutputTokens, 'f', 2, 64)

	// Build Allocation struct
	allocation := interfaces.Allocation{
		Accelerator: acc,
		NumReplicas: numReplicas,
		MaxBatch:    maxBatch,
		// VariantCost removed from Status
		TTFTAverage: ttftAverageStr,
		ITLAverage:  itlAverageStr,
		Load: interfaces.LoadProfile{
			ArrivalRate:     arrivalRateStr,
			AvgInputTokens:  avgInputTokensStr,
			AvgOutputTokens: avgOutputTokensStr,
		},
	}

	return allocation, nil
}
