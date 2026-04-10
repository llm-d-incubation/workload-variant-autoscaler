package benchmark

import (
	autoscalingv2 "k8s.io/api/autoscaling/v2"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/test/e2e/fixtures"
)

// WithBehavior returns an HPAOption that sets custom scaling behavior on the HPA.
func WithBehavior(behavior *autoscalingv2.HorizontalPodAutoscalerBehavior) fixtures.HPAOption {
	return func(hpa *autoscalingv2.HorizontalPodAutoscaler) {
		hpa.Spec.Behavior = behavior
	}
}
