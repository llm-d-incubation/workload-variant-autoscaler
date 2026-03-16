/*
Copyright 2025.

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

package indexers

import (
	"context"
	"fmt"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

const (
	// VAScaleTargetKey is the index field name for looking up VariantAutoscalings by their scale target.
	// The index value is a composite key in the format "Namespace/APIVersion/Kind/Name"
	// (e.g., "default/apps/v1/Deployment/my-app") to uniquely identify scale targets across namespaces
	// and avoid collisions between different resource types and API versions.
	VAScaleTargetKey = ".spec.scaleTargetRef.nsAPIVersionKindName"
)

// scaleTargetIndexKey returns the composite index key for a scale target reference.
// Format: Namespace/APIVersion/Kind/Name (e.g., "default/apps/v1/Deployment/my-app")
func scaleTargetIndexKey(namespace string, ref autoscalingv2.CrossVersionObjectReference) string {

	if ref.APIVersion == "" {
		switch ref.Kind {
		case "Deployment":
			ref.APIVersion = "apps/v1"

		// Note: add other Kinds when support to other scaleTargetRefs is added
		// By default, assume 'apps/v1' for unsupported Kinds
		default:
			logger := ctrl.LoggerFrom(context.TODO())
			logger.V(logging.DEBUG).Info("APIVersion not specified for scale target; defaulting to apps/v1", "kind", ref.Kind, "name", ref.Name)
			ref.APIVersion = "apps/v1"
		}
	}

	return fmt.Sprintf("%s/%s/%s/%s", namespace, ref.APIVersion, ref.Kind, ref.Name)
}

// SetupIndexes registers custom indexes with the manager's cache.
func SetupIndexes(ctx context.Context, mgr manager.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(ctx, &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}, VAScaleTargetKey, VAScaleTargetIndexFunc); err != nil {
		return fmt.Errorf("failed to set up index by scale target for VariantAutoscaling: %w", err)
	}
	return nil
}

// VAScaleTargetIndexFunc is the index function for VariantAutoscaling by scale target.
func VAScaleTargetIndexFunc(o client.Object) []string {
	va := o.(*llmdVariantAutoscalingV1alpha1.VariantAutoscaling)
	if va.Spec.ScaleTargetRef.Kind == "" || va.Spec.ScaleTargetRef.Name == "" {
		return nil
	}
	return []string{scaleTargetIndexKey(va.Namespace, va.Spec.ScaleTargetRef)}
}

// FindVAForScaleTarget returns the VariantAutoscaling that targets the given scale resource.
// Returns nil if no VariantAutoscaling targets this resource.
// Note: A scale target should have at most one VariantAutoscaling targeting it, so the first match is returned.
func FindVAForScaleTarget(ctx context.Context, c client.Client, ref autoscalingv2.CrossVersionObjectReference, namespace string) (*llmdVariantAutoscalingV1alpha1.VariantAutoscaling, error) {
	var vaList llmdVariantAutoscalingV1alpha1.VariantAutoscalingList
	if err := c.List(ctx, &vaList,
		client.InNamespace(namespace),
		client.MatchingFields{VAScaleTargetKey: scaleTargetIndexKey(namespace, ref)},
	); err != nil {
		return nil, fmt.Errorf("failed to list VariantAutoscalings for %s %s/%s: %w", ref.Kind, namespace, ref.Name, err)
	}

	// No VariantAutoscaling found for this scale target
	if len(vaList.Items) == 0 {
		return nil, nil
	}

	// There should be at most one VariantAutoscaling per scale target
	if len(vaList.Items) > 1 {
		return nil, fmt.Errorf("multiple VariantAutoscalings found for %s %s/%s", ref.Kind, namespace, ref.Name)
	}

	return &vaList.Items[0], nil
}

// FindVAForDeployment returns the VariantAutoscaling that targets a Deployment with the given name.
// Returns nil if no VariantAutoscaling targets a Deployment with the given name.
// This is a wrapper around FindVAForScaleTarget for the Deployment scale target.
func FindVAForDeployment(ctx context.Context, c client.Client, deploymentName, namespace string) (*llmdVariantAutoscalingV1alpha1.VariantAutoscaling, error) {
	return FindVAForScaleTarget(ctx, c, autoscalingv2.CrossVersionObjectReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       deploymentName,
	}, namespace)
}
