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

package epp_saturation

import (
	"context"
	"testing"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
)

// fakePrometheusSource is a test double for source.MetricsSource that returns
// a pre-configured saturation value.
type fakePrometheusSource struct {
	queryList  *source.QueryList
	saturation float64
	err        error
}

func newFakePrometheusSource(saturation float64, err error) *fakePrometheusSource {
	return &fakePrometheusSource{
		queryList:  source.NewQueryList(),
		saturation: saturation,
		err:        err,
	}
}

func (f *fakePrometheusSource) QueryList() *source.QueryList {
	return f.queryList
}

func (f *fakePrometheusSource) Refresh(_ context.Context, spec source.RefreshSpec) (map[string]*source.MetricResult, error) {
	if f.err != nil {
		return nil, f.err
	}
	results := make(map[string]*source.MetricResult)
	for _, q := range spec.Queries {
		results[q] = &source.MetricResult{
			QueryName:   q,
			CollectedAt: time.Now(),
			Values: []source.MetricValue{
				{Value: f.saturation, Timestamp: time.Now()},
			},
		}
	}
	return results, nil
}

func (f *fakePrometheusSource) Get(queryName string, params map[string]string) *source.CachedValue {
	return nil
}

func setupAnalyzer(saturation float64) *EPPSaturationAnalyzer {
	registry := source.NewSourceRegistry()
	fakeProm := newFakePrometheusSource(saturation, nil)
	registry.MustRegister("prometheus", fakeProm)
	registration.RegisterEPPSaturationQueries(registry)
	return NewEPPSaturationAnalyzer(registry)
}

func TestAnalyze_LowSaturation_ScaleDown(t *testing.T) {
	analyzer := setupAnalyzer(0.3)
	cfg := &EPPSaturationConfig{
		ScaleUpThreshold:  0.85,
		ScaleDownBoundary: 0.50,
	}

	input := interfaces.AnalyzerInput{
		ModelID:   "test-model",
		Namespace: "default",
		Config:    cfg,
		VariantStates: []interfaces.VariantReplicaState{
			{VariantName: "variant-a", CurrentReplicas: 4, PendingReplicas: 0},
		},
	}

	result, err := analyzer.Analyze(context.Background(), input)
	require.NoError(t, err)

	assert.Equal(t, AnalyzerName, result.AnalyzerName)
	assert.Equal(t, "test-model", result.ModelID)
	// Normalized model: supply=1.0, demand=saturation
	assert.InDelta(t, 1.0, result.TotalSupply, 0.01)
	assert.InDelta(t, 0.3, result.TotalDemand, 0.01)
	assert.InDelta(t, 0.3, result.Utilization, 0.01)
	assert.Equal(t, 0.0, result.RequiredCapacity, "should not need scale-up")
	// spare = 1.0 - 0.3/0.50 = 0.4
	assert.InDelta(t, 0.4, result.SpareCapacity, 0.01, "should have spare capacity for scale-down")
	// Optimizer: floor(0.4 / 0.25) = 1 replica removed (proportional to pool size)
}

func TestAnalyze_HighSaturation_ScaleUp(t *testing.T) {
	analyzer := setupAnalyzer(0.95)
	cfg := &EPPSaturationConfig{
		ScaleUpThreshold:  0.85,
		ScaleDownBoundary: 0.50,
	}

	input := interfaces.AnalyzerInput{
		ModelID:   "test-model",
		Namespace: "default",
		Config:    cfg,
		VariantStates: []interfaces.VariantReplicaState{
			{VariantName: "variant-a", CurrentReplicas: 4, PendingReplicas: 0},
		},
	}

	result, err := analyzer.Analyze(context.Background(), input)
	require.NoError(t, err)

	assert.InDelta(t, 1.0, result.TotalSupply, 0.01)
	assert.InDelta(t, 0.95, result.TotalDemand, 0.01)
	// required = 0.95/0.85 - 1.0 ≈ 0.118
	assert.InDelta(t, 0.118, result.RequiredCapacity, 0.01, "should need scale-up")
	assert.Equal(t, 0.0, result.SpareCapacity, "should not have spare capacity")
	// Optimizer: ceil(0.118 / 0.25) = 1 replica added
}

func TestAnalyze_HighSaturation_LargePool(t *testing.T) {
	// Verify proportional scaling: same saturation, more replicas → more added
	analyzer := setupAnalyzer(0.95)
	cfg := &EPPSaturationConfig{
		ScaleUpThreshold:  0.85,
		ScaleDownBoundary: 0.50,
	}

	input := interfaces.AnalyzerInput{
		ModelID:   "test-model",
		Namespace: "default",
		Config:    cfg,
		VariantStates: []interfaces.VariantReplicaState{
			{VariantName: "variant-a", CurrentReplicas: 40, PendingReplicas: 0},
		},
	}

	result, err := analyzer.Analyze(context.Background(), input)
	require.NoError(t, err)

	// required = 0.95/0.85 - 1.0 ≈ 0.118
	assert.InDelta(t, 0.118, result.RequiredCapacity, 0.01)
	// perReplicaCapacity = 1/40 = 0.025
	assert.InDelta(t, 0.025, result.VariantCapacities[0].PerReplicaCapacity, 0.001)
	// Optimizer: ceil(0.118 / 0.025) = 5 replicas added (vs 1 for N=4)
}

func TestAnalyze_LowSaturation_LargePool(t *testing.T) {
	analyzer := setupAnalyzer(0.3)
	cfg := &EPPSaturationConfig{
		ScaleUpThreshold:  0.85,
		ScaleDownBoundary: 0.50,
	}

	input := interfaces.AnalyzerInput{
		ModelID:   "test-model",
		Namespace: "default",
		Config:    cfg,
		VariantStates: []interfaces.VariantReplicaState{
			{VariantName: "variant-a", CurrentReplicas: 40, PendingReplicas: 0},
		},
	}

	result, err := analyzer.Analyze(context.Background(), input)
	require.NoError(t, err)

	// spare = 1.0 - 0.3/0.50 = 0.4
	assert.InDelta(t, 0.4, result.SpareCapacity, 0.01)
	// perReplicaCapacity = 1/40 = 0.025
	// Optimizer: floor(0.4 / 0.025) = 16 replicas removed → 24 left → saturation 0.50
}

func TestAnalyze_Overloaded(t *testing.T) {
	analyzer := setupAnalyzer(1.5)
	cfg := &EPPSaturationConfig{
		ScaleUpThreshold:  0.85,
		ScaleDownBoundary: 0.50,
	}

	input := interfaces.AnalyzerInput{
		ModelID:   "test-model",
		Namespace: "default",
		Config:    cfg,
		VariantStates: []interfaces.VariantReplicaState{
			{VariantName: "variant-a", CurrentReplicas: 2, PendingReplicas: 0},
		},
	}

	result, err := analyzer.Analyze(context.Background(), input)
	require.NoError(t, err)

	assert.InDelta(t, 1.0, result.TotalSupply, 0.01)
	assert.InDelta(t, 1.5, result.TotalDemand, 0.01)
	assert.InDelta(t, 1.0, result.Utilization, 0.01) // capped at 1.0
	// required = 1.5/0.85 - 1.0 ≈ 0.765
	assert.InDelta(t, 0.765, result.RequiredCapacity, 0.01, "should need scale-up when overloaded")
	// Optimizer: ceil(0.765 / 0.5) = 2 replicas added (N=2, perReplica=0.5)
}

func TestAnalyze_InHysteresisZone_NoAction(t *testing.T) {
	analyzer := setupAnalyzer(0.65) // between 0.50 (down) and 0.85 (up)
	cfg := &EPPSaturationConfig{
		ScaleUpThreshold:  0.85,
		ScaleDownBoundary: 0.50,
	}

	input := interfaces.AnalyzerInput{
		ModelID:   "test-model",
		Namespace: "default",
		Config:    cfg,
		VariantStates: []interfaces.VariantReplicaState{
			{VariantName: "variant-a", CurrentReplicas: 4, PendingReplicas: 0},
		},
	}

	result, err := analyzer.Analyze(context.Background(), input)
	require.NoError(t, err)

	// 0.65/0.85 = 0.765 < 1.0 → no scale-up
	assert.Equal(t, 0.0, result.RequiredCapacity, "should not need scale-up in hysteresis zone")
	// 1.0 - 0.65/0.50 = 1.0 - 1.3 < 0 → no scale-down
	assert.Equal(t, 0.0, result.SpareCapacity, "should not have spare capacity in hysteresis zone")
}

func TestAnalyze_PendingReplicas(t *testing.T) {
	analyzer := setupAnalyzer(0.95)
	cfg := &EPPSaturationConfig{
		ScaleUpThreshold:  0.85,
		ScaleDownBoundary: 0.50,
	}

	input := interfaces.AnalyzerInput{
		ModelID:   "test-model",
		Namespace: "default",
		Config:    cfg,
		VariantStates: []interfaces.VariantReplicaState{
			{VariantName: "variant-a", CurrentReplicas: 4, PendingReplicas: 2},
		},
	}

	result, err := analyzer.Analyze(context.Background(), input)
	require.NoError(t, err)

	// totalReplicas = 4 (current, including pending), normalized supply = 1.0
	assert.InDelta(t, 1.0, result.TotalSupply, 0.01)
	assert.InDelta(t, 0.95, result.TotalDemand, 0.01)

	// Per-variant: readyCount = 4 - 2 = 2, perReplicaCapacity = 1/4 = 0.25
	// variant capacity = 2 * 0.25 = 0.5
	require.Len(t, result.VariantCapacities, 1)
	assert.Equal(t, 2, result.VariantCapacities[0].ReplicaCount)
	assert.InDelta(t, 0.5, result.VariantCapacities[0].TotalCapacity, 0.01)
	assert.InDelta(t, 0.25, result.VariantCapacities[0].PerReplicaCapacity, 0.01)
}

func TestAnalyze_WrongConfigType(t *testing.T) {
	analyzer := setupAnalyzer(0.5)
	input := interfaces.AnalyzerInput{
		ModelID:   "test-model",
		Namespace: "default",
		Config:    nil, // wrong type
	}

	_, err := analyzer.Analyze(context.Background(), input)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "expected *EPPSaturationConfig")
}

func TestConfig_Defaults(t *testing.T) {
	cfg := &EPPSaturationConfig{}
	cfg.ApplyDefaults()
	assert.Equal(t, DefaultEPPScaleUpThreshold, cfg.ScaleUpThreshold)
	assert.Equal(t, DefaultEPPScaleDownBoundary, cfg.ScaleDownBoundary)
}

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     EPPSaturationConfig
		wantErr bool
	}{
		{"valid", EPPSaturationConfig{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.50}, false},
		{"up <= down", EPPSaturationConfig{ScaleUpThreshold: 0.50, ScaleDownBoundary: 0.85}, true},
		{"up zero", EPPSaturationConfig{ScaleUpThreshold: 0, ScaleDownBoundary: 0.50}, true},
		{"down zero", EPPSaturationConfig{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
