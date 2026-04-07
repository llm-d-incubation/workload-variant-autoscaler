package v1alpha1

import (
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VariantAutoscalingConfigSpec holds the optional tuning fields for a VariantAutoscaling.
// It is extracted as a standalone embeddable type so that higher-level controllers
// (e.g. KServe) can inline it without duplicating field definitions.
type VariantAutoscalingConfigSpec struct {
	// VariantCost specifies the cost per replica for this variant (used in saturation analysis).
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Pattern=`^\d+(\.\d+)?$`
	// +kubebuilder:default="10.0"
	VariantCost string `json:"variantCost,omitempty"`
}

// VariantAutoscalingSpec defines the desired state for autoscaling a model variant.
// +kubebuilder:validation:XValidation:rule="!has(self.minReplicas) || self.minReplicas <= self.maxReplicas",message="minReplicas must be less than or equal to maxReplicas"
type VariantAutoscalingSpec struct {
	// ScaleTargetRef references the scalable resource to manage.
	// This follows the same pattern as HorizontalPodAutoscaler.
	// +kubebuilder:validation:Required
	ScaleTargetRef autoscalingv2.CrossVersionObjectReference `json:"scaleTargetRef"`

	// ModelID specifies the unique identifier of the model to be autoscaled.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:Required
	ModelID string `json:"modelID"`

	// MinReplicas is the lower bound on the number of replicas for this variant.
	// A value of 0 enables scale-to-zero when the model is idle.
	// Defaults to 1, preserving existing behavior for VAs that omit this field.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	// +optional
	MinReplicas *int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the upper bound on the number of replicas for this variant.
	// The autoscaler will never scale beyond this value regardless of load.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=2
	MaxReplicas int32 `json:"maxReplicas"`

	// VariantAutoscalingConfigSpec holds optional tuning fields that integrators can embed.
	VariantAutoscalingConfigSpec `json:",inline"`
}

// VariantAutoscalingStatus represents the current status of autoscaling for a variant,
// including the current allocation, desired optimized allocation, and actuation status.
type VariantAutoscalingStatus struct {

	// DesiredOptimizedAlloc indicates the target optimized allocation based on autoscaling logic.
	DesiredOptimizedAlloc OptimizedAlloc `json:"desiredOptimizedAlloc,omitempty"`

	// Actuation provides details about the actuation process and its current status.
	Actuation ActuationStatus `json:"actuation,omitempty"`

	// Conditions represent the latest available observations of the VariantAutoscaling's state
	// +kubebuilder:validation:Optional
	// +patchMergeKey=type
	// +patchStrategy=merge
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
}

// OptimizedAlloc describes the target optimized allocation for a model variant.
type OptimizedAlloc struct {
	// LastRunTime is the timestamp of the last optimization run.
	LastRunTime metav1.Time `json:"lastRunTime,omitempty"`

	// Accelerator is the type of accelerator for the optimized allocation.
	//
	// Deprecated: This field is deprecated and will be removed in a future version. Use node selector or node affinity from scale target instead.
	// +optional
	Accelerator string `json:"accelerator,omitempty"`

	// NumReplicas is the number of replicas for the optimized allocation.
	// nil means no optimization decision has been made yet.
	// +kubebuilder:validation:Minimum=0
	NumReplicas *int32 `json:"numReplicas,omitempty"`
}

// ActuationStatus provides details about the actuation process and its current status.
type ActuationStatus struct {
	// Applied indicates whether the actuation was successfully applied.
	Applied bool `json:"applied"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=va
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=".spec.scaleTargetRef.name"
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=".spec.modelID"
// +kubebuilder:printcolumn:name="Min",type=integer,JSONPath=".spec.minReplicas"
// +kubebuilder:printcolumn:name="Max",type=integer,JSONPath=".spec.maxReplicas"
// +kubebuilder:printcolumn:name="Optimized",type=string,JSONPath=".status.desiredOptimizedAlloc.numReplicas"
// +kubebuilder:printcolumn:name="MetricsReady",type=string,JSONPath=".status.conditions[?(@.type=='MetricsAvailable')].status"
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=".metadata.creationTimestamp"

// VariantAutoscaling is the Schema for the variantautoscalings API.
// It represents the autoscaling configuration and status for a model variant.
type VariantAutoscaling struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec defines the desired state for autoscaling the model variant.
	Spec VariantAutoscalingSpec `json:"spec,omitempty"`

	// Status represents the current status of autoscaling for the model variant.
	Status VariantAutoscalingStatus `json:"status,omitempty"`
}

// VariantAutoscalingList contains a list of VariantAutoscaling resources.
// +kubebuilder:object:root=true
type VariantAutoscalingList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	// Items is the list of VariantAutoscaling resources.
	Items []VariantAutoscaling `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VariantAutoscaling{}, &VariantAutoscalingList{})
}

// Condition Types for VariantAutoscaling
const (
	// TypeTargetResolved indicates whether the target model variant has been resolved successfully
	TypeTargetResolved = "TargetResolved"
	// TypeMetricsAvailable indicates whether vLLM metrics are available from Prometheus
	TypeMetricsAvailable = "MetricsAvailable"
	// TypeOptimizationReady indicates whether the optimization engine can run successfully
	TypeOptimizationReady = "OptimizationReady"
)

// Condition Reasons for MetricsAvailable
const (
	// ReasonMetricsFound indicates vLLM metrics were successfully retrieved
	ReasonMetricsFound = "MetricsFound"
	// ReasonMetricsMissing indicates vLLM metrics are not available (likely ServiceMonitor issue)
	ReasonMetricsMissing = "MetricsMissing"
	// ReasonMetricsStale indicates metrics exist but are outdated
	ReasonMetricsStale = "MetricsStale"
	// ReasonPrometheusError indicates error querying Prometheus
	ReasonPrometheusError = "PrometheusError"
)

// Condition messages for MetricsAvailable
const (
	// MessageMetricsAvailable indicates metrics are available for scaling decisions
	MessageMetricsAvailable = "Saturation metrics data is available for scaling decisions"
	// MessageMetricsUnavailable indicates metrics are not available
	MessageMetricsUnavailable = "No saturation metrics available - pods may not be ready or metrics not yet scraped"
)

// Condition Reasons for OptimizationReady
const (
	// ReasonOptimizationSucceeded indicates optimization completed successfully
	ReasonOptimizationSucceeded = "OptimizationSucceeded"
	// ReasonOptimizationFailed indicates optimization failed
	ReasonOptimizationFailed = "OptimizationFailed"
	// ReasonMetricsUnavailable indicates optimization cannot run due to missing metrics
	ReasonMetricsUnavailable = "MetricsUnavailable"
	// ReasonInvalidConfiguration indicates VA has invalid configuration (e.g., missing ModelID)
	ReasonInvalidConfiguration = "InvalidConfiguration"
	// ReasonSkippedProcessing indicates VA was skipped during processing
	ReasonSkippedProcessing = "SkippedProcessing"

	// ReasonTargetFound indicates the scale target was successfully resolved
	ReasonTargetFound = "TargetFound"
	// ReasonTargetNotFound indicates the scale target could not be found
	ReasonTargetNotFound = "TargetNotFound"
)

// GetScaleTargetAPI returns the API of the scale target resource.
func (va *VariantAutoscaling) GetScaleTargetAPI() string {
	return va.Spec.ScaleTargetRef.APIVersion
}

// GetScaleTargetName returns the name of the scale target resource.
func (va *VariantAutoscaling) GetScaleTargetName() string {
	return va.Spec.ScaleTargetRef.Name
}

// GetScaleTargetKind returns the kind of the scale target resource.
func (va *VariantAutoscaling) GetScaleTargetKind() string {
	return va.Spec.ScaleTargetRef.Kind
}
