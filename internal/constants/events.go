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

package constants

// Kubernetes Event Reasons emitted by the WVA on VariantAutoscaling resources.
// These follow CamelCase convention per Kubernetes API conventions.
// View via: kubectl describe variantautoscaling <name>
const (
	// EventReasonScaledUp is a Normal event emitted when target replicas increased.
	EventReasonScaledUp = "ScaledUp"

	// EventReasonScaledDown is a Normal event emitted when target replicas decreased.
	EventReasonScaledDown = "ScaledDown"

	// EventReasonScaledToZero is a Normal event emitted when the scale-to-zero
	// enforcer set the replica count to 0 due to no active requests.
	EventReasonScaledToZero = "ScaledToZero"

	// EventReasonResourceConstrained is a Warning event emitted when a scaling
	// decision was constrained by GPU resource availability (limiter applied).
	EventReasonResourceConstrained = "ResourceConstrained"

	// EventReasonMetricsUnavailable is a Warning event emitted when Prometheus
	// metrics could not be collected for a variant.
	EventReasonMetricsUnavailable = "MetricsUnavailable"

	// EventReasonOptimizationFailed is a Warning event emitted when the
	// optimization analysis or optimizer returned an error for a variant.
	EventReasonOptimizationFailed = "OptimizationFailed"
)
