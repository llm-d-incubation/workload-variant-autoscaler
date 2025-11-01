package collector

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/constants"
	interfaces "github.com/llm-d-incubation/workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	promv1 "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	appsv1 "k8s.io/api/apps/v1"
)

type AcceleratorModelInfo struct {
	Count  int
	Memory string
}

// TODO: Resource accounting and capacity tracking for limited mode.
// The WVA currently operates in unlimited mode only, where each variant receives
// optimal allocation independently without cluster capacity constraints.
// Limited mode support requires integration with the llmd stack and additional
// design work to handle degraded mode operations without violating SLOs.
// Future work: Implement CollectInventoryK8S and capacity-aware allocation for limited mode.

// vendors list for GPU vendors - kept for future limited mode support
var vendors = []string{
	"nvidia.com",
	"amd.com",
	"intel.com",
}

// CollectInventoryK8S is a stub for future limited mode support.
// Currently returns empty inventory as WVA operates in unlimited mode.
func CollectInventoryK8S(ctx context.Context, r interface{}) (map[string]map[string]AcceleratorModelInfo, error) {
	// Stub implementation - will be properly implemented for limited mode
	return make(map[string]map[string]AcceleratorModelInfo), nil
}

type MetricKV struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// MetricsValidationResult contains the result of metrics availability check
type MetricsValidationResult struct {
	Available bool
	Reason    string
	Message   string
}

// ValidateMetricsAvailability checks if vLLM metrics are available for the given model and namespace
// Returns a validation result with details about metric availability
func ValidateMetricsAvailability(ctx context.Context, promAPI promv1.API, modelName, namespace string) MetricsValidationResult {
	// Handle nil promAPI (e.g., in unit tests without Prometheus)
	if promAPI == nil {
		return MetricsValidationResult{
			Available: false,
			Reason:    llmdVariantAutoscalingV1alpha1.ReasonPrometheusError,
			Message:   "Prometheus API not configured",
		}
	}

	// Query for basic vLLM metric to validate scraping is working
	// Try with namespace label first (real vLLM), fall back to just model_name (vllme emulator)
	testQuery := fmt.Sprintf(`vllm:request_success_total{model_name="%s",namespace="%s"}`, modelName, namespace)

	val, _, err := promAPI.Query(ctx, testQuery, time.Now())
	if err != nil {
		logger.Log.Error(err, "Error querying Prometheus for metrics validation",
			"model", modelName, "namespace", namespace)
		return MetricsValidationResult{
			Available: false,
			Reason:    llmdVariantAutoscalingV1alpha1.ReasonPrometheusError,
			Message:   fmt.Sprintf("Failed to query Prometheus: %v", err),
		}
	}

	// Check if we got any results
	if val.Type() != model.ValVector {
		return MetricsValidationResult{
			Available: false,
			Reason:    llmdVariantAutoscalingV1alpha1.ReasonMetricsMissing,
			Message:   fmt.Sprintf("No vLLM metrics found for model '%s' in namespace '%s'. Check ServiceMonitor configuration and ensure vLLM pods are exposing /metrics endpoint", modelName, namespace),
		}
	}

	vec := val.(model.Vector)
	// If no results with namespace label, try without it (for vllme emulator compatibility)
	if len(vec) == 0 {
		testQueryFallback := fmt.Sprintf(`vllm:request_success_total{model_name="%s"}`, modelName)
		val, _, err = promAPI.Query(ctx, testQueryFallback, time.Now())
		if err != nil {
			return MetricsValidationResult{
				Available: false,
				Reason:    llmdVariantAutoscalingV1alpha1.ReasonPrometheusError,
				Message:   fmt.Sprintf("Failed to query Prometheus: %v", err),
			}
		}

		if val.Type() == model.ValVector {
			vec = val.(model.Vector)
		}

		// If still no results, metrics are truly missing
		if len(vec) == 0 {
			return MetricsValidationResult{
				Available: false,
				Reason:    llmdVariantAutoscalingV1alpha1.ReasonMetricsMissing,
				Message:   fmt.Sprintf("No vLLM metrics found for model '%s' in namespace '%s'. Check: (1) ServiceMonitor exists in monitoring namespace, (2) ServiceMonitor selector matches vLLM service labels, (3) vLLM pods are running and exposing /metrics endpoint, (4) Prometheus is scraping the monitoring namespace", modelName, namespace),
			}
		}
	}

	// Check if metrics are stale (older than 5 minutes)
	for _, sample := range vec {
		age := time.Since(sample.Timestamp.Time())
		if age > 5*time.Minute {
			return MetricsValidationResult{
				Available: false,
				Reason:    llmdVariantAutoscalingV1alpha1.ReasonMetricsStale,
				Message:   fmt.Sprintf("vLLM metrics for model '%s' are stale (last update: %v ago). ServiceMonitor may not be scraping correctly.", modelName, age),
			}
		}
	}

	return MetricsValidationResult{
		Available: true,
		Reason:    llmdVariantAutoscalingV1alpha1.ReasonMetricsFound,
		Message:   "vLLM metrics are available and up-to-date",
	}
}

// CollectAggregateMetricsWithCache collects aggregate metrics for a model,
// using cache to avoid redundant Prometheus queries.
// If cache is nil, behaves like CollectAggregateMetrics (no caching).
func CollectAggregateMetricsWithCache(ctx context.Context,
	modelName string,
	namespace string,
	promAPI promv1.API,
	cache *ModelMetricsCache) (interfaces.LoadProfile, string, string, error) {

	// Check cache first if available
	if cache != nil {
		if cached, found := cache.Get(modelName, namespace); found && cached.Valid {
			logger.Log.Debug("Using cached metrics for model", "model", modelName, "namespace", namespace)
			return cached.Load, cached.TTFTAverage, cached.ITLAverage, nil
		}
	}

	// Cache miss or disabled - query Prometheus
	logger.Log.Debug("Querying Prometheus for model metrics", "model", modelName, "namespace", namespace)
	load, ttftAvg, itlAvg, err := CollectAggregateMetrics(ctx, modelName, namespace, promAPI)

	// Update cache even on error (mark as invalid) to prevent thundering herd
	if cache != nil {
		cache.Set(modelName, namespace, load, ttftAvg, itlAvg, err == nil)
	}

	return load, ttftAvg, itlAvg, err
}

// CollectAggregateMetrics collects aggregate metrics (Load, ITL, TTFT) for a modelID
// across all deployments serving that model. These metrics are shared across all variants.
//
// Note: For production use, prefer CollectAggregateMetricsWithCache to avoid redundant queries.
func CollectAggregateMetrics(ctx context.Context,
	modelName string,
	namespace string,
	promAPI promv1.API) (interfaces.LoadProfile, string, string, error) {

	// Query 1: Arrival rate (requests per minute)
	arrivalQuery := fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMRequestSuccessTotal,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, namespace)
	arrivalVal := 0.0
	if val, warn, err := promAPI.Query(ctx, arrivalQuery, time.Now()); err == nil && val.Type() == model.ValVector {
		vec := val.(model.Vector)
		if len(vec) > 0 {
			arrivalVal = float64(vec[0].Value)
		}
		if warn != nil {
			logger.Log.Warn("Prometheus warnings - ", "warnings: ", warn)
		}
	} else {
		return interfaces.LoadProfile{}, "", "", err
	}
	arrivalVal *= 60 // convert from req/sec to req/min
	FixValue(&arrivalVal)

	// Query 2: Average prompt tokens (Input Tokens)
	avgPromptToksQuery := fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))/sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMRequestPromptTokensSum,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, namespace,
		constants.VLLMRequestPromptTokensCount,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, namespace)
	avgInputTokens := 0.0
	if val, _, err := promAPI.Query(ctx, avgPromptToksQuery, time.Now()); err == nil && val.Type() == model.ValVector {
		vec := val.(model.Vector)
		if len(vec) > 0 {
			avgInputTokens = float64(vec[0].Value)
		}
	} else {
		logger.Log.Warn("failed to get avg prompt tokens, using 0: ", "model: ", modelName)
	}
	FixValue(&avgInputTokens)

	// Query 3: Average output tokens (decode length)
	avgDecToksQuery := fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))/sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMRequestGenerationTokensSum,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, namespace,
		constants.VLLMRequestGenerationTokensCount,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, namespace)
	avgOutputTokens := 0.0
	if val, _, err := promAPI.Query(ctx, avgDecToksQuery, time.Now()); err == nil && val.Type() == model.ValVector {
		vec := val.(model.Vector)
		if len(vec) > 0 {
			avgOutputTokens = float64(vec[0].Value)
		}
	} else {
		return interfaces.LoadProfile{}, "", "", err
	}
	FixValue(&avgOutputTokens)

	// Query 4: Time To First Token (TTFT)
	ttftQuery := fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))/sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMTimeToFirstTokenSecondsSum,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, namespace,
		constants.VLLMTimeToFirstTokenSecondsCount,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, namespace)
	ttftAverageTime := 0.0
	if val, _, err := promAPI.Query(ctx, ttftQuery, time.Now()); err == nil && val.Type() == model.ValVector {
		vec := val.(model.Vector)
		if len(vec) > 0 {
			ttftAverageTime = float64(vec[0].Value) * 1000 //msec
		}
	} else {
		logger.Log.Warn("failed to get avg wait time, using 0: ", "model: ", modelName)
	}
	FixValue(&ttftAverageTime)

	// Query 5: Average ITL (Inter-Token Latency)
	itlQuery := fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))/sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMTimePerOutputTokenSecondsSum,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, namespace,
		constants.VLLMTimePerOutputTokenSecondsCount,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, namespace)
	itlAverage := 0.0
	if val, _, err := promAPI.Query(ctx, itlQuery, time.Now()); err == nil && val.Type() == model.ValVector {
		vec := val.(model.Vector)
		if len(vec) > 0 {
			itlAverage = float64(vec[0].Value) * 1000 //msec
		}
	} else {
		logger.Log.Warn("failed to get avg itl time, using 0: ", "model: ", modelName)
	}
	FixValue(&itlAverage)

	// Return aggregate metrics
	load := interfaces.LoadProfile{
		ArrivalRate:     strconv.FormatFloat(float64(arrivalVal), 'f', 2, 32),
		AvgInputTokens:  strconv.FormatFloat(float64(avgInputTokens), 'f', 2, 32),
		AvgOutputTokens: strconv.FormatFloat(float64(avgOutputTokens), 'f', 2, 32),
	}
	ttftAvg := strconv.FormatFloat(float64(ttftAverageTime), 'f', 2, 32)
	itlAvg := strconv.FormatFloat(float64(itlAverage), 'f', 2, 32)

	return load, ttftAvg, itlAvg, nil
}

// CollectAllocationForDeployment collects allocation information for a single deployment.
// This includes replica count. Other allocation details (cost, max batch) are in the VA spec.
// Aggregate metrics (Load, ITL, TTFT) are collected separately via CollectAggregateMetrics.
func CollectAllocationForDeployment(
	variantID string,
	accelerator string,
	deployment appsv1.Deployment,
) (llmdVariantAutoscalingV1alpha1.Allocation, error) {

	// number of replicas
	numReplicas := int32(0) // Default to 0 if not specified
	if deployment.Spec.Replicas != nil {
		numReplicas = *deployment.Spec.Replicas
	}

	// populate allocation (without aggregate metrics)
	// Note: In single-variant architecture, variantID, accelerator, maxBatch, and variantCost
	// are not needed here as they are already defined in the parent VariantAutoscaling spec.
	allocation := llmdVariantAutoscalingV1alpha1.Allocation{
		NumReplicas: numReplicas,
	}
	return allocation, nil
}

// Helper to handle if a value is NaN or infinite
func FixValue(x *float64) {
	if math.IsNaN(*x) || math.IsInf(*x, 0) {
		*x = 0
	}
}

// AddMetricsToOptStatus collects allocation and scale-to-zero metrics for a variant.
// This function combines allocation collection with scale-to-zero request tracking.
func AddMetricsToOptStatus(ctx context.Context,
	opt *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	deployment appsv1.Deployment,
	promAPI promv1.API,
	metricsCache *ScaleToZeroMetricsCache,
	retentionPeriod time.Duration) (llmdVariantAutoscalingV1alpha1.Allocation, error) {

	deployNamespace := deployment.Namespace
	modelName := opt.Spec.ModelID

	// NOTE: Aggregate metrics (arrival, TTFT, ITL, avgPromptTokens, avgDecTokens) are now
	// collected via CollectAggregateMetricsWithCache() to avoid duplicate queries and enable caching.
	// This function only collects scale-to-zero specific metrics and allocation data.

	// Query: Total requests over retention period (stored in internal cache only for scale-to-zero)
	// Convert retention period to Prometheus duration format (e.g., "10m", "1h", "30s")
	retentionPeriodStr := formatPrometheusDuration(retentionPeriod)
	totalRequestsQuery := fmt.Sprintf(`sum(increase(%s{%s="%s",%s="%s"}[%s]))`,
		constants.VLLMRequestSuccessTotal,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace,
		retentionPeriodStr)
	totalRequestsOverRetention := 0.0
	if val, _, err := promAPI.Query(ctx, totalRequestsQuery, time.Now()); err == nil && val.Type() == model.ValVector {
		vec := val.(model.Vector)
		if len(vec) > 0 {
			totalRequestsOverRetention = float64(vec[0].Value)
		}
	} else {
		logger.Log.Warn("failed to get total requests over retention period, using 0: ",
			"model: ", modelName,
			"retentionPeriod: ", retentionPeriodStr)
	}
	FixValue(&totalRequestsOverRetention)

	// Store total requests in internal metrics cache
	if metricsCache != nil {
		metricsCache.Set(modelName, &ScaleToZeroMetrics{
			TotalRequestsOverRetentionPeriod: totalRequestsOverRetention,
			RetentionPeriod:                  retentionPeriod,
		})
		logger.Log.Info("Cached scale-to-zero metrics",
			"modelName", modelName,
			"totalRequestsOverRetention", totalRequestsOverRetention,
			"retentionPeriod", retentionPeriod,
			"query", totalRequestsQuery)
	}

	// number of replicas
	numReplicas := *deployment.Spec.Replicas

	// populate current alloc (refactored: metrics are passed separately, not stored in allocation)
	// Note: In single-variant architecture, variantID, accelerator, maxBatch, and variantCost
	// are not needed here as they are already defined in the parent VariantAutoscaling spec.
	currentAlloc := llmdVariantAutoscalingV1alpha1.Allocation{
		NumReplicas: numReplicas,
	}
	return currentAlloc, nil
}

// formatPrometheusDuration converts a Go time.Duration to Prometheus duration format
// Examples: 30s, 5m, 1h, 90s (not 1.5m)
func formatPrometheusDuration(d time.Duration) string {
	// Try to express in whole hours first
	if d >= time.Hour && d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	// Try to express in whole minutes
	if d >= time.Minute && d%time.Minute == 0 {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	// Express in seconds
	return fmt.Sprintf("%ds", int(d.Seconds()))
}
