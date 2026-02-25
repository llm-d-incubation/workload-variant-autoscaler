package saturation

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// runV2AnalysisOnly runs the V2 saturation analyzer and returns the raw AnalyzerResult
// without building targets or converting to V1 types. The optimizer will handle
// target building across all models.
func (e *Engine) runV2AnalysisOnly(
	ctx context.Context,
	modelID, namespace string,
	replicaMetrics []interfaces.ReplicaMetrics,
	config interfaces.SaturationScalingConfig,
	variantStates []interfaces.VariantReplicaState,
	deployments map[string]*appsv1.Deployment,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
) (*interfaces.AnalyzerResult, error) {
	logger := ctrl.LoggerFrom(ctx)

	// 1. Pre-populate capacity store with deployment-derived params
	for _, va := range variantAutoscalings {
		deployKey := utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())
		deploy := deployments[deployKey]
		if deploy == nil {
			logger.V(logging.DEBUG).Info("No deployment found for VA, skipping capacity store pre-population",
				"variant", va.Name, "deployKey", deployKey)
			continue
		}
		accelerator := utils.GetAcceleratorType(va)
		gpuCount := getDeploymentGPUsPerReplica(deploy)
		e.capacityStore.LoadFromDeployment(namespace, modelID, va.Name, accelerator, gpuCount, deploy)
		logger.V(logging.DEBUG).Info("Pre-populated capacity store from deployment",
			"variant", va.Name, "accelerator", accelerator, "gpuCount", gpuCount)
	}

	// 2. Build AnalyzerInput
	input := interfaces.AnalyzerInput{
		ModelID:        modelID,
		Namespace:      namespace,
		ReplicaMetrics: replicaMetrics,
		VariantStates:  variantStates,
		Config:         &config,
		// TODO: populate SchedulerQueue when flow control metrics are collected
	}

	// 3. Run V2 analyzer
	result, err := e.saturationV2Analyzer.Analyze(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("V2 saturation analysis failed: %w", err)
	}

	logger.Info("V2 saturation analysis completed",
		"modelID", modelID,
		"totalSupply", result.TotalSupply,
		"totalDemand", result.TotalDemand,
		"utilization", result.Utilization,
		"requiredCapacity", result.RequiredCapacity,
		"spareCapacity", result.SpareCapacity)

	return result, nil
}

// computeCurrentGPUUsage iterates over model scaling requests to compute the
// current GPU usage per accelerator type. Used to provide current usage to
// the ConstraintProvider when building GPU constraints for the optimizer.
func computeCurrentGPUUsage(requests []pipeline.ModelScalingRequest) map[string]int {
	usage := make(map[string]int)
	for _, req := range requests {
		if req.Result == nil {
			continue
		}
		stateMap := make(map[string]interfaces.VariantReplicaState, len(req.VariantStates))
		for _, s := range req.VariantStates {
			stateMap[s.VariantName] = s
		}
		for _, vc := range req.Result.VariantCapacities {
			state := stateMap[vc.VariantName]
			gpusPerReplica := state.GPUsPerReplica
			if gpusPerReplica <= 0 {
				gpusPerReplica = 1
			}
			usage[vc.AcceleratorName] += state.CurrentReplicas * gpusPerReplica
		}
	}
	return usage
}

// collectV2ModelRequest performs V2 analysis for a single model and returns
// a ModelScalingRequest for the optimizer, or nil if analysis should be skipped.
func (e *Engine) collectV2ModelRequest(
	ctx context.Context,
	modelID, namespace string,
	replicaMetrics []interfaces.ReplicaMetrics,
	config interfaces.SaturationScalingConfig,
	variantStates []interfaces.VariantReplicaState,
	deployments map[string]*appsv1.Deployment,
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
) (*pipeline.ModelScalingRequest, error) {
	result, err := e.runV2AnalysisOnly(ctx, modelID, namespace, replicaMetrics, config,
		variantStates, deployments, variantAutoscalings)
	if err != nil {
		return nil, fmt.Errorf("collecting V2 model request for %s/%s: %w", namespace, modelID, err)
	}

	return &pipeline.ModelScalingRequest{
		ModelID:       modelID,
		Namespace:     namespace,
		Result:        result,
		VariantStates: variantStates,
	}, nil
}
