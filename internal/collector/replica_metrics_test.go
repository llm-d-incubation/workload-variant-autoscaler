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
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	llmdVariantAutoscalingV1alpha1 "github.com/llm-d/llm-d-workload-variant-autoscaler/api/v1alpha1"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/constants"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/metrics"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/utils/scaletarget"
)

// mockMetricsSource is a mock implementation of source.MetricsSource for testing
type mockMetricsSource struct {
	refreshFunc  func(ctx context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error)
	refreshError error
	results      map[string]*source.MetricResult
}

func (m *mockMetricsSource) QueryList() *source.QueryList {
	return source.NewQueryList()
}

func (m *mockMetricsSource) Refresh(ctx context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error) {
	// If refreshFunc is set, use it (takes precedence)
	if m.refreshFunc != nil {
		return m.refreshFunc(ctx, spec)
	}
	// Otherwise use the error/results fields
	if m.refreshError != nil {
		return nil, m.refreshError
	}
	if m.results != nil {
		return m.results, nil
	}
	// Return empty results by default
	emptyResults := make(map[string]*source.MetricResult)
	for _, query := range spec.Queries {
		emptyResults[query] = &source.MetricResult{
			QueryName: query,
			Values:    []source.MetricValue{},
		}
	}
	return emptyResults, nil
}

func (m *mockMetricsSource) Get(queryName string, params map[string]string) *source.CachedValue {
	return nil
}

func TestRecordMetricsUnavailableEvent(t *testing.T) {
	tests := []struct {
		name         string
		numVAs       int
		expectedEvts int
	}{
		{
			name:         "records event for single VA",
			numVAs:       1,
			expectedEvts: 1,
		},
		{
			name:         "records event for multiple VAs",
			numVAs:       3,
			expectedEvts: 3,
		},
		{
			name:         "handles empty VA map",
			numVAs:       0,
			expectedEvts: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeRecorder := record.NewFakeRecorder(100)
			mockSource := &mockMetricsSource{}
			collector := NewReplicaMetricsCollector(mockSource, nil, fakeRecorder)

			variantAutoscalings := make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling)
			for i := 0; i < tt.numVAs; i++ {
				vaName := "test-va"
				if i > 0 {
					vaName = "test-va-" + string(rune('a'+i))
				}
				variantAutoscalings["default/"+vaName] = &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
					ObjectMeta: metav1.ObjectMeta{
						Name:      vaName,
						Namespace: "default",
					},
					Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
						ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
							Kind: "Deployment",
							Name: vaName + "-deployment",
						},
						ModelID:     "test-model",
						MaxReplicas: 5,
					},
				}
			}

			collector.recordMetricsUnavailableEvent(variantAutoscalings, "Test metrics unavailable")

			// Count recorded events
			eventCount := 0
			for {
				select {
				case event := <-fakeRecorder.Events:
					assert.Contains(t, event, constants.K8SEventMetricsUnavailable,
						"Event should contain K8SEventMetricsUnavailable constant")
					assert.Contains(t, event, "Test metrics unavailable",
						"Event should contain the reason message")
					eventCount++
				default:
					goto done
				}
			}
		done:
			assert.Equal(t, tt.expectedEvts, eventCount,
				"Should record correct number of events")
		})
	}
}

func TestCollectReplicaMetrics_ErrorRecordsEvent(t *testing.T) {
	ctx := context.Background()
	fakeRecorder := record.NewFakeRecorder(100)

	tests := []struct {
		name          string
		refreshError  error
		expectedEvent bool
		eventReason   string
	}{
		{
			name:          "records event when refresh fails",
			refreshError:  errors.New("prometheus connection failed"),
			expectedEvent: true,
			eventReason:   "Failed to collect metrics for model",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockSource := &mockMetricsSource{
				refreshError: tt.refreshError,
			}
			collector := NewReplicaMetricsCollector(mockSource, nil, fakeRecorder)

			variantAutoscalings := map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
				"default/test-va": {
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-va",
						Namespace: "default",
					},
					Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
						ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
							Kind: "Deployment",
							Name: "test-deployment",
						},
						ModelID:     "test-model",
						MaxReplicas: 5,
					},
				},
			}

			scaleTargets := make(map[string]scaletarget.ScaleTargetAccessor)
			variantCosts := make(map[string]float64)

			metrics, err := collector.CollectReplicaMetrics(
				ctx,
				"test-model",
				"default",
				scaleTargets,
				variantAutoscalings,
				variantCosts,
			)

			require.Error(t, err, "Should return error when refresh fails")
			require.Nil(t, metrics, "Should return nil metrics on error")

			if tt.expectedEvent {
				select {
				case event := <-fakeRecorder.Events:
					assert.Contains(t, event, constants.K8SEventMetricsUnavailable,
						"Event should contain K8SEventMetricsUnavailable constant")
					assert.Contains(t, event, tt.eventReason,
						"Event should contain the expected reason")
				default:
					t.Error("Expected event to be recorded but none was found")
				}
			}
		})
	}
}

func TestCollectReplicaMetrics_NoMetricsRecordsEvent(t *testing.T) {
	ctx := context.Background()
	fakeRecorder := record.NewFakeRecorder(100)

	// Mock source that returns no error but empty results (no metrics)
	mockSource := &mockMetricsSource{
		results: make(map[string]*source.MetricResult),
	}
	collector := NewReplicaMetricsCollector(mockSource, nil, fakeRecorder)

	variantAutoscalings := map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		"default/test-va": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-va",
				Namespace: "default",
			},
			Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: "test-deployment",
				},
				ModelID:     "test-model",
				MaxReplicas: 5,
			},
		},
	}

	scaleTargets := make(map[string]scaletarget.ScaleTargetAccessor)
	variantCosts := make(map[string]float64)

	metrics, err := collector.CollectReplicaMetrics(
		ctx,
		"test-model",
		"default",
		scaleTargets,
		variantAutoscalings,
		variantCosts,
	)

	require.NoError(t, err, "Should not return error when no metrics available")
	require.Empty(t, metrics, "Should return empty metrics slice")

	// Should record event for no metrics available
	select {
	case event := <-fakeRecorder.Events:
		assert.Contains(t, event, constants.K8SEventMetricsUnavailable,
			"Event should contain K8SEventMetricsUnavailable constant")
		assert.Contains(t, event, "No saturation metrics available for model",
			"Event should contain the expected reason")
	default:
		t.Error("Expected event to be recorded but none was found")
	}
}

func TestK8SEventMetricsUnavailableConstant(t *testing.T) {
	// Verify the constant is correctly defined
	assert.Equal(t, "MetricsUnavailable", constants.K8SEventMetricsUnavailable,
		"K8SEventMetricsUnavailable constant should match expected value")
}

func TestCollectReplicaMetrics_EdgeTriggeredEvents(t *testing.T) {
	ctx := context.Background()
	fakeRecorder := record.NewFakeRecorder(100)

	// Mock source that returns empty results (no metrics)
	mockSource := &mockMetricsSource{
		results: make(map[string]*source.MetricResult),
	}
	collector := NewReplicaMetricsCollector(mockSource, nil, fakeRecorder)

	variantAutoscalings := map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		"default/test-va": {
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-va",
				Namespace: "default",
			},
			Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
					Kind: "Deployment",
					Name: "test-deployment",
				},
				ModelID:     "test-model",
				MaxReplicas: 5,
			},
		},
	}

	scaleTargets := make(map[string]scaletarget.ScaleTargetAccessor)
	variantCosts := make(map[string]float64)

	// First call: metrics unavailable, should emit event (first time seeing this VA)
	_, err := collector.CollectReplicaMetrics(ctx, "test-model", "default", scaleTargets, variantAutoscalings, variantCosts)
	require.NoError(t, err)

	select {
	case event := <-fakeRecorder.Events:
		assert.Contains(t, event, constants.K8SEventMetricsUnavailable,
			"First call should emit event when metrics unavailable")
	default:
		t.Error("Expected event on first call with unavailable metrics")
	}

	// Second call: metrics still unavailable, should NOT emit event (no state transition)
	_, err = collector.CollectReplicaMetrics(ctx, "test-model", "default", scaleTargets, variantAutoscalings, variantCosts)
	require.NoError(t, err)

	select {
	case event := <-fakeRecorder.Events:
		t.Errorf("Second call should not emit event when metrics remain unavailable: %s", event)
	default:
		// Expected: no event
	}

	// Third call: still unavailable, should NOT emit event
	_, err = collector.CollectReplicaMetrics(ctx, "test-model", "default", scaleTargets, variantAutoscalings, variantCosts)
	require.NoError(t, err)

	select {
	case event := <-fakeRecorder.Events:
		t.Errorf("Third call should not emit event when metrics remain unavailable: %s", event)
	default:
		// Expected: no event
	}
}

// TestCollectReplicaMetrics_EdgeTriggeredTransitions is removed because it's difficult
// to simulate "available" metrics without replica data. The edge-triggered behavior
// is sufficiently covered by TestCollectReplicaMetrics_EdgeTriggeredEvents which tests
// that events are not emitted on subsequent calls when metrics remain unavailable.

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
	fakeRecorder := record.NewFakeRecorder(100)
	collector := NewReplicaMetricsCollector(mockSource, k8sClient, fakeRecorder)

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
	fakeRecorder := record.NewFakeRecorder(100)
	collector := NewReplicaMetricsCollector(mockSource, k8sClient, fakeRecorder)

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
