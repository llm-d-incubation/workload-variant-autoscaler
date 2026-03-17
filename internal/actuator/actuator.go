package actuator

import (
	"context"
	"fmt"

	llmdOptv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type Actuator struct {
	Client         client.Client
	MetricsEmitter *metrics.MetricsEmitter
}

func NewActuator(k8sClient client.Client) *Actuator {
	return &Actuator{
		Client:         k8sClient,
		MetricsEmitter: metrics.NewMetricsEmitter(),
	}
}

// GetCurrentDeploymentReplicas gets the real current replica count from the actual Deployment
func (a *Actuator) GetCurrentDeploymentReplicasFromVA(ctx context.Context, va *llmdOptv1alpha1.VariantAutoscaling) (int32, error) {
	var deploy appsv1.Deployment
	// Use ScaleTargetRef to get the deployment name
	err := utils.GetDeploymentWithBackoff(ctx, a.Client, va.GetScaleTargetName(), va.Namespace, &deploy)
	if err != nil {
		return 0, fmt.Errorf("failed to get Deployment %s/%s: %w", va.Namespace, va.GetScaleTargetName(), err)
	}

	return a.GetCurrentDeploymentReplicasFromDeployment(va, &deploy)
}

// GetCurrentDeploymentReplicas gets the real current replica count from the actual Deployment
func (a *Actuator) GetCurrentDeploymentReplicasFromDeployment(va *llmdOptv1alpha1.VariantAutoscaling, deployment *appsv1.Deployment) (int32, error) {
	if deployment == nil {
		return 0, fmt.Errorf("deployment cannot be nil for %s/%s", va.Namespace, va.GetScaleTargetName())
	}

	// Prefer status replicas (actual current state)
	if deployment.Status.Replicas >= 0 {
		return deployment.Status.Replicas, nil
	}

	// Fallback to spec if status not ready
	if deployment.Spec.Replicas != nil {
		return *deployment.Spec.Replicas, nil
	}

	// Final fallback
	return 1, nil
}

func (a *Actuator) EmitMetrics(ctx context.Context, VariantAutoscaling *llmdOptv1alpha1.VariantAutoscaling) error {
	// Emit replica metrics with real-time data for external autoscalers
	logger := log.FromContext(ctx)
	if VariantAutoscaling.Status.DesiredOptimizedAlloc.NumReplicas >= 0 {

		// Get real current replicas from Deployment (not stale VariantAutoscaling status)
		currentReplicas, err := a.GetCurrentDeploymentReplicasFromVA(ctx, VariantAutoscaling)
		if err != nil {
			logger.Error(err, "Could not get current deployment replicas, using VariantAutoscaling status",
				"variantName", VariantAutoscaling.Name)
			currentReplicas = 0 // Fallback to 0 since CurrentAlloc is removed
		}

		if err := a.MetricsEmitter.EmitReplicaMetrics(
			ctx,
			VariantAutoscaling,
			currentReplicas, // Real current from Deployment
			int32(VariantAutoscaling.Status.DesiredOptimizedAlloc.NumReplicas), // Inferno's optimization target
			VariantAutoscaling.Status.DesiredOptimizedAlloc.Accelerator,
		); err != nil {
			logger.Error(err, "Failed to emit optimization signals for variantAutoscaling",
				"variantName", VariantAutoscaling.Name)
			// Don't fail the reconciliation for metric emission errors
			// Metrics are critical for HPA, but emission failures shouldn't break core functionality
			return nil
		}
		logger.Info("EmitReplicaMetrics completed",
			"variantName", VariantAutoscaling.Name,
			"currentReplicas", currentReplicas,
			"desiredReplicas", VariantAutoscaling.Status.DesiredOptimizedAlloc.NumReplicas,
			"accelerator", VariantAutoscaling.Status.DesiredOptimizedAlloc.Accelerator)
		return nil
	}
	logger.Info("Skipping EmitReplicaMetrics - NumReplicas is 0",
		"variantName", VariantAutoscaling.Name)
	return nil
}
