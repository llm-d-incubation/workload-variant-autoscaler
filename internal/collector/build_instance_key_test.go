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

package collector

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/locator"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/controller/indexers"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// buildInstanceKeyTestCase drives a single call through CollectReplicaMetrics with
// a source that returns exactly one KV-cache sample, then checks that the resulting
// ReplicaMetrics carries the expected vaName (or none when the label is absent).
type buildInstanceKeyTestCase struct {
	name        string
	labels      map[string]string
	wantVAName  string
	wantSkipped bool // true when buildInstanceKey returns ("","","") → no entry produced
}

var buildInstanceKeyTestCases = []buildInstanceKeyTestCase{
	{
		name: "pod label present – vaName propagated",
		labels: map[string]string{
			"pod":                               "pod-abc",
			"instance":                          "10.0.0.1:8000",
			constants.VariantLabelPrometheusKey: "my-va",
		},
		wantVAName: "my-va",
	},
	{
		name: "pod_name fallback – vaName propagated",
		labels: map[string]string{
			"pod_name":                          "pod-xyz",
			"instance":                          "10.0.0.2:8000",
			constants.VariantLabelPrometheusKey: "other-va",
		},
		wantVAName: "other-va",
	},
	{
		// Pods without llm_d_ai_variant are skipped at line 669 of replica_metrics.go
		// ("Skipping pod that doesn't match any scale target"), so no ReplicaMetrics is produced.
		name: "llm_d_ai_variant label absent – pod skipped, no result",
		labels: map[string]string{
			"pod":      "pod-no-variant",
			"instance": "10.0.0.3:8000",
		},
		wantSkipped: true,
	},
	{
		// Same: empty string is treated the same as missing.
		name: "llm_d_ai_variant label empty string – pod skipped, no result",
		labels: map[string]string{
			"pod":                               "pod-empty-variant",
			"instance":                          "10.0.0.4:8000",
			constants.VariantLabelPrometheusKey: "",
		},
		wantSkipped: true,
	},
	{
		name: "no pod identity labels – entry skipped entirely",
		labels: map[string]string{
			constants.VariantLabelPrometheusKey: "irrelevant",
		},
		wantSkipped: true,
	},
	{
		name: "instance-only (no pod name) – instance used as key, vaName propagated",
		labels: map[string]string{
			"instance":                          "10.0.0.5:8000",
			constants.VariantLabelPrometheusKey: "instance-va",
		},
		wantVAName: "instance-va",
	},
}

func TestBuildInstanceKey_VANameExtraction(t *testing.T) {
	for _, tc := range buildInstanceKeyTestCases {
		t.Run(tc.name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			if err := metrics.InitMetrics(registry); err != nil {
				t.Fatalf("InitMetrics: %v", err)
			}

			scheme := runtime.NewScheme()
			if err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme); err != nil {
				t.Fatalf("AddToScheme: %v", err)
			}
			k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

			mockSource := &mockMetricsSource{
				refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
					return map[string]*source.MetricResult{
						"kv_cache_usage": {
							Values: []source.MetricValue{
								{
									Labels:    tc.labels,
									Value:     0.5,
									Timestamp: time.Now(),
								},
							},
						},
					}, nil
				},
			}

			collector := NewReplicaMetricsCollector(mockSource, k8sClient, nil, nil)
			results, err := collector.CollectReplicaMetrics(
				context.Background(),
				"test-model",
				"test-ns",
				make(map[string]scaletarget.ScaleTargetAccessor),
				make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
				nil,
				make(map[string]float64),
			)
			if err != nil {
				t.Fatalf("CollectReplicaMetrics: %v", err)
			}

			if tc.wantSkipped {
				if len(results) != 0 {
					t.Errorf("expected no results for skipped entry, got %d", len(results))
				}
				return
			}

			if len(results) == 0 {
				t.Fatalf("expected at least one ReplicaMetrics result")
			}

			got := results[0].VariantName
			if got != tc.wantVAName {
				t.Errorf("VariantName: got %q, want %q", got, tc.wantVAName)
			}
		})
	}
}

// mockLocator implements locator.PodLocator for testing.
type mockLocator struct {
	locateFunc  func(ctx context.Context, namespace, podName string) (*locator.ManagedScaler, error)
	resolveFunc func(ctx context.Context, namespace, podName string) (autoscalingv2.CrossVersionObjectReference, bool, error)
}

func (m *mockLocator) Locate(ctx context.Context, namespace, podName string) (*locator.ManagedScaler, error) {
	if m.locateFunc != nil {
		return m.locateFunc(ctx, namespace, podName)
	}
	return nil, nil
}

func (m *mockLocator) LocateByVariant(_ context.Context, _, _ string) (*locator.ManagedScaler, error) {
	return nil, nil
}

// TODO(va-removal): remove ResolveScaleTarget from the mock when the CRD-based
// dual-mode fallback (and the interface method) are removed.
func (m *mockLocator) ResolveScaleTarget(ctx context.Context, namespace, podName string) (autoscalingv2.CrossVersionObjectReference, bool, error) {
	if m.resolveFunc != nil {
		return m.resolveFunc(ctx, namespace, podName)
	}
	return autoscalingv2.CrossVersionObjectReference{}, false, nil
}

// TestBuildInstanceKey_VACRDNameDiffersFromHPAName is the regression test for
// https://github.com/llm-d/llm-d-workload-variant-autoscaler/issues/1290.
//
// KServe creates a VariantAutoscaling CRD named "{isvc}-kserve-va" and an HPA
// named "{isvc}-kserve-hpa", both targeting the same Deployment. Before the fix,
// buildInstanceKey returned the HPA name as vaName; filterReplicaMetricsByVariants
// then filtered every metric out because the HPA name was not in the VA-CRD-keyed
// allowed set.
//
// TODO(va-removal): remove this test when the VariantAutoscaling CRD is removed;
// without the CRD the HPA name is always the correct vaName.
func TestBuildInstanceKey_VACRDNameDiffersFromHPAName(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics: %v", err)
	}

	const namespace = "test-ns"
	const deployName = "foo-deploy"
	const hpaName = "foo-kserve-hpa"
	const wantVAName = "foo-kserve-va"

	va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{
			Name:      wantVAName,
			Namespace: namespace,
		},
		Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deployName,
			},
		},
	}

	scheme := runtime.NewScheme()
	if err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(va).
		WithIndex(&llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}, indexers.VAScaleTargetKey, indexers.VAScaleTargetIndexFunc).
		Build()

	hpa := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      hpaName,
			Namespace: namespace,
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deployName,
			},
		},
	}

	mockLoc := &mockLocator{
		locateFunc: func(_ context.Context, ns, podName string) (*locator.ManagedScaler, error) {
			if ns == namespace && podName == "foo-pod" {
				return &locator.ManagedScaler{HPA: hpa}, nil
			}
			return nil, nil
		},
	}

	mockSource := &mockMetricsSource{
		refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return map[string]*source.MetricResult{
				"kv_cache_usage": {
					Values: []source.MetricValue{
						{
							Labels: map[string]string{
								"pod":      "foo-pod",
								"instance": "10.0.0.1:8000",
							},
							Value:     0.5,
							Timestamp: time.Now(),
						},
					},
				},
			}, nil
		},
	}

	c := NewReplicaMetricsCollector(mockSource, k8sClient, nil, mockLoc)
	results, err := c.CollectReplicaMetrics(
		context.Background(),
		"test-model",
		namespace,
		make(map[string]scaletarget.ScaleTargetAccessor),
		make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
		nil,
		make(map[string]float64),
	)
	if err != nil {
		t.Fatalf("CollectReplicaMetrics: %v", err)
	}

	if len(results) == 0 {
		t.Fatalf("expected one ReplicaMetrics result; got none — locator returned HPA %q but no metric was produced", hpaName)
	}

	got := results[0].VariantName
	if got != wantVAName {
		t.Errorf("VariantName = %q; want %q (HPA name is %q — fix must resolve to VA CRD name via scaleTargetRef lookup)",
			got, wantVAName, hpaName)
	}
}

// TestBuildInstanceKey_UnmanagedHPAFallsBackToVALookup is the regression test for
// the v0.7.0 → v0.8.0-rc4 regression where KServe pod metrics were dropped.
//
// KServe creates its own HPA WITHOUT llm-d.ai/managed=true, so the locator's
// managed-only lookup returns (nil, nil). The fallback resolves the pod's scale
// target (Deployment) via ResolveScaleTarget and looks up the VA that targets
// it, restoring the v0.7.0 Pod → Deployment → VA attribution.
//
// TODO(va-removal): remove this test together with the CRD-based dual-mode
// fallback when the VariantAutoscaling CRD is removed.
func TestBuildInstanceKey_UnmanagedHPAFallsBackToVALookup(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics: %v", err)
	}

	const namespace = "test-ns"
	const deployName = "foo-deploy"
	const wantVAName = "foo-kserve-va"

	deployRef := autoscalingv2.CrossVersionObjectReference{
		APIVersion: "apps/v1",
		Kind:       "Deployment",
		Name:       deployName,
	}

	va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{
			Name:      wantVAName,
			Namespace: namespace,
		},
		Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
			ScaleTargetRef: deployRef,
		},
	}

	scheme := runtime.NewScheme()
	if err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(va).
		WithIndex(&llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}, indexers.VAScaleTargetKey, indexers.VAScaleTargetIndexFunc).
		Build()

	// Locate returns (nil, nil): the KServe HPA is not managed, so no managed
	// scaler is found. ResolveScaleTarget returns the Deployment the pod's owner
	// chain reaches.
	mockLoc := &mockLocator{
		locateFunc: func(_ context.Context, _, _ string) (*locator.ManagedScaler, error) {
			return nil, nil
		},
		resolveFunc: func(_ context.Context, ns, podName string) (autoscalingv2.CrossVersionObjectReference, bool, error) {
			if ns == namespace && podName == "foo-pod" {
				return deployRef, true, nil
			}
			return autoscalingv2.CrossVersionObjectReference{}, false, nil
		},
	}

	mockSource := &mockMetricsSource{
		refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return map[string]*source.MetricResult{
				"kv_cache_usage": {
					Values: []source.MetricValue{
						{
							Labels: map[string]string{
								"pod":      "foo-pod",
								"instance": "10.0.0.1:8000",
							},
							Value:     0.5,
							Timestamp: time.Now(),
						},
					},
				},
			}, nil
		},
	}

	c := NewReplicaMetricsCollector(mockSource, k8sClient, nil, mockLoc)
	results, err := c.CollectReplicaMetrics(
		context.Background(),
		"test-model",
		namespace,
		make(map[string]scaletarget.ScaleTargetAccessor),
		make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
		nil,
		make(map[string]float64),
	)
	if err != nil {
		t.Fatalf("CollectReplicaMetrics: %v", err)
	}

	if len(results) == 0 {
		t.Fatalf("expected one ReplicaMetrics result; got none — unmanaged-HPA fallback did not attribute the pod to VA %q", wantVAName)
	}
	if got := results[0].VariantName; got != wantVAName {
		t.Errorf("VariantName = %q; want %q (fallback must resolve VA via ResolveScaleTarget + FindVAForScaleTarget)", got, wantVAName)
	}
}

// TestBuildInstanceKey_UnmanagedHPANoMatchingVA verifies that when neither a
// managed scaler nor a VA targeting the resolved scale target exists, the pod
// stays unattributed (vaName="") and is skipped — no ReplicaMetrics produced.
//
// TODO(va-removal): remove this test together with the CRD-based dual-mode
// fallback when the VariantAutoscaling CRD is removed.
func TestBuildInstanceKey_UnmanagedHPANoMatchingVA(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics: %v", err)
	}

	const namespace = "test-ns"

	scheme := runtime.NewScheme()
	if err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	// No VA objects: FindVAForScaleTarget returns (nil, nil).
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithIndex(&llmdVariantAutoscalingV1alpha1.VariantAutoscaling{}, indexers.VAScaleTargetKey, indexers.VAScaleTargetIndexFunc).
		Build()

	mockLoc := &mockLocator{
		locateFunc: func(_ context.Context, _, _ string) (*locator.ManagedScaler, error) {
			return nil, nil
		},
		resolveFunc: func(_ context.Context, _, _ string) (autoscalingv2.CrossVersionObjectReference, bool, error) {
			return autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       "foo-deploy",
			}, true, nil
		},
	}

	mockSource := &mockMetricsSource{
		refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return map[string]*source.MetricResult{
				"kv_cache_usage": {
					Values: []source.MetricValue{
						{
							Labels: map[string]string{
								"pod":      "foo-pod",
								"instance": "10.0.0.1:8000",
							},
							Value:     0.5,
							Timestamp: time.Now(),
						},
					},
				},
			}, nil
		},
	}

	c := NewReplicaMetricsCollector(mockSource, k8sClient, nil, mockLoc)
	results, err := c.CollectReplicaMetrics(
		context.Background(),
		"test-model",
		namespace,
		make(map[string]scaletarget.ScaleTargetAccessor),
		make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
		nil,
		make(map[string]float64),
	)
	if err != nil {
		t.Fatalf("CollectReplicaMetrics: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected no results when no VA targets the scale target, got %d", len(results))
	}
}
