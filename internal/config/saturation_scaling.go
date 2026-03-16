package config

import "fmt"

// DefaultPriority is the default model priority multiplier.
// Higher priority → preferential GPU allocation in fair-share.
const DefaultPriority = 1.0

// SaturationScalingConfig holds saturation-based scaling thresholds for a model variant.
// Saturation scaling is enabled by default and uses these thresholds to determine when
// replicas are saturated and when to scale up.
type SaturationScalingConfig struct {
	// ModelID is the model identifier (only used in override entries)
	ModelID string `yaml:"model_id,omitempty"`

	// Namespace is the namespace for this override (only used in override entries)
	Namespace string `yaml:"namespace,omitempty"`

	// KvCacheThreshold: Replica is saturated if KV cache utilization >= this threshold (0.0-1.0)
	KvCacheThreshold float64 `yaml:"kvCacheThreshold"`

	// QueueLengthThreshold: Replica is saturated if queue length >= this threshold
	QueueLengthThreshold float64 `yaml:"queueLengthThreshold"`

	// KvSpareTrigger: Scale-up if average spare KV cache capacity < this value (0.0-1.0)
	KvSpareTrigger float64 `yaml:"kvSpareTrigger"`

	// QueueSpareTrigger: Scale-up if average spare queue capacity < this value
	QueueSpareTrigger float64 `yaml:"queueSpareTrigger"`

	// EnableLimiter: When true, includes the GPU limiter in the scaling pipeline
	// to constrain scaling decisions based on available cluster resources.
	// Default is false (limiter disabled).
	EnableLimiter bool `yaml:"enableLimiter,omitempty"`

	// AnalyzerName selects which saturation analyzer to use.
	// "saturation" uses the V2 token-based analyzer.
	// Empty string (default) uses the V1 percentage-based analyzer.
	// To use the queueing model analyzer, deploy wva-queueing-model-config instead.
	AnalyzerName string `yaml:"analyzerName,omitempty"`

	// ScaleUpThreshold is the utilization threshold above which scale-up is triggered.
	// Used by V2 analyzer: requiredCapacity = totalDemand / ScaleUpThreshold - anticipatedSupply
	// Default: 0.85 (85% utilization triggers scale-up)
	ScaleUpThreshold float64 `yaml:"scaleUpThreshold,omitempty"`

	// ScaleDownBoundary is the utilization boundary below which scale-down is safe.
	// Used by V2 analyzer: spareCapacity = currentSupply - totalDemand / ScaleDownBoundary
	// Default: 0.70 (70% utilization allows scale-down)
	ScaleDownBoundary float64 `yaml:"scaleDownBoundary,omitempty"`

	// Priority is a multiplier for this model's scaling urgency.
	// Higher priority → preferential GPU allocation in fair-share.
	// Default: 1.0 (neutral).
	Priority float64 `yaml:"priority,omitempty"`

	// Analyzers configures the set of analyzers and their weights.
	// When empty and AnalyzerName is "saturation", defaults to
	// [{Name: "saturation", Score: 1.0, Enabled: true}].
	Analyzers []AnalyzerScoreConfig `yaml:"analyzers,omitempty"`
}

// AnalyzerScoreConfig configures an individual analyzer's weight in the
// composite scoring function. Per-analyzer threshold overrides are optional;
// when nil, the global top-level thresholds are used.
type AnalyzerScoreConfig struct {
	Name              string   `yaml:"name"`
	Enabled           *bool    `yaml:"enabled,omitempty"`           // default true
	Score             float64  `yaml:"score,omitempty"`             // default 1.0
	ScaleUpThreshold  *float64 `yaml:"scaleUpThreshold,omitempty"` // overrides global
	ScaleDownBoundary *float64 `yaml:"scaleDownBoundary,omitempty"` // overrides global
}

// EffectiveScaleUpThreshold returns the per-analyzer threshold if set,
// otherwise falls back to the global value.
func (a *AnalyzerScoreConfig) EffectiveScaleUpThreshold(global float64) float64 {
	if a.ScaleUpThreshold != nil {
		return *a.ScaleUpThreshold
	}
	return global
}

// EffectiveScaleDownBoundary returns the per-analyzer boundary if set,
// otherwise falls back to the global value.
func (a *AnalyzerScoreConfig) EffectiveScaleDownBoundary(global float64) float64 {
	if a.ScaleDownBoundary != nil {
		return *a.ScaleDownBoundary
	}
	return global
}

// GetAnalyzerName implements the AnalyzerConfig interface.
// Returns "saturation" if Analyzers list is populated (new-style config),
// otherwise returns the raw AnalyzerName field (backward compat).
func (c *SaturationScalingConfig) GetAnalyzerName() string {
	if len(c.Analyzers) > 0 {
		return "saturation"
	}
	return c.AnalyzerName
}

// IsV2 returns true if this config selects the V2 token-based analyzer path.
// V2 is active when either the Analyzers list is populated (new-style) or
// AnalyzerName is "saturation" (old-style, backward compat).
func (c *SaturationScalingConfig) IsV2() bool {
	return len(c.Analyzers) > 0 || c.AnalyzerName == "saturation"
}

// V2 analyzer default thresholds, applied when fields are omitted from YAML config.
const (
	DefaultScaleUpThreshold  = 0.85
	DefaultScaleDownBoundary = 0.70
)

// ApplyDefaults fills in zero-valued V2 fields with their defaults.
// Must be called before Validate() to handle omitempty zero-values correctly.
func (c *SaturationScalingConfig) ApplyDefaults() {
	if c.Priority == 0 {
		c.Priority = DefaultPriority
	}
	if c.IsV2() {
		if c.ScaleUpThreshold == 0 {
			c.ScaleUpThreshold = DefaultScaleUpThreshold
		}
		if c.ScaleDownBoundary == 0 {
			c.ScaleDownBoundary = DefaultScaleDownBoundary
		}
		// Default analyzers list when empty (backward compat for analyzerName: "saturation")
		if len(c.Analyzers) == 0 {
			enabled := true
			c.Analyzers = []AnalyzerScoreConfig{
				{Name: "saturation", Score: 1.0, Enabled: &enabled},
			}
		}
		// Apply per-entry defaults
		for i := range c.Analyzers {
			if c.Analyzers[i].Score == 0 {
				c.Analyzers[i].Score = 1.0
			}
			if c.Analyzers[i].Enabled == nil {
				enabled := true
				c.Analyzers[i].Enabled = &enabled
			}
		}
	}
}

// Validate checks for invalid threshold values.
// Returns error with descriptive message if validation fails.
// Call ApplyDefaults() before Validate() to handle zero-valued omitempty fields.
func (c *SaturationScalingConfig) Validate() error {
	if c.KvCacheThreshold < 0 || c.KvCacheThreshold > 1 {
		return fmt.Errorf("kvCacheThreshold must be between 0 and 1, got %.2f", c.KvCacheThreshold)
	}
	if c.QueueLengthThreshold < 0 {
		return fmt.Errorf("queueLengthThreshold must be >= 0, got %.1f", c.QueueLengthThreshold)
	}
	if c.KvSpareTrigger < 0 || c.KvSpareTrigger > 1 {
		return fmt.Errorf("kvSpareTrigger must be between 0 and 1, got %.2f", c.KvSpareTrigger)
	}
	if c.QueueSpareTrigger < 0 {
		return fmt.Errorf("queueSpareTrigger must be >= 0, got %.1f", c.QueueSpareTrigger)
	}
	if c.Priority < 0 {
		return fmt.Errorf("priority must be >= 0, got %.2f", c.Priority)
	}

	// KV cache threshold should be greater than spare trigger (otherwise contradictory)
	if c.KvCacheThreshold < c.KvSpareTrigger {
		return fmt.Errorf("kvCacheThreshold (%.2f) should be >= kvSpareTrigger (%.2f)",
			c.KvCacheThreshold, c.KvSpareTrigger)
	}

	// V2 analyzer threshold validation (global defaults)
	if c.IsV2() {
		if c.ScaleUpThreshold <= 0 || c.ScaleUpThreshold > 1 {
			return fmt.Errorf("scaleUpThreshold must be in (0, 1], got %.2f", c.ScaleUpThreshold)
		}
		if c.ScaleDownBoundary <= 0 || c.ScaleDownBoundary > 1 {
			return fmt.Errorf("scaleDownBoundary must be in (0, 1], got %.2f", c.ScaleDownBoundary)
		}
		if c.ScaleUpThreshold <= c.ScaleDownBoundary {
			return fmt.Errorf("scaleUpThreshold (%.2f) must be > scaleDownBoundary (%.2f)", c.ScaleUpThreshold, c.ScaleDownBoundary)
		}
		// Per-analyzer threshold overrides
		for _, a := range c.Analyzers {
			if a.ScaleUpThreshold != nil {
				if *a.ScaleUpThreshold <= 0 || *a.ScaleUpThreshold > 1 {
					return fmt.Errorf("analyzer %q: scaleUpThreshold must be in (0, 1], got %.2f", a.Name, *a.ScaleUpThreshold)
				}
			}
			if a.ScaleDownBoundary != nil {
				if *a.ScaleDownBoundary <= 0 || *a.ScaleDownBoundary > 1 {
					return fmt.Errorf("analyzer %q: scaleDownBoundary must be in (0, 1], got %.2f", a.Name, *a.ScaleDownBoundary)
				}
			}
			up := a.EffectiveScaleUpThreshold(c.ScaleUpThreshold)
			down := a.EffectiveScaleDownBoundary(c.ScaleDownBoundary)
			if up <= down {
				return fmt.Errorf("analyzer %q: scaleUpThreshold (%.2f) must be > scaleDownBoundary (%.2f)", a.Name, up, down)
			}
		}
	}

	return nil
}
