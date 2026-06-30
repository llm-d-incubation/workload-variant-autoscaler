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

import "fmt"

const (
	// DefaultEPPScaleUpThreshold is the saturation level above which scale-up
	// is triggered. At 0.85 saturation (85% of SLO), the pool is approaching
	// its limit and needs more capacity.
	DefaultEPPScaleUpThreshold = 0.85

	// DefaultEPPScaleDownBoundary is the saturation level below which scale-down
	// is safe. At 0.50 saturation (50% of SLO), there is enough headroom to
	// remove capacity without risking SLO violations.
	DefaultEPPScaleDownBoundary = 0.50

	// DefaultEPPSmoothingAlpha is the EMA smoothing factor applied to the raw
	// saturation signal. Range (0.0, 1.0]: 1.0 = no smoothing, 0.1 = heavy
	// smoothing (only ~10% of a new sample flows into the smoothed value per cycle).
	// Lower values reduce reaction to transient spikes/dips at the cost of
	// slower response to real load changes.
	DefaultEPPSmoothingAlpha = 0.3

	// DefaultEPPTTFTSLOMs is the default time-to-first-token SLO (milliseconds)
	// the analyzer divides predicted TTFT by to derive saturation.
	DefaultEPPTTFTSLOMs = 3000.0

	// DefaultEPPTPOTSLOMs is the default time-per-output-token SLO (milliseconds)
	// the analyzer divides predicted TPOT by to derive saturation.
	DefaultEPPTPOTSLOMs = 100.0
)

// EPPSaturationConfig holds configuration for the EPP saturation analyzer.
// It implements interfaces.AnalyzerConfig.
type EPPSaturationConfig struct {
	// ScaleUpThreshold is the saturation score above which scale-up is triggered.
	// Value range: (0.0, 2.0+]. Typical: 0.85 (scale up at 85% of SLO).
	// Since saturation = predictedLatency/SLO, values > 1.0 mean "only scale
	// when already violating SLO" (aggressive), while < 1.0 means "scale
	// proactively before SLO violation" (conservative).
	ScaleUpThreshold float64 `yaml:"scaleUpThreshold,omitempty"`

	// ScaleDownBoundary is the saturation score below which scale-down is safe.
	// Must be less than ScaleUpThreshold to create a hysteresis band.
	ScaleDownBoundary float64 `yaml:"scaleDownBoundary,omitempty"`

	// SmoothingAlpha is the EMA smoothing factor applied to the raw saturation
	// signal before it drives scaling decisions. Range (0.0, 1.0]:
	//   1.0  = no smoothing (use raw signal)
	//   0.3  = moderate smoothing (default)
	//   0.1  = heavy smoothing
	// The smoothed value evolves as: smoothed = alpha*raw + (1-alpha)*smoothed_prev.
	// At alpha=0.3 with a cycle every 5s, the effective window is ~17s (1/alpha × cycle).
	// Helps absorb signal volatility from the EPP latency detector (which updates
	// every probeInterval based on the input-profile-tracker's percentile samples)
	// so transient single-cycle spikes/dips don't translate directly into replica churn.
	SmoothingAlpha float64 `yaml:"smoothingAlpha,omitempty"`

	// TTFTSLOMs and TPOTSLOMs are the latency SLO targets (milliseconds) used to
	// derive saturation from the EPP's predicted latencies:
	//   saturation = max(predictedTTFT / TTFTSLOMs, predictedTPOT / TPOTSLOMs)
	// The analyzer queries predicted latency (falling back to actual latency when
	// the predicted series is unavailable) and divides by these targets, so the
	// SLO policy lives in WVA rather than depending on the EPP to pre-compute a
	// saturation gauge.
	TTFTSLOMs float64 `yaml:"ttftSLOMs,omitempty"`
	TPOTSLOMs float64 `yaml:"tpotSLOMs,omitempty"`
}

// GetAnalyzerName implements interfaces.AnalyzerConfig.
func (c *EPPSaturationConfig) GetAnalyzerName() string {
	return AnalyzerName
}

// ApplyDefaults fills in zero-valued fields with defaults.
func (c *EPPSaturationConfig) ApplyDefaults() {
	if c.ScaleUpThreshold == 0 {
		c.ScaleUpThreshold = DefaultEPPScaleUpThreshold
	}
	if c.ScaleDownBoundary == 0 {
		c.ScaleDownBoundary = DefaultEPPScaleDownBoundary
	}
	if c.SmoothingAlpha == 0 {
		c.SmoothingAlpha = DefaultEPPSmoothingAlpha
	}
	if c.TTFTSLOMs == 0 {
		c.TTFTSLOMs = DefaultEPPTTFTSLOMs
	}
	if c.TPOTSLOMs == 0 {
		c.TPOTSLOMs = DefaultEPPTPOTSLOMs
	}
}

// Validate checks for invalid threshold values.
func (c *EPPSaturationConfig) Validate() error {
	if c.ScaleUpThreshold <= 0 {
		return fmt.Errorf("scaleUpThreshold must be > 0, got %.2f", c.ScaleUpThreshold)
	}
	if c.ScaleDownBoundary <= 0 {
		return fmt.Errorf("scaleDownBoundary must be > 0, got %.2f", c.ScaleDownBoundary)
	}
	if c.ScaleUpThreshold <= c.ScaleDownBoundary {
		return fmt.Errorf("scaleUpThreshold (%.2f) must be > scaleDownBoundary (%.2f)",
			c.ScaleUpThreshold, c.ScaleDownBoundary)
	}
	if c.SmoothingAlpha <= 0 || c.SmoothingAlpha > 1.0 {
		return fmt.Errorf("smoothingAlpha must be in (0, 1], got %.2f", c.SmoothingAlpha)
	}
	if c.TTFTSLOMs <= 0 {
		return fmt.Errorf("ttftSLOMs must be > 0, got %.2f", c.TTFTSLOMs)
	}
	if c.TPOTSLOMs <= 0 {
		return fmt.Errorf("tpotSLOMs must be > 0, got %.2f", c.TPOTSLOMs)
	}
	return nil
}
