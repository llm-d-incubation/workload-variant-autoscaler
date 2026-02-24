package fixtures

import (
	"context"
	"fmt"
	"time"

	autoscalingv1 "k8s.io/api/autoscaling/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
)

// CreateVariantAutoscaling creates a VariantAutoscaling resource
// This function is idempotent: it will delete any existing VA with the same name
// before creating a new one to handle leftover resources from previous test runs.
func CreateVariantAutoscaling(
	ctx context.Context,
	crClient client.Client,
	namespace, name, deploymentName, modelID, accelerator string,
	cost float64,
	controllerInstance string,
) error {
	// Check if VA already exists and delete it to ensure clean state
	existingVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
	err := crClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, existingVA)
	if err == nil {
		// VA exists, delete it first
		deleteErr := crClient.Delete(ctx, existingVA)
		if deleteErr != nil && !errors.IsNotFound(deleteErr) {
			return fmt.Errorf("failed to delete existing VA %s: %w", name, deleteErr)
		}
		// Wait for deletion to complete (with timeout)
		waitCtx, cancel := context.WithTimeout(ctx, 1*time.Minute)
		defer cancel()
		for {
			checkErr := crClient.Get(waitCtx, client.ObjectKey{Namespace: namespace, Name: name}, existingVA)
			if errors.IsNotFound(checkErr) {
				break // VA is fully deleted
			}
			if waitCtx.Err() != nil {
				return fmt.Errorf("timeout waiting for VA %s to be deleted", name)
			}
			time.Sleep(2 * time.Second)
		}
	} else if !errors.IsNotFound(err) {
		// If error is not "not found", return it
		return fmt.Errorf("failed to check for existing VA %s: %w", name, err)
	}

	labels := map[string]string{
		"test-resource":                          "true",
		"inference.optimization/acceleratorName": accelerator,
	}
	if controllerInstance != "" {
		labels["wva.llmd.ai/controller-instance"] = controllerInstance
	}

	va := &variantautoscalingv1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: variantautoscalingv1alpha1.VariantAutoscalingSpec{
			ScaleTargetRef: autoscalingv1.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deploymentName,
			},
			ModelID:     modelID,
			VariantCost: fmt.Sprintf("%.1f", cost),
		},
	}
	return crClient.Create(ctx, va)
}

// CreateVariantAutoscalingWithDefaults creates a VA with default cost
func CreateVariantAutoscalingWithDefaults(
	ctx context.Context,
	crClient client.Client,
	namespace, name, deploymentName, modelID, accelerator string,
	controllerInstance string,
) error {
	return CreateVariantAutoscaling(ctx, crClient, namespace, name, deploymentName, modelID, accelerator, 30.0, controllerInstance)
}
