/*
Copyright 2026 The llm-d Authors

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
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// mockMetricsSource implements source.MetricsSource for testing
type mockMetricsSource struct {
	refreshFunc func(ctx context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error)
}

func (m *mockMetricsSource) Refresh(ctx context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error) {
	if m.refreshFunc != nil {
		return m.refreshFunc(ctx, spec)
	}
	return make(map[string]*source.MetricResult), nil
}

func (m *mockMetricsSource) QueryList() *source.QueryList {
	return source.NewQueryList()
}

func (m *mockMetricsSource) Get(queryName string, params map[string]string) *source.CachedValue {
	return nil
}

func TestCollectReplicaMetrics_MetricsObservation(t *testing.T) {
	// Initialize metrics with a fresh registry
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatalf("Failed to initialize metrics: %v", err)
	}

	// Create a mock source that returns empty results
	mockSource := &mockMetricsSource{
		refreshFunc: func(ctx context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error) {
			// Simulate some query latency
			time.Sleep(10 * time.Millisecond)
			// Return empty results
			return make(map[string]*source.MetricResult), nil
		},
	}

	// Create test dependencies
	scheme := runtime.NewScheme()
	err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme)
	if err != nil {
		t.Fatalf("Failed to add scheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	collector := NewReplicaMetricsCollector(mockSource, k8sClient)

	// Call the function
	_, err = collector.CollectReplicaMetrics(
		context.Background(),
		"test-model",
		"test-namespace",
		make(map[string]scaletarget.ScaleTargetAccessor),
		make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
		make(map[string]float64),
	)
	if err != nil {
		t.Fatalf("CollectReplicaMetrics failed: %v", err)
	}

	// Gather metrics from the registry
	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	// Verify ObserveMetricsCollectionDuration was called for all query types
	var foundDurationMetric bool
	expectedQueryTypes := map[string]bool{
		constants.QueryTypeKVCache:     false,
		constants.QueryTypeQueueLength: false,
		constants.QueryTypeCacheConfig: false,
	}

	for _, mf := range metricFamilies {
		if mf.GetName() == constants.WVAMetricsCollectionDurationSeconds {
			foundDurationMetric = true

			// Check each metric series
			for _, m := range mf.GetMetric() {
				// Find query_type label
				for _, label := range m.GetLabel() {
					if label.GetName() == constants.LabelQueryType {
						queryType := label.GetValue()
						if _, exists := expectedQueryTypes[queryType]; exists {
							expectedQueryTypes[queryType] = true
							histogram := m.GetHistogram()
							if histogram == nil {
								t.Errorf("Expected histogram for query_type=%s", queryType)
								continue
							}
							if histogram.GetSampleCount() == 0 {
								t.Errorf("Expected at least one observation for query_type=%s", queryType)
							}
							if histogram.GetSampleSum() <= 0 {
								t.Errorf("Expected positive duration for query_type=%s", queryType)
							}
						}
					}
				}
			}
		}
	}

	if !foundDurationMetric {
		t.Errorf("Metric %s not found", constants.WVAMetricsCollectionDurationSeconds)
	}

	// Verify all expected query types were recorded
	for queryType, found := range expectedQueryTypes {
		if !found {
			t.Errorf("Expected duration metric for query_type=%s but was not found", queryType)
		}
	}

	// Verify SetMetricsPodsDiscovered was called
	var foundPodsMetric bool
	for _, mf := range metricFamilies {
		if mf.GetName() == constants.WVAMetricsPodsDiscovered {
			foundPodsMetric = true
			// Should have at least one metric (for test-namespace)
			if len(mf.GetMetric()) == 0 {
				t.Error("Expected at least one pods discovered metric")
			}
		}
	}

	if !foundPodsMetric {
		t.Errorf("Metric %s not found", constants.WVAMetricsPodsDiscovered)
	}
}

func TestCollectReplicaMetrics_ErrorMetrics(t *testing.T) {
	// Initialize metrics with a fresh registry
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatalf("Failed to initialize metrics: %v", err)
	}

	// Create a mock source that returns an error
	testErr := context.DeadlineExceeded
	mockSource := &mockMetricsSource{
		refreshFunc: func(ctx context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return nil, testErr
		},
	}

	// Create test dependencies
	scheme := runtime.NewScheme()
	err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme)
	if err != nil {
		t.Fatalf("Failed to add scheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	collector := NewReplicaMetricsCollector(mockSource, k8sClient)

	// Call the function - should return error
	_, err = collector.CollectReplicaMetrics(
		context.Background(),
		"test-model",
		"test-namespace",
		make(map[string]scaletarget.ScaleTargetAccessor),
		make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
		make(map[string]float64),
	)
	if err == nil {
		t.Fatal("Expected error but got nil")
	}

	// Gather metrics from the registry
	metricFamilies, err := registry.Gather()
	if err != nil {
		t.Fatalf("Failed to gather metrics: %v", err)
	}

	// Verify IncMetricsCollectionErrors was called for all query types
	var foundErrorMetric bool
	expectedQueryTypes := map[string]bool{
		constants.QueryTypeKVCache:     false,
		constants.QueryTypeQueueLength: false,
		constants.QueryTypeCacheConfig: false,
	}

	for _, mf := range metricFamilies {
		if mf.GetName() == constants.WVAMetricsCollectionErrorsTotal {
			foundErrorMetric = true

			// Check each metric series
			for _, m := range mf.GetMetric() {
				// Find query_type label
				var queryType string
				for _, label := range m.GetLabel() {
					if label.GetName() == constants.LabelQueryType {
						queryType = label.GetValue()
						break
					}
				}

				if _, exists := expectedQueryTypes[queryType]; exists {
					expectedQueryTypes[queryType] = true
					counter := m.GetCounter()
					if counter == nil {
						t.Errorf("Expected counter for query_type=%s", queryType)
						continue
					}
					if counter.GetValue() != 1.0 {
						t.Errorf("Expected error count 1 for query_type=%s, got %f", queryType, counter.GetValue())
					}
				}
			}
		}
	}

	if !foundErrorMetric {
		t.Errorf("Metric %s not found", constants.WVAMetricsCollectionErrorsTotal)
	}

	// Verify all expected query types were recorded
	for queryType, found := range expectedQueryTypes {
		if !found {
			t.Errorf("Expected error metric for query_type=%s but was not found", queryType)
		}
	}
}
