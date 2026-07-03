// Package epp_saturation implements an analyzer that consumes the pool-level
// saturation score emitted by the EPP's latency detector plugin. Unlike the
// V1/V2 saturation analyzers that scrape per-pod vLLM metrics and compute
// capacity internally, this analyzer relies on a pre-computed signal:
//
//	saturation = predictedLatency / SLO  (averaged across endpoints)
//
// The EPP exposes this as inference_extension_latency_detector_pool_saturation.
// A value < 1.0 means the pool has headroom; >= 1.0 means it is at or over SLO.
package epp_saturation

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/registration"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/collector/source"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/saturation"
	ctrl "sigs.k8s.io/controller-runtime"
)

// AnalyzerName is the canonical name used in config and result metadata.
const AnalyzerName = "epp-saturation"

// EPPSaturationAnalyzer implements interfaces.Analyzer using the EPP's
// pool-level saturation signal. It assumes a single model per pool.
type EPPSaturationAnalyzer struct {
	metricsRegistry *source.SourceRegistry

	// mu protects smoothedByKey.
	mu sync.Mutex
	// smoothedByKey holds the per-model EMA state, keyed by "namespace/modelID".
	// A missing key means no prior observation (first sample is used as-is).
	smoothedByKey map[string]float64
}

// NewEPPSaturationAnalyzer creates a new EPP saturation analyzer.
func NewEPPSaturationAnalyzer(registry *source.SourceRegistry) *EPPSaturationAnalyzer {
	return &EPPSaturationAnalyzer{
		metricsRegistry: registry,
		smoothedByKey:   make(map[string]float64),
	}
}

// smooth applies an EMA update to the per-model saturation state and returns
// the new smoothed value. The first sample for a key is used as-is (no warmup
// required). Safe for concurrent use.
func (a *EPPSaturationAnalyzer) smooth(key string, raw, alpha float64) float64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	prev, ok := a.smoothedByKey[key]
	if !ok {
		a.smoothedByKey[key] = raw
		return raw
	}
	next := alpha*raw + (1.0-alpha)*prev
	a.smoothedByKey[key] = next
	return next
}

// Name implements interfaces.Analyzer.
func (a *EPPSaturationAnalyzer) Name() string {
	return AnalyzerName
}

// Analyze implements interfaces.Analyzer.
// It queries the EPP pool saturation metric from Prometheus and translates
// it into capacity signals for the optimizer pipeline.
func (a *EPPSaturationAnalyzer) Analyze(ctx context.Context, input interfaces.AnalyzerInput) (*interfaces.AnalyzerResult, error) {
	logger := ctrl.LoggerFrom(ctx)

	cfg, ok := input.Config.(*EPPSaturationConfig)
	if !ok {
		return nil, fmt.Errorf("expected *EPPSaturationConfig, got %T", input.Config)
	}

	// Derive saturation from the EPP's predicted latencies and the configured SLOs:
	//   saturation = max(predictedTTFT / TTFTSLO, predictedTPOT / TPOTSLO)
	// Each query prefers predicted latency and falls back to actual latency. A
	// missing value (no recent traffic → NaN) is treated as 0 latency, so an idle
	// pool reports low saturation and scales toward minReplicas. A genuine query
	// error is propagated and handled by the engine's safety-net path.
	ttftSeconds, tpotSeconds, err := a.queryLatenciesSeconds(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to query EPP latencies: %w", err)
	}

	ttftSaturation := ttftSeconds / (cfg.TTFTSLOMs / 1000.0)
	tpotSaturation := tpotSeconds / (cfg.TPOTSLOMs / 1000.0)
	rawSaturation := math.Max(ttftSaturation, tpotSaturation)

	// Clamp the raw signal before smoothing. Near the queueing knee the predicted
	// latency (and thus saturation) can spike to tens or hundreds × SLO; anything
	// above the cap already means "scale up at the max per-cycle rate", so the extra
	// magnitude carries no additional actionable information and only poisons the
	// EMA — a single spike then decays slowly, holding replicas high long after the
	// pool has recovered. Clamping keeps the EMA peak bounded so it recovers in a
	// few cycles regardless of spike size. The true uncapped signal is preserved in
	// RawSignal for observability.
	cappedSaturation := rawSaturation
	if cfg.SaturationCap > 0 && cappedSaturation > cfg.SaturationCap {
		cappedSaturation = cfg.SaturationCap
	}

	// Apply EMA smoothing to absorb single-cycle spikes/dips before the signal
	// drives scaling decisions. First observation per (namespace, modelID) is
	// used as-is (no warmup).
	smoothingKey := input.Namespace + "/" + input.ModelID
	saturationScore := a.smooth(smoothingKey, cappedSaturation, cfg.SmoothingAlpha)

	logger.Info("EPP pool saturation score",
		"modelID", input.ModelID,
		"ttftSeconds", ttftSeconds,
		"tpotSeconds", tpotSeconds,
		"ttftSLOMs", cfg.TTFTSLOMs,
		"tpotSLOMs", cfg.TPOTSLOMs,
		"ttftSaturation", ttftSaturation,
		"tpotSaturation", tpotSaturation,
		"rawSaturation", rawSaturation,
		"cappedSaturation", cappedSaturation,
		"saturationCap", cfg.SaturationCap,
		"smoothedSaturation", saturationScore,
		"smoothingAlpha", cfg.SmoothingAlpha,
		"scaleUpThreshold", cfg.ScaleUpThreshold,
		"scaleDownBoundary", cfg.ScaleDownBoundary)

	// Compute current + ready replica counts from variant states.
	var totalReplicas, readyReplicas int
	for _, vs := range input.VariantStates {
		totalReplicas += vs.CurrentReplicas
		if r := vs.CurrentReplicas - vs.PendingReplicas; r > 0 {
			readyReplicas += r
		}
	}
	if totalReplicas == 0 {
		totalReplicas = 1
	}

	// Credit in-flight (warming) replicas. The saturation signal is measured from
	// the Ready pods; while a scale-up is in flight those Ready pods absorb more
	// than their eventual share, so the raw signal over-states the post-warmup
	// load. Once the pending pods become Ready the load spreads across all
	// totalReplicas, so the demand that should drive scaling is the *anticipated*
	// saturation: signal × ready/total. This makes scale-up ask only for the
	// deficit beyond the pods already coming online (instead of racing to
	// maxReplicas while pods warm), and re-converge as pending pods become Ready.
	// readyFraction = 1.0 in steady state (no pending), so behavior is unchanged.
	readyFraction := 1.0
	if readyReplicas > 0 && readyReplicas < totalReplicas {
		readyFraction = float64(readyReplicas) / float64(totalReplicas)
	}
	effectiveDemand := saturationScore * readyFraction
	if readyFraction < 1.0 {
		logger.Info("EPP saturation crediting in-flight replicas",
			"modelID", input.ModelID, "readyReplicas", readyReplicas, "totalReplicas", totalReplicas,
			"readyFraction", readyFraction, "smoothedSaturation", saturationScore, "effectiveDemand", effectiveDemand)
	}

	// Normalized capacity model for proportional scaling.
	//
	// We normalize the pool's capacity to 1.0 (its full SLO budget) so that
	// each replica contributes perReplicaCapacity = 1/N of the total. The
	// saturation score IS the demand in normalized units (demand/capacity ratio).
	//
	// This makes the optimizer compute proportional replica deltas:
	//   replicasNeeded = ceil(requiredCapacity / perReplicaCapacity)
	//                  = ceil(requiredCapacity * N)
	//
	// Examples (scaleUpThreshold=0.85, scaleDownBoundary=0.50):
	//
	//   saturation=0.95, N=4:
	//     required = 0.95/0.85 - 1.0 = 0.118
	//     replicas = ceil(0.118 * 4) = ceil(0.47) = 1  (add 1 of 4)
	//
	//   saturation=0.95, N=40:
	//     required = 0.118
	//     replicas = ceil(0.118 * 40) = ceil(4.7) = 5  (add 5 of 40)
	//
	//   saturation=0.30, N=40:
	//     spare = 1.0 - 0.30/0.50 = 0.40
	//     replicas = floor(0.40 * 40) = 16  (remove 16 of 40 → 24 left, sat→0.50)
	//
	perReplicaCapacity := 1.0 / float64(totalReplicas)
	totalSupply := 1.0             // normalized pool capacity
	totalDemand := effectiveDemand // in-flight-credited saturation drives scaling

	utilization := saturationScore
	if utilization > 1.0 {
		utilization = 1.0 // cap for the 0-1 field; raw score preserved in demand
	}

	// Scaling signals using the same threshold logic as V2:
	// requiredCapacity > 0 → scale-up needed
	// spareCapacity > 0 → scale-down possible
	//
	// Scale-up uses the credited demand (effectiveDemand) so in-flight replicas
	// are not double-provisioned. Scale-down deliberately uses the UNCREDITED
	// smoothed signal: the credit discounts demand by ready/total, so during a
	// warmup it could push a signal that is above the scale-up threshold below the
	// scale-down boundary and shed replicas mid-scale-up (worse with pods stuck
	// Pending, where the discount persists indefinitely). A pool may only scale
	// down when the measured signal itself has real headroom.
	requiredCapacity := math.Max(0, totalDemand/cfg.ScaleUpThreshold-totalSupply)
	spareCapacity := math.Max(0, totalSupply-saturationScore/cfg.ScaleDownBoundary)

	// Build per-variant capacity breakdown.
	// Single model per pool: distribute proportionally across variants.
	variantCapacities := make([]interfaces.VariantCapacity, 0, len(input.VariantStates))
	for _, vs := range input.VariantStates {
		readyCount := vs.CurrentReplicas - vs.PendingReplicas
		if readyCount < 0 {
			readyCount = 0
		}

		variantSupply := float64(readyCount) * perReplicaCapacity
		variantDemand := saturationScore * variantSupply
		vc := interfaces.VariantCapacity{
			VariantName:        vs.VariantName,
			AcceleratorName:    "", // not needed for EPP-based signal
			Cost:               saturation.DefaultVariantCost,
			ReplicaCount:       readyCount,
			PendingReplicas:    vs.PendingReplicas,
			PerReplicaCapacity: perReplicaCapacity,
			TotalCapacity:      variantSupply,
			TotalDemand:        variantDemand,
			Utilization:        saturationScore,
		}
		variantCapacities = append(variantCapacities, vc)
	}

	return &interfaces.AnalyzerResult{
		AnalyzerName:      a.Name(),
		ModelID:           input.ModelID,
		Namespace:         input.Namespace,
		AnalyzedAt:        time.Now(),
		VariantCapacities: variantCapacities,
		TotalSupply:       totalSupply,
		TotalDemand:       totalDemand,
		Utilization:       utilization,
		RequiredCapacity:  requiredCapacity,
		SpareCapacity:     spareCapacity,
		// Observability: preserve the uncapped raw vs smoothed signal so the
		// engine can emit them as metrics (Utilization above is capped at 1.0).
		RawSignal:      rawSaturation,
		SmoothedSignal: saturationScore,
	}, nil
}

// queryLatenciesSeconds fetches the pool TTFT and TPOT (seconds) from Prometheus
// in a single Refresh (one round trip; the source executes the queries
// concurrently). A query that returns no series, or a NaN value (no recent
// traffic → 0/0 rate), is reported as 0 latency rather than an error, so an idle
// pool yields low saturation. Only a transport/query error is returned as an
// error (handled by the engine's safety-net path).
func (a *EPPSaturationAnalyzer) queryLatenciesSeconds(ctx context.Context) (ttft, tpot float64, err error) {
	promSource := a.metricsRegistry.Get("prometheus")
	if promSource == nil {
		return 0, 0, fmt.Errorf("prometheus source not registered")
	}

	results, err := promSource.Refresh(ctx, source.RefreshSpec{
		Queries: []string{registration.QueryEPPPredictedTTFT, registration.QueryEPPPredictedTPOT},
		Params:  map[string]string{},
	})
	if err != nil {
		return 0, 0, fmt.Errorf("prometheus refresh failed: %w", err)
	}

	ttft, err = latencyFromResult(results, registration.QueryEPPPredictedTTFT)
	if err != nil {
		return 0, 0, err
	}
	tpot, err = latencyFromResult(results, registration.QueryEPPPredictedTPOT)
	if err != nil {
		return 0, 0, err
	}
	return ttft, tpot, nil
}

// latencyFromResult extracts one latency value from a Refresh result map,
// applying the empty/NaN/negative → 0 mapping described on queryLatenciesSeconds.
func latencyFromResult(results map[string]*source.MetricResult, queryName string) (float64, error) {
	result, ok := results[queryName]
	if !ok || result == nil {
		return 0, fmt.Errorf("no result for query %s", queryName)
	}
	if result.Error != nil {
		return 0, fmt.Errorf("query %s failed: %w", queryName, result.Error)
	}
	// No series (no recent traffic) → treat as 0 latency.
	if len(result.Values) == 0 {
		return 0, nil
	}
	v := result.Values[0].Value
	// NaN (0/0 rate when idle) or negative → 0 latency.
	if math.IsNaN(v) || v < 0 {
		return 0, nil
	}
	return v, nil
}
