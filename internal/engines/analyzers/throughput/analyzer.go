package throughput

import (
	"context"
	"sync"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// ThroughputAnalyzer accumulates per-variant workload shape and ITL observations
// across reconcile cycles. It implements interfaces.Analyzer; in the current phase
// it produces no scaling signal (RequiredCapacity=0, SpareCapacity=0). The OLS fit,
// μ_dec supply estimation, and λ_dec vs μ_dec scaling signal are added in PR-4.
//
// State is tracked per variant (keyed by "namespace|modelID|variantName") because
// different variants may run on different hardware with different ITL coefficients,
// and all replicas of the same variant are expected to share OL, IL, and KV_max.
type ThroughputAnalyzer struct {
	mu            sync.Mutex
	variantStates map[string]*variantState
}

// variantState holds the cross-cycle calibration state for a single variant.
type variantState struct {
	shapeTracker      *ShapeTracker
	observationWindow *ObservationWindow
	lastSanityReport  SanityReport
	lastObservedAt    time.Time
}

// NewThroughputAnalyzer creates a ThroughputAnalyzer with default configuration.
func NewThroughputAnalyzer() *ThroughputAnalyzer {
	return &ThroughputAnalyzer{
		variantStates: make(map[string]*variantState),
	}
}

// Name returns the canonical name for this analyzer.
func (a *ThroughputAnalyzer) Name() string {
	return AnalyzerName
}

// Observe processes one reconcile cycle for a model. It groups metrics by
// VariantName and, for each variant:
//  1. Runs sanity checks; skips the variant if any issue is found.
//  2. Computes the variant-average IL, OL, and prefix hit rate.
//  3. Updates the shape tracker; clears the observation window on shape change.
//  4. Adds one (k, ITL) observation per replica to the window.
//  5. Prunes observations older than DefaultObservationMaxAge.
//
// Returns a map of variantName → SanityReport for logging. An empty SanityReport
// (report.OK() == true) means that variant's metrics were healthy this cycle.
func (a *ThroughputAnalyzer) Observe(
	ctx context.Context,
	now time.Time,
	modelID, namespace string,
	metrics []interfaces.ReplicaMetrics,
) map[string]SanityReport {
	byVariant := groupByVariant(metrics)
	reports := make(map[string]SanityReport, len(byVariant))

	a.mu.Lock()
	defer a.mu.Unlock()

	for variantName, variantMetrics := range byVariant {
		report := CheckModelMetrics(variantMetrics)
		reports[variantName] = report

		key := variantKey(namespace, modelID, variantName)
		state := a.getOrCreateVariantState(key)
		state.lastSanityReport = report
		state.lastObservedAt = now

		if !report.OK() {
			ctrl.Log.V(logging.DEBUG).Info("throughput analyzer: sanity issues detected, skipping variant",
				"namespace", namespace,
				"modelID", modelID,
				"variant", variantName,
				"issues", report.Issues,
				"affectedPods", report.AffectedPods,
			)
			continue
		}

		// Compute variant-average shape metrics. All replicas of the same variant
		// are expected to have the same OL and IL (same model, same config); the
		// mean handles any minor per-pod variation.
		il, ol, hitRate := averageShapeMetrics(variantMetrics)

		shape, changed := state.shapeTracker.Observe(il, ol, hitRate)
		if changed {
			ctrl.Log.V(logging.DEBUG).Info("throughput analyzer: workload shape changed, clearing observation window",
				"namespace", namespace,
				"modelID", modelID,
				"variant", variantName,
				"newKVreq", shape.KVreq,
			)
			state.observationWindow.Clear()
		}

		// Collect one (k, ITL) observation per replica. Per-replica variation in k
		// provides the k-spread needed for a reliable OLS fit.
		for _, m := range variantMetrics {
			state.observationWindow.Add(m.KvCacheUsage, m.AvgITL, now)
		}
		state.observationWindow.Prune(now)
	}

	// Evict variant states not observed for longer than twice the observation
	// max age. Prevents stale entries from deleted/recreated VAs from
	// accumulating in memory and causing false shape-change signals on recreate.
	for key, state := range a.variantStates {
		if now.Sub(state.lastObservedAt) > 2*DefaultObservationMaxAge {
			delete(a.variantStates, key)
		}
	}

	return reports
}

// Analyze implements interfaces.Analyzer. It calls Observe to update internal
// state and returns an AnalyzerResult with no scaling signal (PR-3 scope).
// The OLS fit and μ_dec vs λ_dec scaling signal are added in PR-4.
func (a *ThroughputAnalyzer) Analyze(
	ctx context.Context,
	input interfaces.AnalyzerInput,
) (*interfaces.AnalyzerResult, error) {
	now := time.Now()
	a.Observe(ctx, now, input.ModelID, input.Namespace, input.ReplicaMetrics)

	return &interfaces.AnalyzerResult{
		AnalyzerName: AnalyzerName,
		ModelID:      input.ModelID,
		Namespace:    input.Namespace,
		AnalyzedAt:   now,
		// RequiredCapacity and SpareCapacity are zero until PR-4 adds the
		// ITL model fit and μ_dec vs λ_dec computation.
	}, nil
}

// VariantState returns a read-only snapshot of the per-variant calibration state.
// Returns (zero ThroughputVariantState, false) if no data has been observed yet
// for the given variant.
func (a *ThroughputAnalyzer) VariantState(modelID, namespace, variantName string) (ThroughputVariantState, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	key := variantKey(namespace, modelID, variantName)
	state, ok := a.variantStates[key]
	if !ok {
		return ThroughputVariantState{}, false
	}

	shape, _ := state.shapeTracker.Current()
	return ThroughputVariantState{
		Shape:            shape,
		ObservationReady: state.observationWindow.Ready(),
		KSpread:          state.observationWindow.KSpread(),
		SampleCount:      state.observationWindow.Len(),
		LastSanityReport: state.lastSanityReport,
	}, true
}

// --- helpers ---

// variantKey builds the map key for a variant. The null-byte delimiter is safe
// because neither Kubernetes resource names nor operator-provided model IDs can
// contain a null byte.
func variantKey(namespace, modelID, variantName string) string {
	return namespace + "\x00" + modelID + "\x00" + variantName
}

// getOrCreateVariantState returns the variantState for the given key, creating
// it with default configuration if it does not exist yet.
// Must be called with a.mu held.
func (a *ThroughputAnalyzer) getOrCreateVariantState(key string) *variantState {
	if state, ok := a.variantStates[key]; ok {
		return state
	}
	state := &variantState{
		shapeTracker: newShapeTracker(DefaultShapeChangeTolerance),
		observationWindow: newObservationWindow(
			DefaultWindowMaxSize,
			DefaultObservationMaxAge,
			DefaultMinSamples,
			DefaultMinKSpread,
			DefaultMinObservableK,
			DefaultMaxObservableK,
		),
	}
	a.variantStates[key] = state
	return state
}

// groupByVariant partitions a slice of ReplicaMetrics by VariantName.
func groupByVariant(metrics []interfaces.ReplicaMetrics) map[string][]interfaces.ReplicaMetrics {
	groups := make(map[string][]interfaces.ReplicaMetrics)
	for _, m := range metrics {
		groups[m.VariantName] = append(groups[m.VariantName], m)
	}
	return groups
}

// averageShapeMetrics computes the VLLMRequestRate-weighted mean IL, OL, and
// prefix hit rate across a slice of replica metrics. Replicas with zero or
// negative IL or OL are excluded. When all eligible replicas have zero
// VLLMRequestRate, falls back to an unweighted mean.
func averageShapeMetrics(metrics []interfaces.ReplicaMetrics) (il, ol, hitRate float64) {
	var sumIL, sumOL, sumHitRate float64 // weighted accumulators
	var sumILu, sumOLu, sumHRu float64   // unweighted fallback
	var totalWeight, count float64
	for _, m := range metrics {
		if m.AvgInputTokens <= DefaultMinTokensPerRequest || m.AvgOutputTokens <= DefaultMinTokensPerRequest {
			continue
		}
		count++
		sumILu += m.AvgInputTokens
		sumOLu += m.AvgOutputTokens
		sumHRu += m.PrefixCacheHitRate
		if m.VLLMRequestRate > 0 {
			sumIL += m.VLLMRequestRate * m.AvgInputTokens
			sumOL += m.VLLMRequestRate * m.AvgOutputTokens
			sumHitRate += m.VLLMRequestRate * m.PrefixCacheHitRate
			totalWeight += m.VLLMRequestRate
		}
	}
	if count == 0 {
		return 0, 0, 0
	}
	if totalWeight == 0 {
		return sumILu / count, sumOLu / count, sumHRu / count
	}
	return sumIL / totalWeight, sumOL / totalWeight, sumHitRate / totalWeight
}
