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

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
)

// BootstrapAnnotatedScalerTracking enumerates the existing managed HPAs and
// (when KEDA is enabled) ScaledObjects from the controller-runtime cache and
// pre-populates the datastore's AnnotatedScaler namespace tracking. It is
// meant to run once at startup, after the manager's caches have synced but
// before MarkAnnotatedScalersSynced is called.
//
// The pre-population closes a startup race that WaitForCacheSync alone does
// not: cache sync guarantees the informer's local state is current, but it
// does NOT guarantee that HPAReconciler / ScaledObjectReconciler have
// already drained their workqueues and called NamespaceTrack for every
// existing object. Without this step the gate would open while the tracking
// set is still being filled, and the next engine tick could silently drop a
// managed HPA / ScaledObject in a not-yet-reconciled namespace.
//
// NamespaceTrack is idempotent, so the steady-state reconciler events that
// fire afterwards produce no duplicate state. kedaEnabled mirrors the flag
// in main that gates ScaledObject controller registration.
func BootstrapAnnotatedScalerTracking(ctx context.Context, k8sClient client.Client, ds datastore.Datastore, kedaEnabled bool) error {
	var hpaList autoscalingv2.HorizontalPodAutoscalerList
	if err := k8sClient.List(ctx, &hpaList); err != nil {
		return err
	}
	for i := range hpaList.Items {
		hpa := &hpaList.Items[i]
		if annotations.IsManaged(hpa) && hpa.DeletionTimestamp.IsZero() {
			ds.NamespaceTrack("AnnotatedScaler", hpa.Name, hpa.Namespace)
		}
	}

	if !kedaEnabled {
		return nil
	}

	var soList kedav1alpha1.ScaledObjectList
	if err := k8sClient.List(ctx, &soList); err != nil {
		// KEDA may have been uninstalled between reconciler registration and
		// the cache-sync runnable firing; treat NoMatchError as benign so the
		// gate can still open for the HPA-only case.
		if apimeta.IsNoMatchError(err) {
			return nil
		}
		return err
	}
	for i := range soList.Items {
		so := &soList.Items[i]
		if annotations.IsManaged(so) && so.DeletionTimestamp.IsZero() {
			ds.NamespaceTrack("AnnotatedScaler", so.Name, so.Namespace)
		}
	}
	return nil
}
