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

	corev1 "k8s.io/api/core/v1"

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
			collector := NewReplicaMetricsCollector(mockSource, nil, fakeRecorder, nil)

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

			collector.recordMetricsUnavailableEvent(variantAutoscalings, nil, "Test metrics unavailable")

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
	// This test verifies edge-triggered event emission for metrics collection errors.
	// Note: Without actual pod data in the k8s client, replicaMetrics is always empty,
	// so we can't test the full "available → error" transition. This test focuses on
	// verifying that repeated errors don't flood the event stream.

	ctx := context.Background()
	fakeRecorder := record.NewFakeRecorder(100)

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

	// Simulate metrics collection failure
	mockSource := &mockMetricsSource{
		refreshError: errors.New("prometheus connection failed"),
	}
	collector := NewReplicaMetricsCollector(mockSource, nil, fakeRecorder, nil)

	// First call with error: no event (first observation, unknown previous state)
	metrics, err := collector.CollectReplicaMetrics(ctx, "test-model", "default", scaleTargets, variantAutoscalings, nil, variantCosts)
	require.Error(t, err, "Should return error when refresh fails")
	require.Nil(t, metrics, "Should return nil metrics on error")

	select {
	case event := <-fakeRecorder.Events:
		t.Errorf("No event expected on first observation: %s", event)
	default:
		// Expected: no event
	}

	// Second call: metrics still fail, should NOT emit event (no state transition)
	_, err = collector.CollectReplicaMetrics(ctx, "test-model", "default", scaleTargets, variantAutoscalings, nil, variantCosts)
	require.Error(t, err, "Should still return error")

	select {
	case event := <-fakeRecorder.Events:
		t.Errorf("No event expected when metrics remain unavailable: %s", event)
	default:
		// Expected: no event
	}
}

func TestCollectReplicaMetrics_NoMetricsRecordsEvent(t *testing.T) {
	// This test verifies edge-triggered event emission when no metrics are available.
	// Simulates a VA scaled to zero (no pods = no metrics) to verify that repeated
	// "no metrics" states don't flood the event stream.

	ctx := context.Background()
	fakeRecorder := record.NewFakeRecorder(100)

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

	// Mock source with no metrics (e.g., VA scaled to zero)
	mockSource := &mockMetricsSource{
		results: make(map[string]*source.MetricResult),
	}
	collector := NewReplicaMetricsCollector(mockSource, nil, fakeRecorder, nil)

	// First call: no metrics, should NOT emit event (first observation, unknown previous state)
	metrics, err := collector.CollectReplicaMetrics(ctx, "test-model", "default", scaleTargets, variantAutoscalings, nil, variantCosts)
	require.NoError(t, err, "Should not return error when no metrics available")
	require.Empty(t, metrics, "Should return empty metrics slice")

	select {
	case event := <-fakeRecorder.Events:
		t.Errorf("No event expected on first observation: %s", event)
	default:
		// Expected: no event
	}

	// Second call: still no metrics, should NOT emit event (no state transition)
	_, err = collector.CollectReplicaMetrics(ctx, "test-model", "default", scaleTargets, variantAutoscalings, nil, variantCosts)
	require.NoError(t, err, "Should not return error")

	select {
	case event := <-fakeRecorder.Events:
		t.Errorf("No event expected when metrics remain unavailable: %s", event)
	default:
		// Expected: no event
	}

	// Third call: still no metrics, should NOT emit event (no state transition)
	_, err = collector.CollectReplicaMetrics(ctx, "test-model", "default", scaleTargets, variantAutoscalings, nil, variantCosts)
	require.NoError(t, err, "Should not return error")

	select {
	case event := <-fakeRecorder.Events:
		t.Errorf("No event expected when metrics remain unavailable: %s", event)
	default:
		// Expected: no event
	}
}

func TestK8SEventMetricsUnavailableConstant(t *testing.T) {
	// Verify the constant is correctly defined
	assert.Equal(t, "MetricsUnavailable", constants.K8SEventMetricsUnavailable,
		"K8SEventMetricsUnavailable constant should match expected value")
}

func TestCollectReplicaMetrics_EdgeTriggeredEvents(t *testing.T) {
	// This test verifies the core edge-triggered behavior: events are emitted only on
	// state transitions, not on every cycle with unavailable metrics. This prevents
	// event flooding when a VA is legitimately scaled to zero.

	ctx := context.Background()
	fakeRecorder := record.NewFakeRecorder(100)

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

	// Mock source that starts with no metrics (simulates VA scaled to zero)
	mockSource := &mockMetricsSource{
		results: make(map[string]*source.MetricResult),
	}
	collector := NewReplicaMetricsCollector(mockSource, nil, fakeRecorder, nil)

	// First call: metrics unavailable, should NOT emit event (first observation, unknown previous state)
	_, err := collector.CollectReplicaMetrics(ctx, "test-model", "default", scaleTargets, variantAutoscalings, nil, variantCosts)
	require.NoError(t, err)

	select {
	case event := <-fakeRecorder.Events:
		t.Errorf("First call should not emit event (unknown previous state): %s", event)
	default:
		// Expected: no event - prevents false positive for VAs that start at zero
	}

	// Second call: metrics still unavailable, should NOT emit event (no state transition)
	_, err = collector.CollectReplicaMetrics(ctx, "test-model", "default", scaleTargets, variantAutoscalings, nil, variantCosts)
	require.NoError(t, err)

	select {
	case event := <-fakeRecorder.Events:
		t.Errorf("Second call should not emit event when metrics remain unavailable: %s", event)
	default:
		// Expected: no event - prevents flooding on every optimization cycle
	}

	// Third call: still unavailable, should NOT emit event
	_, err = collector.CollectReplicaMetrics(ctx, "test-model", "default", scaleTargets, variantAutoscalings, nil, variantCosts)
	require.NoError(t, err)

	select {
	case event := <-fakeRecorder.Events:
		t.Errorf("Third call should not emit event when metrics remain unavailable: %s", event)
	default:
		// Expected: no event
	}
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
	fakeRecorder := record.NewFakeRecorder(100)
	collector := NewReplicaMetricsCollector(mockSource, k8sClient, fakeRecorder, nil)

	// Call the function
	_, err = collector.CollectReplicaMetrics(
		context.Background(),
		"test-model",
		"test-namespace",
		make(map[string]scaletarget.ScaleTargetAccessor),
		make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
		nil,
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
	collector := NewReplicaMetricsCollector(mockSource, k8sClient, fakeRecorder, nil)

	// Call the function - should return error
	_, err = collector.CollectReplicaMetrics(
		context.Background(),
		"test-model",
		"test-namespace",
		make(map[string]scaletarget.ScaleTargetAccessor),
		make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
		nil,
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

// TestCollectReplicaMetrics_ThroughputKeyMerge verifies that when the KV-cache
// query and the throughput queries (GenerationTokenRate, KvUsageInstant,
// VLLMRequestRate) return results for the same pod, they merge into a single
// ReplicaMetrics entry with all fields non-zero.
//
// Before the Bug A fix, throughput loops used the bare pod name as the podData
// key while all other loops used buildInstanceKey's composite key (pod:port).
// The entries never merged and the throughput fields were always zero.
func TestCollectReplicaMetrics_ThroughputKeyMerge(t *testing.T) {
	registry := prometheus.NewRegistry()
	if err := metrics.InitMetrics(registry); err != nil {
		t.Fatalf("InitMetrics: %v", err)
	}

	scheme := runtime.NewScheme()
	if err := llmdVariantAutoscalingV1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	podLabels := map[string]string{
		"pod":                               "pod-abc",
		"instance":                          "10.0.0.1:8000",
		constants.VariantLabelPrometheusKey: "va-1",
	}
	ts := time.Now()

	mockSource := &mockMetricsSource{
		refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return map[string]*source.MetricResult{
				"kv_cache_usage": {
					Values: []source.MetricValue{
						{Labels: podLabels, Value: 0.55, Timestamp: ts},
					},
				},
				"generation_token_rate": {
					Values: []source.MetricValue{
						{Labels: podLabels, Value: 1500.0, Timestamp: ts},
					},
				},
				"kv_usage_instant": {
					Values: []source.MetricValue{
						{Labels: podLabels, Value: 0.50, Timestamp: ts},
					},
				},
				"vllm_request_rate": {
					Values: []source.MetricValue{
						{Labels: podLabels, Value: 7.0, Timestamp: ts},
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

	if len(results) != 1 {
		t.Fatalf("expected exactly 1 ReplicaMetrics entry (key merge), got %d", len(results))
	}

	m := results[0]
	if m.GenerationTokenRate == 0 {
		t.Errorf("GenerationTokenRate is zero — throughput key merge failed")
	}
	if m.KvUsageInstant == 0 {
		t.Errorf("KvUsageInstant is zero — throughput key merge failed")
	}
	if m.VLLMRequestRate == 0 {
		t.Errorf("VLLMRequestRate is zero — throughput key merge failed")
	}
	if m.KvCacheUsage == 0 {
		t.Errorf("KvCacheUsage is zero — KV cache result not merged")
	}
}

// mockScaleTargetAccessor implements scaletarget.ScaleTargetAccessor for testing.
// Only GetStatusReadyReplicas is meaningful; all other methods return zero/nil.
type mockScaleTargetAccessor struct {
	readyReplicas int32
}

func (m *mockScaleTargetAccessor) GetName() string                                   { return "" }
func (m *mockScaleTargetAccessor) GetNamespace() string                              { return "" }
func (m *mockScaleTargetAccessor) GetReplicas() *int32                               { return nil }
func (m *mockScaleTargetAccessor) GetDeletionTimestamp() *metav1.Time                { return nil }
func (m *mockScaleTargetAccessor) GetStatusReplicas() int32                          { return 0 }
func (m *mockScaleTargetAccessor) GetStatusReadyReplicas() int32                     { return m.readyReplicas }
func (m *mockScaleTargetAccessor) GetTotalGPUsPerReplica() int                       { return 0 }
func (m *mockScaleTargetAccessor) GetLeaderPodTemplateSpec() *corev1.PodTemplateSpec { return nil }
func (m *mockScaleTargetAccessor) GetWorkerPodTemplateSpec() *corev1.PodTemplateSpec { return nil }
func (m *mockScaleTargetAccessor) GetGroupSize() int32                               { return 1 }

// TestCollectReplicaMetrics_UnattributedReadyPodsEvent verifies that when a VA
// has Ready pods but none are attributed this cycle, a Warning/UnattributedReadyPods
// K8s event is emitted exactly once (deduped via vaEventTracker on second call).
func TestCollectReplicaMetrics_UnattributedReadyPodsEvent(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))

	scheme := runtime.NewScheme()
	require.NoError(t, llmdVariantAutoscalingV1alpha1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	fakeRecorder := record.NewFakeRecorder(10)

	// One pod attributed to "va-other", not to "va-target".
	podLabels := map[string]string{
		"pod":                               "pod-other",
		"instance":                          "10.0.0.2:8000",
		constants.VariantLabelPrometheusKey: "va-other",
	}
	ts := time.Now()
	mockSource := &mockMetricsSource{
		refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return map[string]*source.MetricResult{
				"kv_cache_usage": {
					Values: []source.MetricValue{{Labels: podLabels, Value: 0.5, Timestamp: ts}},
				},
			}, nil
		},
	}

	va := &llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		ObjectMeta: metav1.ObjectMeta{Name: "va-target", Namespace: "default"},
		Spec: llmdVariantAutoscalingV1alpha1.VariantAutoscalingSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "dep-target"},
			ModelID:        "test-model",
			MaxReplicas:    5,
		},
	}
	variantAutoscalings := map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling{
		"default/va-target": va,
	}
	scaleTargets := map[string]scaletarget.ScaleTargetAccessor{
		"default/dep-target": &mockScaleTargetAccessor{readyReplicas: 2},
	}
	variantCosts := make(map[string]float64)

	collector := NewReplicaMetricsCollector(mockSource, k8sClient, fakeRecorder, nil)

	// First call: metrics present for a different VA → va-target has 0 attributed but 2 ready.
	vaEventTracker := make(map[string]bool)
	results, err := collector.CollectReplicaMetrics(
		context.Background(), "test-model", "default",
		scaleTargets, variantAutoscalings, vaEventTracker, variantCosts,
	)
	require.NoError(t, err)
	assert.NotEmpty(t, results, "expected attributed results for va-other")

	select {
	case event := <-fakeRecorder.Events:
		assert.Contains(t, event, constants.K8SEventUnattributedReadyPods)
		assert.Contains(t, event, "va-target")
	default:
		t.Error("expected UnattributedReadyPods event but none received")
	}

	// Second call with same vaEventTracker: event must NOT be re-emitted (deduped).
	_, err = collector.CollectReplicaMetrics(
		context.Background(), "test-model", "default",
		scaleTargets, variantAutoscalings, vaEventTracker, variantCosts,
	)
	require.NoError(t, err)
	select {
	case event := <-fakeRecorder.Events:
		t.Errorf("unexpected duplicate event: %s", event)
	default:
		// Expected: no second event
	}
}

// TestCollectReplicaMetrics_ThroughputOrphanSkipped verifies that a throughput
// query result for an instance that has no KV-cache entry (scrape skew or
// throughput-only pod) does not create an orphan podData entry and does not
// appear in the assembled ReplicaMetrics slice.
func TestCollectReplicaMetrics_ThroughputOrphanSkipped(t *testing.T) {
	registry := prometheus.NewRegistry()
	require.NoError(t, metrics.InitMetrics(registry))

	scheme := runtime.NewScheme()
	require.NoError(t, llmdVariantAutoscalingV1alpha1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	// KV-cache: only pod-known at 10.0.0.1:8000.
	kvLabels := map[string]string{
		"pod":                               "pod-known",
		"instance":                          "10.0.0.1:8000",
		constants.VariantLabelPrometheusKey: "va-1",
	}
	// Throughput: pod-orphan at 10.0.0.2:8000 — NOT in the KV query results.
	orphanLabels := map[string]string{
		"pod":                               "pod-orphan",
		"instance":                          "10.0.0.2:8000",
		constants.VariantLabelPrometheusKey: "va-1",
	}
	ts := time.Now()

	mockSource := &mockMetricsSource{
		refreshFunc: func(_ context.Context, _ source.RefreshSpec) (map[string]*source.MetricResult, error) {
			return map[string]*source.MetricResult{
				"kv_cache_usage": {
					Values: []source.MetricValue{{Labels: kvLabels, Value: 0.5, Timestamp: ts}},
				},
				"generation_token_rate": {
					Values: []source.MetricValue{{Labels: orphanLabels, Value: 1000.0, Timestamp: ts}},
				},
			}, nil
		},
	}

	collector := NewReplicaMetricsCollector(mockSource, k8sClient, nil, nil)
	results, err := collector.CollectReplicaMetrics(
		context.Background(), "test-model", "test-ns",
		make(map[string]scaletarget.ScaleTargetAccessor),
		make(map[string]*llmdVariantAutoscalingV1alpha1.VariantAutoscaling),
		nil,
		make(map[string]float64),
	)
	require.NoError(t, err)

	// Only pod-known should be present; pod-orphan must be skipped.
	require.Len(t, results, 1, "orphan throughput-only pod must not produce a ReplicaMetrics entry")
	assert.Equal(t, "pod-known", results[0].PodName)
	assert.Equal(t, float64(0), results[0].GenerationTokenRate, "orphan entry must not contaminate pod-known")
}
