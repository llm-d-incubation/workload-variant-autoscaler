package indexers

import (
	"context"
	"fmt"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// VAScaleTargetKey is the index field name for looking up VariantAutoscalings by their scale target.
	VAScaleTargetKey = ".spec.scaleTargetRef.nsAPIVersionKindName"
)

// VAScaleTargetIndexFunc is the index function for VariantAutoscaling by scale target.
func VAScaleTargetIndexFunc(o client.Object) []string {
	va := o.(*llmdVariantAutoscalingV1alpha1.VariantAutoscaling)
	if va.Spec.ScaleTargetRef.Kind == "" || va.Spec.ScaleTargetRef.Name == "" {
		return nil
	}
	return []string{scaleTargetIndexKey(va.Namespace, va.Spec.ScaleTargetRef)}
}

// FindVAForScaleTarget returns the VariantAutoscaling that targets the given scale resource.
func FindVAForScaleTarget(ctx context.Context, c client.Client, ref autoscalingv2.CrossVersionObjectReference, namespace string) (*llmdVariantAutoscalingV1alpha1.VariantAutoscaling, error) {
	var vaList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
	if err := c.List(ctx, &vaList,
		client.InNamespace(namespace),
		client.MatchingFields{VAScaleTargetKey: scaleTargetIndexKey(namespace, ref)},
	); err != nil {
		return nil, fmt.Errorf("failed to list VariantAutoscalings for %s %s/%s: %w", ref.Kind, namespace, ref.Name, err)
	}
	if len(vaList.Items) == 0 {
		return nil, nil
	}
	if len(vaList.Items) > 1 {
		return nil, fmt.Errorf("multiple VariantAutoscalings found for %s %s/%s", ref.Kind, namespace, ref.Name)
	}
	return &vaList.Items[0], nil
}

// FindVAForDeployment returns the VariantAutoscaling that targets a Deployment with the given name.
func FindVAForDeployment(ctx context.Context, c client.Client, deploymentName, namespace string) (*llmdVariantAutoscalingV1alpha1.VariantAutoscaling, error) {
	return FindVAForScaleTarget(ctx, c, autoscalingv2.CrossVersionObjectReference{
		APIVersion: constants.DeploymentAPIVersion,
		Kind:       constants.DeploymentKind,
		Name:       deploymentName,
	}, namespace)
}

// FindVAForLeaderWorkerSet returns the VariantAutoscaling that targets a LeaderWorkerSet with the given name.
func FindVAForLeaderWorkerSet(ctx context.Context, c client.Client, leaderWorkerSetName, namespace string) (*llmdVariantAutoscalingV1alpha1.VariantAutoscaling, error) {
	return FindVAForScaleTarget(ctx, c, autoscalingv2.CrossVersionObjectReference{
		APIVersion: constants.LeaderWorkerSetAPIVersion,
		Kind:       constants.LeaderWorkerSetKind,
		Name:       leaderWorkerSetName,
	}, namespace)
}
