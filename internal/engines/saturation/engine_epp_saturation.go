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
	"math"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/event"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	epp_saturation "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/epp_saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/common"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// eppSignalUnavailableMessage is the MetricsAvailable=False message used when the
// EPP latency detector's pool saturation signal cannot be queried.
const eppSignalUnavailableMessage = "EPP saturation signal unavailable (latency detector or predictor sidecar may be down)"

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
	// uncappedByModel holds the recommendation the EPP formula would make absent
	// the maxReplicas clamp, keyed by "namespace/modelID". Used post-optimize to
	// flag decisions that were capped (see RFC #1018 proposal #2).
	uncappedByModel := make(map[string]int)

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
			TTFTSLOMs:         saturationConfig.TTFTSLOMs,
			TPOTSLOMs:         saturationConfig.TPOTSLOMs,
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
			// Preserve the last desired replica count via the safety net, and
			// explicitly mark metrics unavailable so the VA surfaces an
			// EPP-specific MetricsAvailable=False condition rather than relying
			// on the implicit (no-decision) fallthrough (RFC #1018 proposal #4).
			e.emitSafetyNetMetrics(ctx, modelVAs, currentAllocations, scaleTargets)
			e.markEPPSignalUnavailable(modelVAs)
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

		// Emit raw vs smoothed saturation per variant so operators can observe the
		// EMA and tune smoothingAlpha. The pool-level signal applies to every
		// variant in the model.
		for i := range modelVAs {
			va := &modelVAs[i]
			e.metricsEmitter.RecordEPPSaturationMetrics(ctx, va.Name, va.Namespace, modelID,
				result.RawSignal, result.SmoothedSignal)
		}

		// Record what the EPP formula would recommend absent the maxReplicas clamp,
		// so we can flag capped decisions after the optimizer runs. Uses the same
		// closed form as the analyzer's scaling math: desired = ceil(S * N / T_up).
		//
		// This is a pool-level total. We only attribute it for single-variant model
		// groups, where the variant target equals the pool total; with multiple
		// variants the optimizer splits the total across them and a per-variant
		// maxReplicas comparison would misfire, so cap detection is skipped (the
		// gauge is still emitted as 0 below).
		if len(modelVAs) == 1 && saturationConfig.ScaleUpThreshold > 0 {
			totalReplicas := 0
			for _, vs := range variantStates {
				totalReplicas += vs.CurrentReplicas
			}
			if totalReplicas == 0 {
				totalReplicas = 1 // match the analyzer's floor so scale-up-from-zero is still flaggable
			}
			uncapped := int(math.Ceil(result.SmoothedSignal * float64(totalReplicas) / saturationConfig.ScaleUpThreshold))
			uncappedByModel[utils.GetNamespacedKey(namespace, modelID)] = uncapped
		} else if len(modelVAs) > 1 {
			logger.V(logging.DEBUG).Info("Skipping cap detection for multi-variant model group (pool total not attributable per variant)",
				"modelID", modelID, "variantCount", len(modelVAs))
		}

		requests = append(requests, pipeline.ModelScalingRequest{
			ModelID:   modelID,
			Namespace: namespace,
			AnalyzerResults: []pipeline.NamedAnalyzerResult{{
				// Optimizer's saturationEntry keys on SaturationAnalyzerName,
				// so the EPP saturation result occupies the saturation slot.
				Name:      interfaces.SaturationAnalyzerName,
				Result:    result,
				Score:     1.0, // EPP path: single analyzer, no per-entry score config
				Remaining: result.RequiredCapacity,
				Spare:     result.SpareCapacity,
			}},
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

	// Flag decisions whose recommendation was clamped to maxReplicas and emit the
	// wva_scale_capped gauge so operators can distinguish "fine at the cap" from
	// "wanted more but was blocked" (RFC #1018 proposal #2). A decision is capped
	// when the uncapped formula recommendation exceeds maxReplicas and the final
	// target is pinned at the cap.
	for i := range allDecisions {
		d := &allDecisions[i]
		uncapped, ok := uncappedByModel[utils.GetNamespacedKey(d.Namespace, d.ModelID)]
		capped := ok && d.MaxReplicas != nil && *d.MaxReplicas > 0 &&
			uncapped > *d.MaxReplicas && d.TargetReplicas >= *d.MaxReplicas
		if capped {
			d.ScalingCapped = true
			d.UncappedReplicas = uncapped
			logger.Info("Scaling recommendation capped by maxReplicas (EPP saturation)",
				"variant", d.VariantName, "modelID", d.ModelID,
				"uncappedReplicas", uncapped, "maxReplicas", *d.MaxReplicas)
		}
		e.metricsEmitter.RecordScaleCappedMetric(ctx, d.VariantName, d.Namespace, d.ModelID, capped)
	}

	return allDecisions
}

// markEPPSignalUnavailable pushes a MetricsAvailable=False decision into the
// shared cache for each non-synthetic VA in the model and triggers a reconcile,
// so the EPP signal-unavailable state is surfaced as a status condition with an
// EPP-specific reason. Mirrors the no-accelerator safety path in applySaturationDecisions.
func (e *Engine) markEPPSignalUnavailable(modelVAs []llmdVariantAutoscalingV1alpha1.VariantAutoscaling) {
	for i := range modelVAs {
		va := &modelVAs[i]
		if utils.IsSynthetic(va) {
			continue
		}
		common.DecisionCache.Set(va.Name, va.Namespace, interfaces.VariantDecision{
			VariantName:      va.Name,
			Namespace:        va.Namespace,
			MetricsAvailable: false,
			MetricsReason:    llmdVariantAutoscalingV1alpha1.ReasonPrometheusError,
			MetricsMessage:   eppSignalUnavailableMessage,
		})
		common.DecisionTrigger <- event.GenericEvent{Object: va}
	}
}
