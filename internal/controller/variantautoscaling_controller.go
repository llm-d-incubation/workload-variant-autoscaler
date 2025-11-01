/*
Copyright 2025.

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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	actuator "github.com/llm-d-incubation/workload-variant-autoscaler/internal/actuator"
	collector "github.com/llm-d-incubation/workload-variant-autoscaler/internal/collector"
	interfaces "github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/metrics"
	analyzer "github.com/llm-d-incubation/workload-variant-autoscaler/internal/modelanalyzer"
	variantAutoscalingOptimizer "github.com/llm-d-incubation/workload-variant-autoscaler/internal/optimizer"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"
	infernoConfig "github.com/llm-d-incubation/workload-variant-autoscaler/pkg/config"
	inferno "github.com/llm-d-incubation/workload-variant-autoscaler/pkg/core"
	infernoManager "github.com/llm-d-incubation/workload-variant-autoscaler/pkg/manager"
	infernoSolver "github.com/llm-d-incubation/workload-variant-autoscaler/pkg/solver"
	"github.com/prometheus/client_golang/api"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	// Reconciliation interval bounds
	DefaultReconciliationInterval = 60 * time.Second
	MinReconciliationInterval     = 1 * time.Second
	MaxReconciliationInterval     = 1 * time.Hour

	// Cache TTL settings
	MinCacheTTL = 5 * time.Second

	// Setup timeout for API calls during controller initialization
	SetupTimeout = 30 * time.Second
)

// VariantAutoscalingReconciler reconciles a variantAutoscaling object
type VariantAutoscalingReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	PromAPI                 promv1.API
	MetricsCache            *collector.ModelMetricsCache       // Cache for model-level Prometheus metrics
	ScaleToZeroMetricsCache *collector.ScaleToZeroMetricsCache // Cache for scale-to-zero internal metrics
}

// cacheCleanupRunnable implements manager.Runnable for periodic cache cleanup
type cacheCleanupRunnable struct {
	cache           *collector.ModelMetricsCache
	cleanupInterval time.Duration
}

// Start implements manager.Runnable
func (r *cacheCleanupRunnable) Start(ctx context.Context) error {
	ticker := time.NewTicker(r.cleanupInterval)
	defer ticker.Stop()

	logger.Log.Info("Cache cleanup runnable started")
	for {
		select {
		case <-ticker.C:
			r.cache.Cleanup()
			logger.Log.Debug("Metrics cache cleanup completed")
		case <-ctx.Done():
			logger.Log.Info("Stopping metrics cache cleanup runnable due to context cancellation")
			return nil
		}
	}
}

// +kubebuilder:rbac:groups=llmd.ai,resources=variantautoscalings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=llmd.ai,resources=variantautoscalings/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=llmd.ai,resources=variantautoscalings/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=nodes/status,verbs=get;list;update;patch;watch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;update;list;watch

const (
	configMapName      = "workload-variant-autoscaler-variantautoscaling-config"
	configMapNamespace = "workload-variant-autoscaler-system"
)

func initMetricsEmitter() {
	logger.Log.Info("Creating metrics emitter instance")
	// Force initialization of metrics by creating a metrics emitter
	_ = metrics.NewMetricsEmitter()
	logger.Log.Info("Metrics emitter created successfully")
}

func (r *VariantAutoscalingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Default requeue duration (used for transient errors)
	requeueDuration := DefaultReconciliationInterval

	// Read optimization config (contains reconciliation interval)
	interval, err := r.readOptimizationConfig(ctx)
	if err != nil {
		logger.Log.Error(err, "Unable to read optimization config, using default interval")
		// Don't fail reconciliation - use default and continue
	} else if interval != "" {
		if parsedDuration, parseErr := time.ParseDuration(interval); parseErr != nil {
			logger.Log.Error(parseErr, "Invalid reconciliation interval format, using default",
				"configured", interval,
				"default", requeueDuration.String())
		} else {
			requeueDuration = parsedDuration
		}
	}

	if strings.EqualFold(os.Getenv("WVA_SCALE_TO_ZERO"), "true") {
		logger.Log.Info("Scaling to zero is enabled!")
	}

	// Read accelerator configuration (required)
	acceleratorCm, err := r.readAcceleratorConfig(ctx, "accelerator-unit-costs", configMapNamespace)
	if err != nil {
		logger.Log.Error(err, "Unable to read accelerator configMap, will retry",
			"requeueAfter", requeueDuration.String())
		return ctrl.Result{RequeueAfter: requeueDuration}, nil
	}

	// Read service class configuration (required)
	serviceClassCm, err := r.readServiceClassConfig(ctx, "service-classes-config", configMapNamespace)
	if err != nil {
		logger.Log.Error(err, "Unable to read service class configMap, will retry",
			"requeueAfter", requeueDuration.String())
		return ctrl.Result{RequeueAfter: requeueDuration}, nil
	}

	// Read scale-to-zero configuration (optional - falls back to global defaults if not found)
	scaleToZeroConfigData, err := r.readScaleToZeroConfig(ctx, "model-scale-to-zero-config", configMapNamespace)
	if err != nil {
		logger.Log.Info("Scale-to-zero config not found, using global defaults", "error", err.Error())
		scaleToZeroConfigData = make(utils.ScaleToZeroConfigData)
	}

	var variantAutoscalingList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
	if err := r.List(ctx, &variantAutoscalingList); err != nil {
		logger.Log.Error(err, "unable to list variantAutoscaling resources")
		return ctrl.Result{}, err
	}

	activeVAs := filterActiveVariantAutoscalings(variantAutoscalingList.Items)

	if len(activeVAs) == 0 {
		logger.Log.Info("No active VariantAutoscalings found, skipping optimization")
		return ctrl.Result{}, nil
	}

	// Check for variants using default variantCost and log warnings
	// Default cost is "10" as defined in CRD (api/v1alpha1/variantautoscaling_types.go)
	if len(activeVAs) > 1 {
		const defaultCost = "10" // Must match CRD default value
		variantsWithDefaultCost := []string{}
		for _, va := range activeVAs {
			if va.Spec.VariantCost == "" || va.Spec.VariantCost == defaultCost {
				variantsWithDefaultCost = append(variantsWithDefaultCost, va.Name)
			}
		}
		if len(variantsWithDefaultCost) > 0 {
			logger.Log.Info("Warning: Multiple variants detected with some using default variantCost",
				"totalVariants", len(activeVAs),
				"variantsUsingDefault", len(variantsWithDefaultCost),
				"variantNames", strings.Join(variantsWithDefaultCost, ", "),
				"recommendation", "Set explicit variantCost for accurate cost comparisons")
		}
	}

	// WVA operates in unlimited mode - no cluster inventory collection needed
	systemData := utils.CreateSystemData(acceleratorCm, serviceClassCm)

	updateList, vaMap, allAnalyzerResponses, err := r.prepareVariantAutoscalings(ctx, activeVAs, acceleratorCm, serviceClassCm, systemData, scaleToZeroConfigData)
	if err != nil {
		logger.Log.Error(err, "failed to prepare variant autoscalings")
		return ctrl.Result{}, err
	}

	// analyze
	system := inferno.NewSystem()
	optimizerSpec := system.SetFromSpec(&systemData.Spec)
	optimizer := infernoSolver.NewOptimizerFromSpec(optimizerSpec)
	manager := infernoManager.NewManager(system, optimizer)

	modelAnalyzer := analyzer.NewModelAnalyzer(system)
	for _, s := range system.Servers() {
		// Safely get VA from map with nil check
		va, ok := vaMap[s.Name()]
		if !ok || va == nil {
			logger.Log.Warn("VA not found in map for server", "serverName", s.Name())
			continue
		}

		modelAnalyzeResponse := modelAnalyzer.AnalyzeModel(ctx, *va)
		if len(modelAnalyzeResponse.Allocations) == 0 {
			logger.Log.Info("No potential allocations found for server", "serverName", s.Name())
			continue
		}
		allAnalyzerResponses[s.Name()] = modelAnalyzeResponse
	}

	// Log system data prepared for optimization in a single structured message
	logger.Log.Debug("System data prepared for optimization",
		"capacity", utils.MarshalStructToJsonString(systemData.Spec.Capacity),
		"accelerators", utils.MarshalStructToJsonString(systemData.Spec.Accelerators),
		"serviceClasses", utils.MarshalStructToJsonString(systemData.Spec.ServiceClasses),
		"models", utils.MarshalStructToJsonString(systemData.Spec.Models),
		"optimizer", utils.MarshalStructToJsonString(systemData.Spec.Optimizer),
		"servers", utils.MarshalStructToJsonString(systemData.Spec.Servers))

	engine := variantAutoscalingOptimizer.NewVariantAutoscalingsEngine(manager, system)

	optimizedAllocation, err := engine.Optimize(ctx, *updateList, allAnalyzerResponses, &scaleToZeroConfigData, r.ScaleToZeroMetricsCache)
	if err != nil {
		logger.Log.Warnw("Optimization failed - will emit fallback metrics for all variants",
			"error", err,
			"variantCount", len(updateList.Items))

		// Emit fallback metrics even when optimization fails
		// The variants in updateList already have fallback DesiredOptimizedAlloc set
		if emitErr := r.applyOptimizedAllocations(ctx, updateList, optimizedAllocation, allAnalyzerResponses, scaleToZeroConfigData, activeVAs); emitErr != nil {
			logger.Log.Error(emitErr, "failed to emit fallback metrics after optimization failure")
		} else {
			logger.Log.Info("Successfully emitted fallback metrics after optimization failure",
				"variantCount", len(updateList.Items))
		}

		return ctrl.Result{RequeueAfter: requeueDuration}, nil
	}

	logger.Log.Debug("Optimization completed successfully, emitting optimization metrics")
	logger.Log.Debug("Optimized allocation map - ", "numKeys: ", len(optimizedAllocation), ", updateList_count: ", len(updateList.Items))

	// Validate optimizer returned results for all variants
	if len(optimizedAllocation) > 0 && len(optimizedAllocation) < len(updateList.Items) {
		logger.Log.Warnw("Optimizer returned partial results - some variants will use fallback allocation",
			"expected", len(updateList.Items),
			"received", len(optimizedAllocation),
			"missing", len(updateList.Items)-len(optimizedAllocation))

		// Log which variants are missing
		missingVariants := make([]string, 0)
		for i := range updateList.Items {
			if _, found := optimizedAllocation[updateList.Items[i].Name]; !found {
				missingVariants = append(missingVariants, updateList.Items[i].Name)
			}
		}
		if len(missingVariants) > 0 {
			logger.Log.Debug("Variants missing from optimizer results", "variants", missingVariants)
		}
	}

	if err := r.applyOptimizedAllocations(ctx, updateList, optimizedAllocation, allAnalyzerResponses, scaleToZeroConfigData, activeVAs); err != nil {
		// If we fail to apply optimized allocations, we log the error
		// In next reconcile, the controller will retry.
		logger.Log.Error(err, "failed to apply optimized allocations")
		return ctrl.Result{RequeueAfter: requeueDuration}, nil
	}

	return ctrl.Result{RequeueAfter: requeueDuration}, nil
}

// filterActiveVariantAutoscalings returns only those VAs not marked for deletion.
func filterActiveVariantAutoscalings(items []llmdVariantAutoscalingV1alpha1.VariantAutoscaling) []llmdVariantAutoscalingV1alpha1.VariantAutoscaling {
	active := make([]llmdVariantAutoscalingV1alpha1.VariantAutoscaling, 0, len(items))
	for _, va := range items {
		if va.DeletionTimestamp.IsZero() {
			active = append(active, va)
		} else {
			logger.Log.Info("skipping deleted variantAutoscaling", "name", va.Name)
		}
	}
	return active
}

// addFailedVariant fetches the latest VA and adds it to updateList with a failed condition.
// This helper reduces code duplication in prepareVariantAutoscalings.
func (r *VariantAutoscalingReconciler) addFailedVariant(
	ctx context.Context,
	va llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	reason string,
	message string,
	updateList *llmdVariantAutoscalingV1alpha1.VariantAutoscalingList,
) bool {
	var updateVA llmdVariantAutoscalingV1alpha1.VariantAutoscaling
	if err := utils.GetVariantAutoscalingWithBackoff(ctx, r.Client, va.Name, va.Namespace, &updateVA); err != nil {
		logger.Log.Error(err, "Unable to get VariantAutoscaling for failed variant",
			"name", va.Name,
			"namespace", va.Namespace)
		return false
	}

	llmdVariantAutoscalingV1alpha1.SetCondition(&updateVA,
		llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
		metav1.ConditionFalse,
		reason,
		message)

	updateList.Items = append(updateList.Items, updateVA)
	return true
}

// addVariantWithFallbackAllocation adds a variant to the update list with fallback allocation
// when metrics or optimization data is unavailable. This ensures metrics are always emitted
// even when optimization cannot proceed normally.
//
// Fallback logic:
//  1. Respect replica bounds (minReplicas, maxReplicas)
//  2. If scale-to-zero is disabled and no minReplicas: set 1 replica for cheapest variant only
//  3. If scale-to-zero is enabled AND aggregate metrics available: scale to zero only if load == 0
//  4. Controller-centric approach (when aggregateLoad is nil and retention NOT exceeded):
//     a) First run: use current deployment replicas as baseline
//     b) Subsequent runs: use previous optimized allocation to maintain controller intent
//     c) Apply max(minReplicas, baseline) to respect bounds
//  5. Retention period checks (when aggregateLoad is nil and time since LastUpdate > retentionPeriod):
//     a) If scale-to-zero enabled AND all variants have minReplicas=0 → scale all to 0
//     b) If scale-to-zero disabled AND all variants have minReplicas=0 → cheapest to 1, others to 0
//     c) If any variant has minReplicas > 0 → set each variant to its minReplicas value
func addVariantWithFallbackAllocation(
	updateVA *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	deploy *appsv1.Deployment,
	reason string,
	message string,
	updateList *llmdVariantAutoscalingV1alpha1.VariantAutoscalingList,
	scaleToZeroConfigData utils.ScaleToZeroConfigData,
	modelName string,
	aggregateLoad *float64,
	isCheapestVariant bool,
	allVariants []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	retentionPeriod time.Duration,
) {
	// Log warning about using fallback allocation
	logger.Log.Warnw("Using fallback allocation for variant - optimization data unavailable",
		"variant", updateVA.Name,
		"namespace", updateVA.Namespace,
		"model", modelName,
		"reason", reason)

	// Get current replicas from deployment
	var currentReplicas int32
	if deploy != nil {
		// Prefer status replicas if deployment has been reconciled
		if deploy.Status.ObservedGeneration > 0 {
			currentReplicas = deploy.Status.Replicas
		} else if deploy.Spec.Replicas != nil {
			// Fallback to spec if status not ready yet
			currentReplicas = *deploy.Spec.Replicas
		}
	}

	// Systematic fallback logic:
	// Priority 1: Determine desired replicas based on load (without bounds)
	// Priority 2: Enforce minReplicas (always respected)
	// Priority 3: Enforce maxReplicas (always respected)

	var desiredReplicas int32
	scaleToZeroEnabled := utils.IsScaleToZeroEnabled(scaleToZeroConfigData, modelName)

	// Step 1: Determine desired replicas based on load conditions
	if aggregateLoad == nil {
		// Metrics unavailable - check retention period for scaling decisions

		// Check if retention period has been exceeded (no activity detected)
		lastUpdate := updateVA.Status.DesiredOptimizedAlloc.LastUpdate
		retentionPeriodExceeded := isRetentionPeriodExceeded(lastUpdate, retentionPeriod)

		// Apply retention period logic if threshold exceeded
		if retentionPeriodExceeded {
			allMinReplicasZero := allVariantsHaveMinReplicasZero(allVariants, modelName)

			if allMinReplicasZero && scaleToZeroEnabled {
				// Case 1: Scale-to-zero enabled, all minReplicas=0 → scale all to 0
				desiredReplicas = 0
				message = fmt.Sprintf("%s. Metrics unavailable, no activity for %v (> retention period %v), scale-to-zero enabled, scaling to 0",
					message, time.Since(lastUpdate.Time), retentionPeriod)
				logger.Log.Info("Scaling to zero based on retention period (scale-to-zero enabled)",
					"variant", updateVA.Name,
					"modelID", modelName,
					"timeSinceUpdate", time.Since(lastUpdate.Time),
					"retentionPeriod", retentionPeriod)
			} else if allMinReplicasZero && !scaleToZeroEnabled {
				// Case 2: Scale-to-zero disabled, all minReplicas=0 → cheapest to 1, others to 0
				if isCheapestVariant {
					desiredReplicas = 1
					message = fmt.Sprintf("%s. Metrics unavailable, no activity for %v (> retention period %v), scale-to-zero disabled, setting cheapest variant to 1",
						message, time.Since(lastUpdate.Time), retentionPeriod)
				} else {
					desiredReplicas = 0
					message = fmt.Sprintf("%s. Metrics unavailable, no activity for %v (> retention period %v), scale-to-zero disabled, setting non-cheapest variant to 0",
						message, time.Since(lastUpdate.Time), retentionPeriod)
				}
				logger.Log.Info("Applying retention period logic with scale-to-zero disabled",
					"variant", updateVA.Name,
					"modelID", modelName,
					"isCheapest", isCheapestVariant,
					"desiredReplicas", desiredReplicas,
					"timeSinceUpdate", time.Since(lastUpdate.Time),
					"retentionPeriod", retentionPeriod)
			} else {
				// Case 3: Some variant has minReplicas > 0 → use this variant's minReplicas
				var minReplicasValue int32
				if updateVA.Spec.MinReplicas != nil {
					minReplicasValue = *updateVA.Spec.MinReplicas
				}
				desiredReplicas = minReplicasValue
				message = fmt.Sprintf("%s. Metrics unavailable, no activity for %v (> retention period %v), some variants have minReplicas > 0, using minReplicas=%d",
					message, time.Since(lastUpdate.Time), retentionPeriod, minReplicasValue)
				logger.Log.Info("Applying retention period logic with minReplicas bounds",
					"variant", updateVA.Name,
					"modelID", modelName,
					"minReplicas", minReplicasValue,
					"desiredReplicas", desiredReplicas,
					"timeSinceUpdate", time.Since(lastUpdate.Time),
					"retentionPeriod", retentionPeriod)
			}
		} else {
			// Retention period NOT exceeded - use controller-centric approach
			// Priority: previous optimized decision > current deployment state

			var minReplicasValue int32
			if updateVA.Spec.MinReplicas != nil {
				minReplicasValue = *updateVA.Spec.MinReplicas
			}

			// Determine baseline replicas: use previous optimized if available, else current
			var baselineReplicas int32
			if !updateVA.Status.DesiredOptimizedAlloc.LastUpdate.IsZero() {
				// Use previous optimized allocation - maintain controller intent
				baselineReplicas = updateVA.Status.DesiredOptimizedAlloc.NumReplicas
				logger.Log.Info("Using previous optimized allocation as baseline during metrics unavailability",
					"variant", updateVA.Name,
					"previousOptimized", baselineReplicas,
					"currentReplicas", currentReplicas)
			} else {
				// First run - use current deployment state as baseline
				baselineReplicas = currentReplicas
				logger.Log.Info("First run: using current deployment replicas as baseline",
					"variant", updateVA.Name,
					"currentReplicas", currentReplicas)
			}

			desiredReplicas = max(minReplicasValue, baselineReplicas)

			// Model-level safety: If result is 0 and scale-to-zero is disabled, ensure cheapest variant has 1 replica
			// This prevents all variants from being 0 when we don't know the load
			if desiredReplicas == 0 && !scaleToZeroEnabled && isCheapestVariant {
				desiredReplicas = 1
				message = fmt.Sprintf("%s. Metrics unavailable, scale-to-zero disabled, ensuring cheapest variant has 1 replica", message)
			} else {
				if !updateVA.Status.DesiredOptimizedAlloc.LastUpdate.IsZero() {
					message = fmt.Sprintf("%s. Metrics unavailable, maintaining controller intent: max(minReplicas=%d, previousOptimized=%d) = %d",
						message, minReplicasValue, baselineReplicas, desiredReplicas)
				} else {
					message = fmt.Sprintf("%s. Metrics unavailable (first run), using max(minReplicas=%d, current=%d) = %d",
						message, minReplicasValue, currentReplicas, desiredReplicas)
				}
			}
		}
	} else if *aggregateLoad > 0 {
		// Active load - preserve current state
		desiredReplicas = currentReplicas
		message = fmt.Sprintf("%s. Active load detected (%.2f), maintaining current replicas (%d)", message, *aggregateLoad, currentReplicas)
	} else {
		// aggregateLoad == 0 (no load)
		if scaleToZeroEnabled {
			// Scale-to-zero enabled and no load -> set to 0 (will be bounded by minReplicas later)
			desiredReplicas = 0
			message = fmt.Sprintf("%s. Scale-to-zero enabled and no load, setting to 0 (subject to minReplicas)", message)
		} else {
			// Scale-to-zero disabled and no load -> keep 1 replica on cheapest variant
			if isCheapestVariant {
				desiredReplicas = 1
				message = fmt.Sprintf("%s. Scale-to-zero disabled, no load, setting cheapest variant to 1 replica", message)
			} else {
				desiredReplicas = 0
				message = fmt.Sprintf("%s. Scale-to-zero disabled, no load, setting non-cheapest variant to 0 (subject to minReplicas)", message)
			}
		}
	}

	// Apply replica bounds (clamp to [minReplicas, maxReplicas])
	desiredReplicas, _ = applyReplicaBounds(desiredReplicas, updateVA.Spec.MinReplicas, updateVA.Spec.MaxReplicas, updateVA.Name)

	// Set current allocation
	updateVA.Status.CurrentAlloc = llmdVariantAutoscalingV1alpha1.Allocation{
		NumReplicas: currentReplicas,
	}

	// Set desired allocation with computed fallback value
	newAlloc := llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
		NumReplicas: desiredReplicas,
		LastRunTime: metav1.Now(),
		Reason:      message,
	}

	// Update LastUpdate only if NumReplicas or Reason changed
	previousAlloc := updateVA.Status.DesiredOptimizedAlloc
	if previousAlloc.NumReplicas != newAlloc.NumReplicas || previousAlloc.Reason != newAlloc.Reason {
		newAlloc.LastUpdate = metav1.Now()
	} else {
		// Preserve previous LastUpdate if nothing changed
		newAlloc.LastUpdate = previousAlloc.LastUpdate
	}

	updateVA.Status.DesiredOptimizedAlloc = newAlloc

	// Mark optimization as succeeded with fallback explanation
	llmdVariantAutoscalingV1alpha1.SetCondition(updateVA,
		llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
		metav1.ConditionTrue,
		reason,
		message)

	// Add to update list for metric emission
	updateList.Items = append(updateList.Items, *updateVA)

	logger.Log.Info("Added variant with fallback allocation for metric emission",
		"variant", updateVA.Name,
		"currentReplicas", currentReplicas,
		"desiredReplicas", desiredReplicas,
		"scaleToZeroEnabled", scaleToZeroEnabled,
		"isCheapest", isCheapestVariant,
		"reason", reason)
}

// max returns the maximum of two int32 values
func max(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

// isRetentionPeriodExceeded checks if the retention period has been exceeded since lastUpdate.
// Returns true if lastUpdate is set and the time since lastUpdate exceeds retentionPeriod.
// Returns false if lastUpdate is zero (never set) or if within retention period.
func isRetentionPeriodExceeded(lastUpdate metav1.Time, retentionPeriod time.Duration) bool {
	if lastUpdate.IsZero() {
		return false
	}
	return time.Since(lastUpdate.Time) > retentionPeriod
}

// applyRetentionPeriodScaling applies scale-to-zero logic when retention period is exceeded.
// Returns the desired replica count and reason message.
func applyRetentionPeriodScaling(
	va *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	allVariants []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	scaleToZeroConfigData utils.ScaleToZeroConfigData,
	retentionPeriod time.Duration,
	pathName string, // "Fallback" or "Last resort" for logging
) (desiredReplicas int32, reason string) {
	modelName := va.Spec.ModelID
	scaleToZeroEnabled := utils.IsScaleToZeroEnabled(scaleToZeroConfigData, modelName)
	allMinReplicasZero := allVariantsHaveMinReplicasZero(allVariants, modelName)
	isCheapest := isCheapestVariantForModel(va, allVariants, modelName)

	minReplicasValue := int32(0)
	if va.Spec.MinReplicas != nil {
		minReplicasValue = *va.Spec.MinReplicas
	}

	if allMinReplicasZero && scaleToZeroEnabled {
		// Case 1: Scale-to-zero enabled, all minReplicas=0 → scale to 0
		desiredReplicas = 0
		reason = fmt.Sprintf("%s: retention period exceeded (>%v), scale-to-zero enabled, scaling to 0",
			pathName, retentionPeriod)
		logger.Log.Info("Scaling to zero after retention period",
			"path", pathName,
			"variant", va.Name,
			"modelID", modelName,
			"retentionPeriod", retentionPeriod)
	} else if allMinReplicasZero && !scaleToZeroEnabled {
		// Case 2: Scale-to-zero disabled, all minReplicas=0 → cheapest to 1, others to 0
		if isCheapest {
			desiredReplicas = 1
			reason = fmt.Sprintf("%s: retention period exceeded (>%v), scale-to-zero disabled, cheapest variant set to 1",
				pathName, retentionPeriod)
		} else {
			desiredReplicas = 0
			reason = fmt.Sprintf("%s: retention period exceeded (>%v), scale-to-zero disabled, non-cheapest variant set to 0",
				pathName, retentionPeriod)
		}
		logger.Log.Info("Applying scale-to-zero disabled logic after retention period",
			"path", pathName,
			"variant", va.Name,
			"modelID", modelName,
			"isCheapest", isCheapest,
			"desiredReplicas", desiredReplicas)
	} else {
		// Case 3: Some variant has minReplicas > 0 → use this variant's minReplicas
		desiredReplicas = minReplicasValue
		reason = fmt.Sprintf("%s: retention period exceeded (>%v), using minReplicas=%d",
			pathName, retentionPeriod, minReplicasValue)
		logger.Log.Info("Using minReplicas after retention period",
			"path", pathName,
			"variant", va.Name,
			"modelID", modelName,
			"minReplicas", minReplicasValue)
	}

	return desiredReplicas, reason
}

// applyReplicaBounds applies minReplicas and maxReplicas bounds to the desired replica count.
// Returns the clamped replica count and whether it was changed.
func applyReplicaBounds(
	desiredReplicas int32,
	minReplicas *int32,
	maxReplicas *int32,
	variantName string,
) (clampedReplicas int32, boundsApplied bool) {
	clampedReplicas = desiredReplicas
	boundsApplied = false

	// Apply minReplicas bound
	if minReplicas != nil {
		minVal := *minReplicas
		if clampedReplicas < minVal {
			logger.Log.Info("Clamping replicas to minReplicas",
				"variant", variantName,
				"original", clampedReplicas,
				"clamped", minVal)
			clampedReplicas = minVal
			boundsApplied = true
		}
	}

	// Apply maxReplicas bound
	if maxReplicas != nil {
		maxVal := *maxReplicas
		if clampedReplicas > maxVal {
			logger.Log.Info("Clamping replicas to maxReplicas",
				"variant", variantName,
				"original", clampedReplicas,
				"clamped", maxVal)
			clampedReplicas = maxVal
			boundsApplied = true
		}
	}

	return clampedReplicas, boundsApplied
}

// applyFallbackAllocation applies fallback allocation logic when optimizer doesn't run.
// This consolidates Path 2 (has precomputed fallback) and Path 3 (no precomputed fallback).
// Returns error if allocation computation fails.
func applyFallbackAllocation(
	updateVa *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	allVariants []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	scaleToZeroConfigData utils.ScaleToZeroConfigData,
	hasPrecomputedFallback bool,
	pathLabel string, // "Fallback" or "Last resort"
) {
	modelName := updateVa.Spec.ModelID
	retentionPeriod := utils.GetScaleToZeroRetentionPeriod(scaleToZeroConfigData, modelName)
	previousAlloc := updateVa.Status.DesiredOptimizedAlloc

	// Check if retention period has been exceeded
	retentionPeriodExceeded := isRetentionPeriodExceeded(previousAlloc.LastUpdate, retentionPeriod)

	var desiredReplicas int32
	var reason string

	if retentionPeriodExceeded {
		// Retention period exceeded - apply scale-to-zero logic
		desiredReplicas, reason = applyRetentionPeriodScaling(
			updateVa,
			allVariants,
			scaleToZeroConfigData,
			retentionPeriod,
			pathLabel,
		)

		// Apply maxReplicas bound (minReplicas already applied in scale-to-zero logic)
		clampedReplicas, boundsApplied := applyReplicaBounds(desiredReplicas, nil, updateVa.Spec.MaxReplicas, updateVa.Name)
		if boundsApplied {
			reason = fmt.Sprintf("%s (clamped to maxReplicas=%d)", reason, clampedReplicas)
			desiredReplicas = clampedReplicas
		}

		// Create allocation with helper
		updateVa.Status.DesiredOptimizedAlloc = createOptimizedAllocWithUpdate(desiredReplicas, reason, previousAlloc)
	} else {
		// Retention period NOT exceeded
		if hasPrecomputedFallback {
			// PATH 2: Preserve existing allocation from previous reconciliation
			// Note: previousAlloc already contains the current DesiredOptimizedAlloc from updateVa (line 637)
			// We don't need to copy it again - just re-apply bounds and update timestamps if needed

			// Re-apply bounds to respect any CRD changes since the allocation was computed
			originalReplicas := previousAlloc.NumReplicas
			clampedReplicas, boundsApplied := applyReplicaBounds(
				originalReplicas,
				updateVa.Spec.MinReplicas,
				updateVa.Spec.MaxReplicas,
				updateVa.Name,
			)

			// Update allocation if bounds were applied
			if boundsApplied {
				updateVa.Status.DesiredOptimizedAlloc.NumReplicas = clampedReplicas
				updateVa.Status.DesiredOptimizedAlloc.Reason = fmt.Sprintf("%s (clamped from %d to %d for bounds)",
					previousAlloc.Reason, originalReplicas, clampedReplicas)
				updateVa.Status.DesiredOptimizedAlloc.LastUpdate = metav1.Now()
			}

			// If Reason is empty, set a default reason
			if updateVa.Status.DesiredOptimizedAlloc.Reason == "" {
				updateVa.Status.DesiredOptimizedAlloc.Reason = "Fallback: preserving previous allocation (no optimizer solution)"
				if previousAlloc.Reason != updateVa.Status.DesiredOptimizedAlloc.Reason {
					updateVa.Status.DesiredOptimizedAlloc.LastUpdate = metav1.Now()
				}
			}

			// If LastUpdate is still zero, set it now
			if updateVa.Status.DesiredOptimizedAlloc.LastUpdate.IsZero() {
				updateVa.Status.DesiredOptimizedAlloc.LastUpdate = metav1.Now()
			}

			logger.Log.Info("Using fallback allocation (retention period not exceeded)",
				"variantName", updateVa.Name,
				"currentReplicas", updateVa.Status.CurrentAlloc.NumReplicas,
				"desiredReplicas", updateVa.Status.DesiredOptimizedAlloc.NumReplicas,
				"boundChanged", boundsApplied,
				"timeSinceUpdate", time.Since(previousAlloc.LastUpdate.Time),
				"retentionPeriod", retentionPeriod)
		} else {
			// PATH 3: Controller-centric approach
			minReplicasValue := int32(0)
			if updateVa.Spec.MinReplicas != nil {
				minReplicasValue = *updateVa.Spec.MinReplicas
			}

			var baselineReplicas int32
			// Note: We check LastUpdate (not NumReplicas >= 0) because NumReplicas defaults to 0,
			// and would incorrectly treat first-run scenarios as having a previous allocation.
			// LastUpdate is only set when a controller decision was actually made.
			if !previousAlloc.LastUpdate.IsZero() {
				// Check if deployment was discovered late (current changed from 0 to non-zero)
				// This handles: User creates VA on existing deployment, deployment discovered in recon 2
				if updateVa.Status.CurrentAlloc.NumReplicas > 0 && previousAlloc.NumReplicas < updateVa.Status.CurrentAlloc.NumReplicas {
					// Current is now non-zero and larger than previous decision
					// Likely means deployment was just discovered - reset to current
					baselineReplicas = updateVa.Status.CurrentAlloc.NumReplicas
					logger.Log.Info("Last resort - current increased significantly (deployment discovered late), resetting to current",
						"variantName", updateVa.Name,
						"previousDesired", previousAlloc.NumReplicas,
						"currentReplicas", updateVa.Status.CurrentAlloc.NumReplicas)
					reason = fmt.Sprintf("Last resort: deployment discovered late, using current=%d (was %d)",
						updateVa.Status.CurrentAlloc.NumReplicas, previousAlloc.NumReplicas)
				} else {
					// Use previous optimized allocation - maintain controller intent
					baselineReplicas = previousAlloc.NumReplicas
					logger.Log.Info("Last resort - using previous optimized allocation as baseline",
						"variantName", updateVa.Name,
						"previousOptimized", baselineReplicas,
						"currentReplicas", updateVa.Status.CurrentAlloc.NumReplicas)
					reason = fmt.Sprintf("Last resort: maintaining controller intent: max(minReplicas=%d, previousOptimized=%d)",
						minReplicasValue, baselineReplicas)
				}
			} else {
				// First run - use current deployment state as baseline
				// IMPORTANT: If current=0, deployment may not have been discovered yet
				// Check if this is truly first run (no previous Reason) vs current temporarily 0
				if updateVa.Status.CurrentAlloc.NumReplicas == 0 && minReplicasValue == 0 {
					if previousAlloc.Reason == "" {
						// First run ever with current=0 - apply defensive logic
						// Check if other variants for this model have non-zero replicas
						hasOtherRunningVariants := false
						for _, v := range allVariants {
							if v.Spec.ModelID == updateVa.Spec.ModelID && v.Name != updateVa.Name {
								if v.Status.CurrentAlloc.NumReplicas > 0 || v.Status.DesiredOptimizedAlloc.NumReplicas > 0 {
									hasOtherRunningVariants = true
									logger.Log.Info("Found other running variant for same model",
										"variantName", updateVa.Name,
										"otherVariant", v.Name,
										"otherCurrent", v.Status.CurrentAlloc.NumReplicas,
										"otherDesired", v.Status.DesiredOptimizedAlloc.NumReplicas)
									break
								}
							}
						}

						// Decision logic:
						// - If other variants are running → safe to start at 0 (they handle load)
						// - If no other variants → use safe default of 1
						//   This prevents both:
						//   1. Premature scale-to-zero before deployment discovery (if scale-to-zero enabled)
						//   2. All variants at 0 for the model (if scale-to-zero disabled)
						//
						// Note: minReplicas is per-variant, scale-to-zero is per-model.
						// Even if scale-to-zero is disabled, individual variants can have minReplicas=0,
						// as long as at least one variant for the model has replicas > 0.
						if hasOtherRunningVariants {
							baselineReplicas = 0
							logger.Log.Info("Last resort - first run with current=0, other variants running, starting at 0",
								"variantName", updateVa.Name)
							reason = "Last resort: first run, other variants handling load, starting at 0"
						} else {
							// No other variants - use safe default to ensure at least one variant running
							baselineReplicas = 1
							logger.Log.Info("Last resort - first run with current=0, no other variants, using safe default",
								"variantName", updateVa.Name,
								"safeDefault", baselineReplicas)
							reason = "Last resort: first run with current=0, using safe default of 1 (waiting for deployment discovery)"
						}
					} else {
						// Not first run - current temporarily 0, preserve previous desired value
						// This handles transient deployment lookup failures
						baselineReplicas = previousAlloc.NumReplicas
						logger.Log.Info("Last resort - current=0 but not first run, preserving previous desired",
							"variantName", updateVa.Name,
							"previousDesired", baselineReplicas)
						reason = fmt.Sprintf("Last resort: preserving previous desired=%d (current temporarily 0)", baselineReplicas)
					}
				} else {
					baselineReplicas = updateVa.Status.CurrentAlloc.NumReplicas
					logger.Log.Info("Last resort - first run, using current deployment replicas as baseline",
						"variantName", updateVa.Name,
						"currentReplicas", updateVa.Status.CurrentAlloc.NumReplicas)
					reason = fmt.Sprintf("Last resort: first run, using max(minReplicas=%d, current=%d)",
						minReplicasValue, baselineReplicas)
				}
			}

			desiredReplicas = max(minReplicasValue, baselineReplicas)

			// Apply maxReplicas bound
			clampedReplicas, boundsApplied := applyReplicaBounds(desiredReplicas, nil, updateVa.Spec.MaxReplicas, updateVa.Name)
			if boundsApplied {
				reason = fmt.Sprintf("%s (clamped to maxReplicas=%d)", reason, clampedReplicas)
				desiredReplicas = clampedReplicas
			} else {
				// Complete the reason message for non-retention case
				reason = fmt.Sprintf("%s = %d", reason, desiredReplicas)
			}

			// Create allocation with helper
			updateVa.Status.DesiredOptimizedAlloc = createOptimizedAllocWithUpdate(desiredReplicas, reason, previousAlloc)
		}
	}
}

// createOptimizedAllocWithUpdate creates an OptimizedAlloc and updates LastUpdate timestamp
// if NumReplicas or Reason changed compared to previous allocation.
func createOptimizedAllocWithUpdate(
	desiredReplicas int32,
	reason string,
	previousAlloc llmdVariantAutoscalingV1alpha1.OptimizedAlloc,
) llmdVariantAutoscalingV1alpha1.OptimizedAlloc {
	newAlloc := llmdVariantAutoscalingV1alpha1.OptimizedAlloc{
		NumReplicas: desiredReplicas,
		Reason:      reason,
	}

	// Update LastUpdate only if NumReplicas or Reason changed
	if previousAlloc.NumReplicas != newAlloc.NumReplicas || previousAlloc.Reason != newAlloc.Reason {
		newAlloc.LastUpdate = metav1.Now()
	} else {
		// Preserve previous LastUpdate if nothing changed
		newAlloc.LastUpdate = previousAlloc.LastUpdate
	}

	return newAlloc
}

// updateConditionsForAllocation updates the conditions for a VA based on the allocation decision path.
// Preserves MetricsAvailable from preparation phase and sets OptimizationReady based on the path.
func updateConditionsForAllocation(
	updateVa *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	preparedVa *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	hasOptimizedAlloc bool,
) {
	// Copy MetricsAvailable condition from preparation phase
	if metricsCond := llmdVariantAutoscalingV1alpha1.GetCondition(preparedVa, llmdVariantAutoscalingV1alpha1.TypeMetricsAvailable); metricsCond != nil {
		llmdVariantAutoscalingV1alpha1.SetCondition(updateVa,
			llmdVariantAutoscalingV1alpha1.TypeMetricsAvailable,
			metricsCond.Status, metricsCond.Reason, metricsCond.Message)
	}

	// Set OptimizationReady condition based on allocation path
	desiredAlloc := updateVa.Status.DesiredOptimizedAlloc
	if hasOptimizedAlloc {
		// Path 1: Optimizer solution
		llmdVariantAutoscalingV1alpha1.SetCondition(updateVa,
			llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
			metav1.ConditionTrue,
			llmdVariantAutoscalingV1alpha1.ReasonOptimizationSucceeded,
			fmt.Sprintf("Optimization completed: %d replicas on %s",
				desiredAlloc.NumReplicas, updateVa.Spec.Accelerator))
	} else if desiredAlloc.Reason != "" {
		// Path 2/Path 3: Fallback or Last Resort
		llmdVariantAutoscalingV1alpha1.SetCondition(updateVa,
			llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
			metav1.ConditionTrue,
			llmdVariantAutoscalingV1alpha1.ReasonFallbackUsed,
			fmt.Sprintf("%s (%d replicas)", desiredAlloc.Reason, desiredAlloc.NumReplicas))
	} else {
		// Fallback: copy from preparation phase if available
		if optCond := llmdVariantAutoscalingV1alpha1.GetCondition(preparedVa, llmdVariantAutoscalingV1alpha1.TypeOptimizationReady); optCond != nil {
			llmdVariantAutoscalingV1alpha1.SetCondition(updateVa,
				llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
				optCond.Status, optCond.Reason, optCond.Message)
		}
	}
}

// isCheapestVariantForModel determines if the given variant is the cheapest among all variants for the same model.
// Cheapest is determined by accelerator count (fewer accelerators = cheaper).
func isCheapestVariantForModel(
	currentVariant *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	allVariants []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	modelName string,
) bool {
	if currentVariant == nil {
		return false
	}

	minAcceleratorCount := currentVariant.Spec.AcceleratorCount
	cheapestVariantID := currentVariant.Spec.VariantID

	// Find the variant with minimum accelerator count for this model
	for _, va := range allVariants {
		if va.Spec.ModelID != modelName {
			continue // Different model
		}
		if va.Spec.AcceleratorCount < minAcceleratorCount {
			minAcceleratorCount = va.Spec.AcceleratorCount
			cheapestVariantID = va.Spec.VariantID
		} else if va.Spec.AcceleratorCount == minAcceleratorCount {
			// If same count, choose by variant ID (deterministic)
			if va.Spec.VariantID < cheapestVariantID {
				cheapestVariantID = va.Spec.VariantID
			}
		}
	}

	return currentVariant.Spec.VariantID == cheapestVariantID
}

// allVariantsHaveMinReplicasZero checks if all variants for the given model have minReplicas set to 0 or nil.
// This is used to determine if scale-to-zero based on retention period is safe for the entire model.
func allVariantsHaveMinReplicasZero(
	allVariants []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	modelName string,
) bool {
	// Find at least one variant for this model
	foundVariant := false
	for _, va := range allVariants {
		if va.Spec.ModelID != modelName {
			continue // Different model
		}
		foundVariant = true

		// If any variant has minReplicas > 0, return false
		if va.Spec.MinReplicas != nil && *va.Spec.MinReplicas > 0 {
			return false
		}
	}

	// Return true only if we found at least one variant and all had minReplicas == 0 or nil
	return foundVariant
}

// prepareVariantAutoscalings collects and prepares all data for optimization.
func (r *VariantAutoscalingReconciler) prepareVariantAutoscalings(
	ctx context.Context,
	activeVAs []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	acceleratorCm map[string]map[string]string,
	serviceClassCm map[string]string,
	systemData *infernoConfig.SystemData,
	scaleToZeroConfigData utils.ScaleToZeroConfigData,
) (*llmdVariantAutoscalingV1alpha1.VariantAutoscalingList, map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling, map[string]*interfaces.ModelAnalyzeResponse, error) {
	var updateList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
	allAnalyzerResponses := make(map[string]*interfaces.ModelAnalyzeResponse)
	vaMap := make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling)

	for _, va := range activeVAs {
		// Check for context cancellation to enable graceful shutdown
		select {
		case <-ctx.Done():
			logger.Log.Info("Context cancelled during variant preparation, stopping early")
			return &updateList, vaMap, allAnalyzerResponses, ctx.Err()
		default:
			// Continue processing
		}

		modelName := va.Spec.ModelID
		if modelName == "" {
			logger.Log.Warn("VariantAutoscaling missing modelID, adding with failed condition",
				"name", va.Name)
			r.addFailedVariant(ctx, va,
				llmdVariantAutoscalingV1alpha1.ReasonOptimizationFailed,
				"ModelID is empty - cannot optimize",
				&updateList)
			continue
		}

		entry, className, err := utils.FindModelSLO(serviceClassCm, modelName)
		if err != nil {
			logger.Log.Error(err, "Failed to locate SLO for model, adding with failed condition",
				"name", va.Name,
				"model", modelName)
			r.addFailedVariant(ctx, va,
				llmdVariantAutoscalingV1alpha1.ReasonOptimizationFailed,
				fmt.Sprintf("SLO not found for model %s", modelName),
				&updateList)
			continue
		}
		logger.Log.Info("Found SLO for model",
			"model", modelName,
			"class", className,
			"sloTPOT", entry.SLOTPOT,
			"sloTTFT", entry.SLOTTFT)

		var deploy appsv1.Deployment
		err = utils.GetDeploymentWithBackoff(ctx, r.Client, va.Name, va.Namespace, &deploy)
		if err != nil {
			logger.Log.Error(err, "Failed to get Deployment, adding with failed condition",
				"name", va.Name)
			r.addFailedVariant(ctx, va,
				llmdVariantAutoscalingV1alpha1.ReasonOptimizationFailed,
				"Deployment not found",
				&updateList)
			continue
		}

		var updateVA llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		err = utils.GetVariantAutoscalingWithBackoff(ctx, r.Client, deploy.Name, deploy.Namespace, &updateVA)
		if err != nil {
			logger.Log.Error(err, "unable to get variantAutoscaling for deployment, using fallback allocation for metric emission - ", "deployment-name: ", deploy.Name, ", namespace: ", deploy.Namespace)
			// This is unusual - deployment exists but VA doesn't (should not happen normally)
			// Skip this one as we can't emit metrics without the VA object
			continue
		}

		// Validate and log the relationship between variant_name and variant_id
		// This helps users understand the dual-identifier pattern used in Prometheus metrics
		utils.ValidateVariantAutoscalingName(&updateVA)

		// Set ownerReference early, before metrics validation, to ensure it's always set
		// This ensures the VA will be garbage collected when the Deployment is deleted
		if !metav1.IsControlledBy(&updateVA, &deploy) {
			original := updateVA.DeepCopy()
			err := controllerutil.SetControllerReference(&deploy, &updateVA, r.Scheme, controllerutil.WithBlockOwnerDeletion(false))
			if err != nil {
				logger.Log.Error(err, "failed to set ownerReference - ", "variantAutoscaling-name: ", updateVA.Name)
				continue
			}

			// Patch metadata change (ownerReferences)
			patch := client.MergeFrom(original)
			if err := r.Patch(ctx, &updateVA, patch); err != nil {
				logger.Log.Error(err, "failed to patch ownerReference - ", "variantAutoscaling-name: ", updateVA.Name)
				continue
			}
			logger.Log.Info("Set ownerReference on VariantAutoscaling - ", "variantAutoscaling-name: ", updateVA.Name, ", owner: ", deploy.Name)
		}

		// Validate metrics availability before collecting metrics
		metricsValidation := collector.ValidateMetricsAvailability(ctx, r.PromAPI, modelName, deploy.Namespace)

		// Update MetricsAvailable condition based on validation result
		if metricsValidation.Available {
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVA,
				llmdVariantAutoscalingV1alpha1.TypeMetricsAvailable,
				metav1.ConditionTrue,
				metricsValidation.Reason,
				metricsValidation.Message)
		} else {
			// Metrics unavailable - add to updateList for fallback allocation
			logger.Log.Warnw("Metrics unavailable, adding to updateList for fallback allocation",
				"variant", updateVA.Name,
				"namespace", updateVA.Namespace,
				"model", modelName,
				"reason", metricsValidation.Reason,
				"troubleshooting", metricsValidation.Message)

			// Set MetricsAvailable condition to False
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVA,
				llmdVariantAutoscalingV1alpha1.TypeMetricsAvailable,
				metav1.ConditionFalse,
				metricsValidation.Reason,
				metricsValidation.Message)

			// Add to updateList without allocation - will be handled in applyOptimizedAllocations
			updateList.Items = append(updateList.Items, updateVA)
			continue
		}

		// Get retention period for this model from scale-to-zero config
		retentionPeriod := utils.GetScaleToZeroRetentionPeriod(scaleToZeroConfigData, modelName)

		// Collect allocation and scale-to-zero metrics for this variant
		allocation, err := collector.AddMetricsToOptStatus(ctx, &updateVA, deploy, r.PromAPI, r.ScaleToZeroMetricsCache, retentionPeriod)
		if err != nil {
			logger.Log.Error(err, "unable to collect allocation data, adding to updateList for fallback allocation")
			// Set MetricsAvailable condition to False
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVA,
				llmdVariantAutoscalingV1alpha1.TypeMetricsAvailable,
				metav1.ConditionFalse,
				llmdVariantAutoscalingV1alpha1.ReasonPrometheusError,
				fmt.Sprintf("Failed to collect allocation data: %v", err))
			// Add to updateList without allocation - will be handled in applyOptimizedAllocations
			updateList.Items = append(updateList.Items, updateVA)
			continue
		}

		// Update status with allocation (metrics are passed separately in refactored architecture)
		updateVA.Status.CurrentAlloc = allocation

		// Collect aggregate metrics (shared across all variants of this model)
		// Use cache to avoid redundant Prometheus queries for same model
		load, ttftAvg, itlAvg, err := collector.CollectAggregateMetricsWithCache(ctx, modelName, deploy.Namespace, r.PromAPI, r.MetricsCache)
		if err != nil {
			logger.Log.Error(err, "unable to fetch aggregate metrics, adding to updateList for fallback allocation")
			// Set MetricsAvailable condition to False
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVA,
				llmdVariantAutoscalingV1alpha1.TypeMetricsAvailable,
				metav1.ConditionFalse,
				llmdVariantAutoscalingV1alpha1.ReasonPrometheusError,
				fmt.Sprintf("Failed to collect aggregate metrics: %v", err))
			// Add to updateList without allocation - will be handled in applyOptimizedAllocations
			updateList.Items = append(updateList.Items, updateVA)
			continue
		}

		// Update status with collected data (allocation already set by AddMetricsToOptStatus)
		// Extract metrics to internal structure (all metrics passed separately from Prometheus)
		metrics, err := interfaces.NewVariantMetrics(load, ttftAvg, itlAvg)
		if err != nil {
			logger.Log.Error(err, "failed to parse variant metrics, adding to updateList for fallback allocation")
			// Set MetricsAvailable condition to False
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVA,
				llmdVariantAutoscalingV1alpha1.TypeMetricsAvailable,
				metav1.ConditionFalse,
				llmdVariantAutoscalingV1alpha1.ReasonPrometheusError,
				fmt.Sprintf("Failed to parse variant metrics: %v", err))
			// Add to updateList without allocation - will be handled in applyOptimizedAllocations
			updateList.Items = append(updateList.Items, updateVA)
			continue
		}

		// Add variant profile to systemData now that we have validated metrics are available
		// This ensures systemData only contains variants that will be optimized
		if err := utils.AddVariantProfileToSystemData(systemData,
			modelName,
			updateVA.Spec.Accelerator,
			updateVA.Spec.AcceleratorCount,
			&updateVA.Spec.VariantProfile); err != nil {
			logger.Log.Error(err, "failed to add variant profile to system data, adding to updateList for fallback allocation", "variantAutoscaling", updateVA.Name)
			// Set OptimizationReady condition to False
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVA,
				llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
				metav1.ConditionFalse,
				llmdVariantAutoscalingV1alpha1.ReasonOptimizationFailed,
				fmt.Sprintf("Failed to add variant profile to system data: %v", err))
			// Add to updateList without allocation - will be handled in applyOptimizedAllocations
			updateList.Items = append(updateList.Items, updateVA)
			continue
		}

		// Add server info with both metrics and scale-to-zero configuration
		if err := utils.AddServerInfoToSystemData(systemData, &updateVA, className, metrics, scaleToZeroConfigData); err != nil {
			logger.Log.Warn("variantAutoscaling bad deployment server data, adding to updateList for fallback allocation")
			// Set OptimizationReady condition to False
			llmdVariantAutoscalingV1alpha1.SetCondition(&updateVA,
				llmdVariantAutoscalingV1alpha1.TypeOptimizationReady,
				metav1.ConditionFalse,
				llmdVariantAutoscalingV1alpha1.ReasonOptimizationFailed,
				fmt.Sprintf("Failed to add server info: %v", err))
			// Add to updateList without allocation - will be handled in applyOptimizedAllocations
			updateList.Items = append(updateList.Items, updateVA)
			continue
		}

		vaFullName := utils.FullName(va.Name, va.Namespace)

		// Add to updateList - allocation will be set in applyOptimizedAllocations
		updateList.Items = append(updateList.Items, updateVA)
		vaMap[vaFullName] = &va
	}
	return &updateList, vaMap, allAnalyzerResponses, nil
}

// applyOptimizedAllocations applies the optimized allocation to all VariantAutoscaling resources.
func (r *VariantAutoscalingReconciler) applyOptimizedAllocations(
	ctx context.Context,
	updateList *llmdVariantAutoscalingV1alpha1.VariantAutoscalingList,
	optimizedAllocation map[string]llmdVariantAutoscalingV1alpha1.OptimizedAlloc,
	allAnalyzerResponses map[string]*interfaces.ModelAnalyzeResponse,
	scaleToZeroConfigData utils.ScaleToZeroConfigData,
	allVariants []llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
) error {
	logger.Log.Debug("Optimization metrics emitted, starting to process variants - ", "variant_count: ", len(updateList.Items))

	// Create metrics emitter for prediction metrics (reused across all variants)
	metricsEmitter := metrics.NewMetricsEmitter()

	// Create actuator once and reuse for all variants (more efficient)
	act := actuator.NewActuator(r.Client)

	// Collect status update errors to report at the end
	var statusUpdateErrors []error

	for i := range updateList.Items {
		// Check for context cancellation to enable graceful shutdown
		select {
		case <-ctx.Done():
			logger.Log.Info("Context cancelled during variant allocation, stopping early")
			return ctx.Err()
		default:
			// Continue processing
		}

		va := &updateList.Items[i]
		optimizedAlloc, hasOptimizedAlloc := optimizedAllocation[va.Name]
		logger.Log.Debug("Processing variant - ", "index: ", i, ", variantAutoscaling-name: ", va.Name, ", namespace: ", va.Namespace, ", has_optimized_alloc: ", hasOptimizedAlloc)

		// Emit prediction metrics for the SELECTED allocation only (when available)
		// This is done AFTER optimization selects which accelerator to use
		if hasOptimizedAlloc {
			if analyzerResponse, found := allAnalyzerResponses[va.Name]; found && analyzerResponse != nil {
				// Get the selected accelerator from the variant spec
				selectedAccelerator := va.Spec.Accelerator

				// Get the allocation data for the selected accelerator only
				if acceleratorAlloc, acceleratorFound := analyzerResponse.Allocations[selectedAccelerator]; acceleratorFound {
					if acceleratorAlloc != nil && acceleratorAlloc.Allocation != nil {
						allocData := acceleratorAlloc.Allocation.AllocationData()

						// Convert from milliseconds to seconds
						ttftSeconds := float64(allocData.TTFTAverage) / 1000.0
						itlSeconds := float64(allocData.ITLAverage) / 1000.0

						// Emit metrics with correct VariantID (business ID, not Kubernetes UID)
						if err := metricsEmitter.EmitPredictionMetrics(ctx, va, va.Spec.ModelID, ttftSeconds, itlSeconds, selectedAccelerator); err != nil {
							logger.Log.Error(err, "Failed to emit prediction metrics",
								"variantName", va.Name,
								"modelID", va.Spec.ModelID,
								"accelerator", selectedAccelerator)
						} else {
							logger.Log.Debug("Successfully emitted prediction metrics",
								"variantName", va.Name,
								"accelerator", selectedAccelerator,
								"ttft_seconds", ttftSeconds,
								"itl_seconds", itlSeconds)
						}
					}
				} else {
					logger.Log.Debug("No allocation found for selected accelerator",
						"variantName", va.Name,
						"accelerator", selectedAccelerator)
				}
			}
		}

		// Fetch the latest version from API server for conflict-free status update
		var updateVa llmdVariantAutoscalingV1alpha1.VariantAutoscaling
		if err := utils.GetVariantAutoscalingWithBackoff(ctx, r.Client, va.Name, va.Namespace, &updateVa); err != nil {
			logger.Log.Error(err, "failed to get latest VariantAutoscaling from API server: ", "variantAutoscaling-name: ", va.Name)
			continue
		}

		// Note: ownerReference is set in prepareVariantAutoscalings
		// Note: All allocation decisions happen here - nothing carried over from preparation phase

		// Initialize CurrentAlloc from actual deployment replicas
		// This ensures Status.CurrentAlloc reflects reality, not stale data
		var currentReplicas int32
		var deploy appsv1.Deployment
		if err := utils.GetDeploymentWithBackoff(ctx, r.Client, va.Name, va.Namespace, &deploy); err != nil {
			// Deployment doesn't exist yet (first reconciliation) or error occurred
			// Use existing status value, or 0 if never initialized
			logger.Log.Debug("Could not get deployment for CurrentAlloc initialization, using existing status",
				"variant", va.Name, "error", err)
			currentReplicas = va.Status.CurrentAlloc.NumReplicas
		} else {
			// Use actual deployment replicas
			// Prefer Status.Replicas if deployment has been reconciled
			if deploy.Status.ObservedGeneration > 0 {
				currentReplicas = deploy.Status.Replicas
			} else if deploy.Spec.Replicas != nil {
				// Fallback to spec if status not ready yet
				currentReplicas = *deploy.Spec.Replicas
			}
			logger.Log.Debug("Initialized CurrentAlloc from deployment",
				"variant", va.Name, "replicas", currentReplicas)
		}

		updateVa.Status.CurrentAlloc = llmdVariantAutoscalingV1alpha1.Allocation{
			NumReplicas: currentReplicas,
		}

		// Use optimized allocation if available, otherwise preserve fallback from updateList
		// This ensures metrics are always emitted, even for zero-traffic scenarios
		if hasOptimizedAlloc {
			// Add reason and conditional LastUpdate to optimizer allocation
			newAlloc := optimizedAlloc
			newAlloc.Reason = "Optimizer solution: cost and latency optimized allocation"

			// PATH 1 RETENTION PERIOD CHECK:
			// If optimizer returns 0 replicas, verify time-based retention period has expired
			// This ensures consistency with Path 2 and Path 3 fallback logic
			if optimizedAlloc.NumReplicas == 0 {
				// IMPORTANT: Use updateVa (fresh from API) for previous allocation state
				previousAlloc := updateVa.Status.DesiredOptimizedAlloc
				modelName := updateVa.Spec.ModelID
				retentionPeriod := utils.GetScaleToZeroRetentionPeriod(scaleToZeroConfigData, modelName)

				// Check if this is first run (LastUpdate is zero) or retention period not exceeded
				if previousAlloc.LastUpdate.IsZero() {
					// First run: preserve currentReplicas as grace period for Prometheus discovery
					logger.Log.Info("Optimizer returned 0 replicas but this is first run, preserving current deployment replicas",
						"variantName", updateVa.Name,
						"currentReplicas", currentReplicas)

					newAlloc.NumReplicas = currentReplicas
					newAlloc.Reason = "First run: preserving current replicas for Prometheus discovery grace period"
				} else {
					timeSinceLastUpdate := time.Since(previousAlloc.LastUpdate.Time)
					if timeSinceLastUpdate <= retentionPeriod {
						// Retention period NOT exceeded - preserve previous allocation
						logger.Log.Info("Optimizer returned 0 replicas but retention period not exceeded, preserving previous allocation",
							"variantName", updateVa.Name,
							"previousReplicas", previousAlloc.NumReplicas,
							"timeSinceLastUpdate", timeSinceLastUpdate.Round(time.Second),
							"retentionPeriod", retentionPeriod)

						// Preserve previous allocation (don't scale to zero yet)
						newAlloc.NumReplicas = previousAlloc.NumReplicas
						// Use static reason to avoid changing Reason on every reconciliation
						// (which would reset LastUpdate and prevent retention period from being exceeded)
						newAlloc.Reason = "Optimizer returned 0 but retention period not exceeded, preserving allocation"
					} else {
						logger.Log.Info("Optimizer returned 0 replicas and retention period exceeded, scaling to zero",
							"variantName", updateVa.Name,
							"previousReplicas", previousAlloc.NumReplicas,
							"timeSinceLastUpdate", timeSinceLastUpdate.Round(time.Second),
							"retentionPeriod", retentionPeriod)
					}
				}
			}

			// Update LastUpdate only if NumReplicas or Reason changed
			// IMPORTANT: Use updateVa (fresh from API) for previous allocation state
			previousAlloc := updateVa.Status.DesiredOptimizedAlloc
			if previousAlloc.NumReplicas != newAlloc.NumReplicas || previousAlloc.Reason != newAlloc.Reason {
				newAlloc.LastUpdate = metav1.Now()
			} else {
				// Preserve previous LastUpdate if nothing changed
				newAlloc.LastUpdate = previousAlloc.LastUpdate
			}

			updateVa.Status.DesiredOptimizedAlloc = newAlloc
			logger.Log.Info("Using optimized allocation from optimizer",
				"variantName", updateVa.Name,
				"currentReplicas", updateVa.Status.CurrentAlloc.NumReplicas,
				"desiredReplicas", newAlloc.NumReplicas,
				"reason", newAlloc.Reason)
		} else {
			// No optimizer solution - apply fallback allocation logic
			// PATH 2 vs PATH 3 decision: Check if a previous allocation decision was made
			// Note: We check LastUpdate (not NumReplicas >= 0) because NumReplicas defaults to 0,
			// and would incorrectly treat first-run scenarios as having a precomputed fallback.
			// LastUpdate is only set when a controller allocation decision was actually made.
			// IMPORTANT: Use updateVa (fresh from API) not va (from updateList which may be stale)
			hasPrecomputedFallback := !updateVa.Status.DesiredOptimizedAlloc.LastUpdate.IsZero()

			if hasPrecomputedFallback {
				// PATH 2: Has precomputed fallback allocation
				applyFallbackAllocation(&updateVa, allVariants, scaleToZeroConfigData, true, "Fallback")
			} else {
				// PATH 3: No precomputed fallback - use last resort logic
				applyFallbackAllocation(&updateVa, allVariants, scaleToZeroConfigData, false, "Last resort")
			}
		}

		updateVa.Status.Actuation.Applied = false // No longer directly applying changes

		// Set LastRunTime on every reconciliation for observability
		// LastRunTime tracks when the controller last processed this variant (updated every reconciliation)
		// LastUpdate tracks when the allocation decision actually changed (updated only when NumReplicas or Reason changes)
		updateVa.Status.DesiredOptimizedAlloc.LastRunTime = metav1.Now()

		// CRITICAL FIX: Ensure Reason and LastUpdate are set before persisting status
		// This is a safety net to catch any cases where these fields might be lost
		if updateVa.Status.DesiredOptimizedAlloc.Reason == "" {
			// If Reason is still empty at this point, something went wrong in the allocation logic
			// Set a clear fallback value so we never persist empty Reason
			updateVa.Status.DesiredOptimizedAlloc.Reason = "Fallback: allocation set but reason missing (controller bug)"
			updateVa.Status.DesiredOptimizedAlloc.LastUpdate = metav1.Now()
			logger.Log.Error(nil, "CRITICAL: DesiredOptimizedAlloc.Reason was empty before status update!",
				"variantName", updateVa.Name,
				"numReplicas", updateVa.Status.DesiredOptimizedAlloc.NumReplicas,
				"hasOptimizedAlloc", hasOptimizedAlloc)
		}
		if updateVa.Status.DesiredOptimizedAlloc.LastUpdate.IsZero() && updateVa.Status.DesiredOptimizedAlloc.NumReplicas >= 0 {
			// LastUpdate should always be set when NumReplicas is set
			updateVa.Status.DesiredOptimizedAlloc.LastUpdate = metav1.Now()
			logger.Log.Warn("LastUpdate was zero before status update, setting it now",
				"variantName", updateVa.Name,
				"reason", updateVa.Status.DesiredOptimizedAlloc.Reason)
		}

		// Update conditions based on allocation decision path
		// Preserves MetricsAvailable from preparation and sets OptimizationReady appropriately
		updateConditionsForAllocation(&updateVa, va, hasOptimizedAlloc)

		// ALWAYS emit optimization signals for external autoscalers (KEDA, HPA, etc.)
		// This is critical for scale-to-zero scenarios where metrics must exist even with no traffic
		if err := act.EmitMetrics(ctx, &updateVa); err != nil {
			logger.Log.Error(err, "failed to emit optimization signals for external autoscalers - ", "variant: ", updateVa.Name)
		} else {
			logger.Log.Debug("Successfully emitted optimization signals for external autoscalers - ", "variant: ", updateVa.Name)
			updateVa.Status.Actuation.Applied = true // Signals emitted successfully
		}

		if err := utils.UpdateStatusWithBackoff(ctx, r.Client, &updateVa, utils.StandardBackoff, "VariantAutoscaling"); err != nil {
			logger.Log.Error(err, "failed to patch status for variantAutoscaling after retries - ", "variantAutoscaling-name: ", updateVa.Name)
			statusUpdateErrors = append(statusUpdateErrors, fmt.Errorf("variant %s/%s: %w", updateVa.Namespace, updateVa.Name, err))
			continue
		}
	}

	logger.Log.Debug("Completed variant processing loop")

	// Return error if any status updates failed
	if len(statusUpdateErrors) > 0 {
		logger.Log.Warnw("Some variant status updates failed",
			"failedCount", len(statusUpdateErrors),
			"totalCount", len(updateList.Items))
		return fmt.Errorf("failed to update status for %d variant(s): %v", len(statusUpdateErrors), statusUpdateErrors)
	}

	// Log summary of reconciliation
	if len(updateList.Items) > 0 {
		logger.Log.Info("Reconciliation completed - ",
			"variants_processed: ", len(updateList.Items),
			", optimization_successful: ", true)
	}

	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *VariantAutoscalingReconciler) SetupWithManager(mgr ctrl.Manager) error {

	// Initialize metrics
	initMetricsEmitter()

	// Configure Prometheus client using flexible configuration with TLS support
	// Use context with timeout to prevent hanging during setup
	ctx, cancel := context.WithTimeout(context.Background(), SetupTimeout)
	defer cancel()

	promConfig, err := r.getPrometheusConfig(ctx)
	if err != nil {
		return fmt.Errorf("failed to get Prometheus configuration: %w", err)
	}

	// ensure we have a valid configuration
	if promConfig == nil {
		return fmt.Errorf("no Prometheus configuration found - this should not happen")
	}

	// Always validate TLS configuration since HTTPS is required
	if err := utils.ValidateTLSConfig(promConfig); err != nil {
		logger.Log.Error(err, "TLS configuration validation failed - HTTPS is required")
		return fmt.Errorf("TLS configuration validation failed: %w", err)
	}

	logger.Log.Info("Initializing Prometheus client -> ", "address: ", promConfig.BaseURL, " tls_enabled: true")

	// Create Prometheus client with TLS support
	promClientConfig, err := utils.CreatePrometheusClientConfig(promConfig)
	if err != nil {
		return fmt.Errorf("failed to create prometheus client config: %w", err)
	}

	promClient, err := api.NewClient(*promClientConfig)
	if err != nil {
		return fmt.Errorf("failed to create prometheus client: %w", err)
	}

	r.PromAPI = promv1.NewAPI(promClient)

	// Initialize scale-to-zero metrics cache for storing internal per-model scale-to-zero metrics
	r.ScaleToZeroMetricsCache = collector.NewScaleToZeroMetricsCache()
	logger.Log.Info("Scale-to-zero metrics cache initialized")

	// Validate that the API is working by testing a simple query with retry logic
	// Use the same context with timeout to prevent hanging
	if err := utils.ValidatePrometheusAPI(ctx, r.PromAPI); err != nil {
		logger.Log.Error(err, "CRITICAL: Failed to connect to Prometheus - Inferno requires Prometheus connectivity for autoscaling decisions")
		return fmt.Errorf("critical: failed to validate Prometheus API connection - autoscaling functionality requires Prometheus: %w", err)
	}
	logger.Log.Info("Prometheus client and API wrapper initialized and validated successfully")

	// Read reconciliation interval from ConfigMap to calculate optimal cache TTL
	// This ensures cache expires between reconciliation loops for fresh Prometheus data
	// Use the same context with timeout to prevent hanging
	intervalStr, err := r.readOptimizationConfig(ctx)
	if err != nil {
		logger.Log.Warn("Failed to read optimization config, using default reconciliation interval",
			"error", err.Error())
		intervalStr = "" // Will default to 60s below
	}

	// Parse reconciliation interval (default 60s if not set)
	reconciliationInterval := DefaultReconciliationInterval
	if intervalStr != "" {
		if parsedInterval, parseErr := time.ParseDuration(intervalStr); parseErr != nil {
			logger.Log.Warn("Failed to parse reconciliation interval, using default",
				"configuredInterval", intervalStr,
				"error", parseErr.Error(),
				"default", reconciliationInterval.String())
		} else {
			// Validate interval bounds to prevent API server overload or stale data
			if parsedInterval < MinReconciliationInterval {
				logger.Log.Warn("Reconciliation interval too short, using minimum",
					"configured", parsedInterval,
					"minimum", MinReconciliationInterval,
					"using", MinReconciliationInterval)
				reconciliationInterval = MinReconciliationInterval
			} else if parsedInterval > MaxReconciliationInterval {
				logger.Log.Warn("Reconciliation interval very long, may cause stale data",
					"configured", parsedInterval,
					"maximum", MaxReconciliationInterval)
				reconciliationInterval = parsedInterval
			} else {
				reconciliationInterval = parsedInterval
			}
		}
	}

	// Calculate cache TTL as half of reconciliation interval
	// This guarantees cache expires between reconciliation loops, ensuring fresh data
	// while maintaining caching benefit for multiple VAs within same reconciliation batch
	cacheTTL := reconciliationInterval / 2

	// Apply minimum TTL of 5 seconds to prevent excessive Prometheus queries
	// if reconciliation interval is configured very short (< 10s)
	if cacheTTL < MinCacheTTL {
		logger.Log.Warn("Calculated cache TTL too short, using minimum",
			"calculated", cacheTTL.String(),
			"minimum", MinCacheTTL.String(),
			"reconciliationInterval", reconciliationInterval.String())
		cacheTTL = MinCacheTTL
	}

	r.MetricsCache = collector.NewModelMetricsCache(cacheTTL)
	logger.Log.Info("Model metrics cache initialized with dynamic TTL",
		"cacheTTL", cacheTTL.String(),
		"reconciliationInterval", reconciliationInterval.String(),
		"ratio", "TTL = interval / 2")

	// Add cache cleanup as a managed runnable
	// This ensures proper shutdown when the manager stops
	if err := mgr.Add(&cacheCleanupRunnable{
		cache:           r.MetricsCache,
		cleanupInterval: cacheTTL,
	}); err != nil {
		return fmt.Errorf("failed to add cache cleanup runnable: %w", err)
	}
	logger.Log.Info("Metrics cache cleanup runnable registered with manager",
		"cleanupInterval", cacheTTL.String())

	//logger.Log.Info("Prometheus client initialized (validation skipped)")

	// Helper to enqueue all VariantAutoscaling resources for reconciliation
	enqueueAllVAs := func(ctx context.Context, obj client.Object) []reconcile.Request {
		var list llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
		if err := r.List(ctx, &list); err != nil {
			logger.Log.Error(err, "Failed to list VariantAutoscalings for ConfigMap watch")
			return nil
		}
		requests := make([]reconcile.Request, len(list.Items))
		for i, va := range list.Items {
			requests[i] = reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&va),
			}
		}
		logger.Log.Debug("ConfigMap changed, enqueuing reconcile requests",
			"configMap", obj.GetName(),
			"requestCount", len(requests))
		return requests
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}).
		// Watch the optimization config ConfigMap to trigger reconcile for all VAs
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				if obj.GetName() == configMapName && obj.GetNamespace() == configMapNamespace {
					return enqueueAllVAs(ctx, obj)
				}
				return nil
			}),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return obj.GetName() == configMapName && obj.GetNamespace() == configMapNamespace
			})),
		).
		// Watch the scale-to-zero config ConfigMap to trigger reconcile for all VAs
		Watches(
			&corev1.ConfigMap{},
			handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
				if obj.GetName() == "model-scale-to-zero-config" && obj.GetNamespace() == configMapNamespace {
					return enqueueAllVAs(ctx, obj)
				}
				return nil
			}),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return obj.GetName() == "model-scale-to-zero-config" && obj.GetNamespace() == configMapNamespace
			})),
		).
		Named("variantAutoscaling").
		WithEventFilter(predicate.Funcs{
			CreateFunc: func(e event.CreateEvent) bool {
				return true
			},
			UpdateFunc: func(e event.UpdateEvent) bool {
				// Reconcile only if spec changed (ignore status-only updates)
				oldVA, oldOK := e.ObjectOld.(*llmdVariantAutoscalingV1alpha1.VariantAutoscaling)
				newVA, newOK := e.ObjectNew.(*llmdVariantAutoscalingV1alpha1.VariantAutoscaling)
				if !oldOK || !newOK {
					return false
				}
				return !reflect.DeepEqual(oldVA.Spec, newVA.Spec)
			},
			DeleteFunc: func(e event.DeleteEvent) bool {
				// Don't reconcile on delete (handled by deletion timestamp check)
				return false
			},
			GenericFunc: func(e event.GenericEvent) bool {
				return false
			},
		}).
		Complete(r)
}

func (r *VariantAutoscalingReconciler) readServiceClassConfig(ctx context.Context, cmName, cmNamespace string) (map[string]string, error) {
	cm := corev1.ConfigMap{}
	err := utils.GetConfigMapWithBackoff(ctx, r.Client, cmName, cmNamespace, &cm)
	if err != nil {
		return nil, err
	}
	return cm.Data, nil
}

func (r *VariantAutoscalingReconciler) readAcceleratorConfig(ctx context.Context, cmName, cmNamespace string) (map[string]map[string]string, error) {
	cm := corev1.ConfigMap{}
	err := utils.GetConfigMapWithBackoff(ctx, r.Client, cmName, cmNamespace, &cm)
	if err != nil {
		return nil, fmt.Errorf("failed to read ConfigMap %s/%s: %w", cmNamespace, cmName, err)
	}
	out := make(map[string]map[string]string)
	for acc, accInfoStr := range cm.Data {
		accInfoMap := make(map[string]string)
		if err := json.Unmarshal([]byte(accInfoStr), &accInfoMap); err != nil {
			return nil, fmt.Errorf("failed to read entry %s in ConfigMap %s/%s: %w", acc, cmNamespace, cmName, err)
		}
		out[acc] = accInfoMap
	}
	return out, nil
}

func (r *VariantAutoscalingReconciler) getPrometheusConfig(ctx context.Context) (*interfaces.PrometheusConfig, error) {
	// Try environment variables first
	config, err := r.getPrometheusConfigFromEnv()
	if err != nil {
		return nil, fmt.Errorf("failed to get config from environment: %w", err)
	}
	if config != nil {
		return config, nil
	}

	// Try ConfigMap second
	config, err = r.getPrometheusConfigFromConfigMap(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get config from ConfigMap: %w", err)
	}
	if config != nil {
		return config, nil
	}

	// No configuration found
	logger.Log.Warn("No Prometheus configuration found. Please set PROMETHEUS_BASE_URL environment variable or configure via ConfigMap")
	return nil, fmt.Errorf("no Prometheus configuration found. Please set PROMETHEUS_BASE_URL environment variable or configure via ConfigMap")
}

func (r *VariantAutoscalingReconciler) getPrometheusConfigFromEnv() (*interfaces.PrometheusConfig, error) {
	promAddr := os.Getenv("PROMETHEUS_BASE_URL")
	if promAddr == "" {
		return nil, nil // No config found, but not an error
	}

	logger.Log.Info("Using Prometheus configuration from environment variables", "address", promAddr)
	return utils.ParsePrometheusConfigFromEnv(), nil
}

func (r *VariantAutoscalingReconciler) getPrometheusConfigFromConfigMap(ctx context.Context) (*interfaces.PrometheusConfig, error) {
	cm := corev1.ConfigMap{}
	err := utils.GetConfigMapWithBackoff(ctx, r.Client, configMapName, configMapNamespace, &cm)
	if err != nil {
		return nil, fmt.Errorf("failed to get ConfigMap for Prometheus config: %w", err)
	}

	promAddr, exists := cm.Data["PROMETHEUS_BASE_URL"]
	if !exists || promAddr == "" {
		return nil, nil // No config found, but not an error
	}

	logger.Log.Info("Using Prometheus configuration from ConfigMap", "address", promAddr)

	// Create config from ConfigMap data
	config := &interfaces.PrometheusConfig{
		BaseURL: promAddr,
	}

	// Parse TLS configuration from ConfigMap (TLS is always enabled for HTTPS-only support)
	config.InsecureSkipVerify = utils.GetConfigValue(cm.Data, "PROMETHEUS_TLS_INSECURE_SKIP_VERIFY", "") == "true"
	config.CACertPath = utils.GetConfigValue(cm.Data, "PROMETHEUS_CA_CERT_PATH", "")
	config.ClientCertPath = utils.GetConfigValue(cm.Data, "PROMETHEUS_CLIENT_CERT_PATH", "")
	config.ClientKeyPath = utils.GetConfigValue(cm.Data, "PROMETHEUS_CLIENT_KEY_PATH", "")
	config.ServerName = utils.GetConfigValue(cm.Data, "PROMETHEUS_SERVER_NAME", "")

	// Add bearer token if provided
	if bearerToken, exists := cm.Data["PROMETHEUS_BEARER_TOKEN"]; exists && bearerToken != "" {
		config.BearerToken = bearerToken
	}

	return config, nil
}

func (r *VariantAutoscalingReconciler) readOptimizationConfig(ctx context.Context) (interval string, err error) {
	cm := corev1.ConfigMap{}
	err = utils.GetConfigMapWithBackoff(ctx, r.Client, configMapName, configMapNamespace, &cm)

	if err != nil {
		return "", fmt.Errorf("failed to get optimization configmap after retries: %w", err)
	}

	interval = cm.Data["GLOBAL_OPT_INTERVAL"]
	return interval, nil
}

// readScaleToZeroConfig reads per-model scale-to-zero configuration from a ConfigMap
// using prefixed-key format with YAML values.
//
// Format: Keys prefixed with "model.", values are YAML
//
// Example:
//
//	model.meta.llama-3.1-8b: |
//	  modelID: "meta/llama-3.1-8b"
//	  enableScaleToZero: true
//	  retentionPeriod: "5m"
//	__defaults__: |
//	  enableScaleToZero: true
//	  retentionPeriod: "15m"
//
// Benefits:
//   - Independently editable (kubectl patch single model)
//   - Original modelID preserved in YAML value (no collision risk)
//   - Better Git diffs (only changed models shown)
//   - Human-readable YAML format
//
// The function returns an empty map if the ConfigMap is not found (it's optional).
func (r *VariantAutoscalingReconciler) readScaleToZeroConfig(ctx context.Context, cmName, cmNamespace string) (utils.ScaleToZeroConfigData, error) {
	cm := corev1.ConfigMap{}
	err := utils.GetConfigMapWithBackoff(ctx, r.Client, cmName, cmNamespace, &cm)
	if err != nil {
		// ConfigMap is optional - return empty map if not found
		logger.Log.Debug("Scale-to-zero ConfigMap not found, using global defaults", "configMap", cmName, "namespace", cmNamespace)
		return make(utils.ScaleToZeroConfigData), nil
	}

	out := make(utils.ScaleToZeroConfigData)
	// Track which keys define which modelIDs to detect duplicates
	modelIDToKeys := make(map[string][]string)

	logger.Log.Debug("Loading scale-to-zero config from prefixed-key format",
		"configMap", cmName,
		"namespace", cmNamespace)

	// Sort keys to ensure deterministic processing order
	// This is critical because map iteration in Go is non-deterministic.
	// If there are duplicate modelIDs, the lexicographically first key will win.
	keys := make([]string, 0, len(cm.Data))
	for k := range cm.Data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, key := range keys {
		configStr := cm.Data[key]
		// Handle global defaults (special key)
		if key == utils.GlobalDefaultsKey {
			var config utils.ModelScaleToZeroConfig
			if err := yaml.Unmarshal([]byte(configStr), &config); err != nil {
				logger.Log.Warn("Failed to parse __defaults__ in scale-to-zero ConfigMap, skipping",
					"configMap", cmName,
					"error", err)
				continue
			}
			out[utils.GlobalDefaultsKey] = config
			continue
		}

		// Handle prefixed model keys
		if strings.HasPrefix(key, "model.") {
			var config utils.ModelScaleToZeroConfig
			if err := yaml.Unmarshal([]byte(configStr), &config); err != nil {
				logger.Log.Warn("Failed to parse scale-to-zero config for prefixed key, skipping",
					"key", key,
					"configMap", cmName,
					"error", err)
				continue
			}

			// Use modelID from YAML (not the key) to avoid collision
			if config.ModelID == "" {
				logger.Log.Warn("Skipping model config without modelID field in scale-to-zero ConfigMap",
					"key", key,
					"configMap", cmName)
				continue
			}

			// Check for duplicate modelID
			if existingKeys, exists := modelIDToKeys[config.ModelID]; exists {
				logger.Log.Warn("Duplicate modelID found in scale-to-zero ConfigMap - first key wins (lexicographic order)",
					"modelID", config.ModelID,
					"winningKey", existingKeys[0],
					"duplicateKey", key,
					"configMap", cmName)
				// Skip this duplicate - first key already processed wins
				continue
			}
			modelIDToKeys[config.ModelID] = append(modelIDToKeys[config.ModelID], key)

			out[config.ModelID] = config
		}
	}

	logger.Log.Debug("Loaded scale-to-zero config",
		"configMap", cmName,
		"namespace", cmNamespace,
		"modelCount", len(out))
	return out, nil
}
