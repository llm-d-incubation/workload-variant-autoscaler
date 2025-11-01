package actuator

import (
	"context"
	"fmt"

	llmdOptv1alpha1 "github.com/llm-d-incubation/workload-variant-autoscaler/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"

	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/logger"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d-incubation/workload-variant-autoscaler/internal/utils"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

// getCurrentDeploymentReplicas gets the real current replica count from the actual Deployment
func (a *Actuator) getCurrentDeploymentReplicas(ctx context.Context, va *llmdOptv1alpha1.VariantAutoscaling) (int32, error) {
	var deploy appsv1.Deployment
	err := utils.GetDeploymentWithBackoff(ctx, a.Client, va.Name, va.Namespace, &deploy)
	if err != nil {
		return 0, fmt.Errorf("failed to get Deployment %s/%s: %w", va.Namespace, va.Name, err)
	}

	// Prefer status replicas if deployment has been reconciled
	// Check if status is populated (deployment controller has run)
	if deploy.Status.ObservedGeneration > 0 {
		return deploy.Status.Replicas, nil
	}

	// Fallback to spec if status not ready yet (deployment just created)
	if deploy.Spec.Replicas != nil {
		return *deploy.Spec.Replicas, nil
	}

	// Final fallback - deployment exists but has no replicas configured
	return 0, nil
}

func (a *Actuator) EmitMetrics(ctx context.Context, VariantAutoscaling *llmdOptv1alpha1.VariantAutoscaling) error {
	// Emit replica metrics with real-time data for external autoscalers
	if VariantAutoscaling.Status.DesiredOptimizedAlloc.NumReplicas >= 0 {

		// Get real current replicas from Deployment (not stale VariantAutoscaling status)
		currentReplicas, err := a.getCurrentDeploymentReplicas(ctx, VariantAutoscaling)
		if err != nil {
			logger.Log.Warn("Could not get current deployment replicas, using VariantAutoscaling status",
				"error", err, "variant", VariantAutoscaling.Name)
			currentReplicas = int32(VariantAutoscaling.Status.CurrentAlloc.NumReplicas) // fallback
		}

		if err := a.MetricsEmitter.EmitReplicaMetrics(
			ctx,
			VariantAutoscaling,
			currentReplicas, // Real current from Deployment
			int32(VariantAutoscaling.Status.DesiredOptimizedAlloc.NumReplicas), // Inferno's optimization target
			VariantAutoscaling.Spec.Accelerator,                                // Use spec field (single-variant architecture)
			VariantAutoscaling.Spec.VariantID,                                  // Use spec field (single-variant architecture)
		); err != nil {
			logger.Log.Error(err, "Failed to emit optimization signals - this will break HPA/KEDA autoscaling",
				"variant", VariantAutoscaling.Name)
			// Return error - metrics are critical for HPA/KEDA functionality
			return fmt.Errorf("critical metric emission failed for %s: %w", VariantAutoscaling.Name, err)
		}
		logger.Log.Debug("EmitReplicaMetrics completed for ", "variantAutoscaling-name: ", VariantAutoscaling.Name, ", current-replicas: ", VariantAutoscaling.Status.CurrentAlloc.NumReplicas, ", desired-replicas: ", VariantAutoscaling.Status.DesiredOptimizedAlloc.NumReplicas, ", accelerator: ", VariantAutoscaling.Spec.Accelerator)
		return nil
	}
	logger.Log.Info("Skipping EmitReplicaMetrics for variantAutoscaling - ", "variantAutoscaling-name: ", VariantAutoscaling.Name, " - NumReplicas is negative")
	return nil
}
