package saturation

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	queueingmodel "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/queueingmodel"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// optimizeQueueingModel runs the queueing model-based analysis path.
// Follows the same three-stage pattern as optimizeV2:
//  1. Collect ModelScalingRequests (metrics + analysis per model)
//  2. Call optimizer to produce VariantDecisions
//  3. Apply enforcer constraints per model
func (e *Engine) optimizeQueueingModel(
	ctx context.Context,
	modelGroups map[string][]llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	currentAllocations map[string]*interfaces.Allocation,
) []interfaces.VariantDecision {
	logger := ctrl.LoggerFrom(ctx)

	// update analyzer given current models
	currentModelKeys := make(map[string]bool, len(modelGroups))
	for _, modelVAs := range modelGroups {
		namespace := modelVAs[0].Namespace // there should be at least one VA in a model group
		modelID := modelVAs[0].Spec.ModelID
		currentModelKeys[queueingmodel.MakeModelKey(namespace, modelID)] = true
	}
	e.queueingModelAnalyzer.Update(currentModelKeys)

	// Stage 1: Collect ModelScalingRequests for all models
	var requests []pipeline.ModelScalingRequest

	for groupKey, modelVAs := range modelGroups {
		modelID := modelVAs[0].Spec.ModelID
		namespace := modelVAs[0].Namespace
		logger.Info("Processing model (queueing-model)",
			"modelID", modelID,
			"namespace", namespace,
			"variantCount", len(modelVAs),
			"groupKey", groupKey)

		data, err := e.prepareModelData(ctx, modelID, modelVAs, e.client)
		if err != nil {
			logger.Error(err, "Model data preparation failed", "modelID", modelID)
			e.emitSafetyNetMetrics(ctx, modelVAs, currentAllocations, nil)
			continue
		}
		if data == nil {
			logger.V(logging.DEBUG).Info("Skipping model: no metrics available", "modelID", modelID)
			continue
		}

		qmConfigMap := e.Config.QMAnalyzerConfigForNamespace(namespace)
		qConfig := buildQMConfig(qmConfigMap, namespace, modelID)

		result, err := e.runQueueingModelAnalysis(ctx, modelID, namespace,
			data.replicaMetrics, qConfig, data.variantStates)
		if err != nil {
			logger.Error(err, "Queueing model analysis failed", "modelID", modelID)
			e.emitSafetyNetMetrics(ctx, modelVAs, currentAllocations, data.scaleTargets)
			continue
		}

		requests = append(requests, pipeline.ModelScalingRequest{
			ModelID:       modelID,
			Namespace:     namespace,
			Result:        result,
			VariantStates: data.variantStates,
		})
	}

	if len(requests) == 0 {
		return nil
	}

	// Stage 2: Call optimizer
	allDecisions := e.optimizer.Optimize(ctx, requests, nil)

	logger.Info("Queueing model optimizer produced decisions",
		"optimizer", e.optimizer.Name(),
		"decisionCount", len(allDecisions),
		"modelCount", len(requests))

	// Stage 3: Apply enforcer per-model (directly on decisions)
	for _, req := range requests {
		scaleToZeroConfig := e.Config.ScaleToZeroConfigForNamespace(req.Namespace)

		scaledToZero := e.ScaleToZeroEnforcer.EnforcePolicyOnDecisions(
			ctx, req.ModelID, req.Namespace,
			allDecisions, scaleToZeroConfig, e.optimizer.Name(),
		)
		if scaledToZero {
			logger.Info("Scale-to-zero enforcement applied (queueing-model)",
				"modelID", req.ModelID)
		}
	}

	return allDecisions
}

// runQueueingModelAnalysis runs the queueing model analyzer for a single model
// and returns the raw AnalyzerResult.
func (e *Engine) runQueueingModelAnalysis(
	ctx context.Context,
	modelID, namespace string,
	replicaMetrics []interfaces.ReplicaMetrics,
	config *queueingmodel.QMConfig,
	variantStates []interfaces.VariantReplicaState,
) (*interfaces.AnalyzerResult, error) {
	logger := ctrl.LoggerFrom(ctx)

	input := interfaces.AnalyzerInput{
		ModelID:        modelID,
		Namespace:      namespace,
		ReplicaMetrics: replicaMetrics,
		VariantStates:  variantStates,
		Config:         config,
	}

	result, err := e.queueingModelAnalyzer.Analyze(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("queueing model analysis failed: %w", err)
	}

	logger.Info("Queueing model analysis completed",
		"modelID", modelID,
		"totalSupply", result.TotalSupply,
		"totalDemand", result.TotalDemand,
		"utilization", result.Utilization,
		"requiredCapacity", result.RequiredCapacity,
		"spareCapacity", result.SpareCapacity)

	return result, nil
}

// buildQMConfig creates a QMConfig for a specific model.
// It starts from the "default" entry in allConfigs, then applies any per-model
// override whose ModelID and Namespace match. Per-model entries can override
// sloMultiplier, tuningEnabled, and provide explicit SLO targets (targetTTFT/targetITL).
// Falls back to defaults when fields are zero/nil.
func buildQMConfig(
	allConfigs map[string]interfaces.QueueingModelScalingConfig,
	namespace, modelID string,
) *queueingmodel.QMConfig {
	cfg := &queueingmodel.QMConfig{
		TuningEnabled: true,
		SLOMultiplier: queueingmodel.DefaultSLOMultiplier,
	}

	// Apply "default" entry as base
	if defaultCfg, ok := allConfigs["default"]; ok {
		if defaultCfg.TuningEnabled != nil {
			cfg.TuningEnabled = *defaultCfg.TuningEnabled
		}
		if defaultCfg.SLOMultiplier > 1.0 {
			cfg.SLOMultiplier = defaultCfg.SLOMultiplier
		}
	}

	// Scan for a per-model override matching this model
	for key, entry := range allConfigs {
		if key == "default" {
			continue
		}
		if entry.ModelID != modelID || entry.Namespace != namespace {
			continue
		}

		// Override sloMultiplier and tuningEnabled from per-model entry
		if entry.SLOMultiplier > 1.0 {
			cfg.SLOMultiplier = entry.SLOMultiplier
		}
		if entry.TuningEnabled != nil {
			cfg.TuningEnabled = *entry.TuningEnabled
		}

		// Populate explicit SLO targets if both are set
		if entry.TargetTTFT > 0 && entry.TargetITL > 0 {
			modelKey := queueingmodel.MakeModelKey(namespace, modelID)
			cfg.SLOTargets = map[string]*queueingmodel.SLOTarget{
				modelKey: {
					TargetTTFT: entry.TargetTTFT,
					TargetITL:  entry.TargetITL,
				},
			}
		}
		break // only one per-model entry should match
	}

	return cfg
}
