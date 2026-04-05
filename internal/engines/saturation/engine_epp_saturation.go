/*
Copyright 2025 The llm-d Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package saturation

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	epp_saturation "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/epp_saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// optimizeEPPSaturation runs the EPP saturation analyzer path.
// Unlike V1/V2, this does not collect per-replica metrics from vLLM pods.
// Instead, it queries the EPP's pre-computed pool saturation score and
// translates it into scaling decisions via the optimizer pipeline.
func (e *Engine) optimizeEPPSaturation(
	ctx context.Context,
	modelGroups map[string][]llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	currentAllocations map[string]*interfaces.Allocation,
) []interfaces.VariantDecision {
	logger := ctrl.LoggerFrom(ctx)

	var requests []pipeline.ModelScalingRequest

	for groupKey, modelVAs := range modelGroups {
		modelID := modelVAs[0].Spec.ModelID
		namespace := modelVAs[0].Namespace
		logger.Info("Processing model (EPP saturation)",
			"modelID", modelID,
			"namespace", namespace,
			"variantCount", len(modelVAs),
			"groupKey", groupKey)

		// Get saturation config for threshold values
		saturationConfigMap := e.Config.SaturationConfigForNamespace(namespace)
		if len(saturationConfigMap) == 0 {
			logger.Info("Saturation scaling config not loaded yet for namespace, skipping model",
				"namespace", namespace, "modelID", modelID)
			continue
		}
		saturationConfig := resolveSaturationConfig(saturationConfigMap, modelID, namespace)

		// Fetch scale targets and build variant states directly — skip per-replica
		// vLLM metrics collection since EPP saturation is a pool-level signal.
		scaleTargets := make(map[string]scaletarget.ScaleTargetAccessor)
		variantAccel := make(map[string]string, len(modelVAs))
		for i := range modelVAs {
			va := &modelVAs[i]
			scaleTarget, err := scaletarget.FetchScaleTarget(ctx, e.client, va.Name, va.Spec.ScaleTargetRef.Kind, va.GetScaleTargetName(), va.Namespace)
			if err != nil {
				logger.V(logging.DEBUG).Info("Could not get scale target for VA",
					"variant", va.Name, "error", err)
				continue
			}
			scaleTargets[utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())] = scaleTarget
			variantAccel[va.Name] = utils.GetAcceleratorNameFromScaleTarget(va, scaleTarget)
		}
		if len(scaleTargets) == 0 {
			logger.Info("Skipping model: no scale targets resolved", "modelID", modelID)
			e.emitSafetyNetMetrics(ctx, modelVAs, currentAllocations, nil)
			continue
		}
		variantStates := e.BuildVariantStates(ctx, modelVAs, scaleTargets, e.client)
		if len(variantStates) == 0 {
			logger.Info("Skipping model: no variant states built", "modelID", modelID)
			e.emitSafetyNetMetrics(ctx, modelVAs, currentAllocations, scaleTargets)
			continue
		}

		// Build EPP saturation config from the saturation config thresholds
		eppCfg := &epp_saturation.EPPSaturationConfig{
			ScaleUpThreshold:  saturationConfig.ScaleUpThreshold,
			ScaleDownBoundary: saturationConfig.ScaleDownBoundary,
			SmoothingAlpha:    saturationConfig.SmoothingAlpha,
		}
		eppCfg.ApplyDefaults()

		// Run the EPP saturation analyzer (queries Prometheus for pool saturation)
		input := interfaces.AnalyzerInput{
			ModelID:       modelID,
			Namespace:     namespace,
			Config:        eppCfg,
			VariantStates: variantStates,
		}

		result, err := e.eppSaturationAnalyzer.Analyze(ctx, input)
		if err != nil {
			logger.Error(err, "EPP saturation analysis failed", "modelID", modelID)
			e.emitSafetyNetMetrics(ctx, modelVAs, currentAllocations, scaleTargets)
			continue
		}

		// Fill in accelerator names on variant capacities (analyzer doesn't know them).
		for i := range result.VariantCapacities {
			if name, ok := variantAccel[result.VariantCapacities[i].VariantName]; ok {
				result.VariantCapacities[i].AcceleratorName = name
			}
		}

		logger.Info("EPP saturation analysis result",
			"modelID", modelID,
			"saturation", result.Utilization,
			"totalSupply", result.TotalSupply,
			"totalDemand", result.TotalDemand,
			"requiredCapacity", result.RequiredCapacity,
			"spareCapacity", result.SpareCapacity)

		requests = append(requests, pipeline.ModelScalingRequest{
			ModelID:       modelID,
			Namespace:     namespace,
			Result:        result,
			VariantStates: variantStates,
			Priority:      saturationConfig.Priority,
		})
	}

	if len(requests) == 0 {
		return nil
	}

	// Run optimizer (same pipeline as V2)
	var constraints []*pipeline.ResourceConstraints
	if _, ok := e.optimizer.(*pipeline.GreedyByScoreOptimizer); ok {
		currentUsage := computeCurrentGPUUsage(requests)
		if limiter, ok := e.GPULimiter.(*pipeline.DefaultLimiter); ok {
			constraint, err := limiter.ComputeConstraints(ctx, currentUsage)
			if err != nil {
				logger.Error(err, "Failed to compute GPU constraints, falling back to unlimited")
			} else {
				constraints = append(constraints, constraint)
			}
		}
	}
	allDecisions := e.optimizer.Optimize(ctx, requests, constraints)

	logger.Info("EPP saturation optimizer produced decisions",
		"optimizer", e.optimizer.Name(),
		"decisionCount", len(allDecisions),
		"modelCount", len(requests))

	// Apply enforcer per-model (scale-to-zero, min replicas)
	for _, req := range requests {
		if hasMinReplicasAboveZero(req.VariantStates) {
			logger.V(logging.DEBUG).Info("Skipping scale-to-zero enforcement (EPP saturation): variant has minReplicas > 0",
				"modelID", req.ModelID)
			continue
		}

		scaleToZeroConfig := e.Config.ScaleToZeroConfigForNamespace(req.Namespace)
		scaledToZero := e.ScaleToZeroEnforcer.EnforcePolicyOnDecisions(
			ctx, req.ModelID, req.Namespace,
			allDecisions, scaleToZeroConfig, e.optimizer.Name(),
		)
		if scaledToZero {
			logger.Info("Scale-to-zero enforcement applied (EPP saturation)",
				"modelID", req.ModelID)
		}
	}

	return allDecisions
}

