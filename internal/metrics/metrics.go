package metrics

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"

	llmdOptv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/prometheus/client_golang/prometheus"
)

// ControllerInstanceEnvVar is the environment variable name for controller instance label
const ControllerInstanceEnvVar = "CONTROLLER_INSTANCE"

var (
	replicaScalingTotal *prometheus.CounterVec
	desiredReplicas     *prometheus.GaugeVec
	currentReplicas     *prometheus.GaugeVec
	desiredRatio        *prometheus.GaugeVec
	errorsTotal         *prometheus.CounterVec

	optimizationDuration *prometheus.HistogramVec
	modelsProcessedGauge *prometheus.GaugeVec

	configInfoGauge                     *prometheus.GaugeVec
	configKvSpareThresholdGauge         *prometheus.GaugeVec
	configQueueSpareThresholdGauge      *prometheus.GaugeVec
	configOptimizationIntervalSecsGauge *prometheus.GaugeVec
	metricsCollectionDuration           *prometheus.HistogramVec
	metricsCollectionErrors             *prometheus.CounterVec
	metricsPodsDiscovered               *prometheus.GaugeVec
	metricsFreshnessStatus              *prometheus.GaugeVec

	// Saturation and capacity metrics
	saturationUtilization *prometheus.GaugeVec
	spareCapacity         *prometheus.GaugeVec
	requiredCapacity      *prometheus.GaugeVec
	kvCacheTokensUsed     *prometheus.GaugeVec
	kvCacheTokensCapacity *prometheus.GaugeVec

	// controllerInstance stores the optional controller instance identifier.
	// When set, it's added as a label to all emitted metrics.
	controllerInstance string
)

// GetControllerInstance returns the configured controller instance label value
// Returns empty string if not configured
func GetControllerInstance() string {
	return controllerInstance
}

// InitMetrics registers all custom metrics with the provided registry.
// This function should be called once during application startup from main().
// It reads CONTROLLER_INSTANCE from the environment to optionally add
// controller instance isolation labels to all emitted metrics.
func InitMetrics(registry prometheus.Registerer) error {
	// Read controller instance from environment
	controllerInstance = os.Getenv(ControllerInstanceEnvVar)

	// Build label sets based on whether controller_instance is configured.
	// Existing replica metrics (baseLabels, scalingLabels) are unchanged.
	// New saturation metrics carry an additional model_name label so dashboards
	// can group/filter by the model a variant serves.
	baseLabels := []string{constants.LabelVariantName, constants.LabelNamespace, constants.LabelAcceleratorType}
	scalingLabels := []string{constants.LabelVariantName, constants.LabelNamespace, constants.LabelDirection, constants.LabelReason}
	errorLabels := []string{constants.LabelComponent, constants.LabelErrorType}
	// satAccelLabels: per-variant per-accelerator saturation metrics.
	satAccelLabels := []string{constants.LabelVariantName, constants.LabelNamespace, constants.LabelModelName, constants.LabelAcceleratorType}
	// satModelLabels: per-variant model-level saturation metrics (no accelerator_type).
	satModelLabels := []string{constants.LabelVariantName, constants.LabelNamespace, constants.LabelModelName}
	// requiredCapacityLabels: satModelLabels + "unit" to disambiguate V1 (binary 0/1)
	// vs V2 (continuous token demand) values of the wva_required_capacity gauge.
	requiredCapacityLabels := []string{constants.LabelVariantName, constants.LabelNamespace, constants.LabelModelName, constants.LabelUnit}

	if controllerInstance != "" {
		baseLabels = append(baseLabels, constants.LabelControllerInstance)
		scalingLabels = append(scalingLabels, constants.LabelControllerInstance)
		errorLabels = append(errorLabels, constants.LabelControllerInstance)
		satAccelLabels = append(satAccelLabels, constants.LabelControllerInstance)
		satModelLabels = append(satModelLabels, constants.LabelControllerInstance)
		requiredCapacityLabels = append(requiredCapacityLabels, constants.LabelControllerInstance)
	}

	replicaScalingTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: constants.WVAReplicaScalingTotal,
			Help: "Total number of replica scaling operations",
		},
		scalingLabels,
	)
	desiredReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVADesiredReplicas,
			Help: "Desired number of replicas for each variant",
		},
		baseLabels,
	)
	currentReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVACurrentReplicas,
			Help: "Current number of replicas for each variant",
		},
		baseLabels,
	)
	desiredRatio = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVADesiredRatio,
			Help: "Ratio of the desired number of replicas and the current number of replicas for each variant",
		},
		baseLabels,
	)
	errorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: constants.WVAErrorsTotal,
			Help: "Total number of errors by component",
		},
		errorLabels,
	)
	saturationUtilization = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVASaturationUtilization,
			Help: "Per-variant utilization ratio (0.0-1.0) from saturation analysis. V1 path: mean of per-replica KV-cache-usage fractions (matches the per-replica threshold V1 checks). V2 path: TotalDemand / TotalCapacity from the analyzer result. Numerically equivalent for uniform-capacity replicas; V2 is capacity-weighted for mixed-capacity cases.",
		},
		satAccelLabels,
	)
	spareCapacity = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVASpareCapacity,
			Help: "Per-variant spare KV-cache capacity (0.0-1.0) from saturation analysis. V1 path: threshold-relative spare (kvCacheThreshold - avg KV usage). V2 path: 1.0 - utilization.",
		},
		satAccelLabels,
	)
	requiredCapacity = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVARequiredCapacity,
			Help: fmt.Sprintf("Model-level required capacity; >0 indicates scale-up needed. Use the %q label to interpret the value: %q → 0/1 scale-up signal (V1), %q → token demand (V2).", constants.LabelUnit, constants.UnitBinary, constants.UnitContinuous),
		},
		requiredCapacityLabels,
	)
	kvCacheTokensUsed = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAKvCacheTokensUsed,
			Help: "Total KV cache tokens currently in use across all replicas of a variant (sum of vLLM TokensInUse).",
		},
		satModelLabels,
	)
	kvCacheTokensCapacity = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAKvCacheTokensCapacity,
			Help: "Total KV cache token capacity across all replicas of a variant (sum of vLLM TotalKvCapacityTokens).",
		},
		satModelLabels,
	)

	optimizationDurationLabels := []string{constants.LabelStatus}
	if controllerInstance != "" {
		optimizationDurationLabels = append(optimizationDurationLabels, constants.LabelControllerInstance)
	}
	optimizationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    constants.WVAOptimizationDurationSeconds,
			Help:    "Duration of optimization loop cycles in seconds",
			Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
		},
		optimizationDurationLabels,
	)
	modelsProcessedLabels := []string{}
	if controllerInstance != "" {
		modelsProcessedLabels = append(modelsProcessedLabels, constants.LabelControllerInstance)
	}
	modelsProcessedGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAModelsProcessed,
			Help: "Number of models processed in the last optimization cycle",
		},
		modelsProcessedLabels,
	)

	// Config info metric with labels
	configInfoLabels := []string{constants.LabelAnalyzerName, constants.LabelLimiterEnabled, constants.LabelScaleToZeroEnabled}
	if controllerInstance != "" {
		configInfoLabels = append(configInfoLabels, constants.LabelControllerInstance)
	}
	configInfoGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAConfigInfo,
			Help: "WVA configuration information (value is always 1)",
		},
		configInfoLabels,
	)

	// Config threshold and interval metrics
	configLabels := []string{}
	if controllerInstance != "" {
		configLabels = append(configLabels, constants.LabelControllerInstance)
	}
	configKvSpareThresholdGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAConfigKvSpareThreshold,
			Help: "KV cache spare threshold configuration value",
		},
		configLabels,
	)
	configQueueSpareThresholdGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAConfigQueueSpareThreshold,
			Help: "Queue spare threshold configuration value",
		},
		configLabels,
	)
	configOptimizationIntervalSecsGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAConfigOptimizationIntervalSeconds,
			Help: "Optimization interval in seconds",
		},
		configLabels,
	)

	metricsCollectionDurationLabels := []string{constants.LabelQueryType}
	if controllerInstance != "" {
		metricsCollectionDurationLabels = append(metricsCollectionDurationLabels, constants.LabelControllerInstance)
	}
	metricsCollectionDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    constants.WVAMetricsCollectionDurationSeconds,
			Help:    "Duration of metrics collection operations in seconds",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5},
		},
		metricsCollectionDurationLabels,
	)

	metricsCollectionErrorsLabels := []string{constants.LabelQueryType, constants.LabelReason}
	if controllerInstance != "" {
		metricsCollectionErrorsLabels = append(metricsCollectionErrorsLabels, constants.LabelControllerInstance)
	}
	metricsCollectionErrors = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: constants.WVAMetricsCollectionErrorsTotal,
			Help: "Total number of metrics collection errors",
		},
		metricsCollectionErrorsLabels,
	)

	metricsPodsDiscoveredLabels := []string{constants.LabelNamespace}
	if controllerInstance != "" {
		metricsPodsDiscoveredLabels = append(metricsPodsDiscoveredLabels, constants.LabelControllerInstance)
	}
	metricsPodsDiscovered = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAMetricsPodsDiscovered,
			Help: "Number of pods discovered for a namespace",
		},
		metricsPodsDiscoveredLabels,
	)

	metricsFreshnessStatusLabels := []string{constants.LabelVariantName, constants.LabelStatus}
	if controllerInstance != "" {
		metricsFreshnessStatusLabels = append(metricsFreshnessStatusLabels, constants.LabelControllerInstance)
	}
	metricsFreshnessStatus = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.WVAMetricsFreshnessStatus,
			Help: "Freshness status of metrics for each variant",
		},
		metricsFreshnessStatusLabels,
	)

	// Register metrics with the registry
	if err := registry.Register(replicaScalingTotal); err != nil {
		return fmt.Errorf("failed to register replicaScalingTotal metric: %w", err)
	}
	if err := registry.Register(desiredReplicas); err != nil {
		return fmt.Errorf("failed to register desiredReplicas metric: %w", err)
	}
	if err := registry.Register(currentReplicas); err != nil {
		return fmt.Errorf("failed to register currentReplicas metric: %w", err)
	}
	if err := registry.Register(desiredRatio); err != nil {
		return fmt.Errorf("failed to register desiredRatio metric: %w", err)
	}
	if err := registry.Register(errorsTotal); err != nil {
		return fmt.Errorf("failed to register errorsTotal metric: %w", err)
	}
	if err := registry.Register(optimizationDuration); err != nil {
		return fmt.Errorf("failed to register optimizationDuration metric: %w", err)
	}
	if err := registry.Register(modelsProcessedGauge); err != nil {
		return fmt.Errorf("failed to register modelsProcessedGauge metric: %w", err)
	}
	if err := registry.Register(configInfoGauge); err != nil {
		return fmt.Errorf("failed to register configInfoGauge metric: %w", err)
	}
	if err := registry.Register(configKvSpareThresholdGauge); err != nil {
		return fmt.Errorf("failed to register configKvSpareThresholdGauge metric: %w", err)
	}
	if err := registry.Register(configQueueSpareThresholdGauge); err != nil {
		return fmt.Errorf("failed to register configQueueSpareThresholdGauge metric: %w", err)
	}
	if err := registry.Register(configOptimizationIntervalSecsGauge); err != nil {
		return fmt.Errorf("failed to register configOptimizationIntervalSecsGauge metric: %w", err)
	}
	if err := registry.Register(metricsCollectionDuration); err != nil {
		return fmt.Errorf("failed to register metricsCollectionDuration metric: %w", err)
	}
	if err := registry.Register(metricsCollectionErrors); err != nil {
		return fmt.Errorf("failed to register metricsCollectionErrors metric: %w", err)
	}
	if err := registry.Register(metricsPodsDiscovered); err != nil {
		return fmt.Errorf("failed to register metricsPodsDiscovered metric: %w", err)
	}
	if err := registry.Register(metricsFreshnessStatus); err != nil {
		return fmt.Errorf("failed to register metricsFreshnessStatus metric: %w", err)
	}
	if err := registry.Register(saturationUtilization); err != nil {
		return fmt.Errorf("failed to register saturationUtilization metric: %w", err)
	}
	if err := registry.Register(spareCapacity); err != nil {
		return fmt.Errorf("failed to register spareCapacity metric: %w", err)
	}
	if err := registry.Register(requiredCapacity); err != nil {
		return fmt.Errorf("failed to register requiredCapacity metric: %w", err)
	}
	if err := registry.Register(kvCacheTokensUsed); err != nil {
		return fmt.Errorf("failed to register kvCacheTokensUsed metric: %w", err)
	}
	if err := registry.Register(kvCacheTokensCapacity); err != nil {
		return fmt.Errorf("failed to register kvCacheTokensCapacity metric: %w", err)
	}

	return nil
}

// InitMetricsAndEmitter registers metrics with Prometheus and creates a metrics emitter
// This is a convenience function that handles both registration and emitter creation
func InitMetricsAndEmitter(registry prometheus.Registerer) (*MetricsEmitter, error) {
	if err := InitMetrics(registry); err != nil {
		return nil, err
	}
	return NewMetricsEmitter(), nil
}

// MetricsEmitter handles emission of custom metrics
type MetricsEmitter struct{}

// NewMetricsEmitter creates a new metrics emitter
func NewMetricsEmitter() *MetricsEmitter {
	return &MetricsEmitter{}
}

// EmitReplicaScalingMetrics emits metrics related to replica scaling
func (m *MetricsEmitter) EmitReplicaScalingMetrics(ctx context.Context, va *llmdOptv1alpha1.VariantAutoscaling, direction, reason string) error {
	labels := prometheus.Labels{
		constants.LabelVariantName: va.Name,
		constants.LabelNamespace:   va.Namespace,
		constants.LabelDirection:   direction,
		constants.LabelReason:      reason,
	}

	// Add controller_instance label if configured
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}

	// These operations are local and should never fail, but we handle errors for debugging
	if replicaScalingTotal == nil {
		return errors.New("replicaScalingTotal metric not initialized")
	}

	replicaScalingTotal.With(labels).Inc()
	return nil
}

// ObserveOptimizationDuration records the duration of an optimization cycle with the given status.
// Status should be one of: "success", "error".
func ObserveOptimizationDuration(durationSeconds float64, status string) {
	if optimizationDuration == nil {
		return
	}
	labels := prometheus.Labels{constants.LabelStatus: status}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	optimizationDuration.With(labels).Observe(durationSeconds)
}

// SetModelsProcessed sets the gauge to the number of models processed in the last optimization cycle.
func SetModelsProcessed(count int) {
	if modelsProcessedGauge == nil {
		return
	}
	labels := prometheus.Labels{}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	modelsProcessedGauge.With(labels).Set(float64(count))
}

// EmitReplicaMetrics emits current and desired replica metrics
func (m *MetricsEmitter) EmitReplicaMetrics(ctx context.Context, va *llmdOptv1alpha1.VariantAutoscaling, current, desired int32, acceleratorType string) error {
	baseLabels := prometheus.Labels{
		constants.LabelVariantName:     va.Name,
		constants.LabelNamespace:       va.Namespace,
		constants.LabelAcceleratorType: acceleratorType,
	}

	// Add controller_instance label if configured
	if controllerInstance != "" {
		baseLabels[constants.LabelControllerInstance] = controllerInstance
	}

	// These operations are local and should never fail, but we handle errors for debugging
	if currentReplicas == nil || desiredReplicas == nil || desiredRatio == nil {
		return errors.New("replica metrics not initialized")
	}

	currentReplicas.With(baseLabels).Set(float64(current))
	desiredReplicas.With(baseLabels).Set(float64(desired))

	// Avoid division by 0 if current replicas is zero: set the ratio to the desired replicas
	// Going 0 -> N is treated by using `desired_ratio = N`
	if current == 0 {
		desiredRatio.With(baseLabels).Set(float64(desired))
		return nil
	}
	desiredRatio.With(baseLabels).Set(float64(desired) / float64(current))
	return nil
}

// RecordError records an error metric for the specified component and error type
// Callers MUST invoke InitMetrics before this method (the package-level
// metric vars are nil otherwise, and the Set calls below would panic).
// InitMetricsAndEmitter is the recommended construction path because it
// performs the registration before returning the emitter.
func RecordError(component, errorType string) {
	if errorsTotal == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelComponent: component,
		constants.LabelErrorType: errorType,
	}

	// Add controller_instance label if configured
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	errorsTotal.With(labels).Inc()
}

// SetConfigInfo sets the config info metric with the given analyzer name and feature flags.
// The metric value is always set to 1 (info-style metric).
func SetConfigInfo(analyzerName string, limiterEnabled, scaleToZeroEnabled bool) {
	if configInfoGauge == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelAnalyzerName:       analyzerName,
		constants.LabelLimiterEnabled:     strconv.FormatBool(limiterEnabled),
		constants.LabelScaleToZeroEnabled: strconv.FormatBool(scaleToZeroEnabled),
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	configInfoGauge.With(labels).Set(1)
}

// SetConfigKvSpareThreshold sets the KV spare threshold configuration value.
func SetConfigKvSpareThreshold(threshold float64) {
	if configKvSpareThresholdGauge == nil {
		return
	}
	labels := prometheus.Labels{}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	configKvSpareThresholdGauge.With(labels).Set(threshold)
}

// SetConfigQueueSpareThreshold sets the queue spare threshold configuration value.
func SetConfigQueueSpareThreshold(threshold float64) {
	if configQueueSpareThresholdGauge == nil {
		return
	}
	labels := prometheus.Labels{}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	configQueueSpareThresholdGauge.With(labels).Set(threshold)
}

// SetConfigOptimizationInterval sets the optimization interval in seconds.
func SetConfigOptimizationInterval(intervalSeconds float64) {
	if configOptimizationIntervalSecsGauge == nil {
		return
	}
	labels := prometheus.Labels{}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	configOptimizationIntervalSecsGauge.With(labels).Set(intervalSeconds)
}

// ObserveMetricsCollectionDuration records the duration of a metrics collection operation.
func ObserveMetricsCollectionDuration(durationSeconds float64, queryType string) {
	if metricsCollectionDuration == nil {
		return
	}
	labels := prometheus.Labels{constants.LabelQueryType: queryType}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	metricsCollectionDuration.With(labels).Observe(durationSeconds)
}

// IncMetricsCollectionErrors increments the metrics collection error counter.
func IncMetricsCollectionErrors(queryType, reason string) {
	if metricsCollectionErrors == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelQueryType: queryType,
		constants.LabelReason:    reason,
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	metricsCollectionErrors.With(labels).Inc()
}

// SetMetricsPodsDiscovered sets the number of pods discovered in a namespace.
func SetMetricsPodsDiscovered(namespace string, count int) {
	if metricsPodsDiscovered == nil {
		return
	}
	labels := prometheus.Labels{constants.LabelNamespace: namespace}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}
	metricsPodsDiscovered.With(labels).Set(float64(count))
}

// SetMetricsFreshnessStatus sets the freshness status count for a variant's metrics.
// status should be one of: "fresh", "stale", "missing", "unavailable".
// count is the number of metrics with this status for the variant.
func SetMetricsFreshnessStatus(variantName, status string, count int) {
	if metricsFreshnessStatus == nil {
		return
	}
	labels := prometheus.Labels{
		constants.LabelVariantName: variantName,
		constants.LabelStatus:      status,
	}
	if controllerInstance != "" {
		labels[constants.LabelControllerInstance] = controllerInstance
	}

	metricsFreshnessStatus.With(labels).Set(float64(count))
}

// RecordSaturationMetrics records saturation analysis and KV cache capacity
// metrics. The "Record" naming reflects the fact that the controller does not
// actively push these metrics — Prometheus scrapes them.
//
// modelID is exposed as the model_name label so dashboards can group/filter by
// the model a variant serves. requiredCapacityUnit ("binary" or "continuous")
// is used as the "unit" label on wva_required_capacity to describe how the
// value should be interpreted.
//
// Callers MUST invoke InitMetrics before this method (the package-level
// metric vars are nil otherwise, and the Set calls below would panic).
// InitMetricsAndEmitter is the recommended construction path because it
// performs the registration before returning the emitter.
func (m *MetricsEmitter) RecordSaturationMetrics(
	ctx context.Context,
	variantName, namespace, modelID, acceleratorType, requiredCapacityUnit string,
	utilization, spare, required float64,
	kvTokensUsed, kvTokensCapacity int64,
) {
	accelLabels := prometheus.Labels{
		constants.LabelVariantName:     variantName,
		constants.LabelNamespace:       namespace,
		constants.LabelModelName:       modelID,
		constants.LabelAcceleratorType: acceleratorType,
	}
	modelLabels := prometheus.Labels{
		constants.LabelVariantName: variantName,
		constants.LabelNamespace:   namespace,
		constants.LabelModelName:   modelID,
	}
	requiredLabels := prometheus.Labels{
		constants.LabelVariantName: variantName,
		constants.LabelNamespace:   namespace,
		constants.LabelModelName:   modelID,
		constants.LabelUnit:        requiredCapacityUnit,
	}

	if controllerInstance != "" {
		accelLabels[constants.LabelControllerInstance] = controllerInstance
		modelLabels[constants.LabelControllerInstance] = controllerInstance
		requiredLabels[constants.LabelControllerInstance] = controllerInstance
	}

	saturationUtilization.With(accelLabels).Set(utilization)
	spareCapacity.With(accelLabels).Set(spare)
	requiredCapacity.With(requiredLabels).Set(required)
	kvCacheTokensUsed.With(modelLabels).Set(float64(kvTokensUsed))
	kvCacheTokensCapacity.With(modelLabels).Set(float64(kvTokensCapacity))
}
