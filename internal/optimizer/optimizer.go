package optimizer

import (
	"context"
	"fmt"
	"math"

	llmdOptv1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	collector "github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector"
	interfaces "github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"
	infernoConfig "github.com/llm-d-incubation/workload-variant-autoscaler/pkg/config"
	inferno "github.com/llm-d-incubation/workload-variant-autoscaler/pkg/core"
	infernoManager "github.com/llm-d-incubation/workload-variant-autoscaler/pkg/manager"
)

// Engine holding all necessary data to perform global optimization across all variants
type VariantAutoscalingsEngine struct {
	manager *infernoManager.Manager
	system  *inferno.System
}

// Create a new instance of a variants autoscaling engine
func NewVariantAutoscalingsEngine(manager *infernoManager.Manager, system *inferno.System) *VariantAutoscalingsEngine {
	return &VariantAutoscalingsEngine{
		manager: manager,
		system:  system,
	}
}

// validateReplicaBounds validates minReplicas and maxReplicas configurations and logs warnings
func (engine *VariantAutoscalingsEngine) validateReplicaBounds(
	ctx context.Context,
	vaList llmdOptv1alpha1.VariantAutoscalingList,
	scaleToZeroConfigData *utils.ScaleToZeroConfigData,
) {
	// Track variants with minReplicas > 0 per model
	modelVariantsWithMin := make(map[string][]string)

	for i := range vaList.Items {
		va := &vaList.Items[i]
		modelID := va.Spec.ModelID
		minReplicas := utils.GetVariantMinReplicas(va)

		if minReplicas > 0 {
			modelVariantsWithMin[modelID] = append(modelVariantsWithMin[modelID], va.Name)
		}
	}

	// Check for multiple variants with minReplicas > 0
	for modelID, variantNames := range modelVariantsWithMin {
		if len(variantNames) > 1 {
			logger.Log.Warn("Multiple variants with minReplicas > 0 may lead to unnecessary GPU utilization",
				"modelID", modelID,
				"variants", variantNames,
				"count", len(variantNames))
		}
	}

	// Check for conflict between scaleToZero and minReplicas > 0
	if scaleToZeroConfigData != nil {
		for modelID, variantNames := range modelVariantsWithMin {
			if utils.IsScaleToZeroEnabled(*scaleToZeroConfigData, modelID) {
				logger.Log.Warn("Model has scaleToZero enabled but variants have minReplicas > 0, preventing scale-to-zero",
					"modelID", modelID,
					"variants", variantNames,
					"scaleToZeroEnabled", true)
			}
		}
	}
}

// Perform a global optimization producing optimized allocations for all variants
func (engine *VariantAutoscalingsEngine) Optimize(ctx context.Context,
	vaList llmdOptv1alpha1.VariantAutoscalingList,
	analysis map[string]*interfaces.ModelAnalyzeResponse,
	scaleToZeroConfigData *utils.ScaleToZeroConfigData,
	scaleToZeroCache *collector.ScaleToZeroMetricsCache,
) (map[string]llmdOptv1alpha1.OptimizedAlloc, error) {

	// Validate replica bounds and log warnings
	engine.validateReplicaBounds(ctx, vaList, scaleToZeroConfigData)

	if err := engine.manager.Optimize(); err != nil {
		// Return empty map instead of nil to prevent panic in controller
		return make(map[string]llmdOptv1alpha1.OptimizedAlloc), err
	}
	allocationSolution := engine.system.GenerateSolution()
	if allocationSolution == nil || len(allocationSolution.Spec) == 0 {
		// Return empty map instead of nil to prevent panic in controller
		return make(map[string]llmdOptv1alpha1.OptimizedAlloc), fmt.Errorf("no feasible allocations found for %d variants", len(vaList.Items))
	}

	logger.Log.Debug("Optimization solution - ", "system: ", engine.system)

	// Apply zero-rate handling logic before creating final allocations
	engine.applyZeroRateHandling(ctx, &vaList, allocationSolution, scaleToZeroConfigData, scaleToZeroCache)

	optimizedAllocMap := make(map[string]llmdOptv1alpha1.OptimizedAlloc)
	for _, va := range vaList.Items {
		vaName := va.Name
		vaNamespace := va.Namespace
		variantID := va.Spec.VariantID
		optimizedAllocation, err := utils.CreateOptimizedAlloc(vaName, vaNamespace, variantID, allocationSolution)
		if err != nil {
			// Fallback to current replicas if no solution found for this variant
			logger.Log.Warn("No optimizer solution found for variant, falling back to current replicas",
				"variant", vaName,
				"namespace", vaNamespace,
				"currentReplicas", va.Status.CurrentAlloc.NumReplicas,
				"error", err)
			optimizedAllocation = &llmdOptv1alpha1.OptimizedAlloc{
				NumReplicas: va.Status.CurrentAlloc.NumReplicas,
				Reason:      "Optimizer fallback: no solution found, using current replicas",
			}
		}

		// Enforce replica bounds (minReplicas and maxReplicas)
		// Note: Bounds must be enforced for both optimizer solutions and fallback values
		minReplicas := utils.GetVariantMinReplicas(&va)
		maxReplicas := utils.GetVariantMaxReplicas(&va)

		// Enforce minReplicas
		if optimizedAllocation.NumReplicas < minReplicas {
			logger.Log.Info("Clamping replicas to minReplicas",
				"variant", vaName,
				"namespace", vaNamespace,
				"optimized", optimizedAllocation.NumReplicas,
				"minReplicas", minReplicas)
			optimizedAllocation.NumReplicas = minReplicas
		}

		// Enforce maxReplicas
		if maxReplicas > 0 && optimizedAllocation.NumReplicas > maxReplicas {
			logger.Log.Info("Clamping replicas to maxReplicas",
				"variant", vaName,
				"namespace", vaNamespace,
				"optimized", optimizedAllocation.NumReplicas,
				"maxReplicas", maxReplicas)
			optimizedAllocation.NumReplicas = maxReplicas
		}

		optimizedAllocMap[vaName] = *optimizedAllocation
	}

	// Apply final scale-to-zero check after all bounds are enforced
	// This handles variants that may have been skipped by optimizer or fallback logic
	engine.applyFinalScaleToZeroCheck(ctx, &vaList, optimizedAllocMap, scaleToZeroConfigData, scaleToZeroCache)

	return optimizedAllocMap, nil
}

// applyZeroRateHandling modifies the allocation solution for zero-rate scenarios
// based on scale-to-zero configuration and request metrics over retention period
func (engine *VariantAutoscalingsEngine) applyZeroRateHandling(
	ctx context.Context,
	vaList *llmdOptv1alpha1.VariantAutoscalingList,
	allocationSolution *infernoConfig.AllocationSolution,
	scaleToZeroConfigData *utils.ScaleToZeroConfigData,
	scaleToZeroCache *collector.ScaleToZeroMetricsCache,
) {
	// Group variants by ModelID
	// Estimate capacity: assume average 2-3 variants per model
	estimatedModels := len(vaList.Items) / 2
	if estimatedModels == 0 {
		estimatedModels = 1
	}
	modelVariants := make(map[string][]*llmdOptv1alpha1.VariantAutoscaling, estimatedModels)
	for i := range vaList.Items {
		va := &vaList.Items[i]
		modelID := va.Spec.ModelID
		modelVariants[modelID] = append(modelVariants[modelID], va)
	}

	// Process each model
	for modelID, variants := range modelVariants {
		// Check scale-to-zero configuration
		var scaleToZeroEnabled bool
		if scaleToZeroConfigData == nil {
			logger.Log.Warn("Scale-to-zero config is nil, treating as disabled", "modelID", modelID)
			scaleToZeroEnabled = false
		} else {
			scaleToZeroEnabled = utils.IsScaleToZeroEnabled(*scaleToZeroConfigData, modelID)
		}

		// Get total requests over retention period from scale-to-zero cache
		totalRequests := 0.0
		if scaleToZeroCache != nil {
			if metrics, exists := scaleToZeroCache.Get(modelID); exists {
				totalRequests = metrics.TotalRequestsOverRetentionPeriod
				logger.Log.Info("Scale-to-zero cache hit",
					"modelID", modelID,
					"totalRequestsOverRetention", totalRequests,
					"retentionPeriod", metrics.RetentionPeriod)
			} else {
				logger.Log.Info("Scale-to-zero cache miss - no metrics found",
					"modelID", modelID)
			}
		}

		logger.Log.Debug("Zero-rate handling",
			"modelID", modelID,
			"scaleToZeroEnabled", scaleToZeroEnabled,
			"totalRequestsOverRetention", totalRequests,
			"variantCount", len(variants))

		// Determine if we should keep at least one replica
		shouldKeepOneReplica := !scaleToZeroEnabled || totalRequests > 0

		logger.Log.Info("Scale-to-zero decision",
			"modelID", modelID,
			"scaleToZeroEnabled", scaleToZeroEnabled,
			"totalRequests", totalRequests,
			"shouldKeepOneReplica", shouldKeepOneReplica)

		if !shouldKeepOneReplica {
			// Scale all variants to zero
			logger.Log.Info("Scaling all variants to zero (scale-to-zero enabled, no recent requests)",
				"modelID", modelID)
			for _, va := range variants {
				serverName := utils.FullName(va.Name, va.Namespace)
				if allocData, exists := allocationSolution.Spec[serverName]; exists {
					allocData.NumReplicas = 0
					allocationSolution.Spec[serverName] = allocData
				}
			}
			continue
		}

		// Check if optimizer already allocated replicas (non-zero rate)
		hasNonZeroAllocation := false
		for _, va := range variants {
			serverName := utils.FullName(va.Name, va.Namespace)
			if allocData, exists := allocationSolution.Spec[serverName]; exists && allocData.NumReplicas > 0 {
				hasNonZeroAllocation = true
				break
			}
		}

		// If optimizer already allocated replicas, no need for zero-rate handling
		if hasNonZeroAllocation {
			logger.Log.Debug("Optimizer already allocated replicas, skipping zero-rate handling",
				"modelID", modelID)
			continue
		}

		// Zero-rate scenario: keep exactly one replica of one variant
		reason := "scale-to-zero disabled"
		if scaleToZeroEnabled {
			reason = fmt.Sprintf("recent requests (%.0f over retention period)", totalRequests)
		}
		logger.Log.Info("Applying zero-rate handling: keeping one replica",
			"modelID", modelID,
			"reason", reason)

		// Choose which variant to keep based on current state and cost
		variantToKeep := engine.selectVariantToKeep(variants, allocationSolution)
		if variantToKeep == nil {
			logger.Log.Warn("No variant selected to keep, skipping", "modelID", modelID)
			continue
		}

		logger.Log.Info("Selected variant to keep one replica",
			"modelID", modelID,
			"variant", variantToKeep.Name,
			"namespace", variantToKeep.Namespace)

		// Set one replica for selected variant, zero for others
		for _, va := range variants {
			serverName := utils.FullName(va.Name, va.Namespace)
			if allocData, exists := allocationSolution.Spec[serverName]; exists {
				if va.Name == variantToKeep.Name && va.Namespace == variantToKeep.Namespace {
					allocData.NumReplicas = 1
				} else {
					allocData.NumReplicas = 0
				}
				allocationSolution.Spec[serverName] = allocData
			}
		}
	}
}

// selectVariantToKeep chooses which variant should keep one replica in zero-rate scenario
// Priority:
// 1. If only one variant exists, keep it
// 2. If only one variant has non-zero current replicas, keep it
// 3. If multiple variants have non-zero replicas, keep the cheapest one
// 4. If all variants have zero replicas, keep the cheapest one from solution
func (engine *VariantAutoscalingsEngine) selectVariantToKeep(
	variants []*llmdOptv1alpha1.VariantAutoscaling,
	allocationSolution *infernoConfig.AllocationSolution,
) *llmdOptv1alpha1.VariantAutoscaling {
	if len(variants) == 0 {
		return nil
	}

	// Case 1: Only one variant exists
	if len(variants) == 1 {
		return variants[0]
	}

	// Find variants with non-zero current replicas
	variantsWithReplicas := make([]*llmdOptv1alpha1.VariantAutoscaling, 0)
	for _, va := range variants {
		if va.Status.CurrentAlloc.NumReplicas > 0 {
			variantsWithReplicas = append(variantsWithReplicas, va)
		}
	}

	// Case 2: Only one variant has non-zero current replicas
	if len(variantsWithReplicas) == 1 {
		return variantsWithReplicas[0]
	}

	// Case 3 & 4: Choose cheapest variant
	// If multiple have replicas, choose from those; otherwise choose from all variants
	candidateVariants := variantsWithReplicas
	if len(candidateVariants) == 0 {
		candidateVariants = variants
	}

	return engine.selectCheapestVariant(candidateVariants, allocationSolution)
}

// selectCheapestVariant returns the variant with lowest cost from the allocation solution
func (engine *VariantAutoscalingsEngine) selectCheapestVariant(
	variants []*llmdOptv1alpha1.VariantAutoscaling,
	allocationSolution *infernoConfig.AllocationSolution,
) *llmdOptv1alpha1.VariantAutoscaling {
	if len(variants) == 0 {
		return nil
	}

	var cheapestVariant *llmdOptv1alpha1.VariantAutoscaling
	var minCost float32 = math.MaxFloat32

	for _, va := range variants {
		serverName := utils.FullName(va.Name, va.Namespace)
		if allocData, exists := allocationSolution.Spec[serverName]; exists {
			if allocData.Cost < minCost {
				minCost = allocData.Cost
				cheapestVariant = va
			}
		}
	}

	// Fallback to first variant if no cost info available
	if cheapestVariant == nil {
		logger.Log.Warn("No cost information available in allocation solution, selecting first variant",
			"variant", variants[0].Name,
			"namespace", variants[0].Namespace)
		return variants[0]
	}

	return cheapestVariant
}

// applyFinalScaleToZeroCheck applies scale-to-zero logic to the final optimized allocations
// This is a post-processing step that runs after all bounds are enforced
func (engine *VariantAutoscalingsEngine) applyFinalScaleToZeroCheck(
	ctx context.Context,
	vaList *llmdOptv1alpha1.VariantAutoscalingList,
	optimizedAllocMap map[string]llmdOptv1alpha1.OptimizedAlloc,
	scaleToZeroConfigData *utils.ScaleToZeroConfigData,
	scaleToZeroCache *collector.ScaleToZeroMetricsCache,
) {
	// ctx is reserved for future use (tracing, cancellation, etc.)
	_ = ctx

	// Group variants by ModelID
	estimatedModels := len(vaList.Items) / 2
	if estimatedModels == 0 {
		estimatedModels = 1
	}
	modelVariants := make(map[string][]*llmdOptv1alpha1.VariantAutoscaling, estimatedModels)
	for i := range vaList.Items {
		va := &vaList.Items[i]
		modelID := va.Spec.ModelID
		modelVariants[modelID] = append(modelVariants[modelID], va)
	}

	// Process each model
	for modelID, variants := range modelVariants {
		// Check scale-to-zero configuration
		var scaleToZeroEnabled bool
		if scaleToZeroConfigData == nil {
			scaleToZeroEnabled = false
		} else {
			scaleToZeroEnabled = utils.IsScaleToZeroEnabled(*scaleToZeroConfigData, modelID)
		}

		// If scale-to-zero not enabled, skip
		if !scaleToZeroEnabled {
			continue
		}

		// Get total requests over retention period
		totalRequests := 0.0
		if scaleToZeroCache != nil {
			if metrics, exists := scaleToZeroCache.Get(modelID); exists {
				totalRequests = metrics.TotalRequestsOverRetentionPeriod
			}
		}

		// If there are recent requests, skip
		if totalRequests > 0 {
			logger.Log.Debug("Skipping scale-to-zero: recent requests detected",
				"modelID", modelID,
				"totalRequests", totalRequests)
			continue
		}

		// Scale variants to zero respecting per-variant minReplicas bounds
		logger.Log.Info("Final scale-to-zero check: evaluating variants for scale-to-zero",
			"modelID", modelID,
			"scaleToZeroEnabled", scaleToZeroEnabled,
			"totalRequests", totalRequests)

		for _, va := range variants {
			if alloc, exists := optimizedAllocMap[va.Name]; exists {
				// Get variant-specific minReplicas
				minReplicas := utils.GetVariantMinReplicas(va)

				// Only scale to zero if variant's minReplicas allows it
				if minReplicas == 0 && alloc.NumReplicas > 0 {
					logger.Log.Info("Scaling variant to zero (final scale-to-zero check)",
						"variant", va.Name,
						"namespace", va.Namespace,
						"previousReplicas", alloc.NumReplicas,
						"minReplicas", minReplicas)
					alloc.NumReplicas = 0
					optimizedAllocMap[va.Name] = alloc
				} else if minReplicas > 0 {
					logger.Log.Debug("Skipping scale-to-zero: variant has minReplicas > 0",
						"variant", va.Name,
						"namespace", va.Namespace,
						"minReplicas", minReplicas,
						"currentReplicas", alloc.NumReplicas)
				}
			}
		}
	}
}
