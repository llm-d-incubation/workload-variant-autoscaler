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
	"errors"
	"sort"
	"testing"

	kedav1alpha1 "github.com/kedacore/keda/v2/apis/keda/v1alpha1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/datastore"
)

func bootstrapScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add clientgoscheme: %v", err)
	}
	if err := kedav1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add kedav1alpha1: %v", err)
	}
	return s
}

func managedHPAFixture(ns, name string) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ns,
			Annotations: map[string]string{annotations.Managed: "true"},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: name + "-deploy"},
			MaxReplicas:    5,
		},
	}
}

func managedSOFixture(ns, name string) *kedav1alpha1.ScaledObject {
	return &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   ns,
			Annotations: map[string]string{annotations.Managed: "true"},
		},
		Spec: kedav1alpha1.ScaledObjectSpec{
			ScaleTargetRef: &kedav1alpha1.ScaleTarget{Kind: "Deployment", Name: name + "-deploy"},
		},
	}
}

func TestBootstrapAnnotatedScalerTracking(t *testing.T) {
	ctx := context.Background()

	t.Run("tracks managed HPAs across namespaces", func(t *testing.T) {
		s := bootstrapScheme(t)
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedHPAFixture("ns1", "hpa-a"),
			managedHPAFixture("ns2", "hpa-b"),
			// Unmanaged HPA — must be skipped.
			&autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "hpa-other", Namespace: "ns3"},
				Spec:       autoscalingv2.HorizontalPodAutoscalerSpec{MaxReplicas: 2},
			},
		).Build()
		ds := datastore.NewDatastore(nil)

		if err := BootstrapAnnotatedScalerTracking(ctx, cl, ds, false); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Tracking pre-population must be observable through the gate, so
		// open it explicitly and read the resulting namespace list.
		ds.MarkAnnotatedScalersSynced()
		got := ds.AnnotatedScalerNamespaces()
		sort.Strings(got)
		if len(got) != 2 || got[0] != "ns1" || got[1] != "ns2" {
			t.Fatalf("want exactly [ns1 ns2], got %v", got)
		}
	})

	t.Run("includes ScaledObject namespaces when keda is enabled", func(t *testing.T) {
		s := bootstrapScheme(t)
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedHPAFixture("ns1", "hpa-a"),
			managedSOFixture("ns2", "so-b"),
		).Build()
		ds := datastore.NewDatastore(nil)

		if err := BootstrapAnnotatedScalerTracking(ctx, cl, ds, true); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		ds.MarkAnnotatedScalersSynced()
		got := ds.AnnotatedScalerNamespaces()
		sort.Strings(got)
		if len(got) != 2 || got[0] != "ns1" || got[1] != "ns2" {
			t.Fatalf("want [ns1 ns2], got %v", got)
		}
	})

	t.Run("kedaEnabled=false skips ScaledObject discovery entirely", func(t *testing.T) {
		s := bootstrapScheme(t)
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedHPAFixture("ns1", "hpa-a"),
			managedSOFixture("ns2", "so-b"),
		).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*kedav1alpha1.ScaledObjectList); ok {
					t.Fatalf("ScaledObject list must not be issued when kedaEnabled=false")
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
		ds := datastore.NewDatastore(nil)

		if err := BootstrapAnnotatedScalerTracking(ctx, cl, ds, false); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		ds.MarkAnnotatedScalersSynced()
		got := ds.AnnotatedScalerNamespaces()
		if len(got) != 1 || got[0] != "ns1" {
			t.Fatalf("want only [ns1], got %v", got)
		}
	})

	t.Run("NoMatchError on ScaledObject list is benign", func(t *testing.T) {
		s := bootstrapScheme(t)
		soGK := schema.GroupKind{Group: "keda.sh", Kind: "ScaledObject"}
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedHPAFixture("ns1", "hpa-a"),
		).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*kedav1alpha1.ScaledObjectList); ok {
					return &apimeta.NoKindMatchError{GroupKind: soGK}
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
		ds := datastore.NewDatastore(nil)

		if err := BootstrapAnnotatedScalerTracking(ctx, cl, ds, true); err != nil {
			t.Fatalf("expected NoMatchError to be swallowed, got: %v", err)
		}
		ds.MarkAnnotatedScalersSynced()
		got := ds.AnnotatedScalerNamespaces()
		if len(got) != 1 || got[0] != "ns1" {
			t.Fatalf("want [ns1] (HPA-only), got %v", got)
		}
	})

	t.Run("HPA list error is propagated", func(t *testing.T) {
		s := bootstrapScheme(t)
		cl := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*autoscalingv2.HorizontalPodAutoscalerList); ok {
					return errors.New("hpa api unavailable")
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()
		ds := datastore.NewDatastore(nil)

		if err := BootstrapAnnotatedScalerTracking(ctx, cl, ds, false); err == nil {
			t.Fatal("want error from HPA list failure, got nil")
		}
	})

	t.Run("skips HPAs and ScaledObjects being deleted", func(t *testing.T) {
		now := metav1.Now()
		s := bootstrapScheme(t)
		deletingHPA := managedHPAFixture("ns-del-hpa", "hpa-deleting")
		deletingHPA.DeletionTimestamp = &now
		deletingHPA.Finalizers = []string{"placeholder.example.com/keep-alive"}
		deletingSO := managedSOFixture("ns-del-so", "so-deleting")
		deletingSO.DeletionTimestamp = &now
		deletingSO.Finalizers = []string{"placeholder.example.com/keep-alive"}
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedHPAFixture("ns1", "hpa-live"),
			deletingHPA,
			deletingSO,
		).Build()
		ds := datastore.NewDatastore(nil)

		if err := BootstrapAnnotatedScalerTracking(ctx, cl, ds, true); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		ds.MarkAnnotatedScalersSynced()
		got := ds.AnnotatedScalerNamespaces()
		if len(got) != 1 || got[0] != "ns1" {
			t.Fatalf("want only [ns1] (deleting objects skipped), got %v", got)
		}
	})
}
