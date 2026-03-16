package saturation

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/config"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

func init() {
	// Initialize logger for tests
	logging.NewTestLogger()
}

func TestAnalyzeModelSaturation_ScaleUp(t *testing.T) {
	analyzer := NewAnalyzer()
	config := config.SaturationScalingConfig{
		KvCacheThreshold:     0.80,
		QueueLengthThreshold: 5,
		KvSpareTrigger:       0.10,
		QueueSpareTrigger:    3,
	}

	tests := []struct {
		name                string
		replicaMetrics      []interfaces.ReplicaMetrics
		expectScaleUp       bool
		expectScaleUpReason string
	}{
		{
			name: "scale up due to low KV spare Saturation",
			replicaMetrics: []interfaces.ReplicaMetrics{
				{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0.75, QueueLength: 2},
				{PodName: "pod-2", VariantName: "v1", KvCacheUsage: 0.76, QueueLength: 2},
			},
			expectScaleUp: true, // avg spare KV = 0.045 < 0.1
		},
		{
			name: "scale up due to low queue spare Saturation",
			replicaMetrics: []interfaces.ReplicaMetrics{
				{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0.50, QueueLength: 3},
				{PodName: "pod-2", VariantName: "v1", KvCacheUsage: 0.50, QueueLength: 3},
			},
			expectScaleUp: true, // avg spare queue = 2 < 3
		},
		{
			name: "no scale up - healthy Saturation",
			replicaMetrics: []interfaces.ReplicaMetrics{
				{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0.50, QueueLength: 1},
				{PodName: "pod-2", VariantName: "v1", KvCacheUsage: 0.50, QueueLength: 1},
			},
			expectScaleUp: false, // avg spare KV = 0.30, avg spare queue = 4
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			analysis, err := analyzer.AnalyzeModelSaturation(
				context.Background(),
				"test-model",
				"test-ns",
				tt.replicaMetrics,
				config,
			)

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if analysis.ShouldScaleUp != tt.expectScaleUp {
				t.Errorf("expected ShouldScaleUp=%v, got %v (reason: %s)",
					tt.expectScaleUp, analysis.ShouldScaleUp, analysis.ScaleUpReason)
			}
		})
	}
}

func TestAnalyzeModelSaturation_ScaleDownSafety(t *testing.T) {
	analyzer := NewAnalyzer()
	config := config.SaturationScalingConfig{
		KvCacheThreshold:     0.80,
		QueueLengthThreshold: 5,
		KvSpareTrigger:       0.10,
		QueueSpareTrigger:    3,
	}

	tests := []struct {
		name                string
		replicaMetrics      []interfaces.ReplicaMetrics
		expectScaleDownSafe bool
	}{
		{
			name: "scale down safe - adequate headroom",
			replicaMetrics: []interfaces.ReplicaMetrics{
				{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0.20, QueueLength: 1},
				{PodName: "pod-2", VariantName: "v1", KvCacheUsage: 0.30, QueueLength: 1},
				{PodName: "pod-3", VariantName: "v1", KvCacheUsage: 0.25, QueueLength: 1},
			},
			expectScaleDownSafe: true,
		},
		{
			name: "scale down unsafe - insufficient headroom",
			replicaMetrics: []interfaces.ReplicaMetrics{
				{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0.70, QueueLength: 2},
				{PodName: "pod-2", VariantName: "v1", KvCacheUsage: 0.75, QueueLength: 2},
			},
			expectScaleDownSafe: false,
		},
		{
			name: "scale down unsafe - only one non-saturated replica",
			replicaMetrics: []interfaces.ReplicaMetrics{
				{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0.50, QueueLength: 2},
			},
			expectScaleDownSafe: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			analysis, err := analyzer.AnalyzeModelSaturation(
				context.Background(),
				"test-model",
				"test-ns",
				tt.replicaMetrics,
				config,
			)

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if analysis.ScaleDownSafe != tt.expectScaleDownSafe {
				t.Errorf("expected ScaleDownSafe=%v, got %v",
					tt.expectScaleDownSafe, analysis.ScaleDownSafe)
			}
		})
	}
}

func TestAnalyzeModelSaturation_MultiVariant(t *testing.T) {
	analyzer := NewAnalyzer()
	config := config.SaturationScalingConfig{
		KvCacheThreshold:     0.80,
		QueueLengthThreshold: 5,
		KvSpareTrigger:       0.10,
		QueueSpareTrigger:    3,
	}

	// Test with metrics from multiple variants
	replicaMetrics := []interfaces.ReplicaMetrics{
		// Variant 1
		{PodName: "v1-pod-1", VariantName: "variant-1", ModelID: "model-a", KvCacheUsage: 0.70, QueueLength: 2},
		{PodName: "v1-pod-2", VariantName: "variant-1", ModelID: "model-a", KvCacheUsage: 0.75, QueueLength: 3},
		// Variant 2
		{PodName: "v2-pod-1", VariantName: "variant-2", ModelID: "model-a", KvCacheUsage: 0.60, QueueLength: 1},
		{PodName: "v2-pod-2", VariantName: "variant-2", ModelID: "model-a", KvCacheUsage: 0.65, QueueLength: 2},
	}

	analysis, err := analyzer.AnalyzeModelSaturation(
		context.Background(),
		"model-a",
		"test-ns",
		replicaMetrics,
		config,
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify aggregation across variants
	if analysis.TotalReplicas != 4 {
		t.Errorf("expected TotalReplicas=4, got %d", analysis.TotalReplicas)
	}

	if analysis.NonSaturatedCount != 4 {
		t.Errorf("expected NonSaturatedCount=4, got %d", analysis.NonSaturatedCount)
	}

	if len(analysis.VariantAnalyses) != 2 {
		t.Errorf("expected 2 variant analyses, got %d", len(analysis.VariantAnalyses))
	}

	// Verify per-variant breakdown
	for _, va := range analysis.VariantAnalyses {
		if va.ReplicaCount != 2 {
			t.Errorf("expected ReplicaCount=2 for variant %s, got %d", va.VariantName, va.ReplicaCount)
		}
	}
}

func TestAnalyzeModelSaturation_EmptyMetrics(t *testing.T) {
	analyzer := NewAnalyzer()
	config := config.SaturationScalingConfig{
		KvCacheThreshold:     0.80,
		QueueLengthThreshold: 5,
		KvSpareTrigger:       0.10,
		QueueSpareTrigger:    3,
	}

	analysis, err := analyzer.AnalyzeModelSaturation(
		context.Background(),
		"test-model",
		"test-ns",
		[]interfaces.ReplicaMetrics{},
		config,
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if analysis.TotalReplicas != 0 {
		t.Errorf("expected TotalReplicas=0, got %d", analysis.TotalReplicas)
	}

	if analysis.ShouldScaleUp {
		t.Errorf("expected ShouldScaleUp=false for empty metrics")
	}

	if analysis.ScaleDownSafe {
		t.Errorf("expected ScaleDownSafe=false for empty metrics")
	}
}

func TestAnalyzeVariant_SaturatedReplicas(t *testing.T) {
	analyzer := &Analyzer{}
	config := config.SaturationScalingConfig{
		KvCacheThreshold:     0.80,
		QueueLengthThreshold: 5,
		KvSpareTrigger:       0.10,
		QueueSpareTrigger:    3,
	}

	metrics := []interfaces.ReplicaMetrics{
		{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0.85, QueueLength: 2}, // Saturated (KV)
		{PodName: "pod-2", VariantName: "v1", KvCacheUsage: 0.50, QueueLength: 6}, // Saturated (Queue)
		{PodName: "pod-3", VariantName: "v1", KvCacheUsage: 0.60, QueueLength: 2}, // Not saturated
	}

	analysis := analyzer.analyzeVariant(context.Background(), "v1", metrics, config)

	if analysis.ReplicaCount != 3 {
		t.Errorf("expected ReplicaCount=3, got %d", analysis.ReplicaCount)
	}

	if analysis.NonSaturatedCount != 1 {
		t.Errorf("expected NonSaturatedCount=1, got %d", analysis.NonSaturatedCount)
	}

	if len(analysis.SaturatedReplicas) != 2 {
		t.Errorf("expected 2 saturated replicas, got %d", len(analysis.SaturatedReplicas))
	}

	// Verify saturated pods are tracked
	saturatedSet := make(map[string]bool)
	for _, pod := range analysis.SaturatedReplicas {
		saturatedSet[pod] = true
	}

	if !saturatedSet["pod-1"] || !saturatedSet["pod-2"] {
		t.Errorf("expected pod-1 and pod-2 to be saturated, got: %v", analysis.SaturatedReplicas)
	}
}

func TestAnalyzeModelSaturation_AllSaturated(t *testing.T) {
	analyzer := NewAnalyzer()
	config := config.SaturationScalingConfig{
		KvCacheThreshold:     0.80,
		QueueLengthThreshold: 5,
		KvSpareTrigger:       0.10,
		QueueSpareTrigger:    3,
	}

	// All replicas are saturated
	replicaMetrics := []interfaces.ReplicaMetrics{
		{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0.85, QueueLength: 2}, // Saturated (KV)
		{PodName: "pod-2", VariantName: "v1", KvCacheUsage: 0.50, QueueLength: 6}, // Saturated (Queue)
		{PodName: "pod-3", VariantName: "v1", KvCacheUsage: 0.90, QueueLength: 7}, // Saturated (both)
	}

	analysis, err := analyzer.AnalyzeModelSaturation(
		context.Background(),
		"test-model",
		"test-ns",
		replicaMetrics,
		config,
	)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// When all replicas are saturated
	if analysis.TotalReplicas != 3 {
		t.Errorf("expected TotalReplicas=3, got %d", analysis.TotalReplicas)
	}

	if analysis.NonSaturatedCount != 0 {
		t.Errorf("expected NonSaturatedCount=0, got %d", analysis.NonSaturatedCount)
	}

	// With no non-saturated replicas, average spare Saturation should be 0
	if analysis.AvgSpareKvCapacity != 0 {
		t.Errorf("expected AvgSpareKvSaturation=0, got %.3f", analysis.AvgSpareKvCapacity)
	}

	if analysis.AvgSpareQueueLength != 0 {
		t.Errorf("expected AvgSpareQueueLength=0, got %.1f", analysis.AvgSpareQueueLength)
	}

	// Should scale up when all replicas are saturated (0 spare Saturation < triggers)
	if !analysis.ShouldScaleUp {
		t.Errorf("expected ShouldScaleUp=true when all saturated (urgently needs more Saturation)")
	}

	// Scale-down should be unsafe
	if analysis.ScaleDownSafe {
		t.Errorf("expected ScaleDownSafe=false when all saturated")
	}
}

func TestAnalyzeModelSaturation_TimestampSet(t *testing.T) {
	analyzer := NewAnalyzer()
	config := config.SaturationScalingConfig{
		KvCacheThreshold:     0.80,
		QueueLengthThreshold: 5,
		KvSpareTrigger:       0.10,
		QueueSpareTrigger:    3,
	}

	before := time.Now()

	replicaMetrics := []interfaces.ReplicaMetrics{
		{PodName: "pod-1", VariantName: "v1", KvCacheUsage: 0.50, QueueLength: 2, Cost: 10},
	}

	analysis, err := analyzer.AnalyzeModelSaturation(
		context.Background(),
		"test-model",
		"test-ns",
		replicaMetrics,
		config,
	)

	after := time.Now()

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify timestamp is set and within reasonable range
	if analysis.AnalyzedAt.IsZero() {
		t.Errorf("expected AnalyzedAt to be set, but it's zero")
	}

	if analysis.AnalyzedAt.Before(before) || analysis.AnalyzedAt.After(after) {
		t.Errorf("AnalyzedAt timestamp %v is outside expected range [%v, %v]",
			analysis.AnalyzedAt, before, after)
	}
}

// Tests for two-step decision logic (CalculatesaturationTargets + ArbitrateWithModelBased)

func TestCalculatesaturationTargets_ScaleUpCheapest(t *testing.T) {
	analyzer := NewAnalyzer()

	saturationAnalysis := &interfaces.ModelSaturationAnalysis{
		ModelID:       "test-model",
		Namespace:     "test-ns",
		ShouldScaleUp: true,
		ScaleUpReason: "KV spare Saturation low",
		VariantAnalyses: []interfaces.VariantSaturationAnalysis{
			{VariantName: "v1-expensive", Cost: 20, ReplicaCount: 2},
			{VariantName: "v2-cheap", Cost: 5, ReplicaCount: 2},
			{VariantName: "v3-medium", Cost: 15, ReplicaCount: 2},
		},
	}

	variantStates := []interfaces.VariantReplicaState{
		{VariantName: "v1-expensive", CurrentReplicas: 2, DesiredReplicas: 0},
		{VariantName: "v2-cheap", CurrentReplicas: 2, DesiredReplicas: 0},
		{VariantName: "v3-medium", CurrentReplicas: 2, DesiredReplicas: 0},
	}

	targets := analyzer.CalculateSaturationTargets(context.Background(), saturationAnalysis, variantStates)

	// Should scale up cheapest variant (v2-cheap)
	if targets["v2-cheap"] != 3 {
		t.Errorf("expected v2-cheap target=3, got %d", targets["v2-cheap"])
	}

	// Others should remain at current
	if targets["v1-expensive"] != 2 {
		t.Errorf("expected v1-expensive target=2, got %d", targets["v1-expensive"])
	}
	if targets["v3-medium"] != 2 {
		t.Errorf("expected v3-medium target=2, got %d", targets["v3-medium"])
	}
}

func TestCalculatesaturationTargets_ScaleDownMostExpensive(t *testing.T) {
	analyzer := NewAnalyzer()

	saturationAnalysis := &interfaces.ModelSaturationAnalysis{
		ModelID:       "test-model",
		Namespace:     "test-ns",
		ShouldScaleUp: false,
		ScaleDownSafe: true,
		VariantAnalyses: []interfaces.VariantSaturationAnalysis{
			{VariantName: "v1-expensive", Cost: 20, ReplicaCount: 2},
			{VariantName: "v2-cheap", Cost: 5, ReplicaCount: 2},
			{VariantName: "v3-medium", Cost: 15, ReplicaCount: 2},
		},
	}

	variantStates := []interfaces.VariantReplicaState{
		{VariantName: "v1-expensive", CurrentReplicas: 2, DesiredReplicas: 0},
		{VariantName: "v2-cheap", CurrentReplicas: 2, DesiredReplicas: 0},
		{VariantName: "v3-medium", CurrentReplicas: 2, DesiredReplicas: 0},
	}

	targets := analyzer.CalculateSaturationTargets(context.Background(), saturationAnalysis, variantStates)

	// Should scale down most expensive variant (v1-expensive)
	if targets["v1-expensive"] != 1 {
		t.Errorf("expected v1-expensive target=1, got %d", targets["v1-expensive"])
	}

	// Others should remain at current
	if targets["v2-cheap"] != 2 {
		t.Errorf("expected v2-cheap target=2, got %d", targets["v2-cheap"])
	}
	if targets["v3-medium"] != 2 {
		t.Errorf("expected v3-medium target=2, got %d", targets["v3-medium"])
	}
}

func TestCalculatesaturationTargets_ModelLevelTransitionBlocking(t *testing.T) {
	analyzer := NewAnalyzer()

	saturationAnalysis := &interfaces.ModelSaturationAnalysis{
		ModelID:       "test-model",
		Namespace:     "test-ns",
		ShouldScaleUp: true,
		ScaleUpReason: "KV spare Saturation low",
		VariantAnalyses: []interfaces.VariantSaturationAnalysis{
			{VariantName: "v1-expensive", Cost: 20, ReplicaCount: 2},
			{VariantName: "v2-cheap", Cost: 5, ReplicaCount: 2},
		},
	}

	// v1 has desired > current (previous optimizer wanted to scale up)
	// This puts the MODEL in transition state, blocking all scaling decisions
	variantStates := []interfaces.VariantReplicaState{
		{VariantName: "v1-expensive", CurrentReplicas: 2, DesiredReplicas: 4},
		{VariantName: "v2-cheap", CurrentReplicas: 2, DesiredReplicas: 0},
	}

	targets := analyzer.CalculateSaturationTargets(context.Background(), saturationAnalysis, variantStates)

	// v1 should preserve its desired replicas (transition in progress)
	if targets["v1-expensive"] != 4 {
		t.Errorf("expected v1-expensive target=4 (preserved desired), got %d", targets["v1-expensive"])
	}

	// v2 should NOT be scaled up because model is in transition (v1 is transitioning)
	// Model-level transition protection blocks all scaling decisions
	if targets["v2-cheap"] != 2 {
		t.Errorf("expected v2-cheap target=2 (blocked by model transition), got %d", targets["v2-cheap"])
	}
}

func TestCalculatesaturationTargets_MetricsMismatchBlocksScaling(t *testing.T) {
	analyzer := NewAnalyzer()

	saturationAnalysis := &interfaces.ModelSaturationAnalysis{
		ModelID:       "test-model",
		Namespace:     "test-ns",
		ShouldScaleUp: true,
		ScaleUpReason: "KV spare Saturation low",
		VariantAnalyses: []interfaces.VariantSaturationAnalysis{
			// v1 has 3 replicas but only 2 are reporting metrics
			{VariantName: "v1-expensive", Cost: 20, ReplicaCount: 2},
			{VariantName: "v2-cheap", Cost: 5, ReplicaCount: 2},
		},
	}

	// v1 has metrics(2) != current(3) - some pods not reporting yet
	// This puts the MODEL in transition state
	variantStates := []interfaces.VariantReplicaState{
		{VariantName: "v1-expensive", CurrentReplicas: 3, DesiredReplicas: 0},
		{VariantName: "v2-cheap", CurrentReplicas: 2, DesiredReplicas: 0},
	}

	targets := analyzer.CalculateSaturationTargets(context.Background(), saturationAnalysis, variantStates)

	// v1 should stay at current replicas (metrics incomplete)
	if targets["v1-expensive"] != 3 {
		t.Errorf("expected v1-expensive target=3 (current, metrics incomplete), got %d", targets["v1-expensive"])
	}

	// v2 should NOT be scaled up because model is in transition (v1 has incomplete metrics)
	if targets["v2-cheap"] != 2 {
		t.Errorf("expected v2-cheap target=2 (blocked by model transition), got %d", targets["v2-cheap"])
	}
}
