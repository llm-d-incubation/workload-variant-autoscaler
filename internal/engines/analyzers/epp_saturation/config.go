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
	return nil
}
