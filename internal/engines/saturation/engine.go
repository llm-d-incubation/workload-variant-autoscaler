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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/common"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/executor"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/pipeline"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/saturation"
	saturation_v2 "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/analyzers/saturation_v2"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// Constants for MetricsAvailable condition
// Note: Reasons should match api/v1alpha1 constants for consistency
const (
	// MetricsReasonAvailable uses ReasonMetricsFound from API for consistency
	MetricsReasonAvailable = llmdVariantAutoscalingV1alpha1.ReasonMetricsFound
	// MetricsReasonUnavailable uses ReasonMetricsMissing from API for consistency
	MetricsReasonUnavailable  = llmdVariantAutoscalingV1alpha1.ReasonMetricsMissing
	MetricsMessageAvailable   = "Saturation metrics data is available for scaling decisions"
	MetricsMessageUnavailable = "No saturation metrics available - pods may not be ready or metrics not yet scraped"
)

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

	// capacityStore is shared with the V2 analyzer for caching capacity knowledge.
	capacityStore *saturation_v2.CapacityKnowledgeStore

	// optimizer is the V2 scaling optimizer that produces VariantDecisions from
	// AnalyzerResults. Selected at engine init: CostAwareOptimizer (unlimited)
	// or GreedyBySaturationOptimizer (limited).
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

	// Select optimizer at init time based on global config.
	// CostAwareOptimizer (unlimited mode) is the default.
	// When limited mode is enabled, a GPU-constrained optimizer will be used
	// (GreedyBySaturationOptimizer, added in a follow-up).
	var scalingOptimizer pipeline.ScalingOptimizer
	if cfg.LimitedModeEnabled() {
		// TODO: use GreedyBySaturationOptimizer when available
		scalingOptimizer = pipeline.NewCostAwareOptimizer()
	} else {
		scalingOptimizer = pipeline.NewCostAwareOptimizer()
	}

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

	activeVAs, err := utils.ActiveVariantAutoscaling(ctx, e.client)
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

	// Determine whether to use V2 token-based optimizer path from global config.
	// Config value "saturation" selects the V2 token-based analyzer;
	// empty/other values use the V1 percentage-based analyzer.
	globalSatCfgMap := e.Config.SaturationConfig()
	useV2 := false
	if cfg, ok := globalSatCfgMap["default"]; ok {
		cfg.ApplyDefaults()
		useV2 = cfg.AnalyzerName == "saturation"
	}

	var allDecisions []interfaces.VariantDecision

	// V1 and V2 have separate optimize paths because they use fundamentally
	// different analysis types and target-building flows:
	//   - V1: saturation.Analyzer → ModelSaturationAnalysis → CalculateSaturationTargets → Enforcer → Limiter
	//   - V2: saturation_v2.Analyzer → AnalyzerResult → Optimizer.Optimize → Enforcer bridge
	// V1 will be deprecated once V2 is fully validated, at which point the
	// V1 path and the saturation.Analyzer can be removed.
	if useV2 {
		allDecisions = e.optimizeV2(ctx, modelGroups, currentAllocations)
	} else {
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

		saturationConfig, ok := saturationConfigMap["default"]
		if !ok {
			logger.Info("Default saturation scaling config not found for namespace, skipping model",
				"namespace", namespace,
				"modelID", modelID)
			continue
		}

		saturationTargets, saturationAnalysis, variantStates, err := e.RunSaturationAnalysis(ctx, modelID, modelVAs, saturationConfig, e.client)
		if err != nil {
			logger.Error(err, "Saturation analysis failed", "modelID", modelID)
			e.emitSafetyNetMetrics(ctx, modelVAs, currentAllocations)
			continue
		}

		var finalDecisions []interfaces.VariantDecision
		if saturationAnalysis != nil {
			// Apply scale-to-zero enforcement after saturation analysis
			// Get namespace-aware scale-to-zero config (namespace-local > global)
			scaleToZeroConfig := e.Config.ScaleToZeroConfigForNamespace(namespace)

			// Copy original targets for logging (enforcer modifies map in place)
			originalTargets := make(map[string]int, len(saturationTargets))
			for k, v := range saturationTargets {
				originalTargets[k] = v
			}

			enforcedTargets, scaledToZero := e.ScaleToZeroEnforcer.EnforcePolicy(
				ctx,
				modelID,
				modelVAs[0].Namespace,
				saturationTargets,
				saturationAnalysis.VariantAnalyses,
				scaleToZeroConfig,
			)
			if scaledToZero {
				logger.Info("Scale-to-zero enforcement applied",
					"modelID", modelID,
					"originalTargets", originalTargets,
					"enforcedTargets", enforcedTargets)
			}
			saturationTargets = enforcedTargets

			finalDecisions = e.convertSaturationTargetsToDecisions(ctx, saturationTargets, saturationAnalysis, variantStates)
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
	var globalSaturationConfig interfaces.SaturationScalingConfig
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
		saturationConfig, ok := saturationConfigMap["default"]
		if !ok {
			logger.Info("Default saturation scaling config not found for namespace, skipping model",
				"namespace", namespace, "modelID", modelID)
			continue
		}
		saturationConfig.ApplyDefaults()

		data, err := e.prepareModelData(ctx, modelID, modelVAs, e.client)
		if err != nil {
			logger.Error(err, "Model data preparation failed", "modelID", modelID)
			e.emitSafetyNetMetrics(ctx, modelVAs, currentAllocations)
			continue
		}
		if data == nil {
			logger.V(logging.DEBUG).Info("Skipping model: no metrics available", "modelID", modelID)
			continue
		}

		req, err := e.collectV2ModelRequest(ctx, modelID, namespace,
			data.replicaMetrics, saturationConfig, data.variantStates,
			data.deployments, data.variantAutoscalings)
		if err != nil {
			logger.Error(err, "V2 analysis failed", "modelID", modelID)
			e.emitSafetyNetMetrics(ctx, modelVAs, currentAllocations)
			continue
		}

		requests = append(requests, *req)
	}

	if len(requests) == 0 {
		return nil
	}

	// Stage 2: Call optimizer (no constraints for now — CostAwareOptimizer ignores them)
	allDecisions := e.optimizer.Optimize(ctx, requests, nil)

	logger.Info("V2 optimizer produced decisions",
		"optimizer", e.optimizer.Name(),
		"decisionCount", len(allDecisions),
		"modelCount", len(requests))

	// Stage 3: Apply enforcer per-model (bridge from decisions to targets map)
	for _, req := range requests {
		scaleToZeroConfig := e.Config.ScaleToZeroConfigForNamespace(req.Namespace)

		targets := extractTargetsFromDecisions(allDecisions, req.ModelID, req.Namespace)
		variantAnalyses := buildVariantAnalysesFromDecisions(allDecisions, req.ModelID, req.Namespace)

		enforcedTargets, scaledToZero := e.ScaleToZeroEnforcer.EnforcePolicy(
			ctx, req.ModelID, req.Namespace,
			targets, variantAnalyses, scaleToZeroConfig,
		)
		if scaledToZero {
			logger.Info("Scale-to-zero enforcement applied (V2)",
				"modelID", req.ModelID, "enforcedTargets", enforcedTargets)
		}

		allDecisions = applyEnforcedTargetsToDecisions(allDecisions, enforcedTargets, req.ModelID, req.Namespace, e.optimizer.Name())
	}

	return allDecisions
}

// BuildVariantStates extracts current and desired replica counts from VAs for capacity analysis.
func (e *Engine) BuildVariantStates(
	ctx context.Context,
	vas []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	deployments map[string]*appsv1.Deployment,
	k8sClient client.Client,
) []interfaces.VariantReplicaState {
	states := make([]interfaces.VariantReplicaState, 0, len(vas))

	for _, va := range vas {
		// Get current replicas from deployment using ScaleTargetRef
		// Get current replicas from deployment using ScaleTargetRef
		var deploy *appsv1.Deployment
		var found bool

		// Try to look up in provided map first (optimization)
		if deployments != nil {
			deploy, found = deployments[utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())]
		}

		if !found {
			// Fallback to API call
			fetchedDeploy := &appsv1.Deployment{}
			if err := utils.GetDeploymentWithBackoff(ctx, k8sClient, va.GetScaleTargetName(), va.Namespace, fetchedDeploy); err != nil {
				ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Could not get deployment for VA, skipping",
					"variant", va.Name,
					"error", err)
				continue
			}
			deploy = fetchedDeploy
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("BuildVariantStates fallback lookup", "variant", va.Name, "deployName", deploy.Name, "specReplicas", deploy.Spec.Replicas, "statusReplicas", deploy.Status.Replicas, "readyReplicas", deploy.Status.ReadyReplicas)
		} else {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("BuildVariantStates map lookup", "variant", va.Name, "deployName", deploy.Name, "specReplicas", deploy.Spec.Replicas, "statusReplicas", deploy.Status.Replicas, "readyReplicas", deploy.Status.ReadyReplicas)
		}

		currentReplicas := int(deploy.Status.Replicas)
		if currentReplicas == 0 && deploy.Spec.Replicas != nil {
			currentReplicas = int(*deploy.Spec.Replicas)
		}

		// Calculate pending replicas (not yet ready)
		readyReplicas := int(deploy.Status.ReadyReplicas)
		pendingReplicas := currentReplicas - readyReplicas
		if pendingReplicas < 0 {
			// This indicates an unexpected state where readyReplicas exceeds currentReplicas.
			// Log at Info level since this inconsistency should be visible to operators.
			ctrl.LoggerFrom(ctx).Info("Unexpected state: readyReplicas exceeds currentReplicas, clamping pendingReplicas to 0",
				"variant", va.Name, "currentReplicas", currentReplicas, "readyReplicas", readyReplicas)
			pendingReplicas = 0
		}

		// Extract GPUs per replica from deployment's pod template
		gpusPerReplica := getDeploymentGPUsPerReplica(deploy)

		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("BuildVariantStates result", "variant", va.Name, "currentReplicas", currentReplicas, "readyReplicas", readyReplicas, "pendingReplicas", pendingReplicas, "gpusPerReplica", gpusPerReplica)

		states = append(states, interfaces.VariantReplicaState{
			VariantName:     va.Name,
			CurrentReplicas: currentReplicas,
			DesiredReplicas: va.Status.DesiredOptimizedAlloc.NumReplicas,
			PendingReplicas: pendingReplicas,
			GPUsPerReplica:  gpusPerReplica,
		})
	}

	return states
}

// gpuVendors lists the resource name prefixes for GPU vendors
var gpuVendors = []string{"nvidia.com", "amd.com", "intel.com"}

// getDeploymentGPUsPerReplica extracts the total GPU requests from a deployment's pod template.
// It sums GPU requests across all containers for supported vendors (nvidia.com, amd.com, intel.com).
// Returns 1 as default if no GPU requests are found (assumes at least 1 GPU for inference workloads).
func getDeploymentGPUsPerReplica(deploy *appsv1.Deployment) int {
	if deploy == nil {
		return 1
	}

	total := 0
	for _, container := range deploy.Spec.Template.Spec.Containers {
		for _, vendor := range gpuVendors {
			resName := corev1.ResourceName(vendor + "/gpu")
			if qty, ok := container.Resources.Requests[resName]; ok {
				total += int(qty.Value())
			}
		}
	}

	// Default to 1 GPU if no explicit requests found
	// (common for inference workloads that may not have resource requests)
	if total == 0 {
		return 1
	}
	return total
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

		// Use GPUsPerReplica from variant state (extracted from deployment)
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

// modelData holds the pre-processed data for a model, shared between V1 and V2 paths.
type modelData struct {
	modelID             string
	namespace           string
	replicaMetrics      []interfaces.ReplicaMetrics
	deployments         map[string]*appsv1.Deployment
	variantAutoscalings map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling
	variantCosts        map[string]float64
	variantStates       []interfaces.VariantReplicaState
}

// prepareModelData collects metrics and builds lookup maps for a model's VAs.
// This is shared by both V1 and V2 paths.
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
	deployments := make(map[string]*appsv1.Deployment)
	variantAutoscalings := make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling)

	for i := range modelVAs {
		va := &modelVAs[i]

		var deploy appsv1.Deployment
		err := utils.GetDeploymentWithBackoff(ctx, k8sClient, va.GetScaleTargetName(), va.Namespace, &deploy)
		if err != nil {
			logger.V(logging.DEBUG).Info("Could not get deployment for VA",
				"variant", va.Name,
				"deployment", va.GetScaleTargetName(),
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

		deploymentKey := utils.GetNamespacedKey(va.Namespace, va.GetScaleTargetName())
		deployments[deploymentKey] = &deploy

		variantKey := utils.GetNamespacedKey(va.Namespace, va.Name)
		variantAutoscalings[variantKey] = va
		variantCosts[variantKey] = cost
	}

	logger.V(logging.DEBUG).Info("Using source infrastructure for replica metrics",
		"modelID", modelID,
		"namespace", namespace)
	replicaMetrics, err := e.ReplicaMetricsCollector.CollectReplicaMetrics(ctx, modelID, namespace, deployments, variantAutoscalings, variantCosts)
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

	variantStates := e.BuildVariantStates(ctx, modelVAs, deployments, k8sClient)

	return &modelData{
		modelID:             modelID,
		namespace:           namespace,
		replicaMetrics:      replicaMetrics,
		deployments:         deployments,
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
	SaturationConfig interfaces.SaturationScalingConfig,
	k8sClient client.Client,
) (map[string]int, *interfaces.ModelSaturationAnalysis, []interfaces.VariantReplicaState, error) {
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

	return saturationTargets, saturationAnalysis, data.variantStates, nil
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
			if updateVa.Status.DesiredOptimizedAlloc.NumReplicas > 0 {
				targetReplicas = updateVa.Status.DesiredOptimizedAlloc.NumReplicas
			} else if curr, ok := currentAllocations[vaName]; ok {
				targetReplicas = curr.NumReplicas
			}
			// Keep existing accelerator or use current
			if updateVa.Status.DesiredOptimizedAlloc.Accelerator != "" {
				acceleratorName = updateVa.Status.DesiredOptimizedAlloc.Accelerator
			} else if curr, ok := currentAllocations[vaName]; ok {
				acceleratorName = curr.Accelerator
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
				MetricsReason:    MetricsReasonUnavailable,
				MetricsMessage:   MetricsMessageUnavailable,
			})
			// Trigger reconciler to apply the condition
			common.DecisionTrigger <- event.GenericEvent{
				Object: &updateVa,
			}
			continue
		}

		// Update DesiredOptimizedAlloc
		// ALWAYS update LastRunTime to trigger reconciliation in the controller
		updateVa.Status.DesiredOptimizedAlloc = llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
			NumReplicas: targetReplicas,
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
		metricsReason := MetricsReasonUnavailable
		metricsMessage := MetricsMessageUnavailable
		if metricsAvailable {
			metricsReason = MetricsReasonAvailable
			metricsMessage = MetricsMessageAvailable
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
) {
	logger := ctrl.LoggerFrom(ctx)
	act := actuator.NewActuator(e.client)

	for _, va := range modelVAs {
		// Determine desired replicas
		var desiredReplicas int32
		var fallbackSource string

		// Get current replicas for metric emission
		currentReplicas, err := act.GetCurrentDeploymentReplicas(ctx, &va)
		if err != nil {
			logger.Error(err, "Safety net: failed to get current replicas from Deployment for metrics", "using cached allocation",
				"variant", va.Name)
			if curr, ok := currentAllocations[utils.GetNamespacedKey(va.Namespace, va.Name)]; ok {
				currentReplicas = int32(curr.NumReplicas)
			}
		}

		// Strategy 1: Use previous desired replicas if available
		if va.Status.DesiredOptimizedAlloc.NumReplicas > 0 {
			desiredReplicas = int32(va.Status.DesiredOptimizedAlloc.NumReplicas)
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
			// Try to get from VA labels as last resort
			if val, ok := va.Labels[utils.AcceleratorNameLabel]; ok && val != "" {
				accelerator = val
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
