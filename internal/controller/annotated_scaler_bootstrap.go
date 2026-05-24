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

package controller

import (
	"context"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
)

// BootstrapAnnotatedHPATracking enumerates the existing managed HPAs from the
// controller-runtime cache and pre-populates the datastore's
// ResourceTypeAnnotatedHPA namespace tracking. It runs once at startup,
// between mgr.GetCache().WaitForCacheSync and Datastore.MarkAnnotatedHPAsSynced.
//
// Pre-population closes a startup race that WaitForCacheSync alone does not:
// cache sync only guarantees the informer's local state is current, not that
// HPAReconciler has already drained its workqueue and called NamespaceTrack
// for every existing object. Without this step the gate would open while the
// tracking set is still being filled, and the next engine tick could silently
// drop a managed HPA in a not-yet-reconciled namespace.
//
// NamespaceTrack is idempotent, so the steady-state reconciler events that
// fire afterwards produce no duplicate state.
//
// ScaledObject discovery deliberately stays cluster-wide (see
// utils.annotationSourcedVariants), so this helper does not pre-populate
// ResourceTypeAnnotatedScaledObject tracking. If a follow-up scopes
// ScaledObject discovery too, mirror this helper for SOs with its own sync
// gate; the AnnotatedHPA flag must not be reused.
func BootstrapAnnotatedHPATracking(ctx context.Context, k8sClient client.Client, ds datastore.Datastore) error {
	var hpaList autoscalingv2.HorizontalPodAutoscalerList
	if err := k8sClient.List(ctx, &hpaList); err != nil {
		return err
	}
	for i := range hpaList.Items {
		hpa := &hpaList.Items[i]
		if annotations.IsManaged(hpa) && hpa.DeletionTimestamp.IsZero() {
			ds.NamespaceTrack(datastore.ResourceTypeAnnotatedHPA, hpa.Name, hpa.Namespace)
		}
	}
	return nil
}
