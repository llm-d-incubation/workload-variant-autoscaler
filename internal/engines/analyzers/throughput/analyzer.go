package throughput

import (
	"context"
	"math"
	"sync"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/engines/aggregation"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/interfaces"
	"github.com/llm-d/llm-d-workload-variant-autoscaler/internal/logging"
)

// ThroughputAnalyzer accumulates per-variant workload shape and ITL observations
// across reconcile cycles and computes a μ_dec supply vs λ_dec demand scaling signal.
// It implements interfaces.Analyzer.
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
	// role is the P/D disaggregation role ("prefill", "decode", "both", "").
	// Updated from VariantStates at the start of each Analyze call.
	role             string
	lastSanityReport SanityReport
	lastObservedAt   time.Time
	// lastFittedB is the B coefficient from the most recent successful Tier-1 OLS fit.
	// It is used as the pinned baseline in Tier-2 instead of DefaultBaselineITLSec,
	// because B reflects hardware/model characteristics rather than workload shape.
	// A shape change clears the observation window but must NOT clear lastFittedB.
	lastFittedB float64
	hasFittedB  bool
	// consecutiveGPSMismatches counts how many consecutive Analyze cycles have
	// produced a GPS mismatch for this variant. The observation window is cleared
	// when this reaches DefaultGPSMismatchClearThreshold. Always reset alongside
	// observationWindow.Clear() so it is bound to the current window's lifetime.
	consecutiveGPSMismatches int
	// set by Analyze() for VariantState() snapshots
	lastITLModel         ITLModel
	lastPerReplicaSupply float64
	lastTotalSupply      float64
	lastDemand           float64
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
	if err := ctx.Err(); err != nil {
		return nil
	}
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

		if report.Has(SanityIssueNoReplicas) {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("throughput analyzer: no replicas, skipping variant",
				"namespace", namespace,
				"modelID", modelID,
				"variant", variantName,
			)
			continue
		}
		if !report.OK() {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("throughput analyzer: sanity issues detected, some pods excluded",
				"namespace", namespace,
				"modelID", modelID,
				"variant", variantName,
				"issues", report.Issues,
				"affectedPods", report.AffectedPods,
			)
		}

		// Only healthy pods contribute to shape averaging and window observations.
		// Pods with per-replica issues (cold start, stale metrics, missing KV) are
		// excluded so one bad replica cannot block the entire variant.
		healthyMetrics := filterHealthyForShape(variantMetrics)
		if len(healthyMetrics) == 0 {
			continue
		}

		// Compute variant-average shape metrics. All replicas of the same variant
		// are expected to have the same OL and IL (same model, same config); the
		// mean handles any minor per-pod variation.
		il, ol, hitRate := averageShapeMetrics(healthyMetrics)

		shape, changed := state.shapeTracker.Observe(il, ol, hitRate)
		if changed {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("throughput analyzer: workload shape changed, clearing observation window",
				"namespace", namespace,
				"modelID", modelID,
				"variant", variantName,
				"newKVreq", shape.KVreq,
			)
			state.observationWindow.Clear()
			state.consecutiveGPSMismatches = 0
		}

		// Collect one (k*, ITL) observation per healthy replica. Per-replica variation
		// in k* provides the k-spread needed for a reliable OLS fit.
		for _, m := range healthyMetrics {
			if dropped := state.observationWindow.Add(m.KvUsageInstant, m.AvgITL, now); dropped {
				ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("throughput analyzer: observation dropped (k out of range or ITL invalid)",
					"namespace", namespace, "modelID", modelID, "variant", variantName,
					"k", m.KvUsageInstant, "itl", m.AvgITL)
			}
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
// state, then computes a supply vs demand scaling signal for each variant using
// a two-tier ITL model resolution strategy:
//
//   - Tier 1 (OLS): observation window Ready — fit ITL(k) = A·k + B via OLS.
//   - Tier 2 (constrained OLS): window not ready — fit A with B = DefaultBaselineITLSec
//     using all replica (k*, ITL_obs) points: A = Σ((ITL_i−B)·k_i) / Σ(k_i²).
//
// Demand per variant is estimated in priority order:
//  1. EPP primary: Σ ArrivalRate × AvgOutputTokens (when ArrivalRate > 0 on any replica).
//  2. vLLM fallback: VLLMRequestRate × avgOL (when EPP absent but vLLM rate is nonzero).
//  3. k*-based local: Σ k_r* × KV_max_r / KVreq / ITL(k_r*) (scale-up only; no EPP needed).
//
// Scheduler queue demand (QueueSize / (DefaultQueueDrainFactor × ITL(k_sat))) is added
// to model-level demand after all variants are processed (non-prefill roles only).
//
// TA publishes TotalSupply, TotalAnticipatedSupply, and TotalDemand on the
// returned AnalyzerResult; RequiredCapacity and SpareCapacity are left zero.
// The engine's universal threshold post-step writes RC/SC after Analyze returns.
// PendingReplicas are included in TotalAnticipatedSupply to suppress redundant
// scale-up while pods are starting. Scheduler queue demand is split across
// non-prefill roles via distributeQueueDemandByRole.
//
// For P/D disaggregated models, RoleCapacities carries per-role Total* fields
// (TotalSupply, TotalAnticipatedSupply, TotalDemand); RC/SC per role are also
// left zero for the engine post-step. Prefill TotalDemand is negligible after
// the OL guard in computeLocalDemand.
func (a *ThroughputAnalyzer) Analyze(
	ctx context.Context,
	input interfaces.AnalyzerInput,
) (*interfaces.AnalyzerResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	now := time.Now()

	// Build lookup tables from VariantStates before taking any locks.
	pendingByVariant := make(map[string]int, len(input.VariantStates))
	for _, vs := range input.VariantStates {
		pendingByVariant[vs.VariantName] = vs.PendingReplicas
	}

	// Analyze is assumed single-flight per model; concurrent VariantState() snapshots
	// may observe partial state across the lock gaps below.
	// Update variant roles so state.role is current when Observe() runs.
	a.mu.Lock()
	for _, vs := range input.VariantStates {
		key := variantKey(input.Namespace, input.ModelID, vs.VariantName)
		state := a.getOrCreateVariantState(key)
		state.role = vs.Role
	}
	a.mu.Unlock()

	// Observe updates internal state (acquires/releases a.mu internally).
	a.Observe(ctx, now, input.ModelID, input.Namespace, input.ReplicaMetrics)

	byVariant := groupByVariant(input.ReplicaMetrics)

	a.mu.Lock()
	defer a.mu.Unlock()

	var (
		anyEPP, anyGPSMismatch bool
		totalDecodeITLSat      float64
		nDecodeVariants        int
	)
	variantCapacities := make([]interfaces.VariantCapacity, 0, len(byVariant))

	for variantName, variantMetrics := range byVariant {
		key := variantKey(input.Namespace, input.ModelID, variantName)
		state, ok := a.variantStates[key]
		if !ok {
			continue
		}

		shape, hasShape := state.shapeTracker.Current()
		if !hasShape || shape.KVreq <= 0 {
			continue
		}

		// TODO(#1261): skip variant when state.lastSanityReport is not OK — stale/invalid
		// metrics from Observe() do not currently block demand computation here.
		// Gating demand on the sanity report is deferred to the per-analyzer status-return
		// PR; it requires the engine contract to accept an AnalyzerStatus opt-out signal.

		// Filter to healthy replicas for ITL model fitting and GPS verification.
		// Stale replicas (cold-start, missing fields) bias the tier-2 OLS slope A
		// upward → systematic under-provisioning. Supply counting (computeVariantSupply,
		// computeDemand) uses the unfiltered variantMetrics to include booting replicas.
		healthyMetrics := filterHealthyForShape(variantMetrics)
		model, ok := a.resolveITLModel(ctx, state, healthyMetrics, input.Namespace, input.ModelID, variantName)
		if !ok {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("throughput analyzer: no ITL model available, skipping variant",
				"namespace", input.Namespace,
				"modelID", input.ModelID,
				"variant", variantName,
			)
			continue
		}

		itlSat := model.ITLAt(DefaultKSat)
		if itlSat <= 0 {
			continue
		}

		supply, perReplicaSupply, nKV := computeVariantSupply(variantMetrics, shape, itlSat)
		if supply == 0 {
			continue
		}

		demand, isEPP := computeDemand(variantMetrics)
		// k*-based local demand: when EPP and vLLM rate are both absent, or when
		// EPP is present but yields zero usable demand (warm-up, no completions yet),
		// derive demand from observed KV utilization so a busy replica is not
		// mis-classified as idle and spuriously scaled down.
		if demand == 0 {
			demand = computeLocalDemand(variantMetrics, shape, model)
		}

		// Update state for VariantState() snapshots.
		state.lastITLModel = model
		state.lastPerReplicaSupply = perReplicaSupply
		state.lastTotalSupply = supply
		state.lastDemand = demand

		pending := pendingByVariant[variantName]
		if isEPP {
			anyEPP = true
		}
		// Track ITL(k_sat) across non-prefill variants for queue demand estimation.
		if state.role != interfaces.RolePrefill {
			totalDecodeITLSat += itlSat
			nDecodeVariants++
		}

		if checkVariantGPSMismatch(ctx, healthyMetrics, shape, model, input.Namespace, input.ModelID, variantName) {
			anyGPSMismatch = true
			state.consecutiveGPSMismatches++
			if state.consecutiveGPSMismatches >= DefaultGPSMismatchClearThreshold {
				state.observationWindow.Clear()
				state.consecutiveGPSMismatches = 0
				ctrl.LoggerFrom(ctx).Info("throughput analyzer: GPS mismatch persisted, clearing observation window for recalibration",
					"namespace", input.Namespace,
					"modelID", input.ModelID,
					"variant", variantName,
					"threshold", DefaultGPSMismatchClearThreshold,
				)
			}
		} else {
			state.consecutiveGPSMismatches = 0
		}

		// ReplicaCount is the count of KV-capable replicas (nKV), and TotalCapacity is the
		// measured supply over exactly those replicas — so TotalSupply (which the engine uses
		// for SpareCapacity) is not inflated by still-booting KV=0 replicas. Not-ready replicas
		// are already reflected in PendingReplicas (currentReplicas − readyReplicas); they count
		// toward TotalAnticipatedSupply and so still suppress RequiredCapacity during scale-out.
		// This mirrors saturation_v2 (ReplicaCount = readyCount, PendingReplicas separate) and
		// avoids double-counting booting replicas in both ReplicaCount and PendingReplicas.
		// TotalCapacity is the product ReplicaCount × PerReplicaCapacity (the VariantCapacity
		// contract, and what aggregation.SumTotalSupply recomputes); equals supply for nKV ≥ 1.
		totalCapacity := float64(nKV) * perReplicaSupply
		variantCapacities = append(variantCapacities, interfaces.VariantCapacity{
			VariantName:        variantName,
			Role:               state.role,
			ReplicaCount:       nKV,
			PendingReplicas:    pending,
			PerReplicaCapacity: perReplicaSupply,
			TotalCapacity:      totalCapacity,
			TotalDemand:        demand,
			Utilization:        safeDivide(demand, totalCapacity),
		})
	}

	// Model-level totals computed from the per-variant slice.
	// TotalAnticipatedSupply is published so the engine's post-step can compute RC/SC.
	totalSupply := aggregation.SumTotalSupply(variantCapacities)
	totalAnticipatedSupply := aggregation.SumTotalAnticipatedSupply(variantCapacities)
	totalDemand := aggregation.SumTotalDemand(variantCapacities)

	// Scheduler queue demand is decode-rate-denominated and not variant-attributed.
	// Add to model-level demand and distribute across active non-prefill roles so
	// per-role TotalDemand satisfies the linearity invariant.
	var queueDemandByRole map[string]float64
	// nDecodeVariants > 0 is guaranteed here: the loop above only increments it for
	// variants that produced supply > 0 (itlSat > 0), so totalDecodeITLSat / nDecodeVariants
	// is safe from division-by-zero.
	if nDecodeVariants > 0 {
		avgDecodeITLSat := totalDecodeITLSat / float64(nDecodeVariants)
		queueDemand := estimateQueueDemand(input.SchedulerQueue, avgDecodeITLSat, DefaultQueueDrainFactor)
		totalDemand += queueDemand
		queueDemandByRole = distributeQueueDemandByRole(queueDemand, variantCapacities)
	}

	// TA publishes raw Total* fields; RequiredCapacity and SpareCapacity are left
	// zero — the engine's universal threshold post-step writes them after Analyze returns.
	// The GPS/EPP gate that previously suppressed SpareCapacity is dropped here; without
	// it, a missing SchedulerQueue (nil) causes TotalDemand to be under-estimated and the
	// engine may compute SC > 0 when queued requests exist — a scale-down risk.
	// Restoring the gate (conditioned on SchedulerQueue == nil || anyGPSMismatch) is
	// tracked in issue #1261 and requires a per-analyzer status-return signal in the
	// engine contract. anyEPP and anyGPSMismatch are retained as placeholders for that PR.
	_ = anyEPP
	_ = anyGPSMismatch

	return &interfaces.AnalyzerResult{
		AnalyzerName:           AnalyzerName,
		ModelID:                input.ModelID,
		Namespace:              input.Namespace,
		AnalyzedAt:             now,
		VariantCapacities:      variantCapacities,
		TotalSupply:            totalSupply,
		TotalAnticipatedSupply: totalAnticipatedSupply,
		TotalDemand:            totalDemand,
		Utilization:            safeDivide(totalDemand, totalSupply),
		RoleCapacities:         aggregateRoleCapacities(variantCapacities, queueDemandByRole),
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
		ITLModel:         state.lastITLModel,
		PerReplicaSupply: state.lastPerReplicaSupply,
		TotalSupply:      state.lastTotalSupply,
		Demand:           state.lastDemand,
		Role:             state.role,
		LastFittedB:      state.lastFittedB,
		HasFittedB:       state.hasFittedB,
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

// resolveITLModel returns the ITL model to use for a variant using a two-tier strategy:
//
//   - Tier 1: OLS fit from the observation window (when Ready).
//   - Tier 2: constrained OLS with B pinned. B is taken from the last successful Tier-1 fit
//     (state.lastFittedB) when one exists, because B reflects hardware/model characteristics
//     that survive workload-shape changes. Falls back to DefaultBaselineITLSec when no
//     prior fit exists. Only possible when at least one replica has k* > 0; replicas with
//     k* = 0 (idle) carry no ITL signal and are excluded.
//
// When replicas are present but all are idle (k* = 0), both tiers fail and we return (zero, false).
// A future tier-3 (knowledge store) path for the scale-from-zero case will be added once Analyze()
// is extended to iterate variants with state but no current replica metrics.
//
// Must be called with a.mu held.
func (a *ThroughputAnalyzer) resolveITLModel(ctx context.Context, state *variantState, metrics []interfaces.ReplicaMetrics, namespace, modelID, variantName string) (ITLModel, bool) {
	// Tier 1: OLS fit.
	if state.observationWindow.Ready() {
		obs := state.observationWindow.Observations()
		if model, ok := FitITLModel(obs); ok {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("throughput analyzer: tier-1 OLS fit",
				"namespace", namespace, "modelID", modelID, "variant", variantName,
				"A", model.A, "B", model.B, "samples", len(obs),
			)
			state.lastFittedB = model.B
			state.hasFittedB = true
			return model, true
		}
		ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("throughput analyzer: tier-1 OLS fit failed, trying tier-2",
			"namespace", namespace, "modelID", modelID, "variant", variantName,
			"samples", len(obs),
		)
	}

	// Tier 2: constrained OLS with B pinned.
	// Minimize Σ(ITL_i − A·k_i − B)² → A = Σ((ITL_i − B)·k_i) / Σ(k_i²).
	// Using per-replica (k*, ITL) directly is better than collapsing to a centroid
	// when replicas have spread k* values — it is the same least-squares criterion
	// as tier-1 OLS but with B pinned instead of fitted.
	baselineB := DefaultBaselineITLSec
	if state.hasFittedB {
		baselineB = state.lastFittedB
	}
	var numerator, sumK2 float64
	var n float64
	for _, m := range metrics {
		if m.KvUsageInstant > 0 && m.AvgITL > 0 {
			numerator += (m.AvgITL - baselineB) * m.KvUsageInstant
			sumK2 += m.KvUsageInstant * m.KvUsageInstant
			n++
		}
	}
	if n > 0 && sumK2 > 0 {
		A := numerator / sumK2
		if A > 0 {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("throughput analyzer: tier-2 constrained OLS fit",
				"namespace", namespace, "modelID", modelID, "variant", variantName,
				"A", A, "B", baselineB, "replicas", int(n),
			)
			return ITLModel{A: A, B: baselineB}, true
		}
	}
	return ITLModel{}, false
}

// computeDemand aggregates λ_dec (decode token demand in tokens/sec) across replicas.
//
// Primary path (EPP deployed): Σ ArrivalRate_r × AvgOutputTokens_r.
// Fallback path (EPP absent): Σ VLLMRequestRate_r × AvgOutputTokens_r.
//
// Both paths use the per-replica product rather than sumRate × avgOL to avoid
// averaging-the-averages: replicas with higher throughput contribute proportionally
// more to λ_dec without requiring raw histogram sums.
//
// Returns (λ_dec, isEPP). isEPP is true when at least one replica reports ArrivalRate > 0.
// When EPP is present but yields zero usable demand (warm-up: ArrivalRate > 0 but
// AvgOutputTokens == 0), the function falls through to the vLLM proxy so the caller
// can use computeLocalDemand when both paths yield zero. isEPP still reflects "EPP
// present" so the anyEPP tracking in Analyze is unaffected.
func computeDemand(metrics []interfaces.ReplicaMetrics) (float64, bool) {
	var lambdaDec float64
	var isEPP bool
	for _, m := range metrics {
		if m.ArrivalRate > 0 {
			isEPP = true // EPP present, even if AvgOutputTokens is not yet observed (warm-up)
			if m.AvgOutputTokens > 0 {
				lambdaDec += m.ArrivalRate * m.AvgOutputTokens
			}
		}
	}
	if lambdaDec > 0 {
		return lambdaDec, isEPP // EPP present and gave usable demand
	}
	// EPP absent, OR EPP present but zero usable demand (warm-up, no completions yet):
	// fall through to the vLLM request-rate proxy.
	// Σ VLLMRequestRate_r × AvgOutputTokens_r mirrors the EPP formula structure and
	// correctly weights each replica's OL by its own throughput.
	var lambdaDecFallback float64
	for _, m := range metrics {
		if m.VLLMRequestRate > 0 && m.AvgOutputTokens > 0 {
			lambdaDecFallback += m.VLLMRequestRate * m.AvgOutputTokens
		}
	}
	return lambdaDecFallback, isEPP // isEPP still reflects "EPP present"
}

// computeLocalDemand estimates decode token demand from per-replica k* observations
// when the EPP ArrivalRate and vLLM request rate are both unavailable.
//
//	λ_local = Σ_r (k_r* × KV_max_r / KVreq) / ITL(k_r*)
//
// Each replica's in-flight request count N_r = k_r* × KV_max_r / KVreq is divided
// by ITL(k_r*) to approximate its current throughput. Replicas with k* = 0 or
// KV_max = 0 are excluded (no meaningful signal at idle).
// This path is scale-up only: k*-based demand may undercount arriving load
// without EPP. The engine post-step determines SC from the published totals.
func computeLocalDemand(metrics []interfaces.ReplicaMetrics, shape WorkloadShape, model ITLModel) float64 {
	if shape.KVreq <= 0 || shape.AvgOutputTokens <= DefaultMinDecodeOLForLocalDemand {
		return 0
	}
	var total float64
	for _, m := range metrics {
		if m.KvUsageInstant <= 0 || m.TotalKvCapacityTokens <= 0 {
			continue
		}
		itlAtK := model.ITLAt(m.KvUsageInstant)
		if itlAtK <= 0 {
			continue
		}
		total += m.KvUsageInstant * float64(m.TotalKvCapacityTokens) / shape.KVreq / itlAtK
	}
	return total
}

// estimateQueueDemand converts the scheduler queue depth into an equivalent
// decode token demand rate (tokens/sec).
//
//	drain_time = QueueDrainFactor × ITL(k_sat) × avgOL
//	λ_queue    = QueueSize × avgOL / drain_time
//	           = QueueSize / (QueueDrainFactor × ITL(k_sat))   (avgOL cancels)
//
// ITL(k_sat) is used as the reference latency so that admitted queue demand
// bounds per-request queueing time to ≤ QueueDrainFactor × ITL(k_sat) × avgOL.
func estimateQueueDemand(sq *interfaces.SchedulerQueueMetrics, itlSat, drainFactor float64) float64 {
	if sq == nil || sq.QueueSize <= 0 || itlSat <= 0 || drainFactor <= 0 {
		return 0
	}
	return float64(sq.QueueSize) / (drainFactor * itlSat)
}

// computeVariantSupply computes the aggregate μ_dec_sat supply for a variant.
//
// Per replica: N_dec_sat = DefaultKSat × KV_max / KVreq; μ_dec_sat = N_dec_sat / itlSat.
// Returns (totalSupply Σμ_dec_sat, perReplicaSupply mean(μ_dec_sat), nKV count of
// KV-capable replicas). All are zero when no replica has KV capacity data.
func computeVariantSupply(metrics []interfaces.ReplicaMetrics, shape WorkloadShape, itlSat float64) (total, perReplica float64, nKV int) {
	var sum float64
	var n int
	for _, m := range metrics {
		if m.TotalKvCapacityTokens <= 0 {
			continue
		}
		kvMax := float64(m.TotalKvCapacityTokens)
		nSat := DefaultKSat * kvMax / shape.KVreq
		sum += nSat / itlSat
		n++
	}
	if n == 0 {
		return 0, 0, 0
	}
	return sum, sum / float64(n), n
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

// filterHealthyForShape returns only the replicas that pass all per-replica
// sanity checks. Replicas with cold-start (ITL=0), stale metrics, or missing
// KV capacity are excluded so a single bad pod cannot block the variant.
func filterHealthyForShape(metrics []interfaces.ReplicaMetrics) []interfaces.ReplicaMetrics {
	healthy := make([]interfaces.ReplicaMetrics, 0, len(metrics))
	for _, m := range metrics {
		if len(checkReplicaMetrics(m)) == 0 {
			healthy = append(healthy, m)
		}
	}
	return healthy
}

// safeDivide returns num/denom, or 0 when denom is zero.
func safeDivide(num, denom float64) float64 {
	if denom == 0 {
		return 0
	}
	return num / denom
}

// checkVariantGPSMismatch compares each replica's observed GenerationTokenRate (GPS_obs,
// i.e. μ_dec^obs) against the model-predicted decode rate μ_dec(k*) = N_dec(k*) / ITL(k*).
// Returns true when any replica exceeds DefaultGPSMismatchThresholdPct at k* ≥
// DefaultGPSMinKForVerification, indicating the ITL model may be wrong.
//
// When a mismatch is detected near saturation (k* ≥ DefaultKSat − DefaultNearKSatMargin),
// additional diagnostics are logged to distinguish between two root causes:
//   - ITL model drift / bad data points: observed AvgITL deviates from ITL(k*).
//   - Shape mismatch: ITL fits well but GPS × AvgITL disagrees with KV-derived N_dec,
//     suggesting IL, OL, or prefix-hit-rate parameters are wrong.
func checkVariantGPSMismatch(
	ctx context.Context,
	metrics []interfaces.ReplicaMetrics,
	shape WorkloadShape,
	model ITLModel,
	namespace, modelID, variantName string,
) bool {
	if shape.KVreq <= 0 {
		return false
	}
	mismatch := false
	for _, m := range metrics {
		if m.GenerationTokenRate <= 0 || m.KvUsageInstant < DefaultGPSMinKForVerification {
			continue
		}
		if m.TotalKvCapacityTokens <= 0 {
			continue
		}
		itlAtK := model.ITLAt(m.KvUsageInstant)
		if itlAtK <= 0 {
			continue
		}
		nDec := m.KvUsageInstant * float64(m.TotalKvCapacityTokens) / shape.KVreq
		muDecModel := nDec / itlAtK
		if muDecModel <= 0 {
			continue
		}
		gpsErrPct := math.Abs(muDecModel-m.GenerationTokenRate) / m.GenerationTokenRate * 100
		if gpsErrPct <= DefaultGPSMismatchThresholdPct {
			continue
		}
		mismatch = true
		ctrl.LoggerFrom(ctx).Info("throughput analyzer: GPS mismatch detected",
			"namespace", namespace,
			"modelID", modelID,
			"variant", variantName,
			"pod", m.PodName,
			"k", m.KvUsageInstant,
			"GPSObs", m.GenerationTokenRate,
			"muDecModel", muDecModel,
			"gpsErrPct", gpsErrPct,
		)

		// Near k_sat: run deeper diagnostics to identify root cause.
		if m.KvUsageInstant < DefaultKSat-DefaultNearKSatMargin || m.AvgITL <= 0 {
			continue
		}
		itlResidual := math.Abs(m.AvgITL-itlAtK) / m.AvgITL
		if itlResidual > DefaultNearKSatITLResidualThreshold {
			ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("throughput analyzer: near-k_sat ITL residual high (model drift or bad data)",
				"namespace", namespace,
				"modelID", modelID,
				"variant", variantName,
				"pod", m.PodName,
				"k", m.KvUsageInstant,
				"avgITLObs", m.AvgITL,
				"itlModel", itlAtK,
				"itlResidualPct", itlResidual*100,
			)
		} else {
			// ITL model matches observed ITL but GPS disagrees: N_dec derivation
			// (shape.KVreq via IL/OL/hit-rate) may be wrong.
			nDecGPS := m.GenerationTokenRate * m.AvgITL
			nDecErrPct := math.Abs(nDec-nDecGPS) / nDec * 100
			if nDecErrPct > DefaultNearKSatNDecResidualThreshold*100 {
				ctrl.LoggerFrom(ctx).V(logging.DEBUG).Info("throughput analyzer: near-k_sat N_dec mismatch (shape wrong?)",
					"namespace", namespace,
					"modelID", modelID,
					"variant", variantName,
					"pod", m.PodName,
					"k", m.KvUsageInstant,
					"nDecModel", nDec,
					"nDecGPS", nDecGPS,
					"nDecErrPct", nDecErrPct,
					"hint", "check AvgInputTokens/AvgOutputTokens/PrefixCacheHitRate",
				)
			}
		}
	}
	return mismatch
}

// distributeQueueDemandByRole splits queueDemand evenly across active non-prefill
// roles derived from vcs. Queue demand is decode-rate-denominated so prefill roles
// are excluded. Returns nil when queueDemand is zero or no non-prefill roles exist.
func distributeQueueDemandByRole(queueDemand float64, vcs []interfaces.VariantCapacity) map[string]float64 {
	if queueDemand == 0 {
		return nil
	}
	roles := make(map[string]struct{})
	for _, vc := range vcs {
		role := vc.Role
		if role == "" {
			role = interfaces.RoleBoth
		}
		if role != interfaces.RolePrefill {
			roles[role] = struct{}{}
		}
	}
	if len(roles) == 0 {
		return nil
	}
	share := queueDemand / float64(len(roles))
	result := make(map[string]float64, len(roles))
	for role := range roles {
		result[role] = share
	}
	return result
}

// aggregateRoleCapacities groups variant capacities by P/D role and computes
// per-role raw Total* fields. queueDemandByRole adds queue demand to each role's
// TotalDemand (nil is safe — treated as zero). Returns nil for non-disaggregated
// models (all variants role "" or "both"). RequiredCapacity and SpareCapacity are
// left zero — the engine's universal threshold post-step writes them.
func aggregateRoleCapacities(vcs []interfaces.VariantCapacity, queueDemandByRole map[string]float64) map[string]interfaces.RoleCapacity {
	byRole := aggregation.AggregateByRole(vcs)
	// Non-disaggregated: only a "both" bucket (or nothing) — no per-role breakdown.
	if _, hasBoth := byRole[interfaces.RoleBoth]; len(byRole) == 0 || (len(byRole) == 1 && hasBoth) {
		return nil
	}

	result := make(map[string]interfaces.RoleCapacity, len(byRole))
	for role, t := range byRole {
		result[role] = interfaces.RoleCapacity{
			Role:                   role,
			TotalSupply:            t.TotalSupply,
			TotalAnticipatedSupply: t.TotalAnticipatedSupply,
			TotalDemand:            t.TotalDemand + queueDemandByRole[role],
		}
	}
	return result
}
