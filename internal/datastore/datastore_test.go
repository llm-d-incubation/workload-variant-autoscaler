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

// TestAnnotatedHPANamespacesGatedOnSync covers the cache-sync gate that keeps
// utils.annotationSourcedVariants on its cluster-wide HPA fallback until
// MarkAnnotatedHPAsSynced is called. While the gate is closed, the method
// must return nil regardless of tracked state, so a partially-populated
// tracking set during startup does not silently hide managed HPAs.
func TestAnnotatedHPANamespacesGatedOnSync(t *testing.T) {
	ds := NewDatastore(nil)

	// Pre-sync tracking is invisible — gate must report nil.
	ds.NamespaceTrack(ResourceTypeAnnotatedHPA, "hpa-a", "ns1")
	if got := ds.AnnotatedHPANamespaces(); got != nil {
		t.Errorf("expected nil before MarkAnnotatedHPAsSynced (cluster-wide fallback), got %v", got)
	}

	// Once the gate opens, scoped discovery becomes active.
	ds.MarkAnnotatedHPAsSynced()
	ds.NamespaceTrack(ResourceTypeAnnotatedHPA, "hpa-b", "ns2")

	got := ds.AnnotatedHPANamespaces()
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

	// MarkAnnotatedHPAsSynced is idempotent and never flips back to false.
	ds.MarkAnnotatedHPAsSynced()
	if ds.AnnotatedHPANamespaces() == nil {
		t.Errorf("gate should stay open after repeated MarkAnnotatedHPAsSynced calls")
	}
}

// TestAnnotatedHPANamespacesEmptyAfterSync covers the nil-vs-empty distinction
// that lets the caller skip the HPA list entirely when no managed HPAs exist.
// Returning nil from the synced state would conflate "sync not done" with
// "synced, no managed HPAs" and force a cluster-wide fallback that
// permanently negates the optimization for CRD-only clusters.
func TestAnnotatedHPANamespacesEmptyAfterSync(t *testing.T) {
	ds := NewDatastore(nil)
	ds.MarkAnnotatedHPAsSynced()

	// No HPAs tracked. Other resource types must not lift the namespace into
	// the result either (the caller must be able to skip the HPA list).
	ds.NamespaceTrack("VariantAutoscaling", "va-a", "ns-va")
	ds.NamespaceTrack("InferencePool", "pool-a", "ns-pool")

	got := ds.AnnotatedHPANamespaces()
	if got == nil {
		t.Fatalf("want non-nil empty slice after sync (skip-list signal), got nil (cluster-wide fallback)")
	}
	if len(got) != 0 {
		t.Errorf("want empty slice, got %v", got)
	}
}

// TestAnnotatedHPANamespacesFiltersByResourceType guards against a regression
// where the gate forwarded the union of every tracked namespace (VAs,
// InferencePools, etc.). In CRD-heavy clusters that would expand annotation
// discovery from one cluster-wide HPA list into N namespaced lists with
// nothing to find, worse than the cluster-wide baseline.
func TestAnnotatedHPANamespacesFiltersByResourceType(t *testing.T) {
	ds := NewDatastore(nil)
	ds.MarkAnnotatedHPAsSynced()

	// VA-only and InferencePool-only namespaces must not be returned.
	ds.NamespaceTrack("VariantAutoscaling", "va-a", "ns-va")
	ds.NamespaceTrack("InferencePool", "pool-a", "ns-pool")
	// A namespace with a managed HPA must be returned.
	ds.NamespaceTrack(ResourceTypeAnnotatedHPA, "hpa-a", "ns-annotated")
	// A namespace with BOTH a VA and an annotated HPA is still relevant.
	ds.NamespaceTrack("VariantAutoscaling", "va-b", "ns-mixed")
	ds.NamespaceTrack(ResourceTypeAnnotatedHPA, "hpa-b", "ns-mixed")
	// AnnotatedScaledObject-only namespaces must not appear in the HPA query —
	// the two kinds use distinct resource types so the gates can move
	// independently when SO scoping is added later.
	ds.NamespaceTrack(ResourceTypeAnnotatedScaledObject, "so-a", "ns-so")

	got := ds.AnnotatedHPANamespaces()
	seen := map[string]bool{}
	for _, ns := range got {
		seen[ns] = true
	}
	if !seen["ns-annotated"] || !seen["ns-mixed"] {
		t.Errorf("want ns-annotated and ns-mixed in result, got %v", got)
	}
	if seen["ns-va"] || seen["ns-pool"] || seen["ns-so"] {
		t.Errorf("non-HPA-tracked namespaces must not appear, got %v", got)
	}
	if len(got) != 2 {
		t.Errorf("want exactly 2 annotated-HPA namespaces, got %d (%v)", len(got), got)
	}
}

// TestAnnotatedHPANamespacesAndScaledObjectsAreSeparate guards against the
// kind-collision risk: HPA and ScaledObject with the same metadata.name in
// the same namespace must be tracked independently. With distinct resource
// types, untracking one cannot remove the namespace if the other is still
// tracked under its own type.
func TestAnnotatedHPANamespacesAndScaledObjectsAreSeparate(t *testing.T) {
	ds := NewDatastore(nil)
	ds.MarkAnnotatedHPAsSynced()

	ds.NamespaceTrack(ResourceTypeAnnotatedHPA, "foo", "ns1")
	ds.NamespaceTrack(ResourceTypeAnnotatedScaledObject, "foo", "ns1")

	// Untrack the HPA. The HPA-scoped query should now exclude ns1 because
	// no annotated HPA remains there, even though the same-named
	// ScaledObject still exists under its own resource type.
	ds.NamespaceUntrack(ResourceTypeAnnotatedHPA, "foo", "ns1")

	if got := ds.AnnotatedHPANamespaces(); len(got) != 0 {
		t.Errorf("want HPA query to exclude ns1 after HPA untrack, got %v", got)
	}
	if !ds.IsNamespaceTracked("ns1") {
		t.Errorf("want ns1 still tracked overall (ScaledObject entry remains)")
	}
}
