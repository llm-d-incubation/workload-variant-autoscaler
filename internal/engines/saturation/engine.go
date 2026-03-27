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
	"fmt"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	actuator "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/actuator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/discovery"
	queueingmodel "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/queueingmodel"
	saturation_v2 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation_v2"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/common"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/executor"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/saturation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// resolveSaturationConfig resolves config for a model.
// Lookup: "{modelID}#{namespace}" → "default" → zero-value with defaults.
func resolveSaturationConfig(
	configMap map[string]config.SaturationScalingConfig,
	modelID, namespace string,
) config.SaturationScalingConfig {
	if cfg, ok := configMap[modelID+"#"+namespace]; ok {
		cfg.ApplyDefaults()
		return cfg
	}
	if cfg, ok := configMap["default"]; ok {
		cfg.ApplyDefaults()
		return cfg
	}
	cfg := config.SaturationScalingConfig{}
	cfg.ApplyDefaults()
	return cfg
}

type Engine struct {
	client   client.Client
	scheme   *runtime.Scheme
	executor executor.Executor

	Recorder record.EventRecorder
	Config   *config.Config // Unified configuration (injected from main.go)

	// ReplicaMetricsCollector is the collector for replica metrics using the source infrastructure
	ReplicaMetricsCollector *collector.ReplicaMetricsCollector

	// ScaleToZeroEnforcer applies scale-to-zero and minimum replica enforcement
	ScaleToZeroEnforcer *pipeline.Enforcer

	// GPULimiter constrains scaling decisions based on available GPU resources.
	// Only applied when EnableLimiter is true in the saturation config.
	GPULimiter pipeline.Limiter

	// metricsRegistry is used to access metrics sources for request count queries
	metricsRegistry *source.SourceRegistry

	// saturationV2Analyzer is the V2 token-based saturation analyzer (initialized once).
	saturationV2Analyzer *saturation_v2.SaturationAnalyzer

	// queueingModelAnalyzer is the queueing model-based analyzer (initialized once).
	// Selected via analyzerName: "queueing-model" in SaturationScalingConfig.
	queueingModelAnalyzer *queueingmodel.QueueingModelAnalyzer

	// capacityStore is shared with the V2 analyzer for caching capacity knowledge.
	capacityStore *saturation_v2.CapacityKnowledgeStore

	// optimizer is the V2 scaling optimizer that produces VariantDecisions from
	// AnalyzerResults. Selected per-cycle based on enableLimiter config:
	// CostAwareOptimizer (unlimited) or GreedyByScoreOptimizer (limited).
	optimizer pipeline.ScalingOptimizer
}

// NewEngine creates a new instance of the saturation engine.
// Config must be non-nil (validated in main.go before engine creation).
// Panics if cfg is nil to fail fast on programming errors.
func NewEngine(client client.Client, scheme *runtime.Scheme, recorder record.EventRecorder, metricsRegistry *source.SourceRegistry, cfg *config.Config) *Engine {
	if cfg == nil {
		panic("config is nil in NewEngine - this should not happen (validated in main.go before engine creation)")
	}
	promSource := metricsRegistry.Get("prometheus") // assume prometheus source is registered

	// Create request count function wrapper for scale-to-zero enforcer
	requestCountFunc := func(ctx context.Context, modelID, namespace string, retentionPeriod time.Duration) (float64, error) {
		return registration.CollectModelRequestCount(ctx, promSource, modelID, namespace, retentionPeriod)
	}

	// Create GPU limiter with TypeInventory and GreedyBySaturation algorithm
	gpuDiscovery := discovery.NewK8sWithGpuOperator(client)
	gpuInventory := pipeline.NewTypeInventoryWithUsage("cluster-gpu-inventory", gpuDiscovery)
	gpuAlgorithm := pipeline.NewGreedyBySaturation()
	gpuLimiter := pipeline.NewDefaultLimiter("gpu-limiter", gpuInventory, gpuAlgorithm)

	capacityStore := saturation_v2.NewCapacityKnowledgeStore()

	// Initialize with default optimizer. The actual optimizer is selected
	// per-cycle in optimize() based on dynamic config (enableLimiter flag
	// from ConfigMap), since config arrives after engine init.
	var scalingOptimizer pipeline.ScalingOptimizer = pipeline.NewCostAwareOptimizer()

	engine := Engine{
		client:                  client,
		scheme:                  scheme,
		Recorder:                recorder,
		Config:                  cfg,
		ReplicaMetricsCollector: collector.NewReplicaMetricsCollector(promSource, client),
		ScaleToZeroEnforcer:     pipeline.NewEnforcer(requestCountFunc),
		GPULimiter:              gpuLimiter,
		metricsRegistry:         metricsRegistry,
		saturationV2Analyzer:    saturation_v2.NewSaturationAnalyzer(capacityStore),
		queueingModelAnalyzer:   queueingmodel.NewQueueingModelAnalyzer(),
		capacityStore:           capacityStore,
		optimizer:               scalingOptimizer,
	}

	engine.executor = executor.NewPollingExecutor(executor.PollingConfig{
		Config: executor.Config{
			OptimizeFunc: engine.optimize,
		},
		Interval:     30 * time.Second,
		RetryBackoff: 100 * time.Millisecond,
	})

	// Register saturation queries in the metrics registry.
	// Both V1 (percentage-based) and V2 (token-based) analyzers share the same
	// base queries (kv_cache_usage, queue_length). V2-specific queries
	// (cache_config_info, avg_output_tokens, etc.) are registered but unused
	// when V1 is active — they're just query templates with no runtime cost.
	registration.RegisterSaturationQueries(metricsRegistry)

	// Register scale-to-zero queries in the metrics registry
	registration.RegisterScaleToZeroQueries(metricsRegistry)

	// Register queueing model queries (scheduler dispatch rate per endpoint).
	// These are collected alongside saturation metrics into the shared
	// ReplicaMetrics struct and used by the queueing model analyzer to
	// estimate per-replica arrival rate and model queue behavior.
	registration.RegisterQueueingModelQueries(metricsRegistry)

	return &engine
}

// StartOptimizeLoop starts the optimization loop for the saturation engine.
// It runs until the context is cancelled.
func (e *Engine) StartOptimizeLoop(ctx context.Context) {
	e.executor.Start(ctx)
}

// optimize performs the optimization logic.
func (e *Engine) optimize(ctx context.Context) error {
	logger := ctrl.LoggerFrom(ctx)

	// Get optimization interval from Config (already a time.Duration)
	interval := e.Config.OptimizationInterval()

	// Update the executor interval if changed
	// Note: simple polling executor might not support dynamic interval update easily without restart,
	// but here we just check it. The original code used RequeueAfter.
	// The PollingExecutor uses fixed interval.
	// TODO: Support dynamic interval in Executor if needed. For now, we log and proceed.
	if interval > 0 {
		// e.executor.SetInterval(interval) // If supported
		_ = interval
	}

	if e.Config.ScaleToZeroEnabled() {
		logger.Info("Scaling to zero is enabled")
	}

	activeVAs, _, err := utils.ActiveVariantAutoscaling(ctx, e.client)
	if err != nil {
		logger.Error(err, "Unable to get active variant autoscalings")
		return err
	}

	if len(activeVAs) == 0 {
		logger.Info("No active VariantAutoscalings found, skipping optimization")
		return nil
	}

	// Collected accelerator inventory (only in limited mode)
	if e.Config.LimitedModeEnabled() {
		inventory, err := collector.CollectInventoryK8S(ctx, e.client)
		if err != nil {
			logger.Error(err, "Failed to collect cluster inventory")
			// do not proceed to optimization if inventory collection fails in limited mode
			return err
		}
		// always print inventory until optimizer consumes it
		logger.Info("Collected cluster accelerator inventory (Limited Mode)", "inventory", inventory)
	}

	// Group VAs by model for per-model capacity analysis
	modelGroups := utils.GroupVariantAutoscalingByModel(activeVAs)
	logger.Info("Grouped VAs by model",
		"modelCount", len(modelGroups),
		"totalVAs", len(activeVAs))

	// Create VA lookup map for applySaturationDecisions (used to access VA status and update decisions)
	// Use namespace/vaName as key to avoid collisions when multiple namespaces have same VA name
	// Use slice index directly to avoid pointer-to-loop-variable bug
	vaMap := make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling, len(activeVAs))
	for i := range activeVAs {
		vaMap[utils.GetNamespacedKey(activeVAs[i].Namespace, activeVAs[i].Name)] = &activeVAs[i]
	}

	// Create map to store current allocations populated during metrics collection
	// Keyed by VariantAutoscaling Namespace/Name
	currentAllocations := make(map[string]*interfaces.Allocation)

	// Determine which analyzer to use.
	// Priority: queueing model ConfigMap (presence-based) > saturation config analyzerName.
	// If wva-queueing-model-config exists with a "default" entry, the queueing model
	// analyzer is active regardless of the saturation config's analyzerName field.
	qmConfigMap := e.Config.QMAnalyzerConfig()
	_, hasQMAnalyzerConfig := qmConfigMap["default"]

	// Read saturation config for fallback analyzer selection and limiter flag.
	globalSatCfgMap := e.Config.SaturationConfig()
	analyzerName := ""
	enableLimiter := false
	if cfg, ok := globalSatCfgMap["default"]; ok {
		cfg.ApplyDefaults()
		analyzerName = cfg.GetAnalyzerName()
		enableLimiter = cfg.EnableLimiter
	}

	// Queueing model ConfigMap takes priority over saturation analyzerName.
	if hasQMAnalyzerConfig {
		analyzerName = interfaces.QueueingModelAnalyzerName
	}

	// Select optimizer based on enableLimiter flag (both are stateless, safe to swap)
	// Applies to V2 and queueing-model paths which both use the optimizer pipeline.
	if analyzerName == "saturation" || analyzerName == interfaces.QueueingModelAnalyzerName {
		if enableLimiter {
			e.optimizer = pipeline.NewGreedyByScoreOptimizer()
		} else {
			e.optimizer = pipeline.NewCostAwareOptimizer()
		}
		logger.V(logging.DEBUG).Info("Optimizer selected", "analyzer", analyzerName, "optimizer", e.optimizer.Name(), "enableLimiter", enableLimiter)
	}

	var allDecisions []interfaces.VariantDecision

	// Each analyzer has a separate optimize path because they use fundamentally
	// different analysis types and target-building flows:
	//   - V1: saturation.Analyzer → ModelSaturationAnalysis → CalculateSaturationTargets → Enforcer → Limiter
	//   - V2 (saturation): saturation_v2.Analyzer → AnalyzerResult → Optimizer.Optimize → Enforcer bridge
	//   - Queueing model: QueueingModelAnalyzer → AnalyzerResult → Optimizer.Optimize → Enforcer bridge
	// V1 will be deprecated once V2 is fully validated.
	// Queueing model is activated by presence of wva-queueing-model-config ConfigMap.
	switch analyzerName {
	case interfaces.QueueingModelAnalyzerName:
		allDecisions = e.optimizeQueueingModel(ctx, modelGroups, currentAllocations)
	case "saturation":
		allDecisions = e.optimizeV2(ctx, modelGroups, currentAllocations)
	default:
		allDecisions = e.optimizeV1(ctx, modelGroups, currentAllocations)
	}

	// STEP 3: Apply decisions and update VA status
	// Always call applySaturationDecisions, even with empty decisions.
	// This function also updates VA.Status.CurrentAlloc with collected metrics
	// and emits HPA metrics, which must happen every reconciliation cycle.
	if len(allDecisions) > 0 {
		logger.Info("Applying scaling decisions",
			"totalDecisions", len(allDecisions))
	} else {
		logger.Info("No scaling decisions to apply, updating VA status with metrics")
	}
	if err := e.applySaturationDecisions(ctx, allDecisions, vaMap, currentAllocations); err != nil {
		logger.Error(err, "Failed to apply saturation decisions")
		return err
	}

	logger.Info("Optimization completed successfully",
		"mode", "saturation-only",
		"modelsProcessed", len(modelGroups),
		"decisionsApplied", len(allDecisions))

	return nil
}

// optimizeV1 runs the V1 percentage-based saturation analysis path (saturation-percentage-based).
// Processes each model independently: analyze → enforce → convert → limiter.
func (e *Engine) optimizeV1(
	ctx context.Context,
	modelGroups map[string][]llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	currentAllocations map[string]*interfaces.Allocation,
) []interfaces.VariantDecision {
	logger := ctrl.LoggerFrom(ctx)
	var allDecisions []interfaces.VariantDecision

	for groupKey, modelVAs := range modelGroups {
		modelID := modelVAs[0].Spec.ModelID
		namespace := modelVAs[0].Namespace
		logger.Info("Processing model (V1)",
			"modelID", modelID,
			"namespace", namespace,
			"variantCount", len(modelVAs),
			"groupKey", groupKey)

		// Get namespace-aware saturation config (namespace-local > global)
		saturationConfigMap := e.Config.SaturationConfigForNamespace(namespace)
		if len(saturationConfigMap) == 0 {
			logger.Info("Saturation scaling config not loaded yet for namespace, skipping model",
				"namespace", namespace,
				"modelID", modelID)
			continue
		}

		saturationConfig := resolveSaturationConfig(saturationConfigMap, modelID, namespace)
		saturationTargets, saturationAnalysis, data, err := e.RunSaturationAnalysis(ctx, modelID, modelVAs, saturationConfig, e.client)
		if err != nil {
			logger.Error(err, "Saturation analysis failed", "modelID", modelID)
			if data == nil {
				e.emitSafetyNetMetrics(ctx, modelVAs, currentAllocations, nil)
			} else {
				e.emitSafetyNetMetrics(ctx, modelVAs, currentAllocations, data.scaleTargets)
			}
			continue
		}

		var finalDecisions []interfaces.VariantDecision
		if saturationAnalysis != nil && data != nil {
			// Convert saturation targets to decisions first, then apply enforcer
			finalDecisions = e.convertSaturationTargetsToDecisions(ctx, saturationTargets, saturationAnalysis, data.variantStates)

			// Check if any variant has minReplicas > 0 — if so, skip scale-to-zero enforcement
			if !hasMinReplicasAboveZero(data.variantStates) {
				// Apply scale-to-zero enforcement on decisions
				scaleToZeroConfig := e.Config.ScaleToZeroConfigForNamespace(namespace)
				scaledToZero := e.ScaleToZeroEnforcer.EnforcePolicyOnDecisions(
					ctx, modelID, namespace,
					finalDecisions, scaleToZeroConfig, "v1-saturation",
				)
				if scaledToZero {
					logger.Info("Scale-to-zero enforcement applied",
						"modelID", modelID)
				}
			} else {
				logger.V(logging.DEBUG).Info("Skipping scale-to-zero enforcement: variant has minReplicas > 0",
					"modelID", modelID)
			}

			logger.Info("Saturation-only decisions made for model",
				"modelID", modelID,
				"decisionCount", len(finalDecisions))
			allDecisions = append(allDecisions, finalDecisions...)
		} else {
			logger.V(logging.DEBUG).Info("Skipping decision application for model: saturation analysis is nil (likely no metrics)",
				"modelID", modelID)
		}
	}

	// Apply GPU limiter if enabled
	// Note: Limiter uses global saturation config since it's applied globally to all decisions
	globalSaturationConfigMap := e.Config.SaturationConfig()
	var globalSaturationConfig config.SaturationScalingConfig
	if len(globalSaturationConfigMap) > 0 {
		if cfg, ok := globalSaturationConfigMap["default"]; ok {
			globalSaturationConfig = cfg
		}
	}
	if globalSaturationConfig.EnableLimiter && len(allDecisions) > 0 {
		logger.Info("Applying GPU limiter to scaling decisions",
			"decisionCount", len(allDecisions))

		decisionPtrs := make([]*interfaces.VariantDecision, len(allDecisions))
		for i := range allDecisions {
			decisionPtrs[i] = &allDecisions[i]
		}

		if err := e.GPULimiter.Limit(ctx, decisionPtrs); err != nil {
			logger.Error(err, "GPU limiter failed, proceeding with original decisions")
		} else {
			for _, d := range decisionPtrs {
				if d.WasLimited {
					logger.Info("Decision was limited by GPU availability",
						"variant", d.VariantName,
						"originalTarget", d.OriginalTargetReplicas,
						"limitedTarget", d.TargetReplicas,
						"limitedBy", d.LimitedBy)
				}
			}
		}
	}

	return allDecisions
}

// optimizeV2 runs the V2 token-based optimizer path (saturation-token-based).
// Collects AnalyzerResults for all models, calls the optimizer once, then applies enforcer per-model.
func (e *Engine) optimizeV2(
	ctx context.Context,
	modelGroups map[string][]llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	currentAllocations map[string]*interfaces.Allocation,
) []interfaces.VariantDecision {
	logger := ctrl.LoggerFrom(ctx)

	// Stage 1: Collect ModelScalingRequests for all models
	var requests []pipeline.ModelScalingRequest

	for groupKey, modelVAs := range modelGroups {
		modelID := modelVAs[0].Spec.ModelID
		namespace := modelVAs[0].Namespace
		logger.Info("Processing model (V2)",
			"modelID", modelID,
			"namespace", namespace,
			"variantCount", len(modelVAs),
			"groupKey", groupKey)

		// Get namespace-aware saturation config
		saturationConfigMap := e.Config.SaturationConfigForNamespace(namespace)
		if len(saturationConfigMap) == 0 {
			logger.Info("Saturation scaling config not loaded yet for namespace, skipping model",
				"namespace", namespace, "modelID", modelID)
			continue
		}
		saturationConfig := resolveSaturationConfig(saturationConfigMap, modelID, namespace)

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

		req, err := e.collectV2ModelRequest(ctx, modelID, namespace,
			data.replicaMetrics, saturationConfig, data.variantStates,
			data.scaleTargets, data.variantAutoscalings)
		if err != nil {
			logger.Error(err, "V2 analysis failed", "modelID", modelID)
			e.emitSafetyNetMetrics(ctx, modelVAs, currentAllocations, data.scaleTargets)
			continue
		}

		requests = append(requests, *req)
	}

	if len(requests) == 0 {
		return nil
	}

	// Stage 2: Compute GPU constraints and call optimizer
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

	logger.Info("V2 optimizer produced decisions",
		"optimizer", e.optimizer.Name(),
		"decisionCount", len(allDecisions),
		"modelCount", len(requests))

	// Stage 3: Apply enforcer per-model (directly on decisions)
	for _, req := range requests {
		// Skip scale-to-zero enforcement if any variant has minReplicas > 0
		if hasMinReplicasAboveZero(req.VariantStates) {
			logger.V(logging.DEBUG).Info("Skipping scale-to-zero enforcement (V2): variant has minReplicas > 0",
				"modelID", req.ModelID)
			continue
		}

		scaleToZeroConfig := e.Config.ScaleToZeroConfigForNamespace(req.Namespace)

		scaledToZero := e.ScaleToZeroEnforcer.EnforcePolicyOnDecisions(
			ctx, req.ModelID, req.Namespace,
			allDecisions, scaleToZeroConfig, e.optimizer.Name(),
		)
		if scaledToZero {
			logger.Info("Scale-to-zero enforcement applied (V2)",
				"modelID", req.ModelID)
		}
	}

	return allDecisions
}

// BuildVariantStates extracts current and desired replica counts from VAs for capacity analysis.
func (e *Engine) BuildVariantStates(
	ctx context.Context,
	vas []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
	k8sClient client.Client,
) []interfaces.VariantReplicaState {
	states := make([]interfaces.VariantReplicaState, 0, len(vas))

	for _, va := range vas {
		// Get current replicas using ScaleTargetRef
		var scaleTarget scaletarget.ScaleTargetAccessor
		var found bool

		// Try to look up in provided map first (optimization)
		if scaleTargets != nil {
			scaleTarget, found = scaleTargets[utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())]
		}

		if !found {
			// Fallback to API call
			var fetchedScaleTarget scaletarget.ScaleTargetAccessor
			var err error
			if fetchedScaleTarget, err = scaletarget.FetchScaleTarget(ctx, k8sClient, va.Name, va.Spec.ScaleTargetRef.Kind, va.GetScaleTargetName(), va.Namespace); err != nil {
				ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Could not get scale target for VA, skipping",
					"variant", va.Name,
					"error", err)
				continue
			}
			scaleTarget = fetchedScaleTarget
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("BuildVariantStates fallback lookup", "variant", va.Name, "scaleTargetName", va.GetScaleTargetName(), "specReplicas", scaleTarget.GetReplicas(), "statusReplicas", scaleTarget.GetStatusReplicas(), "readyReplicas", scaleTarget.GetStatusReadyReplicas())
		} else {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("BuildVariantStates map lookup", "variant", va.Name, "scaleTargetName", va.GetScaleTargetName(), "specReplicas", scaleTarget.GetReplicas(), "statusReplicas", scaleTarget.GetStatusReplicas(), "readyReplicas", scaleTarget.GetStatusReadyReplicas())
		}

		currentReplicas := int(scaleTarget.GetStatusReplicas())
		if currentReplicas == 0 && scaleTarget.GetReplicas() != nil {
			currentReplicas = int(*scaleTarget.GetReplicas())
		}

		// Calculate pending replicas (not yet ready)
		readyReplicas := int(scaleTarget.GetStatusReadyReplicas())
		pendingReplicas := currentReplicas - readyReplicas
		if pendingReplicas < 0 {
			// This indicates an unexpected state where readyReplicas exceeds currentReplicas.
			// Log at Info level since this inconsistency should be visible to operators.
			ctrl.LoggerFrom(ctx).Info("Unexpected state: readyReplicas exceeds currentReplicas, clamping pendingReplicas to 0",
				"variant", va.Name, "currentReplicas", currentReplicas, "readyReplicas", readyReplicas)
			pendingReplicas = 0
		}

		// Extract GPUs per replica from scale target's pod template
		gpusPerReplica := scaleTarget.GetTotalGPUsPerReplica()

		// Extract P/D role from scale target labels
		role := getRoleFromScaleTarget(scaleTarget)

		// Read min/max replica bounds from VA spec fields
		var minReplicas *int
		if va.Spec.MinReplicas != nil {
			v := int(*va.Spec.MinReplicas)
			minReplicas = &v
		}
		var maxReplicas *int
		if va.Spec.MaxReplicas > 0 {
			v := int(va.Spec.MaxReplicas)
			maxReplicas = &v
		}

		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("BuildVariantStates result", "variant", va.Name, "currentReplicas", currentReplicas, "readyReplicas", readyReplicas, "pendingReplicas", pendingReplicas, "gpusPerReplica", gpusPerReplica, "role", role, "minReplicas", minReplicas, "maxReplicas", maxReplicas)

		desiredReplicas := 0
		if va.Status.DesiredOptimizedAlloc.NumReplicas != nil {
			desiredReplicas = int(*va.Status.DesiredOptimizedAlloc.NumReplicas)
		}
		states = append(states, interfaces.VariantReplicaState{
			VariantName:     va.Name,
			CurrentReplicas: currentReplicas,
			DesiredReplicas: desiredReplicas,
			PendingReplicas: pendingReplicas,
			GPUsPerReplica:  gpusPerReplica,
			Role:            role,
			MinReplicas:     minReplicas,
			MaxReplicas:     maxReplicas,
		})
	}

	return states
}

// getRoleFromScaleTarget extracts the P/D role from a scale target's pod template labels.
// Returns "prefill", "decode", or "both" (default when no role label is present).
func getRoleFromScaleTarget(scaleTarget scaletarget.ScaleTargetAccessor) string {
	both := "both"
	if scaleTarget == nil {
		return both
	}
	podTemplateSpec := scaleTarget.GetLeaderPodTemplateSpec()
	if podTemplateSpec == nil {
		return both
	}
	labels := podTemplateSpec.Labels
	if labels == nil {
		return both
	}
	if val, ok := labels["llm-d.ai/role"]; ok {
		switch val {
		case "prefill":
			return "prefill"
		case "decode":
			return "decode"
		default:
			return "both"
		}
	}
	return both
}

// convertSaturationTargetsToDecisions converts saturation-only targets to VariantDecisions.
// Used when model-based optimizer is disabled (saturation-only mode).
func (e *Engine) convertSaturationTargetsToDecisions(
	ctx context.Context,
	saturationTargets map[string]int,
	saturationAnalysis *interfaces.ModelSaturationAnalysis,
	variantStates []interfaces.VariantReplicaState,
) []interfaces.VariantDecision {
	logger := ctrl.LoggerFrom(ctx)
	decisions := make([]interfaces.VariantDecision, 0, len(saturationTargets))

	// Build variant analysis map for quick lookup
	vaMap := make(map[string]*interfaces.VariantSaturationAnalysis)
	for i := range saturationAnalysis.VariantAnalyses {
		va := &saturationAnalysis.VariantAnalyses[i]
		vaMap[va.VariantName] = va
	}

	// Build state map for quick lookup
	stateMap := make(map[string]interfaces.VariantReplicaState)
	for _, state := range variantStates {
		stateMap[state.VariantName] = state
	}

	for variantName, targetReplicas := range saturationTargets {
		state := stateMap[variantName]
		va := vaMap[variantName]

		var action interfaces.SaturationAction
		if targetReplicas > state.CurrentReplicas {
			action = interfaces.ActionScaleUp
		} else if targetReplicas < state.CurrentReplicas {
			action = interfaces.ActionScaleDown
		} else {
			action = interfaces.ActionNoChange
		}

		// Use GPUsPerReplica from variant state (extracted from scale target)
		gpusPerReplica := state.GPUsPerReplica
		if gpusPerReplica <= 0 {
			gpusPerReplica = 1 // Fallback default
		}

		decision := interfaces.VariantDecision{
			VariantName:            variantName,
			Namespace:              saturationAnalysis.Namespace,
			ModelID:                saturationAnalysis.ModelID,
			CurrentReplicas:        state.CurrentReplicas,
			TargetReplicas:         targetReplicas,
			OriginalTargetReplicas: targetReplicas, // Store original before limiter modifies it
			DesiredReplicas:        state.DesiredReplicas,
			Action:                 action,
			SaturationBased:        true,
			SaturationOnly:         true,
			ModelBasedDecision:     false,
			SafetyOverride:         false,
			Reason:                 "saturation-only mode: " + string(action),
			GPUsPerReplica:         gpusPerReplica,
			MinReplicas:            state.MinReplicas,
			MaxReplicas:            state.MaxReplicas,
		}

		if va != nil {
			decision.AcceleratorName = va.AcceleratorName
			decision.Cost = va.Cost
			// Use average spare KV capacity as the SpareCapacity indicator for limiter prioritization
			decision.SpareCapacity = va.AvgSpareKvCapacity
		} else {
			logger.Info("No variant analysis found for decision (metrics may be unavailable)",
				"variant", variantName)
		}

		decisions = append(decisions, decision)
	}

	return decisions
}

// hasMinReplicasAboveZero returns true if any variant in the states has MinReplicas > 0.
func hasMinReplicasAboveZero(states []interfaces.VariantReplicaState) bool {
	for _, state := range states {
		if state.MinReplicas != nil && *state.MinReplicas > 0 {
			return true
		}
	}
	return false
}

// modelData holds the pre-processed data for a model, shared between V1 and V2 paths.
type modelData struct {
	modelID             string
	namespace           string
	replicaMetrics      []interfaces.ReplicaMetrics
	scaleTargets        map[string]scaletarget.ScaleTargetAccessor
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling
	variantCosts        map[string]float64
	variantStates       []interfaces.VariantReplicaState
}

// prepareModelData collects metrics and builds lookup maps for a model's VAs.
// This is shared by both V1 and V2 paths.
// Also shared by the Queueing Model Analyzer engine.
// Returns nil modelData (not error) when no metrics are available — caller should skip the model.
func (e *Engine) prepareModelData(
	ctx context.Context,
	modelID string,
	modelVAs []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	k8sClient client.Client,
) (*modelData, error) {
	if len(modelVAs) == 0 {
		return nil, fmt.Errorf("no VAs provided for model %s", modelID)
	}

	logger := ctrl.LoggerFrom(ctx)
	namespace := modelVAs[0].Namespace

	variantCosts := make(map[string]float64)
	scaleTargets := make(map[string]scaletarget.ScaleTargetAccessor)
	variantAutoscalings := make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling)

	for i := range modelVAs {
		va := &modelVAs[i]
		scaleTarget, err := scaletarget.FetchScaleTarget(ctx, k8sClient, va.Name, va.Spec.ScaleTargetRef.Kind, va.GetScaleTargetName(), va.Namespace)
		if err != nil {
			logger.V(logging.DEBUG).Info("Could not get scale target for VA",
				"variant", va.Name,
				"scaleTarget", va.GetScaleTargetName(),
				"error", err)
			continue
		}

		cost := saturation.DefaultVariantCost
		if va.Spec.VariantCost != "" {
			if parsedCost, err := strconv.ParseFloat(va.Spec.VariantCost, 64); err == nil {
				cost = parsedCost
			} else {
				logger.V(logging.DEBUG).Info("Failed to parse variant cost, using default",
					"variant", va.Name, "variantCost", va.Spec.VariantCost, "default", cost, "error", err)
			}
		}

		key := utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())
		scaleTargets[key] = scaleTarget

		variantKey := utils.GetNamespacedKey(va.Namespace, va.Name)
		variantAutoscalings[variantKey] = va
		variantCosts[variantKey] = cost
	}

	logger.V(logging.DEBUG).Info("Using source infrastructure for replica metrics",
		"modelID", modelID,
		"namespace", namespace)
	replicaMetrics, err := e.ReplicaMetricsCollector.CollectReplicaMetrics(ctx, modelID, namespace, scaleTargets, variantAutoscalings, variantCosts)
	if err != nil {
		return nil, fmt.Errorf("failed to collect Saturation metrics for model %s: %w", modelID, err)
	}

	logger.V(logging.DEBUG).Info("Collected saturation metrics",
		"modelID", modelID,
		"namespace", namespace,
		"metricsCount", len(replicaMetrics))

	if len(replicaMetrics) == 0 {
		logger.Info("No saturation metrics available for model, skipping analysis",
			"modelID", modelID,
			"namespace", namespace)
		return nil, nil // nil modelData signals skip
	}

	variantStates := e.BuildVariantStates(ctx, modelVAs, scaleTargets, k8sClient)

	return &modelData{
		modelID:             modelID,
		namespace:           namespace,
		replicaMetrics:      replicaMetrics,
		scaleTargets:        scaleTargets,
		variantAutoscalings: variantAutoscalings,
		variantCosts:        variantCosts,
		variantStates:       variantStates,
	}, nil
}

// RunSaturationAnalysis performs V1 saturation analysis for a model and returns targets.
// This is the V1 path only — V2 uses the optimizer flow in optimize().
func (e *Engine) RunSaturationAnalysis(
	ctx context.Context,
	modelID string,
	modelVAs []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	SaturationConfig config.SaturationScalingConfig,
	k8sClient client.Client,
) (map[string]int, *interfaces.ModelSaturationAnalysis, *modelData, error) {
	logger := ctrl.LoggerFrom(ctx)

	SaturationConfig.ApplyDefaults()

	data, err := e.prepareModelData(ctx, modelID, modelVAs, k8sClient)
	if err != nil {
		return nil, nil, nil, err
	}
	if data == nil {
		return nil, nil, nil, nil // No metrics available
	}

	saturationAnalyzer := saturation.NewAnalyzer()
	saturationAnalysis, err := saturationAnalyzer.AnalyzeModelSaturation(ctx, modelID, data.namespace, data.replicaMetrics, SaturationConfig)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("failed to analyze Saturation for model %s: %w", modelID, err)
	}

	logger.Info("Saturation analysis completed",
		"modelID", modelID,
		"totalReplicas", saturationAnalysis.TotalReplicas,
		"nonSaturated", saturationAnalysis.NonSaturatedCount,
		"avgSpareKv", saturationAnalysis.AvgSpareKvCapacity,
		"avgSpareQueue", saturationAnalysis.AvgSpareQueueLength,
		"shouldScaleUp", saturationAnalysis.ShouldScaleUp,
		"scaleUpReason", saturationAnalysis.ScaleUpReason,
		"scaleDownSafe", saturationAnalysis.ScaleDownSafe)

	saturationTargets := saturationAnalyzer.CalculateSaturationTargets(ctx, saturationAnalysis, data.variantStates)

	logger.V(logging.DEBUG).Info("Saturation targets calculated",
		"modelID", modelID,
		"targets", saturationTargets)

	return saturationTargets, saturationAnalysis, data, nil
}

// applySaturationDecisions updates VA status and emits metrics based on Saturation decisions.
func (e *Engine) applySaturationDecisions(
	ctx context.Context,
	decisions []interfaces.VariantDecision,
	vaMap map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	currentAllocations map[string]*interfaces.Allocation,
) error {
	logger := ctrl.LoggerFrom(ctx)
	// Create a map of decisions for O(1) lookup
	// Use namespace/variantName as key to match vaMap and avoid collisions
	decisionMap := make(map[string]interfaces.VariantDecision)
	for _, d := range decisions {
		decisionMap[utils.GetNamespacedKey(d.Namespace, d.VariantName)] = d
	}

	// Iterate over ALL active VAs to ensure we update status and trigger reconciliation for everyone
	for vaName, va := range vaMap {
		decision, hasDecision := decisionMap[vaName]

		if hasDecision {
			logger.Info("Processing decision for VA",
				"variant", vaName,
				"action", decision.Action,
				"current", decision.CurrentReplicas,
				"target", decision.TargetReplicas)
		} else {
			logger.V(logging.DEBUG).Info("No scaling decision for VA, but updating status to trigger reconcile",
				"variant", vaName)
		}

		// Fetch latest version from API server to avoid conflicts
		var updateVa llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		if err := utils.GetVariantAutoscalingWithBackoff(ctx, e.client, va.Name, va.Namespace, &updateVa); err != nil {
			logger.Error(err, "Failed to get latest VA from API server",
				"name", va.Name)
			continue
		}

		// Update CurrentAlloc from local analysis (which has the latest metrics)
		// We use currentAllocations map instead of Status.CurrentAlloc
		if currentAlloc, ok := currentAllocations[vaName]; ok {
			// If we have a decision, attach current alloc to it for cache
			// If we have a decision, attach current alloc to it for cache
			// (Future logic if needed)
			_ = currentAlloc // Used for something?
			// Previously we updated va.Status.CurrentAlloc = currentAlloc
			// Now we just don't update status with it.
		}

		// Check if we have metrics data for this VA (used for cache below)
		_, hasAllocation := currentAllocations[vaName]

		// Determine target replicas and accelerator
		var targetReplicas int
		var acceleratorName string
		var reason string

		if hasDecision {
			targetReplicas = decision.TargetReplicas
			acceleratorName = decision.AcceleratorName
			reason = decision.Reason
		} else {
			// No change/decision: Keep current target or default to current replicas
			// We effectively explicitly "decide" to keep things as they are if no decision was made
			if updateVa.Status.DesiredOptimizedAlloc.NumReplicas != nil && *updateVa.Status.DesiredOptimizedAlloc.NumReplicas > 0 {
				targetReplicas = int(*updateVa.Status.DesiredOptimizedAlloc.NumReplicas)
			} else if curr, ok := currentAllocations[vaName]; ok {
				targetReplicas = curr.NumReplicas
			}
			// Keep existing accelerator or use current
			if updateVa.Status.DesiredOptimizedAlloc.Accelerator != "" {
				acceleratorName = updateVa.Status.DesiredOptimizedAlloc.Accelerator
			} else if curr, ok := currentAllocations[vaName]; ok {
				acceleratorName = curr.Accelerator
			}

			// Fallback for new VAs without prior status or collected metrics:
			// resolve accelerator from deployment nodeSelector/nodeAffinity or VA label,
			// and use current deployment replicas as target to avoid unintended scaling.
			if acceleratorName == "" {
				scaleTargetName := updateVa.GetScaleTargetName()
				if scaleTargetName != "" {
					var scaleTarget scaletarget.ScaleTargetAccessor
					var err error
					if scaleTarget, err = scaletarget.FetchScaleTarget(ctx, e.client, va.Name, va.Spec.ScaleTargetRef.Kind, scaleTargetName, va.Namespace); err == nil {
						acceleratorName = utils.GetAcceleratorNameFromScaleTarget(&updateVa, scaleTarget)
						if targetReplicas == 0 && scaleTarget.GetReplicas() != nil {
							targetReplicas = int(*scaleTarget.GetReplicas())
						}
					} else {
						// If scaleTarget fetch fails, try VA label directly
						acceleratorName = utils.GetAcceleratorNameFromScaleTarget(&updateVa, nil)
					}
				}
			}

			reason = "No scaling decision (optimization loop)"
		}

		// If we still don't have an accelerator name (e.g. new VA, no decision, no current alloc), we can't update status sensibly
		// But we still need to set MetricsAvailable condition via the cache
		if acceleratorName == "" {
			logger.Info("Skipping status update for VA without accelerator info, but setting MetricsAvailable=False",
				"variant", vaName, "cacheKey.name", va.Name, "cacheKey.namespace", va.Namespace)
			// Still set the cache entry so the controller can set MetricsAvailable=False.
			// This is a partial decision for metrics status only - other fields like
			// TargetReplicas and AcceleratorName are left at zero values since we don't
			// have enough information to set them.
			common.DecisionCache.Set(va.Name, va.Namespace, interfaces.VariantDecision{
				VariantName:      vaName,
				Namespace:        va.Namespace,
				MetricsAvailable: false,
				MetricsReason:    llmdVariantAutoscalingV1alpha1.ReasonMetricsMissing,
				MetricsMessage:   llmdVariantAutoscalingV1alpha1.MessageMetricsUnavailable,
			})
			// Trigger reconciler to apply the condition
			common.DecisionTrigger <- event.GenericEvent{
				Object: &updateVa,
			}
			continue
		}

		// Update DesiredOptimizedAlloc
		// ALWAYS update LastRunTime to trigger reconciliation in the controller
		numReplicas := int32(targetReplicas)
		updateVa.Status.DesiredOptimizedAlloc = llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
			NumReplicas: &numReplicas,
			Accelerator: acceleratorName,
			LastRunTime: metav1.Now(),
		}
		updateVa.Status.Actuation.Applied = false // Reset applied status until Actuator handles it (if needed)

		// Set condition based on decision characteristics (or lack thereof)
		if hasDecision {
			if decision.SafetyOverride {
				llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
					llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
					metav1.ConditionTrue,
					"SaturationSafetyOverride",
					fmt.Sprintf("saturation safety override: %s", reason))
			} else if decision.SaturationOnly {
				llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
					llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
					metav1.ConditionTrue,
					"SaturationOnlyMode",
					fmt.Sprintf("saturation-only decision: %s (target: %d replicas)", reason, targetReplicas))
			} else {
				llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
					llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
					metav1.ConditionTrue,
					llmdVariantAutoscalingV1alpha1.ReasonOptimizationSucceeded,
					fmt.Sprintf("Hybrid mode: %s (target: %d replicas)", reason, targetReplicas))
			}
		} else {
			// No active decision (just refreshing)
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVa,
				llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
				metav1.ConditionTrue,
				llmdVariantAutoscalingV1alpha1.ReasonOptimizationSucceeded,
				"Optimization loop ran (no scaling change needed)")
		}

		// Emit metrics for external autoscalers (Important: Actuator emits these)
		// We should emit metrics even if no decision changed, to keep HPA alive
		act := actuator.NewActuator(e.client)
		/*
		   NOTE: emitSafetyNetMetrics handles cases where optimization FAILS.
		   Here we are in the success path (optimization ran, even if no change).
		   We should ensure metrics are emitted for the External Scaler.
		*/

		// Ensure we have a valid SAT/Model decision "SaturationOnly" flag for metric emission context if needed
		// For now we assume if no decision, it's not saturation-only forced override, just normal op.
		// isSaturationOnly := false
		// if hasDecision {
		// 	isSaturationOnly = decision.SaturationOnly
		// }

		if err := act.EmitMetrics(ctx, &updateVa); err != nil {
			logger.Error(err, "Failed to emit metrics for external autoscalers",
				"variant", updateVa.Name)
		} else {
			// Only log detail if we had a decision or periodically (to avoid spamming logs on every loop for no-ops)
			if hasDecision {
				logger.Info("Successfully emitted metrics",
					"variant", updateVa.Name,
					"target", targetReplicas,
					"accelerator", acceleratorName)
			}
			updateVa.Status.Actuation.Applied = true
		}

		// Update Shared State and Trigger Reconcile via Channel
		// This avoids any API server interaction from the Engine.

		// 1. Update Cache
		// Determine MetricsAvailable status for the cache.
		// - hasAllocation is true when we successfully collected current replica metrics
		//   for this variant during this loop (metrics pipeline is working).
		// - hasDecision is true when the optimizer produced a scaling decision based on
		//   saturation metrics in this run.
		// Either condition implies saturation metrics were available and usable.
		metricsAvailable := hasAllocation || hasDecision
		metricsReason := llmdVariantAutoscalingV1alpha1.ReasonMetricsMissing
		metricsMessage := llmdVariantAutoscalingV1alpha1.MessageMetricsUnavailable
		if metricsAvailable {
			metricsReason = llmdVariantAutoscalingV1alpha1.ReasonMetricsFound
			metricsMessage = llmdVariantAutoscalingV1alpha1.MessageMetricsAvailable
		}

		common.DecisionCache.Set(va.Name, va.Namespace, interfaces.VariantDecision{
			VariantName:       vaName,
			Namespace:         va.Namespace,
			TargetReplicas:    targetReplicas,
			AcceleratorName:   acceleratorName,
			LastRunTime:       metav1.Now(),
			CurrentAllocation: currentAllocations[vaName],
			MetricsAvailable:  metricsAvailable,
			MetricsReason:     metricsReason,
			MetricsMessage:    metricsMessage,
		})

		// 2. Trigger Reconciler
		common.DecisionTrigger <- event.GenericEvent{
			Object: &updateVa,
		}

		if hasDecision {
			logger.Info("Applied saturation decision via shared cache",
				"variant", vaName,
				"namespace", updateVa.Namespace,
				"action", decision.Action,
				"target", targetReplicas,
				"reason", reason)
		}
	}

	return nil
}

// emitSafetyNetMetrics emits fallback metrics when saturation analysis fails.
func (e *Engine) emitSafetyNetMetrics(
	ctx context.Context,
	modelVAs []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	currentAllocations map[string]*interfaces.Allocation,
	scaleTargets map[string]scaletarget.ScaleTargetAccessor,
) {
	logger := ctrl.LoggerFrom(ctx)
	act := actuator.NewActuator(e.client)

	for _, va := range modelVAs {
		// Determine desired replicas
		var desiredReplicas, currentReplicas int32
		var fallbackSource string
		var scaleTarget scaletarget.ScaleTargetAccessor
		var err error
		if scaleTargets != nil {
			if target, ok := scaleTargets[utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())]; ok {
				scaleTarget = target
				// Get current replicas for metric emission.
				currentReplicas, err = act.GetCurrentScaleTargetReplicasFromScaleTarget(&va, scaleTarget)
				if err != nil {
					logger.Error(err, "Safety net: failed to get current replicas from scale target for metrics", "using cached allocation",
						"variant", va.Name)
					if curr, ok := currentAllocations[utils.GetNamespacedKey(va.Namespace, va.Name)]; ok {
						currentReplicas = int32(curr.NumReplicas)
					}
				}
			}
		}

		// Strategy 1: Use previous desired replicas if available
		if va.Status.DesiredOptimizedAlloc.NumReplicas != nil && *va.Status.DesiredOptimizedAlloc.NumReplicas > 0 {
			desiredReplicas = *va.Status.DesiredOptimizedAlloc.NumReplicas
			fallbackSource = "previous-desired"
		} else {
			desiredReplicas = currentReplicas
			fallbackSource = "current-replicas"
		}

		// Determine accelerator - try status first, then labels, skip if unavailable
		// TODO: remove this checks when we will move to a new version of the CRD
		// with required accelerator field
		accelerator := va.Status.DesiredOptimizedAlloc.Accelerator
		if accelerator == "" {
			if curr, ok := currentAllocations[utils.GetNamespacedKey(va.Namespace, va.Name)]; ok {
				accelerator = curr.Accelerator
			}
		}
		if accelerator == "" {
			// Try to get accelerator name from scale target nodeSelector/nodeAffinity or VA labels
			if scaleTarget == nil {
				logger.V(logging.DEBUG).Info("Safety net: no scale target found for VA",
					"variant", va.Name)
			} else {
				if acceleratorName := utils.GetAcceleratorNameFromScaleTarget(&va, scaleTarget); acceleratorName != "" {
					accelerator = acceleratorName
				}
			}
		}
		if accelerator == "" {
			logger.Info("Safety net: skipping metric emission - no accelerator name available",
				"variant", va.Name)
			continue
		}

		// Emit safety net metrics
		if err := act.MetricsEmitter.EmitReplicaMetrics(
			ctx,
			&va,
			currentReplicas,
			desiredReplicas,
			accelerator,
		); err != nil {
			logger.Error(err, "Safety net: failed to emit metrics",
				"variant", va.Name)
			continue
		}

		logger.Info("Safety net activated: emitted fallback metrics",
			"variant", va.Name,
			"currentReplicas", currentReplicas,
			"desiredReplicas", desiredReplicas,
			"accelerator", accelerator,
			"fallbackSource", fallbackSource)
	}
}
