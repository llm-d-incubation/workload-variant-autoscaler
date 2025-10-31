package controller

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"time"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/constants"
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

// queryAndExtractMetric performs a Prometheus query and extracts the float value,
func queryAndExtractMetric(ctx context.Context, promAPI promv1.API, query string, metricName string) (float64, error) {
	val, warn, err := promAPI.Query(ctx, query, time.Now())
	if err != nil {
		return 0.0, fmt.Errorf("failed to query Prometheus for %s: %w", metricName, err)
	}

	if warn != nil {
		logger.Log.Warn("Prometheus warnings", "metric", metricName, "warnings", warn)
	}

	// Check if the result type is a Vector
	if val.Type() != model.ValVector {
		logger.Log.Debug("Prometheus query returned non-vector type", "metric", metricName, "type", val.Type().String())
		return 0.0, nil
	}

	vec := val.(model.Vector)
	resultVal := 0.0
	if len(vec) > 0 {
		resultVal = float64(vec[0].Value)
		// Handle NaN or Inf values
		FixValue(&resultVal)
	}

	return resultVal, nil
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
	// Query for basic vLLM metric to validate scraping is working
	// Try with namespace label first (real vLLM), fall back to just model_name (vllme emulator)
	testQuery := fmt.Sprintf(`%s{model_name="%s",namespace="%s"}`, constants.VLLMNumRequestRunning, modelName, namespace)

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
		testQueryFallback := fmt.Sprintf(`%s{model_name="%s"}`, constants.VLLMNumRequestRunning, modelName)
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

func AddMetricsToOptStatus(ctx context.Context,
	opt *llmdVariantAutoscalingV1alpha1.VariantAutoscaling,
	deployment appsv1.Deployment,
	acceleratorCostVal float64,
	promAPI promv1.API) (llmdVariantAutoscalingV1alpha1.Allocation, error) {

	deployNamespace := deployment.Namespace
	modelName := opt.Spec.ModelID

	// --- 1. Define Queries ---

	// Metric 1: Arrival rate (requests per minute)
	arrivalQuery := fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMRequestSuccessTotal,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace)

	// Metric 2: Average prompt length (Input Tokens)
	avgPromptToksQuery := fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))/sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMRequestPromptTokensSum,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace,
		constants.VLLMRequestPromptTokensCount,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace)

	// Metric 3: Average decode length (Output Tokens)
	avgDecToksQuery := fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))/sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMRequestGenerationTokensSum,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace,
		constants.VLLMRequestGenerationTokensCount,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace)

	// Metric 4: Average TTFT (Time to First Token) ms
	ttftQuery := fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))/sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMTimeToFirstTokenSecondsSum,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace,
		constants.VLLMTimeToFirstTokenSecondsCount,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace)

	// Metric 5: Average ITL (Inter-Token Latency) ms
	itlQuery := fmt.Sprintf(`sum(rate(%s{%s="%s",%s="%s"}[1m]))/sum(rate(%s{%s="%s",%s="%s"}[1m]))`,
		constants.VLLMTimePerOutputTokenSecondsSum,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace,
		constants.VLLMTimePerOutputTokenSecondsCount,
		constants.LabelModelName, modelName,
		constants.LabelNamespace, deployNamespace)

	// --- 2. Execute Queries ---

	arrivalVal, err := queryAndExtractMetric(ctx, promAPI, arrivalQuery, "ArrivalRate")
	if err != nil {
		return llmdVariantAutoscalingV1alpha1.Allocation{}, err
	}
	arrivalVal *= 60 // convert from req/sec to req/min

	avgInputTokens, err := queryAndExtractMetric(ctx, promAPI, avgPromptToksQuery, "AvgInputTokens")
	if err != nil {
		return llmdVariantAutoscalingV1alpha1.Allocation{}, err
	}

	avgOutputTokens, err := queryAndExtractMetric(ctx, promAPI, avgDecToksQuery, "AvgOutputTokens")
	if err != nil {
		return llmdVariantAutoscalingV1alpha1.Allocation{}, err
	}

	ttftAverageTime, err := queryAndExtractMetric(ctx, promAPI, ttftQuery, "TTFTAverageTime")
	if err != nil {
		return llmdVariantAutoscalingV1alpha1.Allocation{}, err
	}
	ttftAverageTime *= 1000 // convert to msec

	itlAverage, err := queryAndExtractMetric(ctx, promAPI, itlQuery, "ITLAverage")
	if err != nil {
		return llmdVariantAutoscalingV1alpha1.Allocation{}, err
	}
	itlAverage *= 1000 // convert to msec

	// --- 3. Collect K8s and Static Info ---

	// number of replicas
	numReplicas := int(*deployment.Spec.Replicas)

	// accelerator type
	acc := ""
	if val, ok := opt.Labels["inference.optimization/acceleratorName"]; ok {
		acc = val
	} else {
		logger.Log.Warn("acceleratorName label not found on VariantAutoscaling object", "object-name", opt.Name)
	}

	// cost
	discoveredCost := float64(*deployment.Spec.Replicas) * acceleratorCostVal

	// max batch size
	// TODO: collect value from server
	maxBatch := 256

	// --- 4. Populate Allocation Status ---

	// populate current alloc
	currentAlloc := llmdVariantAutoscalingV1alpha1.Allocation{
		Accelerator: acc,
		NumReplicas: numReplicas,
		MaxBatch:    maxBatch,
		VariantCost: strconv.FormatFloat(float64(discoveredCost), 'f', 2, 32),
		TTFTAverage: strconv.FormatFloat(float64(ttftAverageTime), 'f', 2, 32),
		ITLAverage:  strconv.FormatFloat(float64(itlAverage), 'f', 2, 32),
		Load: llmdVariantAutoscalingV1alpha1.LoadProfile{
			ArrivalRate:     strconv.FormatFloat(float64(arrivalVal), 'f', 2, 32),
			AvgInputTokens:  strconv.FormatFloat(float64(avgInputTokens), 'f', 2, 32),
			AvgOutputTokens: strconv.FormatFloat(float64(avgOutputTokens), 'f', 2, 32),
		},
	}
	return currentAlloc, nil
}

// Helper to handle if a value is NaN or infinite
func FixValue(x *float64) {
	if math.IsNaN(*x) || math.IsInf(*x, 0) {
		*x = 0
	}
}
