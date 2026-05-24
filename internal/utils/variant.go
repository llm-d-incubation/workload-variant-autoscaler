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

package utils

import (
	"context"
	"fmt"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	wvav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// VariantFilter is a function that determines if a VA should be included.
type VariantFilter func(scaletarget.ScaleTargetAccessor) bool

// ActiveVariantAutoscalingByModel retrieves all VariantAutoscaling resources that are ready for optimization
// and have at least one target replica.
// hpaScopedNamespaces controls how annotation-sourced HPA discovery is scoped;
// see annotationSourcedVariants for the nil / empty / non-empty semantics.
// ScaledObject discovery stays cluster-wide regardless.
// Returns the shallow-copied VAs (not safe for mutation) grouped by ModelID.
func ActiveVariantAutoscalingByModel(ctx context.Context, client client.Client, hpaScopedNamespaces []string) (map[string][]wvav1alpha1.VariantAutoscaling, error) {
	vas, _, err := ActiveVariantAutoscaling(ctx, client, hpaScopedNamespaces)
	if err != nil {
		return nil, err
	}
	return GroupVariantAutoscalingByModel(vas), nil
}

// InactiveVariantAutoscalingByModel retrieves all VariantAutoscaling resources that are ready for optimization
// and have no target replicas.
// hpaScopedNamespaces controls how annotation-sourced HPA discovery is scoped;
// see annotationSourcedVariants for the nil / empty / non-empty semantics.
// ScaledObject discovery stays cluster-wide regardless.
// Returns the shallow-copied VAs (not safe for mutation) grouped by ModelID.
func InactiveVariantAutoscalingByModel(ctx context.Context, client client.Client, hpaScopedNamespaces []string) (map[string][]wvav1alpha1.VariantAutoscaling, error) {
	vas, _, err := InactiveVariantAutoscaling(ctx, client, hpaScopedNamespaces)
	if err != nil {
		return nil, err
	}
	return GroupVariantAutoscalingByModel(vas), nil
}

// AcceleratorNameLabel is the label key used to specify the accelerator name for a VA.
const AcceleratorNameLabel = "inference.optimization/acceleratorName"

// GroupVariantAutoscalingByModel groups VariantAutoscalings by model ID and namespace.
// Variants of the same model on different accelerators are grouped together to enable
// cost-based optimization (scale up cheaper variants, scale down expensive variants).
// The key format is "modelID|namespace".
func GroupVariantAutoscalingByModel(
	vas []wvav1alpha1.VariantAutoscaling,
) map[string][]wvav1alpha1.VariantAutoscaling {
	groups := make(map[string][]wvav1alpha1.VariantAutoscaling)
	for _, va := range vas {
		// Use modelID + namespace as key to group all variants of same model
		key := va.Spec.ModelID + "|" + va.Namespace
		groups[key] = append(groups[key], va)
	}
	return groups
}

// ActiveVariantAutoscaling retrieves all VariantAutoscaling resources that are ready for optimization
// and have at least one target replica.
// hpaScopedNamespaces controls annotation-sourced HPA discovery scoping;
// see annotationSourcedVariants for the nil / empty / non-empty semantics.
// Returns a slice of deep-copied VariantAutoscaling objects.
// It also returns a map of scaleTargetAccessors keyed by "namespace/scaleTargetName".
func ActiveVariantAutoscaling(ctx context.Context, client client.Client, hpaScopedNamespaces []string) ([]wvav1alpha1.VariantAutoscaling, map[string]scaletarget.ScaleTargetAccessor, error) {
	return filterVariantsByScaleTargetAccessor(ctx, client, hpaScopedNamespaces, isActive, "active")
}

// InactiveVariantAutoscaling retrieves all VariantAutoscaling resources that are ready for optimization
// and have no target replicas.
// hpaScopedNamespaces controls annotation-sourced HPA discovery scoping;
// see annotationSourcedVariants for the nil / empty / non-empty semantics.
// Returns a slice of deep-copied VariantAutoscaling objects.
// It also returns a map of scaleTargetAccessors keyed by "namespace/scaleTargetName".
func InactiveVariantAutoscaling(ctx context.Context, client client.Client, hpaScopedNamespaces []string) ([]wvav1alpha1.VariantAutoscaling, map[string]scaletarget.ScaleTargetAccessor, error) {
	return filterVariantsByScaleTargetAccessor(ctx, client, hpaScopedNamespaces, isInactive, "inactive")
}

// filterVariantsByScaleTargetAccessors is a generic function to filter VAs based on scaleTarget state.
// Returns filtered VAs and a map of scaleTargetAccessors keyed by "namespace/scaleTargetName".
func filterVariantsByScaleTargetAccessor(ctx context.Context, client client.Client, hpaScopedNamespaces []string, filter VariantFilter, filterName string) ([]wvav1alpha1.VariantAutoscaling, map[string]scaletarget.ScaleTargetAccessor, error) {
	readyVAs, err := readyVariantAutoscalings(ctx, client, hpaScopedNamespaces)
	if err != nil {
		return nil, nil, err
	}

	filteredVAs := make([]wvav1alpha1.VariantAutoscaling, 0, len(readyVAs))
	scaleTargetAccessors := make(map[string]scaletarget.ScaleTargetAccessor)

	for _, va := range readyVAs {
		// Check if the context is done
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		default:
		}

		// Skip VAs without scaleTargetRef (required to know which deployment to look up)
		// TODO: Remove this check once scaleTargetRef.name is made a required field in the CRD.
		// This defensive check exists because the CRD currently allows empty scaleTargetRef,
		// but it should be enforced at the schema level instead.
		if va.Spec.ScaleTargetRef.Name == "" {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Skipping VA without scaleTargetRef", "namespace", va.Namespace, "name", va.Name)
			continue
		}

		scaleTargetName := va.Spec.ScaleTargetRef.Name
		var scaleTargetAccessor scaletarget.ScaleTargetAccessor
		if scaleTargetAccessor, err = scaletarget.FetchScaleTarget(ctx, client, va.Name, va.Spec.ScaleTargetRef.Kind, scaleTargetName, va.Namespace); err != nil {
			if apierrors.IsNotFound(err) {
				// Deployment/LWS doesn't exist yet, this is expected for VAs without corresponding scale targets
				ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Scale target not found for VariantAutoscaling, skipping",
					"namespace", va.Namespace,
					"scaleTargetName", scaleTargetName,
					"vaName", va.Name)
			} else {
				// Unexpected error (permissions, network issues, etc.)
				ctrl.LoggerFrom(ctx).Error(err, "Failed to get scale target",
					"namespace", va.Namespace,
					"scaleTargetName", scaleTargetName,
					"vaName", va.Name)
			}
			continue
		}

		// Skip deleted scaleTargetAccessor
		if scaleTargetAccessor.GetDeletionTimestamp() != nil && !scaleTargetAccessor.GetDeletionTimestamp().IsZero() {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Skipping deleted scale target", "namespace", va.Namespace, "scaleTargetName", scaleTargetName)
			continue
		}

		// Apply the filter function
		if filter(scaleTargetAccessor) {
			filteredVAs = append(filteredVAs, va)
			// Store scaleTargetAccessor in map using namespace/scaleTargetName as key
			key := GetNamespacedKey(va.Namespace, scaleTargetName)
			scaleTargetAccessors[key] = scaleTargetAccessor
		}
	}
	ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("Found filtered VariantAutoscaling resources",
		"filterType", filterName,
		"count", len(filteredVAs))

	return filteredVAs, scaleTargetAccessors, nil
}

// readyVariantAutoscalings retrieves all VariantAutoscaling resources that are ready for optimization
// using the informer cache. When CONTROLLER_INSTANCE is configured, only VAs with matching
// controller-instance labels are returned to enable multi-controller isolation.
// It also merges in-memory VAs synthesized from annotated ScaledObjects and HPAs
// (annotation-based discovery, Phase 1 dual-mode). CRD-sourced VAs take precedence
// when both refer to the same scale target in the same namespace.
// hpaScopedNamespaces controls how annotation-sourced HPA discovery is scoped;
// see annotationSourcedVariants for the nil / empty / non-empty semantics.
func readyVariantAutoscalings(ctx context.Context, k8sClient client.Client, hpaScopedNamespaces []string) ([]wvav1alpha1.VariantAutoscaling, error) {
	logger := ctrl.LoggerFrom(ctx)

	// Build list options based on controller instance configuration
	listOpts := []client.ListOption{}
	controllerInstance := metrics.GetControllerInstance()
	if controllerInstance != "" {
		// Filter by controller-instance label for multi-controller isolation
		listOpts = append(listOpts, client.MatchingLabels{
			constants.ControllerInstanceLabelKey: controllerInstance,
		})
		logger.V(logging.DEBUG).Info("Filtering VAs by controller instance",
			"controllerInstance", controllerInstance)
	}

	// List VAs using the informer cache with optional label selector
	var vaList wvav1alpha1.VariantAutoscalingList
	if err := k8sClient.List(ctx, &vaList, listOpts...); err != nil {
		return nil, err
	}

	// Filter out VAs being deleted
	readyVAs := make([]wvav1alpha1.VariantAutoscaling, 0, len(vaList.Items))
	for _, va := range vaList.Items {
		// Skip deleted VAs
		if !va.DeletionTimestamp.IsZero() {
			continue
		}
		readyVAs = append(readyVAs, va)
	}

	logger.V(logging.DEBUG).Info("Found VariantAutoscaling resources ready for optimization",
		"count", len(readyVAs),
		"controllerInstance", controllerInstance)

	// Merge annotation-sourced variants (dual-mode: CRD wins on conflict).
	annotated, err := annotationSourcedVariants(ctx, k8sClient, hpaScopedNamespaces)
	if err != nil {
		// Non-fatal: log and continue with CRD-sourced only.
		logger.Error(err, "Error while listing annotation-sourced variants (non-fatal)")
	}
	if len(annotated) == 0 {
		return readyVAs, nil
	}

	// Build set of (namespace/kind/name) already covered by CRD-sourced VAs.
	// Kind is sufficient for disambiguation: the only in-play kinds are Deployment,
	// LeaderWorkerSet, and StatefulSet, which are unique names in practice.
	crdTargets := make(map[string]bool, len(readyVAs))
	for _, va := range readyVAs {
		if va.Spec.ScaleTargetRef.Name != "" {
			key := fmt.Sprintf("%s/%s/%s", va.Namespace, va.Spec.ScaleTargetRef.Kind, va.Spec.ScaleTargetRef.Name)
			crdTargets[key] = true
		}
	}
	for _, va := range annotated {
		key := fmt.Sprintf("%s/%s/%s", va.Namespace, va.Spec.ScaleTargetRef.Kind, va.Spec.ScaleTargetRef.Name)
		if !crdTargets[key] {
			readyVAs = append(readyVAs, va)
		}
	}

	logger.V(logging.DEBUG).Info("Merged annotation-sourced variants",
		"annotatedCount", len(annotated),
		"totalCount", len(readyVAs))

	return readyVAs, nil
}

// annotationSourcedVariants lists HPAs and KEDA ScaledObjects bearing llm-d.ai/managed: "true"
// and synthesizes in-memory VariantAutoscaling objects from them. ScaledObject discovery is
// skipped gracefully when the KEDA CRD is not installed. When both an HPA and a ScaledObject
// target the same scale target, the ScaledObject entry wins.
//
// hpaScopedNamespaces controls the per-tick HPA List calls via three states:
//   - nil           = HPA cache sync not yet complete; fall back to a single
//     cluster-wide HPA list so the startup window does not
//     silently drop managed HPAs in unreconciled namespaces.
//   - []string{}    = synced, but no managed HPAs exist anywhere; skip the
//     HPA list entirely instead of issuing a cluster-wide
//     scan that the caller already knows will be empty.
//   - non-empty []  = scope HPA discovery to these namespaces.
//
// ScaledObject discovery deliberately stays cluster-wide regardless of
// hpaScopedNamespaces. Scoping SOs would require its own sync gate and would
// regress the late-KEDA-install case (KEDA installed after WVA startup) where
// the cluster-wide List was previously the path that lazily created the SO
// informer. A follow-up issue can add SO scoping with its own gate.
func annotationSourcedVariants(ctx context.Context, k8sClient client.Client, hpaScopedNamespaces []string) ([]wvav1alpha1.VariantAutoscaling, error) {
	logger := ctrl.LoggerFrom(ctx)
	// keyed by namespace/kind/name for deduplication; ScaledObject entries overwrite HPA entries.
	byTarget := make(map[string]wvav1alpha1.VariantAutoscaling)

	// HPAs are a core Kubernetes type — always available (lower priority for deduplication).
	for _, scope := range hpaListScopes(hpaScopedNamespaces) {
		var hpaList autoscalingv2.HorizontalPodAutoscalerList
		if err := k8sClient.List(ctx, &hpaList, scope...); err != nil {
			return nil, fmt.Errorf("listing HPAs: %w", err)
		}
		for i := range hpaList.Items {
			hpa := &hpaList.Items[i]
			if !annotations.IsManaged(hpa) || !hpa.DeletionTimestamp.IsZero() {
				continue
			}
			va, err := VariantAutoscalingFromHPA(hpa)
			if err != nil {
				logger.V(logging.DEBUG).Info("Skipping HPA with invalid WVA annotations",
					"namespace", hpa.Namespace, "name", hpa.Name, "error", err)
				continue
			}
			key := fmt.Sprintf("%s/%s/%s", va.Namespace, va.Spec.ScaleTargetRef.Kind, va.Spec.ScaleTargetRef.Name)
			byTarget[key] = *va
		}
	}

	// KEDA ScaledObjects — may not be installed; handle gracefully.
	// ScaledObject takes precedence over HPA for the same scale target.
	// One cluster-wide list per tick; see the doc comment for why this is
	// not yet scoped.
	var soList kedav1alpha1.ScaledObjectList
	if err := k8sClient.List(ctx, &soList); err != nil {
		if apimeta.IsNoMatchError(err) {
			logger.V(logging.DEBUG).Info("KEDA ScaledObject CRD not available, skipping annotation discovery for ScaledObjects")
			return byTargetToSlice(byTarget), nil
		}
		return byTargetToSlice(byTarget), fmt.Errorf("listing ScaledObjects: %w", err)
	}
	for i := range soList.Items {
		so := &soList.Items[i]
		if !annotations.IsManaged(so) || !so.DeletionTimestamp.IsZero() {
			continue
		}
		va, err := VariantAutoscalingFromScaledObject(so)
		if err != nil {
			logger.V(logging.DEBUG).Info("Skipping ScaledObject with invalid WVA annotations",
				"namespace", so.Namespace, "name", so.Name, "error", err)
			continue
		}
		key := fmt.Sprintf("%s/%s/%s", va.Namespace, va.Spec.ScaleTargetRef.Kind, va.Spec.ScaleTargetRef.Name)
		byTarget[key] = *va
	}

	return byTargetToSlice(byTarget), nil
}

// hpaListScopes turns the gate's tri-state slice into list-option groups for
// the HPA List loop:
//   - nil input              → one cluster-wide scope (gate not yet open)
//   - empty non-nil input    → zero scopes; the caller skips the HPA list
//   - non-empty input        → one InNamespace scope per tracked namespace
func hpaListScopes(hpaScopedNamespaces []string) [][]client.ListOption {
	if hpaScopedNamespaces == nil {
		return [][]client.ListOption{nil}
	}
	if len(hpaScopedNamespaces) == 0 {
		return nil
	}
	scopes := make([][]client.ListOption, 0, len(hpaScopedNamespaces))
	for _, ns := range hpaScopedNamespaces {
		scopes = append(scopes, []client.ListOption{client.InNamespace(ns)})
	}
	return scopes
}

// byTargetToSlice flattens the byTarget dedup map into the return slice.
func byTargetToSlice(byTarget map[string]wvav1alpha1.VariantAutoscaling) []wvav1alpha1.VariantAutoscaling {
	result := make([]wvav1alpha1.VariantAutoscaling, 0, len(byTarget))
	for _, va := range byTarget {
		result = append(result, va)
	}
	return result
}

// isActive explicitly requires that replicas > 0
func isActive(scaleTargetAccessor scaletarget.ScaleTargetAccessor) bool {
	return GetDesiredReplicas(scaleTargetAccessor) > 0
}

// isInactive explicitly requires that replicas == 0
func isInactive(scaleTargetAccessor scaletarget.ScaleTargetAccessor) bool {
	return GetDesiredReplicas(scaleTargetAccessor) == 0
}

// Helper function makes behavior explicit
func GetDesiredReplicas(scaleTargetAccessor scaletarget.ScaleTargetAccessor) int32 {
	if scaleTargetAccessor.GetReplicas() == nil {
		return 1 // Kubernetes default
	}
	return *scaleTargetAccessor.GetReplicas()
}

// GetNamespacedKey is a helper for building namespaced resource keys.
func GetNamespacedKey(namespace, name string) string {
	return namespace + "/" + name
}
