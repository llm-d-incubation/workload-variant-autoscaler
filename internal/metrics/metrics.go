package metrics

import (
	"context"
	"errors"
	"fmt"
	"os"

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

	optimizationDuration *prometheus.HistogramVec
	modelsProcessedGauge *prometheus.GaugeVec

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

	// Build label sets based on whether controller_instance is configured
	baseLabels := []string{constants.LabelVariantName, constants.LabelNamespace, constants.LabelAcceleratorType}
	scalingLabels := []string{constants.LabelVariantName, constants.LabelNamespace, constants.LabelDirection, constants.LabelReason}

	if controllerInstance != "" {
		baseLabels = append(baseLabels, constants.LabelControllerInstance)
		scalingLabels = append(scalingLabels, constants.LabelControllerInstance)
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
	if err := registry.Register(optimizationDuration); err != nil {
		return fmt.Errorf("failed to register optimizationDuration metric: %w", err)
	}
	if err := registry.Register(modelsProcessedGauge); err != nil {
		return fmt.Errorf("failed to register modelsProcessedGauge metric: %w", err)
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
