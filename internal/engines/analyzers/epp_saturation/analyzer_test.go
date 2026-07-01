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
	"math"
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
	saturation float64 // returned for every query unless ttftVal/tpotVal are set
	ttftVal    *float64
	tpotVal    *float64
	nan        bool // when true, every query returns NaN (simulates no recent traffic)
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
		val := f.saturation
		switch {
		case f.nan:
			val = math.NaN()
		case q == registration.QueryEPPPredictedTTFT && f.ttftVal != nil:
			val = *f.ttftVal
		case q == registration.QueryEPPPredictedTPOT && f.tpotVal != nil:
			val = *f.tpotVal
		}
		results[q] = &source.MetricResult{
			QueryName:   q,
			CollectedAt: time.Now(),
			Values: []source.MetricValue{
				{Value: val, Timestamp: time.Now()},
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
		SmoothingAlpha:    1.0, // no smoothing — use raw signal directly
		TTFTSLOMs:         1000,
		TPOTSLOMs:         10000,
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
		SmoothingAlpha:    1.0, // no smoothing — use raw signal directly
		TTFTSLOMs:         1000,
		TPOTSLOMs:         10000,
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
		SmoothingAlpha:    1.0, // no smoothing — use raw signal directly
		TTFTSLOMs:         1000,
		TPOTSLOMs:         10000,
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
		SmoothingAlpha:    1.0, // no smoothing — use raw signal directly
		TTFTSLOMs:         1000,
		TPOTSLOMs:         10000,
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
		SmoothingAlpha:    1.0, // no smoothing — use raw signal directly
		TTFTSLOMs:         1000,
		TPOTSLOMs:         10000,
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

func TestAnalyze_SaturationCap_ClampsRawBeforeEMA(t *testing.T) {
	// Raw saturation of 5.0 (TTFT 5s / 1s SLO) is deep in the knee. With a cap of
	// 2.0 the value feeding the EMA/decision is clamped to 2.0, but the true
	// uncapped signal is still surfaced in RawSignal for observability.
	input := func() interfaces.AnalyzerInput {
		return interfaces.AnalyzerInput{
			ModelID:   "test-model",
			Namespace: "default",
			VariantStates: []interfaces.VariantReplicaState{
				{VariantName: "variant-a", CurrentReplicas: 4, PendingReplicas: 0},
			},
		}
	}

	// With cap: demand is clamped to 2.0, raw signal preserved at 5.0.
	analyzer := setupAnalyzer(5.0)
	in := input()
	in.Config = &EPPSaturationConfig{
		ScaleUpThreshold:  0.85,
		ScaleDownBoundary: 0.50,
		SmoothingAlpha:    1.0, // no smoothing — isolate the clamp
		TTFTSLOMs:         1000,
		TPOTSLOMs:         10000,
		SaturationCap:     2.0,
	}
	result, err := analyzer.Analyze(context.Background(), in)
	require.NoError(t, err)
	assert.InDelta(t, 5.0, result.RawSignal, 0.01, "uncapped raw preserved for observability")
	assert.InDelta(t, 2.0, result.SmoothedSignal, 0.01, "EMA fed the clamped value")
	assert.InDelta(t, 2.0, result.TotalDemand, 0.01, "demand clamped to the cap")
	// required = 2.0/0.85 - 1.0 ≈ 1.353 (vs 4.88 uncapped)
	assert.InDelta(t, 1.353, result.RequiredCapacity, 0.01)

	// Without cap (0 disables clamping): demand is the full raw 5.0.
	analyzerNoCap := setupAnalyzer(5.0)
	inNoCap := input()
	inNoCap.Config = &EPPSaturationConfig{
		ScaleUpThreshold:  0.85,
		ScaleDownBoundary: 0.50,
		SmoothingAlpha:    1.0,
		TTFTSLOMs:         1000,
		TPOTSLOMs:         10000,
		SaturationCap:     0, // disabled
	}
	resultNoCap, err := analyzerNoCap.Analyze(context.Background(), inNoCap)
	require.NoError(t, err)
	assert.InDelta(t, 5.0, resultNoCap.TotalDemand, 0.01, "no clamp when cap disabled")
}

func TestAnalyze_InHysteresisZone_NoAction(t *testing.T) {
	analyzer := setupAnalyzer(0.65) // between 0.50 (down) and 0.85 (up)
	cfg := &EPPSaturationConfig{
		ScaleUpThreshold:  0.85,
		ScaleDownBoundary: 0.50,
		SmoothingAlpha:    1.0, // no smoothing — use raw signal directly
		TTFTSLOMs:         1000,
		TPOTSLOMs:         10000,
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
		SmoothingAlpha:    1.0, // no smoothing — use raw signal directly
		TTFTSLOMs:         1000,
		TPOTSLOMs:         10000,
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
	// In-flight credit: readyReplicas = 4 - 2 = 2, readyFraction = 2/4 = 0.5, so
	// the demand driving scaling is the anticipated post-warmup saturation:
	// 0.95 * 0.5 = 0.475 (the 2 warming pods are credited, so scale-up only asks
	// for the deficit beyond what is already coming online).
	assert.InDelta(t, 0.475, result.TotalDemand, 0.01)

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
	assert.Equal(t, DefaultEPPSmoothingAlpha, cfg.SmoothingAlpha)
	assert.Equal(t, DefaultEPPSaturationCap, cfg.SaturationCap)
}

func TestConfig_Validate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     EPPSaturationConfig
		wantErr bool
	}{
		{"valid", EPPSaturationConfig{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.50, SmoothingAlpha: 0.3, TTFTSLOMs: 3000, TPOTSLOMs: 100}, false},
		{"up <= down", EPPSaturationConfig{ScaleUpThreshold: 0.50, ScaleDownBoundary: 0.85, SmoothingAlpha: 0.3}, true},
		{"up zero", EPPSaturationConfig{ScaleUpThreshold: 0, ScaleDownBoundary: 0.50, SmoothingAlpha: 0.3}, true},
		{"down zero", EPPSaturationConfig{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0, SmoothingAlpha: 0.3}, true},
		{"alpha zero", EPPSaturationConfig{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.50, SmoothingAlpha: 0}, true},
		{"alpha too high", EPPSaturationConfig{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.50, SmoothingAlpha: 1.5}, true},
		{"alpha one ok", EPPSaturationConfig{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.50, SmoothingAlpha: 1.0, TTFTSLOMs: 3000, TPOTSLOMs: 100}, false},
		{"cap ok", EPPSaturationConfig{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.50, SmoothingAlpha: 0.3, TTFTSLOMs: 3000, TPOTSLOMs: 100, SaturationCap: 2.0}, false},
		{"cap below scaleUpThreshold", EPPSaturationConfig{ScaleUpThreshold: 0.85, ScaleDownBoundary: 0.50, SmoothingAlpha: 0.3, TTFTSLOMs: 3000, TPOTSLOMs: 100, SaturationCap: 0.5}, true},
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

// setupAnalyzerMutable returns the analyzer plus a pointer to the fake
// source's saturation value so tests can change it between calls.
func setupAnalyzerMutable(initial float64) (*EPPSaturationAnalyzer, *fakePrometheusSource) {
	registry := source.NewSourceRegistry()
	fakeProm := newFakePrometheusSource(initial, nil)
	registry.MustRegister("prometheus", fakeProm)
	registration.RegisterEPPSaturationQueries(registry)
	return NewEPPSaturationAnalyzer(registry), fakeProm
}

func TestAnalyze_EMASmoothing(t *testing.T) {
	analyzer, src := setupAnalyzerMutable(0.5)
	cfg := &EPPSaturationConfig{
		ScaleUpThreshold:  0.85,
		ScaleDownBoundary: 0.50,
		SmoothingAlpha:    0.3,
		TTFTSLOMs:         1000,
		TPOTSLOMs:         10000,
	}
	input := interfaces.AnalyzerInput{
		ModelID:   "test-model",
		Namespace: "default",
		Config:    cfg,
		VariantStates: []interfaces.VariantReplicaState{
			{VariantName: "variant-a", CurrentReplicas: 10, PendingReplicas: 0},
		},
	}

	// First sample: no smoothing — use raw value as-is.
	result, err := analyzer.Analyze(context.Background(), input)
	require.NoError(t, err)
	assert.InDelta(t, 0.5, result.Utilization, 0.001, "first sample should be raw")

	// Second sample: spike to 1.0. EMA(0.3): 0.3*1.0 + 0.7*0.5 = 0.65
	src.saturation = 1.0
	result, err = analyzer.Analyze(context.Background(), input)
	require.NoError(t, err)
	assert.InDelta(t, 0.65, result.Utilization, 0.001, "after one spike, smoothed should be 0.65")

	// Third sample: still 1.0. EMA: 0.3*1.0 + 0.7*0.65 = 0.755
	result, err = analyzer.Analyze(context.Background(), input)
	require.NoError(t, err)
	assert.InDelta(t, 0.755, result.Utilization, 0.001)

	// Fourth sample: dip to 0.1. EMA: 0.3*0.1 + 0.7*0.755 = 0.5585
	src.saturation = 0.1
	result, err = analyzer.Analyze(context.Background(), input)
	require.NoError(t, err)
	assert.InDelta(t, 0.5585, result.Utilization, 0.001, "single dip should not push smoothed below scaleDownBoundary")
	assert.Equal(t, 0.0, result.SpareCapacity, "single dip should not trigger scale-down")
}

func TestAnalyze_NoSmoothing_AlphaOne(t *testing.T) {
	analyzer, src := setupAnalyzerMutable(0.5)
	cfg := &EPPSaturationConfig{
		ScaleUpThreshold:  0.85,
		ScaleDownBoundary: 0.50,
		SmoothingAlpha:    1.0, // no smoothing
		TTFTSLOMs:         1000,
		TPOTSLOMs:         10000,
	}
	input := interfaces.AnalyzerInput{
		ModelID:   "test-model",
		Namespace: "default",
		Config:    cfg,
		VariantStates: []interfaces.VariantReplicaState{
			{VariantName: "variant-a", CurrentReplicas: 10, PendingReplicas: 0},
		},
	}

	// Each sample should be used raw.
	for _, raw := range []float64{0.2, 0.9, 0.1, 1.5} {
		src.saturation = raw
		result, err := analyzer.Analyze(context.Background(), input)
		require.NoError(t, err)
		// Utilization is capped at 1.0 internally, so compare the total demand instead.
		assert.InDelta(t, raw, result.TotalDemand, 0.001, "alpha=1.0 should not smooth (raw=%v)", raw)
	}
}

func float64Ptr(v float64) *float64 { return &v }

// TestAnalyze_DerivesSaturationFromLatency verifies saturation is the max of the
// per-target terms (predictedTTFT/TTFTSLO, predictedTPOT/TPOTSLO).
func TestAnalyze_DerivesSaturationFromLatency(t *testing.T) {
	analyzer, src := setupAnalyzerMutable(0)
	cfg := &EPPSaturationConfig{
		ScaleUpThreshold:  0.85,
		ScaleDownBoundary: 0.50,
		SmoothingAlpha:    1.0, // no smoothing
		TTFTSLOMs:         3000,
		TPOTSLOMs:         100,
	}
	input := interfaces.AnalyzerInput{
		ModelID:   "test-model",
		Namespace: "default",
		Config:    cfg,
		VariantStates: []interfaces.VariantReplicaState{
			{VariantName: "variant-a", CurrentReplicas: 10},
		},
	}

	// TTFT binds: ttft=4.5s/3s = 1.5 ; tpot=0.05s/0.1s = 0.5 ; max = 1.5
	src.ttftVal = float64Ptr(4.5)
	src.tpotVal = float64Ptr(0.05)
	result, err := analyzer.Analyze(context.Background(), input)
	require.NoError(t, err)
	assert.InDelta(t, 1.5, result.TotalDemand, 0.001, "TTFT term should bind (1.5)")

	// TPOT binds: ttft=1.5s/3s = 0.5 ; tpot=0.08s/0.1s = 0.8 ; max = 0.8
	src.ttftVal = float64Ptr(1.5)
	src.tpotVal = float64Ptr(0.08)
	result, err = analyzer.Analyze(context.Background(), input)
	require.NoError(t, err)
	assert.InDelta(t, 0.8, result.TotalDemand, 0.001, "TPOT term should bind (0.8)")
}

// TestAnalyze_NoTrafficIsZeroSaturation verifies a NaN latency (no recent
// traffic) is treated as zero saturation, so an idle pool reports spare capacity.
func TestAnalyze_NoTrafficIsZeroSaturation(t *testing.T) {
	analyzer, src := setupAnalyzerMutable(0)
	src.nan = true
	cfg := &EPPSaturationConfig{
		ScaleUpThreshold:  0.85,
		ScaleDownBoundary: 0.50,
		SmoothingAlpha:    1.0,
		TTFTSLOMs:         3000,
		TPOTSLOMs:         100,
	}
	input := interfaces.AnalyzerInput{
		ModelID:   "idle-model",
		Namespace: "default",
		Config:    cfg,
		VariantStates: []interfaces.VariantReplicaState{
			{VariantName: "variant-a", CurrentReplicas: 10},
		},
	}
	result, err := analyzer.Analyze(context.Background(), input)
	require.NoError(t, err)
	assert.Equal(t, 0.0, result.TotalDemand, "no traffic => zero saturation")
	assert.Greater(t, result.SpareCapacity, 0.0, "idle pool should report spare capacity (scale down)")
}
