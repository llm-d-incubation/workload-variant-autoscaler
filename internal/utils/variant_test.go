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
	"errors"
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

	wvav1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/annotations"
)

func TestGroupVariantAutoscalingByModel(t *testing.T) {
	tests := []struct {
		name           string
		vas            []wvav1alpha1.VariantAutoscaling
		expectedGroups int
		expectedKeys   []string
	}{
		{
			name: "same model different accelerators groups together for cost optimization",
			vas: []wvav1alpha1.VariantAutoscaling{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "va-a100",
						Namespace: "default",
						Labels: map[string]string{
							AcceleratorNameLabel: "A100",
						},
					},
					Spec: wvav1alpha1.VariantAutoscalingSpec{
						ModelID: "llama-8b",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "va-h100",
						Namespace: "default",
						Labels: map[string]string{
							AcceleratorNameLabel: "H100",
						},
					},
					Spec: wvav1alpha1.VariantAutoscalingSpec{
						ModelID: "llama-8b",
					},
				},
			},
			expectedGroups: 1,
			expectedKeys:   []string{"llama-8b|default"},
		},
		{
			name: "same model same namespace groups together",
			vas: []wvav1alpha1.VariantAutoscaling{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "va-1",
						Namespace: "default",
					},
					Spec: wvav1alpha1.VariantAutoscalingSpec{
						ModelID: "llama-8b",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "va-2",
						Namespace: "default",
					},
					Spec: wvav1alpha1.VariantAutoscalingSpec{
						ModelID: "llama-8b",
					},
				},
			},
			expectedGroups: 1,
			expectedKeys:   []string{"llama-8b|default"},
		},
		{
			name: "different namespaces creates separate groups",
			vas: []wvav1alpha1.VariantAutoscaling{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "va-1",
						Namespace: "ns1",
					},
					Spec: wvav1alpha1.VariantAutoscalingSpec{
						ModelID: "llama-8b",
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "va-2",
						Namespace: "ns2",
					},
					Spec: wvav1alpha1.VariantAutoscalingSpec{
						ModelID: "llama-8b",
					},
				},
			},
			expectedGroups: 2,
			expectedKeys:   []string{"llama-8b|ns1", "llama-8b|ns2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GroupVariantAutoscalingByModel(tt.vas)

			if len(result) != tt.expectedGroups {
				t.Errorf("GroupVariantAutoscalingByModel() returned %d groups, want %d", len(result), tt.expectedGroups)
			}

			for _, key := range tt.expectedKeys {
				if _, exists := result[key]; !exists {
					t.Errorf("GroupVariantAutoscalingByModel() missing expected key %q", key)
				}
			}
		})
	}
}

// variantTestScheme builds a scheme with WVA, core Kubernetes (incl. HPA), and KEDA types.
func variantTestScheme(t testing.TB) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add clientgoscheme: %v", err)
	}
	if err := wvav1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add wvav1alpha1: %v", err)
	}
	if err := kedav1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add kedav1alpha1: %v", err)
	}
	return s
}

func managedHPA(ns, name, targetName, modelID string) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Annotations: map[string]string{
				annotations.Managed: "true",
				annotations.ModelID: modelID,
			},
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: targetName},
			MaxReplicas:    5,
		},
	}
}

// managedSO builds a ScaledObject fixture targeting a Deployment. Tests in
// this package only need the Deployment kind, so it's hardcoded; expand to a
// parameter if a future test needs a different kind.
func managedSO(ns, name, targetName, modelID string) *kedav1alpha1.ScaledObject {
	return &kedav1alpha1.ScaledObject{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Annotations: map[string]string{
				annotations.Managed: "true",
				annotations.ModelID: modelID,
			},
		},
		Spec: kedav1alpha1.ScaledObjectSpec{
			ScaleTargetRef: &kedav1alpha1.ScaleTarget{Kind: "Deployment", Name: targetName},
		},
	}
}

func TestAnnotationSourcedVariants(t *testing.T) {
	ctx := context.Background()

	t.Run("HPAs only", func(t *testing.T) {
		s := variantTestScheme(t)
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedHPA("ns1", "hpa-a", "deploy-a", "model-x"),
			managedHPA("ns1", "hpa-b", "deploy-b", "model-x"),
			// unmanaged HPA — must be filtered out
			&autoscalingv2.HorizontalPodAutoscaler{
				ObjectMeta: metav1.ObjectMeta{Name: "hpa-unmanaged", Namespace: "ns1"},
				Spec:       autoscalingv2.HorizontalPodAutoscalerSpec{MaxReplicas: 3},
			},
		).Build()

		result, err := annotationSourcedVariants(ctx, cl, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 2 {
			t.Errorf("want 2 VAs, got %d", len(result))
		}
	})

	t.Run("ScaledObjects only", func(t *testing.T) {
		s := variantTestScheme(t)
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedSO("ns1", "so-a", "deploy-a", "model-x"),
		).Build()

		result, err := annotationSourcedVariants(ctx, cl, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 1 {
			t.Errorf("want 1 VA, got %d", len(result))
		}
	})

	t.Run("mixed HPAs and ScaledObjects", func(t *testing.T) {
		s := variantTestScheme(t)
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedHPA("ns1", "hpa-a", "deploy-a", "model-x"),
			managedSO("ns1", "so-b", "deploy-b", "model-x"),
		).Build()

		result, err := annotationSourcedVariants(ctx, cl, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 2 {
			t.Errorf("want 2 VAs, got %d", len(result))
		}
	})

	t.Run("KEDA not installed — NoMatchError skipped gracefully", func(t *testing.T) {
		s := variantTestScheme(t)
		soGK := schema.GroupKind{Group: "keda.sh", Kind: "ScaledObject"}
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedHPA("ns1", "hpa-a", "deploy-a", "model-x"),
		).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*kedav1alpha1.ScaledObjectList); ok {
					return &apimeta.NoKindMatchError{GroupKind: soGK}
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

		result, err := annotationSourcedVariants(ctx, cl, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 1 {
			t.Errorf("want 1 VA from HPA, got %d", len(result))
		}
	})

	t.Run("KEDA non-NoMatch error propagated", func(t *testing.T) {
		s := variantTestScheme(t)
		cl := fake.NewClientBuilder().WithScheme(s).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*kedav1alpha1.ScaledObjectList); ok {
					return errors.New("keda api unavailable")
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

		_, err := annotationSourcedVariants(ctx, cl, nil)
		if err == nil {
			t.Fatal("want error for non-NoMatch ScaledObject list failure, got nil")
		}
	})

	t.Run("deduplication: ScaledObject wins over HPA for same scale target", func(t *testing.T) {
		s := variantTestScheme(t)
		// Both an HPA and a ScaledObject point at the same Deployment — ScaledObject wins.
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedHPA("ns1", "hpa-a", "deploy-a", "model-hpa"),
			managedSO("ns1", "so-a", "deploy-a", "model-so"),
		).Build()

		result, err := annotationSourcedVariants(ctx, cl, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 1 {
			t.Errorf("want 1 VA (deduplicated), got %d", len(result))
		}
		if result[0].Spec.ModelID != "model-so" {
			t.Errorf("want ScaledObject to win, got modelID %q", result[0].Spec.ModelID)
		}
	})

	t.Run("hpaScopedNamespaces scopes HPA discovery; SO list stays cluster-wide", func(t *testing.T) {
		// HPA scoping must exclude managed HPAs outside the tracked set, while
		// ScaledObject discovery still finds managed SOs everywhere because
		// SO listing is unconditionally cluster-wide (preserves correctness
		// for KEDA installed after WVA startup).
		s := variantTestScheme(t)
		var hpaListNamespaces, soListNamespaces []string
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedHPA("ns1", "hpa-a", "deploy-a", "model-x"),
			managedHPA("ns2", "hpa-b", "deploy-b", "model-x"), // outside HPA scope
			managedSO("ns3", "so-c", "deploy-c", "model-x"),
		).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				var listOpts client.ListOptions
				listOpts.ApplyOptions(opts)
				switch list.(type) {
				case *autoscalingv2.HorizontalPodAutoscalerList:
					hpaListNamespaces = append(hpaListNamespaces, listOpts.Namespace)
				case *kedav1alpha1.ScaledObjectList:
					soListNamespaces = append(soListNamespaces, listOpts.Namespace)
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

		result, err := annotationSourcedVariants(ctx, cl, []string{"ns1"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Expect ns1's HPA (scoped in) and ns3's SO (cluster-wide). ns2's HPA must be excluded.
		gotByNamespace := map[string]bool{}
		for _, va := range result {
			gotByNamespace[va.Namespace] = true
		}
		if !gotByNamespace["ns1"] || !gotByNamespace["ns3"] {
			t.Errorf("want ns1's HPA and ns3's SO to be present, got %v", gotByNamespace)
		}
		if gotByNamespace["ns2"] {
			t.Errorf("ns2's HPA is outside hpaScopedNamespaces but was returned")
		}
		// HPA list must be scoped to ns1.
		for _, ns := range hpaListNamespaces {
			if ns != "ns1" {
				t.Errorf("want HPA list scoped to ns1, got namespace %q (full: %v)", ns, hpaListNamespaces)
			}
		}
		// SO list must be cluster-wide (Namespace == "" on ListOptions).
		if len(soListNamespaces) != 1 || soListNamespaces[0] != "" {
			t.Errorf("want one cluster-wide SO list, got %v", soListNamespaces)
		}
	})

	t.Run("hpaScopedNamespaces with multiple entries unions HPA results; SOs still cluster-wide", func(t *testing.T) {
		s := variantTestScheme(t)
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedHPA("ns1", "hpa-a", "deploy-a", "model-x"),
			managedHPA("ns2", "hpa-b", "deploy-b", "model-x"),
			managedHPA("ns3", "hpa-c", "deploy-c", "model-x"), // not in HPA scope
			managedSO("ns3", "so-d", "deploy-d", "model-x"),
		).Build()

		result, err := annotationSourcedVariants(ctx, cl, []string{"ns1", "ns2"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		gotByNamespace := map[string]bool{}
		gotByModelTarget := map[string]bool{}
		for _, va := range result {
			gotByNamespace[va.Namespace] = true
			gotByModelTarget[va.Namespace+"/"+va.Spec.ScaleTargetRef.Name] = true
		}
		// HPAs from ns1 and ns2 are present; ns3's HPA is filtered out.
		if !gotByModelTarget["ns1/deploy-a"] || !gotByModelTarget["ns2/deploy-b"] {
			t.Errorf("want HPAs from ns1 and ns2, got %v", gotByModelTarget)
		}
		if gotByModelTarget["ns3/deploy-c"] {
			t.Errorf("ns3's HPA is outside hpaScopedNamespaces but was returned")
		}
		// ns3's SO is present because SO discovery is cluster-wide.
		if !gotByModelTarget["ns3/deploy-d"] {
			t.Errorf("want ns3's SO to be present (cluster-wide list), got %v", gotByModelTarget)
		}
	})

	t.Run("nil hpaScopedNamespaces falls back to cluster-wide HPA list", func(t *testing.T) {
		// nil = sync gate not open yet. HPAs from every namespace must be
		// returned so the startup window does not silently drop them.
		s := variantTestScheme(t)
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedHPA("ns1", "hpa-a", "deploy-a", "model-x"),
			managedHPA("ns2", "hpa-b", "deploy-b", "model-x"),
		).Build()

		result, err := annotationSourcedVariants(ctx, cl, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 2 {
			t.Errorf("want 2 VAs (cluster-wide fallback) for nil, got %d", len(result))
		}
	})

	t.Run("empty non-nil hpaScopedNamespaces skips HPA list entirely; SOs still discovered", func(t *testing.T) {
		// []string{} = synced, no managed HPAs anywhere. The HPA list must
		// be skipped so a CRD-only deployment doesn't keep cluster-wide
		// scanning forever. ScaledObject discovery is unrelated and still
		// runs cluster-wide.
		s := variantTestScheme(t)
		var hpaListCount int
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedHPA("ns1", "hpa-a", "deploy-a", "model-x"), // exists but must be skipped
			managedSO("ns1", "so-b", "deploy-b", "model-x"),
		).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*autoscalingv2.HorizontalPodAutoscalerList); ok {
					hpaListCount++
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

		result, err := annotationSourcedVariants(ctx, cl, []string{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if hpaListCount != 0 {
			t.Errorf("want zero HPA List calls when hpaScopedNamespaces is empty non-nil, got %d", hpaListCount)
		}
		// Only the SO should be returned; the HPA was skipped.
		if len(result) != 1 || result[0].Spec.ScaleTargetRef.Name != "deploy-b" {
			t.Errorf("want only the SO-sourced VA, got %v", result)
		}
	})
}

func TestReadyVariantAutoscalingsMergePath(t *testing.T) {
	ctx := context.Background()

	makeCRDVA := func(ns, name, targetKind, targetName string) *wvav1alpha1.VariantAutoscaling {
		return &wvav1alpha1.VariantAutoscaling{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			Spec: wvav1alpha1.VariantAutoscalingSpec{
				ModelID:        "model-crd",
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: targetKind, Name: targetName},
			},
		}
	}

	t.Run("CRD and annotation with different targets — both returned", func(t *testing.T) {
		s := variantTestScheme(t)
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			makeCRDVA("ns1", "va-crd", "Deployment", "deploy-crd"),
			managedHPA("ns2", "hpa-ann", "deploy-ann", "model-ann"),
		).Build()

		result, err := readyVariantAutoscalings(ctx, cl, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 2 {
			t.Errorf("want 2 VAs, got %d", len(result))
		}
	})

	t.Run("CRD wins when same namespace/kind/name as annotation-sourced", func(t *testing.T) {
		s := variantTestScheme(t)
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			makeCRDVA("ns1", "va-crd", "Deployment", "shared-deploy"),
			managedHPA("ns1", "hpa-ann", "shared-deploy", "model-ann"),
		).Build()

		result, err := readyVariantAutoscalings(ctx, cl, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 1 {
			t.Errorf("want 1 VA (CRD wins), got %d", len(result))
		}
		if result[0].Spec.ModelID != "model-crd" {
			t.Errorf("want CRD VA to win, got modelID %q", result[0].Spec.ModelID)
		}
	})

	t.Run("annotationSourcedVariants error is non-fatal — CRD-sourced still returned", func(t *testing.T) {
		s := variantTestScheme(t)
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			makeCRDVA("ns1", "va-crd", "Deployment", "deploy-crd"),
		).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				// fail HPA listing to force annotationSourcedVariants to return an error
				if _, ok := list.(*autoscalingv2.HorizontalPodAutoscalerList); ok {
					return errors.New("hpa api unavailable")
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

		result, err := readyVariantAutoscalings(ctx, cl, nil)
		if err != nil {
			t.Fatalf("expected non-fatal path, got error: %v", err)
		}
		if len(result) != 1 || result[0].Name != "va-crd" {
			t.Errorf("want 1 CRD VA, got %d VAs", len(result))
		}
	})

	t.Run("no CRD VA, annotated HPA, KEDA listing fails — HPA VA still returned", func(t *testing.T) {
		// annotationSourcedVariants successfully lists the HPA but then fails listing
		// ScaledObjects with a non-NoMatch error (e.g. transient API error).
		// readyVariantAutoscalings logs the error as non-fatal and still merges the
		// partial annotation results, so the HPA-sourced VA is returned.
		s := variantTestScheme(t)
		cl := fake.NewClientBuilder().WithScheme(s).WithObjects(
			managedHPA("ns1", "hpa-ann", "deploy-ann", "model-ann"),
		).WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*kedav1alpha1.ScaledObjectList); ok {
					return errors.New("keda api unavailable")
				}
				return c.List(ctx, list, opts...)
			},
		}).Build()

		result, err := readyVariantAutoscalings(ctx, cl, nil)
		if err != nil {
			t.Fatalf("expected non-fatal path, got error: %v", err)
		}
		if len(result) != 1 {
			t.Errorf("want 1 VA (HPA-sourced, KEDA error is non-fatal), got %d", len(result))
		}
	})
}
