package v1alpha1

import (
	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
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

// Scaling backend type constants.
const (
	ScalingBackendTypeHPA  = "hpa"
	ScalingBackendTypeKEDA = "keda"
)

// ScalingBackendSpec selects how the controller materializes a scaler for this VA.
// When nil on the parent VariantAutoscalingSpec, the controller only emits
// Prometheus metrics (metrics-only / legacy mode).
type ScalingBackendSpec struct {
	// Type is the scaling backend: "hpa" creates a controller-managed
	// autoscaling/v2 HorizontalPodAutoscaler backed by the wva_desired_replicas
	// external metric; "keda" creates a controller-managed keda.sh/v1alpha1
	// ScaledObject with a prometheus trigger on the same metric.
	// +kubebuilder:validation:Enum=hpa;keda
	// +kubebuilder:default=hpa
	// +kubebuilder:validation:Required
	Type string `json:"type"`

	// HPA contains HPA-specific configuration. Used when Type is "hpa".
	// +kubebuilder:validation:Optional
	HPA *HPAConfig `json:"hpa,omitempty"`

	// KEDA contains KEDA-specific configuration. Used when Type is "keda".
	// +kubebuilder:validation:Optional
	KEDA *KEDAConfig `json:"keda,omitempty"`
}

// HPAConfig holds settings for a controller-managed autoscaling/v2
// HorizontalPodAutoscaler.
type HPAConfig struct {
	// Behavior configures scaling behavior policies on the generated HPA.
	// Maps directly to HorizontalPodAutoscalerSpec.Behavior.
	// +kubebuilder:validation:Optional
	Behavior *autoscalingv2.HorizontalPodAutoscalerBehavior `json:"behavior,omitempty"`
}

// KEDAConfig holds optional tuning for a controller-managed keda.sh/v1alpha1
// ScaledObject. Field names mirror KEDA ScaledObject spec keys where applicable.
//
// Important terminology:
//   - IdleReplicaCount maps to ScaledObject.spec.idleReplicaCount: the replica
//     target when KEDA triggers are inactive. This is NOT the same as
//     VariantAutoscaling.spec.minReplicas (which maps to ScaledObject.spec.minReplicaCount,
//     the floor while KEDA is actively scaling from metrics).
type KEDAConfig struct {
	// PollingInterval is the interval in seconds to check each trigger.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=1
	PollingInterval *int32 `json:"pollingInterval,omitempty"`

	// CooldownPeriod is the period in seconds to wait after the last trigger
	// fired before scaling the resource back to minReplicaCount.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=0
	CooldownPeriod *int32 `json:"cooldownPeriod,omitempty"`

	// InitialCooldownPeriod is the delay in seconds after ScaledObject creation
	// before the first scale-down is allowed.
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=0
	InitialCooldownPeriod *int32 `json:"initialCooldownPeriod,omitempty"`

	// IdleReplicaCount is the replica count when all KEDA triggers report inactive.
	// Maps to ScaledObject.spec.idleReplicaCount. Omit to use KEDA defaults.
	// Not the same as VariantAutoscaling.spec.minReplicas (see type-level doc).
	// +kubebuilder:validation:Optional
	// +kubebuilder:validation:Minimum=0
	IdleReplicaCount *int32 `json:"idleReplicaCount,omitempty"`

	// Fallback configures KEDA fallback behavior when a scaler fails.
	// Maps to ScaledObject.spec.fallback.
	// +kubebuilder:validation:Optional
	Fallback *kedav1alpha1.Fallback `json:"fallback,omitempty"`

	// Advanced configures KEDA advanced options such as HPA behavior overrides
	// and replica restoration. Maps to ScaledObject.spec.advanced.
	// +kubebuilder:validation:Optional
	Advanced *kedav1alpha1.AdvancedConfig `json:"advanced,omitempty"`
}

// EffectiveScalingBackendType returns the resolved backend type.
// Returns empty string when sb is nil (metrics-only mode).
func EffectiveScalingBackendType(sb *ScalingBackendSpec) string {
	if sb == nil {
		return ""
	}
	if sb.Type == "" {
		return ScalingBackendTypeHPA
	}
	return sb.Type
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

	// ScalingBackend selects whether WVA manages an HPA or a KEDA ScaledObject
	// for this VariantAutoscaling. When nil the controller only emits Prometheus
	// metrics (legacy / metrics-only mode).
	// +kubebuilder:validation:Optional
	ScalingBackend *ScalingBackendSpec `json:"scalingBackend,omitempty"`

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

// Condition Types for ScalingBackend
const (
	// TypeScalingBackendReady indicates whether the managed scaler resource
	// (HPA or ScaledObject) has been reconciled successfully.
	TypeScalingBackendReady = "ScalingBackendReady"
)

// Condition Reasons for ScalingBackendReady
const (
	// ReasonScalingBackendReconciled indicates the managed scaler was created or updated.
	ReasonScalingBackendReconciled = "ScalingBackendReconciled"
	// ReasonScalingBackendKEDANotInstalled indicates KEDA CRDs are not present in the cluster.
	ReasonScalingBackendKEDANotInstalled = "KEDANotInstalled"
	// ReasonScalingBackendError indicates an error occurred while reconciling the scaler.
	ReasonScalingBackendError = "ScalingBackendError"
	// ReasonScalingBackendUnsupportedTarget indicates the scale target kind is not
	// compatible with the selected backend.
	ReasonScalingBackendUnsupportedTarget = "UnsupportedScaleTarget"
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
