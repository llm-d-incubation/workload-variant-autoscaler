package epp_saturation

import "fmt"

const (
	// DefaultEPPScaleUpThreshold is the saturation level above which scale-up
	// is triggered. Set below the queueing knee on purpose: the P90 signal's
	// healthy band sits around 0.2–0.45 of the SLO (latency barely moves with
	// load under continuous batching, then goes vertical), so a threshold near
	// 1.0 only fires after the SLO budget is nearly burned. 0.55 fires on the
	// P90 pre-rise at the knee's base — benchmarked at 95.8% SLO attainment vs
	// 84.7% for a 0.85 threshold (docs/developer-guide/epp-saturation-benchmark.md).
	// Workloads whose SLO is close to their base latency (healthy band shifted
	// upward) should raise this.
	DefaultEPPScaleUpThreshold = 0.55

	// DefaultEPPScaleDownBoundary is the saturation level below which scale-down
	// is safe. 0.40 sits just below the P90 signal's healthy-load band
	// (~0.35–0.45), so pools serving real traffic retain their replicas through
	// short lulls while idle pools (signal ≈ 0.1) still scale down.
	DefaultEPPScaleDownBoundary = 0.40

	// DefaultEPPSmoothingAlpha is the EMA smoothing factor applied to the raw
	// saturation signal. Range (0.0, 1.0]: 1.0 = no smoothing, lower = heavier
	// smoothing. 0.6 lets the smoothed signal reach the (clamped) raw level in
	// 1–2 cycles so the scale-up ask is sized promptly; spike protection is the
	// clamp's job (SaturationCap), not the EMA's.
	DefaultEPPSmoothingAlpha = 0.6

	// DefaultEPPTTFTSLOMs is the default time-to-first-token SLO (milliseconds)
	// the analyzer divides predicted TTFT by to derive saturation.
	DefaultEPPTTFTSLOMs = 3000.0

	// DefaultEPPTPOTSLOMs is the default time-per-output-token SLO (milliseconds)
	// the analyzer divides predicted TPOT by to derive saturation.
	DefaultEPPTPOTSLOMs = 100.0

	// DefaultEPPSaturationCap bounds the raw saturation signal before it feeds the
	// EMA. Near the queueing knee, predicted latency (and thus saturation) can spike
	// to tens or hundreds × SLO; any value above the cap already means "scale up at
	// the max per-cycle rate", so the extra magnitude carries no additional
	// actionable information and only poisons the EMA (a single spike then decays
	// slowly, holding replicas high long after the pool recovers). Clamping the raw
	// signal keeps the EMA peak bounded so it recovers in a few cycles regardless of
	// spike size. 2.0 = "at most 2× SLO worth of demand pressure per cycle".
	DefaultEPPSaturationCap = 2.0
)

// EPPSaturationConfig holds configuration for the EPP saturation analyzer.
// It implements interfaces.AnalyzerConfig.
type EPPSaturationConfig struct {
	// ScaleUpThreshold is the saturation score above which scale-up is triggered.
	// Value range: (0.0, 2.0+]. Since saturation = P90 predictedLatency/SLO, the
	// threshold should sit at the top of the signal's healthy band (measured
	// ~0.2–0.45 when the SLO is generously above base latency) so it fires on
	// the pre-knee rise; values near 1.0 fire only after the SLO budget is
	// nearly consumed. Default 0.55.
	ScaleUpThreshold float64 `yaml:"scaleUpThreshold,omitempty"`

	// ScaleDownBoundary is the saturation score below which scale-down is safe.
	// Must be less than ScaleUpThreshold to create a hysteresis band. Placed
	// just below the healthy-load band so serving pools hold replicas through
	// lulls while idle pools shed. Default 0.40.
	ScaleDownBoundary float64 `yaml:"scaleDownBoundary,omitempty"`

	// SmoothingAlpha is the EMA smoothing factor applied to the (clamped) raw
	// saturation signal before it drives scaling decisions. Range (0.0, 1.0]:
	//   1.0  = no smoothing (use raw signal)
	//   0.6  = light smoothing (default) — reaches the clamped level in 1–2 cycles
	//   0.1  = heavy smoothing
	// The smoothed value evolves as: smoothed = alpha*raw + (1-alpha)*smoothed_prev.
	// Spike suppression is primarily SaturationCap's job; alpha mainly sets how
	// fast the scale-up ask is sized after a load change.
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

	// SaturationCap bounds the raw saturation signal before EMA smoothing. Values
	// above the cap are clamped to it, so a single knee-region spike (which can be
	// tens or hundreds × SLO) cannot dominate the smoothed signal for many cycles.
	// The true uncapped signal is still surfaced for observability (RawSignal).
	// A zero value is replaced by the default (2.0) via ApplyDefaults — it does
	// NOT disable clamping; to effectively disable, set a very large value.
	SaturationCap float64 `yaml:"saturationCap,omitempty"`
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
	if c.SaturationCap == 0 {
		c.SaturationCap = DefaultEPPSaturationCap
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
	// The cap must leave room to cross the scale-up threshold, otherwise clamping
	// would make scale-up impossible.
	if c.SaturationCap > 0 && c.SaturationCap < c.ScaleUpThreshold {
		return fmt.Errorf("saturationCap (%.2f) must be >= scaleUpThreshold (%.2f)",
			c.SaturationCap, c.ScaleUpThreshold)
	}
	return nil
}
