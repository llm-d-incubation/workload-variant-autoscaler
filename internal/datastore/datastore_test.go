/*
Copyright 2025 The Kubernetes Authors.

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

package datastore

import (
	"context"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	poolutil "github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/pool"
	unittestutil "github.com/llm-d/llm-d-workload-variant-autoscaler/test/utils"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	v1 "sigs.k8s.io/gateway-api-inference-extension/api/v1"
	testutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/testing"
)

func TestDatastore(t *testing.T) {
	pool1Selector := map[string]string{"app": "vllm_v1"}
	pool1 := testutil.MakeInferencePool("pool1").
		Namespace("default").
		Selector(pool1Selector).
		EndpointPickerRef("epp-svc").ObjRef()
	tests := []struct {
		name                 string
		inferencePool        *v1.InferencePool
		labels               map[string]string
		wantPool             *v1.InferencePool
		wantErr              error
		wantLabelsMatch      bool
		listResultLen        int
		clearDeleteResultLen int
	}{
		{
			name:                 "Ready when InferencePool exists in data store",
			inferencePool:        pool1,
			wantPool:             pool1,
			wantLabelsMatch:      false,
			clearDeleteResultLen: 0,
			listResultLen:        1,
		},
		{
			name:                 "Labels matched",
			inferencePool:        pool1,
			labels:               map[string]string{"app": "vllm_v1"},
			wantPool:             pool1,
			wantLabelsMatch:      true,
			clearDeleteResultLen: 0,
			listResultLen:        1,
		},
		{
			name:    "Not ready when InferencePool is nil",
			wantErr: errPoolIsNull,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {

			// Define the EPP service object
			eppSvc := unittestutil.MakeService("epp-svc", "default")

			// Set up the scheme.
			scheme := runtime.NewScheme()
			_ = clientgoscheme.AddToScheme(scheme)
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(eppSvc).
				Build()

			ds := NewDatastore(nil)
			ctx := context.Background()

			ep, err := poolutil.InferencePoolToEndpointPool(ctx, fakeClient, tt.inferencePool)
			if err != nil {
				t.Errorf("Unexpected InferencePoolToEndpointPool error: %v", err)
			}

			// Test PoolSet
			gotErr := ds.PoolSet(ctx, fakeClient, ep)
			if diff := cmp.Diff(tt.wantErr, gotErr, cmpopts.EquateErrors()); diff != "" {
				t.Errorf("Unexpected error diff (+got/-want): %s", diff)
			}

			// Test PoolGetFromLabels
			if tt.wantLabelsMatch {
				wantPoolMatch, err := poolutil.InferencePoolToEndpointPool(ctx, fakeClient, tt.wantPool)
				if err != nil {
					t.Errorf("Unexpected InferencePoolToEndpointPool error: %v", err)
				}

				// Pass the namespace from the wantPool to match the pool in the same namespace
				gotPoolMatch, err := ds.PoolGetFromLabels(tt.wantPool.Namespace, tt.labels)
				if err != nil {
					t.Errorf("Unexpected PoolGetFromLabels error: %v", err)
				}

				if diff := cmp.Diff(wantPoolMatch, gotPoolMatch); diff != "" {
					t.Errorf("Unexpected labels match diff (+got/-want): %s", diff)
				}
			}

			if tt.wantErr == nil {
				// Test PoolGet
				wantPool, err := poolutil.InferencePoolToEndpointPool(ctx, fakeClient, tt.wantPool)
				if err != nil {
					t.Errorf("Unexpected InferencePoolToEndpointPool error: %v", err)
				}

				gotPool, err := ds.PoolGet(ep.Namespace + "/" + ep.Name)
				if err != nil {
					t.Errorf("failed to add endpoint into the datastore: %v", err)
				}

				if diff := cmp.Diff(wantPool, gotPool); diff != "" {
					t.Errorf("Unexpected pool diff (+got/-want): %s", diff)
				}

				// Verify metrics source exists before deletion
				namespacedName := ep.Namespace + "/" + ep.Name
				metricsSource := ds.PoolGetMetricsSource(namespacedName)
				assert.NotNil(t, metricsSource, "Metrics source should exist in registry before deletion")

				// Test Delete & PoolList
				ds.PoolDelete(namespacedName)
				assert.Equal(t, len(ds.PoolList()), tt.clearDeleteResultLen, "Pools map should have the expected length after item deleted")

				// Verify metrics source is cleaned up from registry after deletion
				metricsSourceAfterDelete := ds.PoolGetMetricsSource(namespacedName)
				assert.Nil(t, metricsSourceAfterDelete, "Metrics source should be removed from registry after pool deletion")

				if err := ds.PoolSet(ctx, fakeClient, ep); err != nil {
					t.Errorf("failed to add endpoint into the datastore: %v", err)
				}
				assert.Equal(t, len(ds.PoolList()), tt.listResultLen, "Pools map should have the expected length after item added")

			}

			// Test Clear
			ds.Clear()
			assert.Equal(t, len(ds.PoolList()), tt.clearDeleteResultLen, "Pools map should have the expected length after clearing")
		})
	}
}

// TestAnnotatedScalerNamespacesGatedOnSync covers the cache-sync gate that
// keeps utils.annotationSourcedVariants on its cluster-wide fallback path
// until the HPA / ScaledObject informer caches have completed their initial
// sync. Until MarkAnnotatedScalersSynced is called, AnnotatedScalerNamespaces
// must return nil even when namespaces are tracked, so a partially-populated
// tracking set during startup does not silently hide managed scalers from
// discovery.
func TestAnnotatedScalerNamespacesGatedOnSync(t *testing.T) {
	ds := NewDatastore(nil)

	// Simulate partial reconciler progress: ns1 already tracked, ns2 not yet.
	ds.NamespaceTrack(ResourceTypeAnnotatedScaler, "HPA/hpa-a", "ns1")

	if got := ds.AnnotatedScalerNamespaces(); got != nil {
		t.Errorf("expected nil before MarkAnnotatedScalersSynced (cluster-wide fallback), got %v", got)
	}

	// Once the gate opens, scoped discovery becomes active.
	ds.MarkAnnotatedScalersSynced()
	ds.NamespaceTrack(ResourceTypeAnnotatedScaler, "ScaledObject/so-b", "ns2")

	got := ds.AnnotatedScalerNamespaces()
	if len(got) != 2 {
		t.Fatalf("want 2 tracked namespaces after sync, got %d (%v)", len(got), got)
	}
	seen := map[string]bool{}
	for _, ns := range got {
		seen[ns] = true
	}
	if !seen["ns1"] || !seen["ns2"] {
		t.Errorf("want both ns1 and ns2 tracked, got %v", seen)
	}

	// MarkAnnotatedScalersSynced is idempotent and never flips back to false.
	ds.MarkAnnotatedScalersSynced()
	if ds.AnnotatedScalerNamespaces() == nil {
		t.Errorf("gate should stay open after repeated MarkAnnotatedScalersSynced calls")
	}
}

// TestAnnotatedScalerNamespacesFiltersByResourceType is the regression guard
// for the Codex P1 finding: AnnotatedScalerNamespaces must NOT forward the
// union of every tracked namespace. Returning namespaces tracked under
// "VariantAutoscaling" or "InferencePool" would expand annotation discovery
// from 2 cluster-wide List calls per tick into 2*N namespaced List calls
// (N = total tracked namespaces), a substantial regression in CRD-heavy
// clusters with few annotated scalers.
func TestAnnotatedScalerNamespacesFiltersByResourceType(t *testing.T) {
	ds := NewDatastore(nil)
	ds.MarkAnnotatedScalersSynced()

	// VA-only namespaces must not be returned by AnnotatedScalerNamespaces.
	ds.NamespaceTrack("VariantAutoscaling", "va-a", "ns-va")
	ds.NamespaceTrack("InferencePool", "pool-a", "ns-pool")
	// A namespace with an annotated scaler must be returned.
	ds.NamespaceTrack(ResourceTypeAnnotatedScaler, "HPA/hpa-a", "ns-annotated")
	// A namespace with BOTH a VA and an annotated scaler is still relevant.
	ds.NamespaceTrack("VariantAutoscaling", "va-b", "ns-mixed")
	ds.NamespaceTrack(ResourceTypeAnnotatedScaler, "ScaledObject/so-b", "ns-mixed")

	got := ds.AnnotatedScalerNamespaces()
	seen := map[string]bool{}
	for _, ns := range got {
		seen[ns] = true
	}
	if !seen["ns-annotated"] || !seen["ns-mixed"] {
		t.Errorf("want ns-annotated and ns-mixed in result, got %v", got)
	}
	if seen["ns-va"] || seen["ns-pool"] {
		t.Errorf("VA-only and InferencePool-only namespaces must not appear, got %v", got)
	}
	if len(got) != 2 {
		t.Errorf("want exactly 2 annotated-scaler namespaces, got %d (%v)", len(got), got)
	}
}

// TestAnnotatedScalerNamespacesKindCollisionResilience is the regression guard
// for the Codex P2 finding: when a managed HPA and a managed ScaledObject
// share metadata.name in one namespace, the tracker must keep the namespace
// returned by AnnotatedScalerNamespaces even after one of them is untracked.
// This requires kind-qualified resourceName arguments from callers, which the
// HPA and ScaledObject reconcilers now produce via annotatedScalerKey.
func TestAnnotatedScalerNamespacesKindCollisionResilience(t *testing.T) {
	ds := NewDatastore(nil)
	ds.MarkAnnotatedScalersSynced()

	// Same metadata.name "foo", same namespace, different kinds.
	ds.NamespaceTrack(ResourceTypeAnnotatedScaler, "HPA/foo", "ns1")
	ds.NamespaceTrack(ResourceTypeAnnotatedScaler, "ScaledObject/foo", "ns1")

	// Delete the HPA. The ScaledObject must still keep ns1 tracked.
	ds.NamespaceUntrack(ResourceTypeAnnotatedScaler, "HPA/foo", "ns1")

	got := ds.AnnotatedScalerNamespaces()
	if len(got) != 1 || got[0] != "ns1" {
		t.Fatalf("want ns1 still tracked after HPA untrack (ScaledObject remains), got %v", got)
	}

	// Now delete the ScaledObject too; ns1 should drop out.
	ds.NamespaceUntrack(ResourceTypeAnnotatedScaler, "ScaledObject/foo", "ns1")
	if got := ds.AnnotatedScalerNamespaces(); len(got) != 0 {
		t.Errorf("want ns1 untracked after both kinds are removed, got %v", got)
	}
}
