package fixtures

import (
	"context"
	"fmt"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	variantautoscalingv1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils"
)

// VAOption is a functional option for configuring VariantAutoscaling resources.
type VAOption func(*variantautoscalingv1alpha1.VariantAutoscaling)

// WithMinReplicas sets the MinReplicas field on the VA spec.
func WithMinReplicas(min int32) VAOption {
	return func(va *variantautoscalingv1alpha1.VariantAutoscaling) {
		va.Spec.MinReplicas = ptr.To(min)
	}
}

// WithMaxReplicas sets the MaxReplicas field on the VA spec.
func WithMaxReplicas(max int32) VAOption {
	return func(va *variantautoscalingv1alpha1.VariantAutoscaling) {
		va.Spec.MaxReplicas = max
	}
}

// CreateVariantAutoscaling creates a VariantAutoscaling resource. Fails if it already exists.
func CreateVariantAutoscaling(
	ctx context.Context,
	crClient client.Client,
	namespace, name, deploymentName, modelID, accelerator string,
	cost float64,
	controllerInstance string,
	opts ...VAOption,
) error {
	va := buildVariantAutoscaling(namespace, name, deploymentName, modelID, accelerator, cost, controllerInstance, opts...)
	return crClient.Create(ctx, va)
}

// DeleteVariantAutoscaling deletes the VariantAutoscaling. Idempotent; ignores NotFound.
func DeleteVariantAutoscaling(ctx context.Context, crClient client.Client, namespace, name string) error {
	va := &variantautoscalingv1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
	err := crClient.Delete(ctx, va)
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("delete VA %s: %w", name, err)
	}
	return nil
}

// EnsureVariantAutoscaling creates or replaces the VariantAutoscaling (idempotent for test setup).
func EnsureVariantAutoscaling(
	ctx context.Context,
	crClient client.Client,
	namespace, name, deploymentName, modelID, accelerator string,
	cost float64,
	controllerInstance string,
	opts ...VAOption,
) error {
	existingVA := &variantautoscalingv1alpha1.VariantAutoscaling{}
	err := crClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, existingVA)
	if err == nil {
		deleteErr := crClient.Delete(ctx, existingVA)
		if deleteErr != nil && !errors.IsNotFound(deleteErr) {
			return fmt.Errorf("delete existing VA %s: %w", name, deleteErr)
		}
		waitCtx, cancel := context.WithTimeout(ctx, 1*time.Minute)
		defer cancel()
		for {
			checkErr := crClient.Get(waitCtx, client.ObjectKey{Namespace: namespace, Name: name}, existingVA)
			if errors.IsNotFound(checkErr) {
				break
			}
			if waitCtx.Err() != nil {
				return fmt.Errorf("timeout waiting for VA %s to be deleted", name)
			}
			time.Sleep(2 * time.Second)
		}
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("check existing VA %s: %w", name, err)
	}
	va := buildVariantAutoscaling(namespace, name, deploymentName, modelID, accelerator, cost, controllerInstance, opts...)
	return crClient.Create(ctx, va)
}

// CreateVariantAutoscalingWithDefaults creates a VA with default cost. Fails if it already exists.
func CreateVariantAutoscalingWithDefaults(
	ctx context.Context,
	crClient client.Client,
	namespace, name, deploymentName, modelID, accelerator string,
	controllerInstance string,
) error {
	return CreateVariantAutoscaling(ctx, crClient, namespace, name, deploymentName, modelID, accelerator, 30.0, controllerInstance)
}

// EnsureVariantAutoscalingWithDefaults creates or replaces a VA with default cost (idempotent for test setup).
func EnsureVariantAutoscalingWithDefaults(
	ctx context.Context,
	crClient client.Client,
	namespace, name, deploymentName, modelID, accelerator string,
	controllerInstance string,
) error {
	return EnsureVariantAutoscaling(ctx, crClient, namespace, name, deploymentName, modelID, accelerator, 30.0, controllerInstance)
}

func buildVariantAutoscaling(namespace, name, deploymentName, modelID, accelerator string, cost float64, controllerInstance string, opts ...VAOption) *variantautoscalingv1alpha1.VariantAutoscaling {
	labels := map[string]string{
		"test-resource":            "true",
		utils.AcceleratorNameLabel: accelerator,
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
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deploymentName,
			},
			ModelID:     modelID,
			MinReplicas: ptr.To(int32(1)),
			MaxReplicas: 10,
			VariantAutoscalingConfigSpec: variantautoscalingv1alpha1.VariantAutoscalingConfigSpec{
				VariantCost: fmt.Sprintf("%.1f", cost),
			},
		},
	}
	for _, opt := range opts {
		opt(va)
	}
	return va
}
